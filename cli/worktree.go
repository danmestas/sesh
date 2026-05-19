package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	// Imported for its side effect: registers the modernc SQLite driver
	// libfossil requires before any Repo.Open succeeds. The hub package
	// is also imported (via up.go) and itself imports the same driver,
	// but worktree is reachable as a standalone subcommand whose Run()
	// must not depend on up.go being linked into the test binary or on
	// runtime ordering between subcommand entrypoints.
	_ "github.com/danmestas/EdgeSync/hub"

	libfossil "github.com/danmestas/libfossil"
)

// WorktreeCmd provisions a fossil working directory at
// <cwd>/.sesh/checkouts/<label>/ backed by the session's or project's
// fossil repo file. It is the worker-side primitive for the fossil-as-
// trunk swarm workflow (issue #64): once the operator has run `sesh up`,
// `sesh worktree <label>` materializes a tree the worker can `cd` into
// and use `fossil commit` / `fossil update` against, without ever
// touching the surrounding git worktree.
//
// Contract:
//
//   - `sesh worktree <label>`                  — checkout backed by <cwd>/.sesh/sessions/<label>.repo
//   - `sesh worktree <label> --scope=project` — checkout backed by <cwd>/.sesh/project.repo
//   - `sesh worktree <label> --force-recreate` — remove an existing checkout dir, then materialize fresh
//
// The backing repo MUST already exist; the subcommand errors with a
// pointer to `sesh up --session=<label>` if the operator forgot to bring
// the session up first. The output on success is a single line — the
// absolute checkout path — so `cd "$(sesh worktree <label>)"` works as
// natural shell glue.
//
// Idempotency: a second `sesh worktree <label>` call against an existing
// checkout dir whose vvar->repository points at the same backing repo
// is a no-op. Files in the checkout (committed or otherwise) survive.
// The only destructive operation is `--force-recreate`, which
// os.RemoveAll's exactly <cwd>/.sesh/checkouts/<label>/ — never any
// sibling path under .sesh/.
//
// Fossil access: this subcommand uses the in-process libfossil Go API
// (Open + CreateCheckout / OpenCheckout). No dependency on the external
// `fossil` CLI binary on PATH — the bytes flow through the same SQL
// driver the rest of sesh uses. Workers running inside the checkout DO
// still need fossil-on-PATH if they want to use `fossil commit` / etc.,
// but that's a worker-side concern handled by the orch outfit (Slice 4).
type WorktreeCmd struct {
	Label string `arg:"" required:"" help:"Session/checkout label. Must match a session brought up via 'sesh up --session=<label>' (or, with --scope=project, must match any session that's mounted the shared project.repo)."`

	Scope string `help:"Backing repo scope: 'session' uses .sesh/sessions/<label>.repo; 'project' uses the shared .sesh/project.repo." enum:"session,project" default:"session"`

	ForceRecreate bool `name:"force-recreate" help:"Remove the existing checkout dir (.sesh/checkouts/<label>/) and re-materialize. Adjacent .sesh/sessions/ and .sesh/messaging/ are NOT touched. Use sparingly — destroys uncommitted edits in the checkout."`
}

func (c *WorktreeCmd) Run() error {
	// Tier-1 safety: validate the label before ANY path math. Without
	// this, a hostile label like "../sessions" would let the rest of
	// the function read or os.RemoveAll a sibling path under .sesh/.
	if err := validateLabel(c.Label); err != nil {
		return fmt.Errorf("invalid label: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	scope := SeshScope(c.Scope)
	repoPath, err := repoPathFor(scope, cwd, c.Label)
	if err != nil {
		return err
	}
	if _, err := os.Stat(repoPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf(
				"backing repo missing at %s — run 'sesh up --session=%s%s' first",
				repoPath, c.Label, scopeHint(scope))
		}
		return fmt.Errorf("stat backing repo %s: %w", repoPath, err)
	}

	dir, err := checkoutDir(cwd, c.Label)
	if err != nil {
		return err
	}

	// --force-recreate: scoped strictly to the per-label checkout dir.
	// We never touch the parent .sesh/checkouts/ tree, never .sesh/sessions/,
	// never .sesh/messaging/. Tier-1 safety lives in this single RemoveAll.
	if c.ForceRecreate {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("force-recreate: remove %s: %w", dir, err)
		}
	}

	// Idempotency probe: if a checkout already exists, peek at its vvar
	// 'repository' row to decide whether we can no-op (it points at our
	// repo) or must error (it points elsewhere — the operator is about
	// to lose data if we silently re-link the dir). The latter is rare
	// in practice but the safer default is to refuse rather than mutate.
	if _, err := os.Stat(checkoutMarkerPath(dir)); err == nil {
		existing, repoErr := readCheckoutRepository(dir)
		if repoErr == nil && sameRepoPath(existing, repoPath) {
			if err := ensureAutosync(repoPath); err != nil {
				return err
			}
			fmt.Println(dir)
			return nil
		}
		if repoErr != nil {
			return fmt.Errorf("inspect existing checkout %s: %w (re-run with --force-recreate to discard)", dir, repoErr)
		}
		return fmt.Errorf("checkout at %s already exists and is bound to a different repo (%s); re-run with --force-recreate to discard",
			dir, existing)
	}

	// Materialize: open the backing repo and stamp a fresh checkout. The
	// parent dir (.sesh/checkouts/) is created on demand by libfossil's
	// MkdirAll inside Create — but we MkdirAll ourselves up to that point
	// for symmetry with projectStateDir's pattern.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dir), err)
	}

	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return fmt.Errorf("open backing repo %s: %w", repoPath, err)
	}
	defer repo.Close()

	co, err := repo.CreateCheckout(dir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		return fmt.Errorf("materialize checkout %s: %w", dir, err)
	}
	defer co.Close()

	// Extract trunk-tip files into the working dir. CreateCheckout sets
	// vvar to tip but leaves the working dir empty by default; without
	// Extract the worker would see a "ghost" checkout — fossil status
	// would list every tracked file as 'missing'. ridZero = tip per the
	// libfossil UpdateOpts / ExtractOpts contract.
	rid, _, verErr := co.Version()
	if verErr != nil {
		return fmt.Errorf("read checkout version: %w", verErr)
	}
	if rid != 0 {
		if err := co.Extract(rid, libfossil.ExtractOpts{}); err != nil {
			return fmt.Errorf("extract checkout files: %w", err)
		}
	}

	// Autosync on. Stored in the repo's config table under the standard
	// fossil 'autosync' key. The value '1' matches fossil-CLI semantics
	// (full-sync; pull-before-commit and push-after-commit). For sesh's
	// in-process publish-hook model the effect is mostly cosmetic —
	// commits via hub.Repo.Commit propagate over NATS regardless — but a
	// worker that uses the fossil CLI binary against a checkout backed
	// by the hub's HTTP xfer endpoint relies on this flag to pick up
	// peer commits. See PR body for the propagation-gap follow-up.
	if err := ensureAutosync(repoPath); err != nil {
		return err
	}

	slog.Info("provisioned fossil checkout",
		"label", c.Label,
		"scope", scope,
		"repo", repoPath,
		"checkout", dir,
		"tip_rid", rid,
	)
	fmt.Println(dir)
	return nil
}

// ensureAutosync sets the repo's 'autosync' config row to '1'. Idempotent:
// fossil's SetConfig is an UPSERT under the hood. Returns an error only
// if the repo open or the SQL write itself fails.
func ensureAutosync(repoPath string) error {
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return fmt.Errorf("autosync: open %s: %w", repoPath, err)
	}
	defer repo.Close()
	if err := repo.SetConfig("autosync", "1"); err != nil {
		return fmt.Errorf("autosync: set on %s: %w", repoPath, err)
	}
	return nil
}

// readCheckoutRepository reads the 'repository' vvar row from a
// checkout's .fslckout SQLite DB. We bypass libfossil.OpenCheckout
// because OpenCheckout requires already knowing the repo; here the
// whole point is to ask the checkout where it thinks its repo lives so
// we can detect the mis-pointed case. The DB is opened read-only and
// closed immediately.
func readCheckoutRepository(dir string) (string, error) {
	dbPath := checkoutMarkerPath(dir)
	// Use libfossil's registered driver via database/sql. The driver is
	// already registered by the EdgeSync/hub import above.
	return readVVarRepository(dbPath)
}

// scopeHint returns the trailing CLI flag to append to the 'run sesh
// up' suggestion in the missing-repo error. Per the existing pattern
// (cli/scope.go), --scope is opt-in for project mode; the empty hint
// for session-scope keeps the suggestion uncluttered for the common
// case.
func scopeHint(scope SeshScope) string {
	if scope == ScopeProject {
		return " --scope=project"
	}
	return ""
}

// sameRepoPath compares two repo paths after resolving them through
// EvalSymlinks. The vvar repository row stores the absolute path the
// checkout was first created against; if either side of the comparison
// (the existing vvar or the freshly-computed repoPath) goes through a
// symlinked .sesh/ tree the raw string comparison would spuriously
// reject the no-op case. EvalSymlinks is a no-op on plain paths.
func sameRepoPath(a, b string) bool {
	resolveA, errA := filepath.EvalSymlinks(a)
	resolveB, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return resolveA == resolveB
	}
	// EvalSymlinks fails if a path doesn't exist; fall back to lexical
	// match so a missing-on-disk vvar still produces a clean error
	// message rather than a misleading "different repo" branch.
	return a == b
}
