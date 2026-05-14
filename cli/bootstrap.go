package cli

import (
	"context"
	"fmt"
	"log/slog"

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
// MakePlan is the pure decision tree; Apply is the effectful runner.
// Splitting them keeps the fork unit-testable without spinning up hubs.
//
// Project-code reconciliation (adopt the hub's value into the local
// pin when they drift; issue #26 / #34) is a separate concern handled
// by ResolveProjectCode in paths.go. It runs BEFORE the hub binds; by
// the time Apply is called the hub already exists and the project-code
// is settled, so Apply only handles the in-session content bootstrap.
//
// ProbeHub composes ReadHubInfo + ReadHubProjectCode (cli/hubinfo.go)
// — the atomic file I/O and libfossil read both live behind that module
// so adding a new published URL is a one-line struct change.

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

// Deps carries the I/O Apply needs. Cwd is the working directory the
// seed snapshot is taken from. RepoPath is informational, surfaced in
// slog so the operator can locate the affected file.
type Deps struct {
	Cwd      string
	RepoPath string
}

// Apply runs the post-hub action MakePlan chose. The hub is a required
// parameter — Apply only runs after NewHub returns, when the session
// has a bound libfossil writer to commit through (SourceGitWorktree)
// and the SourceHub clone has already been performed by hub.NewHub's
// SeedFromUpstream path.
//
// Source dispatch:
//
//   - SourceNone: the session's repo file pre-existed; log and return.
//   - SourceHub: hub.NewHub's clone-from-upstream already ran; log and
//     return. Project-code reconciliation (adopting the hub's value
//     when it differed from the local pin) happened earlier in
//     ResolveProjectCode, before this Apply call.
//   - SourceGitWorktree: commit the cwd worktree as the initial Fossil
//     check-in via h.Commit.
func Apply(ctx context.Context, p Plan, h *hub.Hub, d Deps) error {
	switch p.Source {
	case SourceNone:
		slog.Info("fossil repo pre-existed; not re-seeding", "path", d.RepoPath)
		return nil
	case SourceHub:
		slog.Info("fossil repo cloned from hub upstream",
			"path", d.RepoPath, "upstream", p.HubFossilURL)
		return nil
	case SourceGitWorktree:
		return seedFromGitWorktree(ctx, h, d.Cwd, p.SeedMode)
	}
	return fmt.Errorf("bootstrap: unknown source %d", p.Source)
}

// HubProbe is the result of ProbeHub. Present=true means the hub has
// both a reachable Fossil HTTP xfer URL AND at least one check-in (the
// project-code is settled). Both are needed for clone-from-hub to
// succeed; Present=false short-circuits to seed-from-cwd in MakePlan.
//
// FossilURL and ProjectCode are populated iff Present=true.
type HubProbe struct {
	Present     bool
	FossilURL   string
	ProjectCode string
}

// ProbeHub reads the hub's published Fossil URL and on-disk project-code
// so MakePlan can decide between hub-clone and seed-from-cwd. Returns
// HubProbe{Present: false} on the expected "no usable hub content yet"
// case (no hub running, hub mid-boot, or hub bound but never committed) —
// the caller proceeds to seed-from-cwd.
//
// err != nil is reserved for real I/O failures: a present-but-unreadable
// hub.fossil.url file, a SQL error reading the libfossil config, etc.
// The caller MUST surface these to the operator rather than silently
// degrade to seed-from-cwd. A corrupt hub.repo masquerading as "no hub
// content" would overwrite state the operator wanted preserved.
//
// Composition: ReadHubInfo surfaces the atomically-published URLs;
// ReadHubProjectCode reads the hub's libfossil config. Either coming
// back empty short-circuits to Present=false because the caller needs
// BOTH a reachable Fossil URL AND a settled project-code to
// clone-from-hub safely.
func ProbeHub() (HubProbe, error) {
	stateDir, err := seshHome()
	if err != nil {
		return HubProbe{}, err
	}
	info, err := ReadHubInfo(stateDir)
	if err != nil {
		return HubProbe{}, fmt.Errorf("read hub info: %w", err)
	}
	if info.FossilURL == "" {
		return HubProbe{}, nil
	}
	code, err := ReadHubProjectCode(stateDir)
	if err != nil {
		return HubProbe{}, fmt.Errorf("read hub project-code: %w", err)
	}
	if code == "" {
		return HubProbe{}, nil
	}
	return HubProbe{
		Present:     true,
		FossilURL:   info.FossilURL,
		ProjectCode: code,
	}, nil
}
