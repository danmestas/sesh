package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/danmestas/EdgeSync/hub"
)

// Bootstrap is one source of truth for how a fresh session's Fossil repo
// gets its initial state. Three paths fork here:
//
//   - peer session has already committed and the hub mirrored it via
//     autosync → clone-from-hub so the new session starts convergent;
//   - first session in the project (hub empty) → seed-from-cwd after
//     NewHub binds;
//   - the per-session repo file pre-existed → no bootstrap, retain
//     whatever commits already accumulated.
//
// MakePlan is the pure decision tree, Execute is the effectful runner.
// Splitting them keeps the fork unit-testable without spinning up hubs.
// When the hub's on-disk project-code disagrees with the local pin
// (issue #26 / #34), Execute adopts the hub's value into
// <cwd>/.sesh/project-code before EdgeSync's seed-from-upstream runs —
// the hub is shared across all sessions in the project and is the
// natural source of truth.
//
// ProbeHub composes ReadHubInfo + ProjectCode (cli/hubinfo.go) — the
// atomic file I/O and libfossil read both live behind that module so
// adding a new published URL is a one-line struct change.

// Source is the chosen bootstrap source for a fresh repo.
type Source int

const (
	SourceNone Source = iota
	SourceHub
	SourceGitWorktree
)

// World is the input to MakePlan. All fields are derived by the caller;
// MakePlan does no I/O of its own and so can be exercised under unit
// tests without filesystem state.
type World struct {
	// LocalProjectCode is the project-code pinned at <cwd>/.sesh/project-code.
	LocalProjectCode string

	// HubFossilURL is the hub's Fossil HTTP xfer endpoint; "" iff no hub
	// has published a URL or the hub has no content yet. Pair with
	// HubProjectCode — ProbeHub returns these together.
	HubFossilURL string

	// HubProjectCode is the project-code read from ~/.sesh/hub.repo's
	// fossil config table; "" iff no hub or hub has no content.
	HubProjectCode string

	// FreshRepo is true iff the session's own Fossil repo file does not
	// yet exist on disk. Bootstrap only fires for fresh repos — a re-up
	// of an existing session keeps whatever commits accumulated.
	FreshRepo bool

	// SeedMode controls what seedFromGitWorktree commits when the
	// bootstrap source is SourceGitWorktree. Ignored for other sources.
	SeedMode SeedMode
}

// Plan is the decision MakePlan returned. Source dictates which (if any)
// bootstrap action Execute will run.
type Plan struct {
	Source       Source
	HubFossilURL string   // populated iff Source == SourceHub
	SeedMode     SeedMode // populated iff Source == SourceGitWorktree
}

// MakePlan computes the bootstrap plan from world state. Pure — same
// inputs always produce the same plan.
//
// Decision tree:
//
//   - !FreshRepo         → {Source: None}
//   - HubFossilURL set   → {Source: Hub}
//   - else               → {Source: GitWorktree}
//
// Project-code drift between the local pin and the hub's on-disk repo
// is reconciled at Execute time (the hub wins; see Execute), so MakePlan
// does not branch on HubProjectCode.
func MakePlan(w World) (Plan, error) {
	if !w.FreshRepo {
		return Plan{Source: SourceNone}, nil
	}
	if w.HubFossilURL != "" {
		return Plan{Source: SourceHub, HubFossilURL: w.HubFossilURL}, nil
	}
	return Plan{Source: SourceGitWorktree, SeedMode: w.SeedMode}, nil
}

// Deps carries the I/O Execute needs. Ctx scopes any commit operations
// to the session's lifetime; Hub is the already-bound session hub
// (NewHub has returned, only required by the SourceGitWorktree branch);
// Cwd is the working directory the seed snapshot is taken from.
// RepoPath is informational, surfaced in slog so the operator can
// locate the affected file. HubProjectCode is the project-code probed
// from the hub's on-disk repo and is consulted by the SourceHub branch
// to adopt-on-drift into <cwd>/.sesh/project-code; empty means "no hub
// content / nothing to adopt".
type Deps struct {
	Ctx            context.Context
	Hub            *hub.Hub
	Cwd            string
	RepoPath       string
	HubProjectCode string
}

// Execute runs the action MakePlan chose.
//
// For SourceHub, if the hub published a project-code that differs from
// the local pin at <cwd>/.sesh/project-code, Execute adopts the hub's
// value before EdgeSync's seed-from-upstream runs — the hub is shared
// across all sessions in the project and is the natural source of
// truth. This means Execute must run BEFORE hub.NewHub for SourceHub.
// The actual clone happens inside hub.NewHub's SeedFromUpstream; that
// step is keyed off the now-adopted project-code so the upstream
// project-code agrees with what the session's Fossil repo expects.
//
// For SourceGitWorktree, the session's hub must already be bound
// (d.Hub != nil) because the seed commits through it.
//
// Adoption is idempotent: if the file already matches the hub's code,
// Execute is a no-op.
func Execute(p Plan, d Deps) error {
	switch p.Source {
	case SourceNone:
		slog.Info("fossil repo pre-existed; not re-seeding", "path", d.RepoPath)
		return nil
	case SourceHub:
		if err := adoptHubProjectCode(d.Cwd, d.HubProjectCode); err != nil {
			return err
		}
		slog.Info("fossil repo cloned from hub upstream",
			"path", d.RepoPath, "upstream", p.HubFossilURL)
		return nil
	case SourceGitWorktree:
		return seedFromGitWorktree(d.Ctx, d.Hub, d.Cwd, p.SeedMode)
	}
	return fmt.Errorf("bootstrap: unknown source %d", p.Source)
}

// adoptHubProjectCode updates <cwd>/.sesh/project-code to hubCode iff
// the file currently disagrees with it. Empty hubCode is a no-op — the
// hub probe surfaces "" when the hub has no content yet, and there's
// nothing to adopt from. A missing-or-unreadable local file is treated
// as "current value is empty," which triggers a write to seed the pin.
func adoptHubProjectCode(cwd, hubCode string) error {
	if hubCode == "" {
		return nil
	}
	path := projectCodePath(cwd)
	currentBytes, _ := os.ReadFile(path)
	current := strings.TrimSpace(string(currentBytes))
	if current == hubCode {
		return nil
	}
	slog.Info("adopting hub project-code",
		"hub", hubCode, "was_pinned_to", current, "path", path)
	if err := writeAtomic(path, hubCode+"\n"); err != nil {
		return fmt.Errorf("adopt hub project-code: %w", err)
	}
	return nil
}

// ProbeHub reads the hub's published Fossil URL and on-disk project-code
// so MakePlan can decide between hub-clone and seed-from-cwd. Returns
// ("","",nil) for any "hub absent or empty" case — the caller treats
// that as "no hub content" and proceeds to seed-from-cwd.
//
// Composition: ReadHubInfo surfaces the atomically-published URLs;
// ProjectCode reads the hub's libfossil config. Either coming back
// empty short-circuits to ("","",nil) because the caller needs BOTH a
// reachable Fossil URL AND a settled project-code to clone-from-hub
// safely.
//
// Errors are reserved for unexpected I/O failures (a present-but-
// unreadable URL file, a SQL error). The caller logs and treats those
// as "no hub content" — the safer default, since any duplicate seed
// gets reconciled by autosync.
func ProbeHub() (hubURL, hubProjectCode string, err error) {
	stateDir, err := seshHome()
	if err != nil {
		return "", "", err
	}
	info, _, err := ReadHubInfo(stateDir)
	if err != nil {
		return "", "", fmt.Errorf("read hub info: %w", err)
	}
	if info.FossilURL == "" {
		return "", "", nil
	}
	code, err := ProjectCode(stateDir)
	if err != nil {
		return "", "", err
	}
	if code == "" {
		return "", "", nil
	}
	return info.FossilURL, code, nil
}
