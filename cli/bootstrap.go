package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
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
// Splitting them keeps the fork unit-testable without spinning up hubs
// and lets the conflict case (project-code drift between the local pin
// and the hub's on-disk repo, issue #26) abort with an actionable
// message before any hub bring-up.
//
// The hub-project-code probe currently lives here in ProbeHub. When
// issue #29 (HubInfo) lands, that probe moves behind HubInfo.ProjectCode()
// and ProbeHub goes away; the Plan/Execute split stays put.

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
// bootstrap action Execute will run. Conflict is non-nil iff the caller
// should abort with the actionable message instead of continuing into
// hub bring-up.
type Plan struct {
	Source       Source
	HubFossilURL string   // populated iff Source == SourceHub
	SeedMode     SeedMode // populated iff Source == SourceGitWorktree
	Conflict     *Conflict
}

// Conflict carries everything the user needs to resolve a project-code
// drift between the local pin and the hub's on-disk repo. Kind is a
// stable enum string for future tooling; Message is the human-actionable
// text the caller surfaces verbatim.
type Conflict struct {
	Kind     string
	Message  string
	LocalPin string
	HubPin   string
}

// MakePlan computes the bootstrap plan from world state. Pure — same
// inputs always produce the same plan.
//
// Decision tree:
//
//   - !FreshRepo                                    → {Source: None}
//   - HubFossilURL set && HubProjectCode set
//       && HubProjectCode != LocalProjectCode       → {Conflict}
//   - HubFossilURL set                              → {Source: Hub}
//   - else                                          → {Source: GitWorktree}
//
// The drift check explicitly requires HubProjectCode to be non-empty.
// ProbeHub keeps HubFossilURL and HubProjectCode paired today, but
// guarding here means a future probe variant that surfaces one without
// the other won't fabricate a phantom conflict against "".
func MakePlan(w World) (Plan, error) {
	if !w.FreshRepo {
		return Plan{Source: SourceNone}, nil
	}
	if w.HubFossilURL != "" && w.HubProjectCode != "" && w.HubProjectCode != w.LocalProjectCode {
		return Plan{
			Conflict: &Conflict{
				Kind:     "project-code-drift",
				Message:  formatDriftMessage(w.LocalProjectCode, w.HubProjectCode),
				LocalPin: w.LocalProjectCode,
				HubPin:   w.HubProjectCode,
			},
		}, nil
	}
	if w.HubFossilURL != "" {
		return Plan{Source: SourceHub, HubFossilURL: w.HubFossilURL}, nil
	}
	return Plan{Source: SourceGitWorktree, SeedMode: w.SeedMode}, nil
}

// Deps carries the I/O Execute needs. Ctx scopes any commit operations
// to the session's lifetime; Hub is the already-bound session hub
// (NewHub has returned); Cwd is the working directory the seed snapshot
// is taken from. RepoPath is informational, surfaced in slog so the
// operator can locate the affected file.
type Deps struct {
	Ctx      context.Context
	Hub      *hub.Hub
	Cwd      string
	RepoPath string
}

// Execute runs the action MakePlan chose. The hub-clone path is a no-op
// here because the actual clone is wired through hub.NewHub's
// SeedFromUpstream — by the time Execute runs, NewHub has already
// fetched the upstream content. Execute exists so the caller has one
// callsite to invoke whatever the bootstrap decided to do, including
// the case where the decision was "do nothing."
func Execute(p Plan, d Deps) error {
	switch p.Source {
	case SourceNone:
		slog.Info("fossil repo pre-existed; not re-seeding", "path", d.RepoPath)
		return nil
	case SourceHub:
		slog.Info("fossil repo cloned from hub upstream",
			"path", d.RepoPath, "upstream", p.HubFossilURL)
		return nil
	case SourceGitWorktree:
		return seedFromGitWorktree(d.Ctx, d.Hub, d.Cwd, p.SeedMode)
	}
	return fmt.Errorf("bootstrap: unknown source %d", p.Source)
}

// ProbeHub reads the hub's published URL and on-disk project-code so
// MakePlan can decide between hub-clone and seed-from-cwd. Returns
// ("","",nil) for any "hub absent or empty" case — the caller treats
// that as "no hub content" and proceeds to seed-from-cwd.
//
// SQLite WAL mode lets this read coexist with the hub's open writer; the
// read-only pragma eliminates contention. Schema is libfossil's:
// event.type='ci' marks a check-in, and project-code lives in the
// config table.
//
// Errors are reserved for unexpected I/O failures (a present-but-unreadable
// URL file, a stat failure that isn't ENOENT, a SQL error). The caller
// logs and treats those as "no hub content" — the safer default, since
// any duplicate seed gets reconciled by autosync.
//
// When issue #29 (HubInfo) lands, the project-code read moves behind
// HubInfo.ProjectCode().
func ProbeHub() (hubURL, hubProjectCode string, err error) {
	urlPath, err := hubFossilURLPath()
	if err != nil {
		return "", "", err
	}
	urlBytes, err := os.ReadFile(urlPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read hub.fossil.url: %w", err)
	}
	hubURL = strings.TrimSpace(string(urlBytes))
	if hubURL == "" {
		return "", "", nil
	}

	repoPath, err := hubRepoPath()
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(repoPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("stat hub repo: %w", err)
	}

	db, err := sql.Open("sqlite", "file:"+repoPath+"?_pragma=mode(ro)")
	if err != nil {
		return "", "", fmt.Errorf("open hub repo: %w", err)
	}
	defer db.Close()

	var commits int
	if err := db.QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&commits); err != nil {
		return "", "", fmt.Errorf("count check-ins: %w", err)
	}
	if commits == 0 {
		return "", "", nil
	}

	if err := db.QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&hubProjectCode); err != nil {
		return "", "", fmt.Errorf("read project-code: %w", err)
	}
	return hubURL, hubProjectCode, nil
}

// formatDriftMessage builds the user-visible conflict text. Paths are
// rendered in their conventional display form (`.sesh/project-code`
// relative to cwd; `~/.sesh/hub.repo` with literal tilde) rather than
// fully-resolved absolute paths — the recovery commands are intended
// to be copy-pasted into the same shell the operator is already in.
func formatDriftMessage(localPin, hubPin string) string {
	return fmt.Sprintf(`sesh up: project-code drift detected
  local pin   .sesh/project-code  = %s
  hub repo    ~/.sesh/hub.repo    = %s

Pick a side and re-run:

  # treat hub as canonical (other sessions in this project agree with it):
  rm .sesh/project-code

  # treat local cwd as canonical (you want a fresh hub for this project):
  sesh hub stop && rm -rf ~/.sesh/hub.repo*
  sesh hub serve`, localPin, hubPin)
}
