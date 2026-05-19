package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// WorkerCwdCmd resolves the absolute path of a provisioned fossil
// checkout for <label>, so a worker spawner (orch-spawn et al.) can land
// the worker's cwd inside it without re-running the provisioning side
// effects baked into `sesh worktree`.
//
// Contract:
//
//   - `sesh worker-cwd <label>`                  — resolves <cwd>/.sesh/checkouts/<label>/, expects a session-scope provisioning
//   - `sesh worker-cwd <label> --scope=project`  — same path, project-scope provisioning
//
// Stdout on success: one line, the absolute checkout path, no decoration.
// Exit 0. Same output shape as `sesh worktree`'s success line so callers
// can swap one for the other without parser changes.
//
// Stderr on failure: a human-readable explanation. Exit non-zero. The two
// error categories are (1) the label is hostile (validateLabel rejects it
// before any path math touches disk) and (2) the checkout dir is missing
// or has no .fslckout marker — the operator forgot `sesh worktree <label>`
// or the worktree provisioning failed earlier.
//
// Why a separate subcommand vs reading `sesh worktree`'s output? `sesh
// worktree` is provisioning: it materializes the checkout dir, opens the
// libfossil repo, extracts trunk-tip files, and sets autosync. Even
// idempotent re-runs touch the SQLite config row. orch-spawn just wants
// the path — read-only — and shouldn't trigger any of those side effects
// in the worker's hot path. The cost of two subcommands is a few lines of
// dispatcher code; the benefit is a clean read/write split.
//
// Tier-1 safety: this subcommand is read-only. It does not call os.Remove,
// os.MkdirAll, libfossil.Open, or any other mutating API. The
// validateLabel call still runs first as defense-in-depth: even though we
// don't write, a hostile label could let a malicious caller probe
// arbitrary filesystem locations through error messages.
type WorkerCwdCmd struct {
	Label string `arg:"" required:"" help:"Session/checkout label. Must match a label previously passed to 'sesh worktree <label>'."`

	Scope string `help:"Backing repo scope: 'session' resolves .sesh/checkouts/<label>/ against the per-session repo; 'project' resolves it under the shared project.repo. Must match the scope used at 'sesh worktree' time." enum:"session,project" default:"session"`
}

func (c *WorkerCwdCmd) Run() error {
	// Tier-1 safety: validate the label before ANY path math. Without
	// this, a hostile label like "../sessions" would let the rest of
	// the function stat a sibling path under .sesh/ and reveal its
	// existence through the error message.
	if err := validateLabel(c.Label); err != nil {
		return fmt.Errorf("sesh worker-cwd: invalid label %q: %w", c.Label, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	// Scope is accepted for symmetry with `sesh worktree` (so callers can
	// pass the same flag value through). The resolved path is identical
	// under both scopes today — .sesh/checkouts/<label>/ — because the
	// scope only affects where the *backing repo* lives, not the checkout
	// directory. We still validate the flag so a typo surfaces here
	// rather than silently being ignored downstream.
	scope := SeshScope(c.Scope)
	_ = scope

	dir, err := checkoutDir(cwd, c.Label)
	if err != nil {
		return fmt.Errorf("sesh worker-cwd: %w", err)
	}

	// Stat the .fslckout marker. We don't stat the dir itself because a
	// stray empty directory (e.g. leftover from a partial mkdir) would
	// otherwise satisfy the existence check without actually being a live
	// checkout. The marker is the cheapest libfossil-defined predicate
	// for "this is a real checkout."
	marker := checkoutMarkerPath(dir)
	if _, err := os.Stat(marker); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf(
				"no fossil checkout at %s — run 'sesh worktree %s%s' first",
				dir, c.Label, scopeHint(scope))
		}
		return fmt.Errorf("stat checkout marker %s: %w", marker, err)
	}

	// Output contract: one line, absolute path, no decoration. Matches
	// `sesh worktree` so `cd "$(sesh worker-cwd <label>)"` works as
	// natural shell glue.
	fmt.Println(dir)
	return nil
}
