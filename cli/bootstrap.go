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
	// Registering the operator's OS $USER in the seeded fossil user
	// table is independent of which bootstrap source we picked: every
	// source ends with a usable repo the operator (or a worker spawned
	// under the operator's UID) may want to `fossil commit` into via
	// the fossil CLI. The CLI path enforces the user-table check;
	// libfossil-direct paths (cli/seed.go's hub.Commit) do not, so
	// seeded-but-empty user tables silently break the CLI path. See
	// sesh#77 for the failure mode and the fossil-worker accessory
	// protocol that drove it.
	//
	// registerOperatorUser is idempotent (HasUser pre-check) and logs+
	// continues on any failure — seed-blocking on user-table state
	// would be worse UX than the late `cannot determine user` error
	// the operator already tolerates today.
	if err := registerOperatorUser(h); err != nil {
		slog.Warn("operator user registration failed (continuing)", "err", err)
	}

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

// registerOperatorUser ensures the operator's OS user ($USER) exists in
// the bound hub's fossil user table with check-in capability ('i'). The
// fossil CLI uses $USER to derive the committer login, and rejects
// `fossil commit` with "cannot determine user" when that login is not
// in the user table. cli/seed.go's libfossil-direct path bypasses that
// check, so without this call a freshly-seeded repo can be committed
// to via libfossil but NOT via the fossil CLI — which is exactly the
// path real workers take under the fossil-worker accessory.
//
// Idempotency: a HasUser pre-check skips the AddUser call when the
// user is already present. AddUser also surfaces an "already exists"
// error from libfossil's UNIQUE constraint when racing — we treat that
// as success so concurrent `sesh up` invocations against the same
// project repo don't fight.
//
// Capability 'i' (check-in) is the minimum useful — enough to commit
// but not enough to administer the repo. The operator can elevate
// manually via `fossil user capabilities` if they need 's' or 'a'.
//
// Empty / unusable $USER (container with no USER env, login containing
// shell metacharacters, etc.) is logged + skipped, not fatal: the seed
// itself succeeds and the operator surfaces the gap later if they hit
// `fossil commit`. Refusing to seed because $USER looks weird is worse
// UX than letting the seed land and waving the warning.
func registerOperatorUser(h *hub.Hub) error {
	if h == nil {
		return fmt.Errorf("hub is nil")
	}
	login := strings.TrimSpace(os.Getenv("USER"))
	if login == "" {
		slog.Warn("operator $USER empty; skipping fossil user registration " +
			"— `fossil commit` from a worktree will hit 'cannot determine user'")
		return nil
	}
	// Reject obviously-broken logins. Fossil's user table tolerates a
	// lot, but a login with control chars or path separators would just
	// break the eventual `fossil commit` lookup. The validateLabel
	// matrix elsewhere in this package is overkill for usernames; a
	// minimal sanity check is enough.
	if strings.ContainsAny(login, "\x00\n\r/\\") {
		slog.Warn("operator $USER contains invalid characters; "+
			"skipping fossil user registration", "user", login)
		return nil
	}
	if h.HasUser(login) {
		slog.Debug("operator already in fossil user table", "user", login)
		return nil
	}
	if err := h.AddUser(hub.User{Login: login, Caps: "i"}); err != nil {
		// Race: another `sesh up` (or this run's retry) won the
		// AddUser. HasUser is the strongest available
		// post-condition; if it now returns true, treat as success.
		if h.HasUser(login) {
			return nil
		}
		return fmt.Errorf("add user %q: %w", login, err)
	}
	slog.Info("registered operator in fossil user table", "user", login, "caps", "i")
	return nil
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
