package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSeshWorkerCwd_PrintsCheckoutPath is the happy path. After
// `sesh worktree alpha` has provisioned the checkout dir, `sesh worker-cwd
// alpha` must print the same absolute path on stdout, on a single line.
//
// The contract is intentionally identical to `sesh worktree`'s success
// output so callers (orch-spawn) can swap one for the other when they
// only need the path, not the provisioning side effects.
func TestSeshWorkerCwd_PrintsCheckoutPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	sesh, stderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, sesh, stderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, stderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", stderr.String())
	}

	// Provision first via worktree (the supported pairing).
	provisioned := mustRunWorktree(t, bin, home, project, "alpha")

	// worker-cwd should print the same path.
	got := mustRunWorkerCwd(t, bin, home, project, "alpha")
	if got == "" {
		t.Fatalf("worker-cwd printed empty stdout")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("worker-cwd output %q is not absolute", got)
	}
	if got != provisioned {
		// Compare after symlink resolution in case t.TempDir() returned
		// a symlinked path on some platforms (macOS /var → /private/var).
		gotR, _ := filepath.EvalSymlinks(got)
		wantR, _ := filepath.EvalSymlinks(provisioned)
		if gotR != wantR {
			t.Errorf("worker-cwd output = %q; want %q (post-symlink-resolution)", got, provisioned)
		}
	}

	expected := filepath.Join(project, ".sesh", "checkouts", "alpha")
	gotR, _ := filepath.EvalSymlinks(got)
	wantR, _ := filepath.EvalSymlinks(expected)
	if gotR != wantR {
		t.Errorf("worker-cwd output (resolved) = %q; want %q", gotR, wantR)
	}
}

// TestSeshWorkerCwd_ErrorsIfCheckoutMissing exercises the operator-facing
// error path when `sesh worker-cwd` is invoked before `sesh worktree` has
// provisioned the dir. The command must exit non-zero and the stderr must
// mention `sesh worktree` so the operator knows the remediation.
//
// This is the most common failure mode in practice: orch-spawn calls
// worker-cwd before the operator has remembered to provision the
// checkout. The error must point at the fix, not just say "not found."
func TestSeshWorkerCwd_ErrorsIfCheckoutMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()

	cmd := exec.Command(bin, "worker-cwd", "alpha")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("worker-cwd against missing checkout unexpectedly succeeded; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sesh worktree") {
		t.Errorf("error stderr lacks 'sesh worktree' hint; got: %s", stderr.String())
	}
	// Sanity: the printed path the operator sees in the error should
	// match where they'd actually find the missing dir, so they can
	// inspect it themselves.
	wantDir := filepath.Join(project, ".sesh", "checkouts", "alpha")
	if !strings.Contains(stderr.String(), wantDir) {
		t.Errorf("error stderr lacks checkout path %q; got: %s", wantDir, stderr.String())
	}
}

// TestSeshWorkerCwd_RejectsLabelTraversal is the tier-1 safety test.
// Hostile labels (path traversal, control chars, dotfile prefix, etc.)
// must be rejected before any path math touches disk. The matrix is
// verbatim from TestSeshMaterialize_RejectsLabelTraversal because
// validateLabel is the shared safety gate every label-consuming
// entrypoint funnels through; the same inputs must fail at every door.
//
// After each hostile run we re-fingerprint the .sesh/ tree to assert the
// validator caught the input before it could stat or read anything.
// Tier-1 paths under .sesh/ (sessions/, messaging/) must be byte-for-byte
// unchanged after the whole matrix has run.
func TestSeshWorkerCwd_RejectsLabelTraversal(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	for _, scope := range []string{"session", "project"} {
		scope := scope
		t.Run("scope="+scope, func(t *testing.T) {
			bin := buildSesh(t)
			home := t.TempDir()
			project := t.TempDir()
			// Seed the tier-1 paths so we can prove they survive the
			// hostile-input runs intact.
			seshDir := filepath.Join(project, ".sesh")
			if err := os.MkdirAll(filepath.Join(seshDir, "sessions"), 0o755); err != nil {
				t.Fatalf("seed .sesh/sessions: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(seshDir, "messaging"), 0o755); err != nil {
				t.Fatalf("seed .sesh/messaging: %v", err)
			}
			canary := filepath.Join(seshDir, "messaging", "canary.txt")
			if err := os.WriteFile(canary, []byte("tier-1\n"), 0o644); err != nil {
				t.Fatalf("seed canary: %v", err)
			}
			before := fingerprintTree(t, seshDir)

			for _, tc := range hostileLabelInputs {
				tc := tc
				t.Run(tc.Name, func(t *testing.T) {
					cmd := exec.Command(bin, "worker-cwd", tc.Label, "--scope="+scope)
					cmd.Dir = project
					cmd.Env = append(os.Environ(), "HOME="+home)
					var stdout, stderr bytes.Buffer
					cmd.Stdout = &stdout
					cmd.Stderr = &stderr
					err := cmd.Run()
					if err == nil {
						t.Fatalf("worker-cwd accepted hostile label %q under scope=%s; stdout=%q stderr=%q",
							tc.Label, scope, stdout.String(), stderr.String())
					}
					// Either Kong rejects the arg (empty label) or the
					// validator rejects it; both are acceptable as long
					// as the exit is non-zero and tier-1 paths survive.
					_ = stderr.String()
				})
			}

			after := fingerprintTree(t, seshDir)
			if before != after {
				t.Errorf("tier-1 .sesh/ tree fingerprint drifted after hostile inputs under scope=%s:\nbefore=%s\nafter=%s",
					scope, before, after)
			}
			if got, err := os.ReadFile(canary); err != nil || string(got) != "tier-1\n" {
				t.Errorf("canary %s mutated by hostile-input run; got=%q err=%v",
					canary, string(got), err)
			}
		})
	}
}

// TestSeshWorkerCwd_ScopeProject mirrors _PrintsCheckoutPath but exercises
// --scope=project. The checkout dir is the same under both scopes
// (.sesh/checkouts/<label>/) — only the backing repo differs — so the
// happy-path output must match.
func TestSeshWorkerCwd_ScopeProject(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	sesh, stderr := startSeshArgs(t, bin, home, project, "alpha", "--scope=project")
	defer killAndWait(t, sesh, stderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, stderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded under --scope=project:\n%s", stderr.String())
	}

	provisioned := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")
	got := mustRunWorkerCwd(t, bin, home, project, "alpha", "--scope=project")

	gotR, _ := filepath.EvalSymlinks(got)
	wantR, _ := filepath.EvalSymlinks(provisioned)
	if gotR != wantR {
		t.Errorf("worker-cwd output (resolved) = %q; want %q", gotR, wantR)
	}
	expected := filepath.Join(project, ".sesh", "checkouts", "alpha")
	expR, _ := filepath.EvalSymlinks(expected)
	if gotR != expR {
		t.Errorf("worker-cwd output (resolved) = %q; want %q (checkout dir under --scope=project)", gotR, expR)
	}
}

// mustRunWorkerCwd runs `sesh worker-cwd <label> [extra...]` and returns
// the trimmed stdout (the absolute checkout path). Fails the test if the
// subcommand exits non-zero. Mirrors mustRunWorktree's shape so the
// happy-path tests read the same.
func mustRunWorkerCwd(t *testing.T, bin, home, project, label string, extra ...string) string {
	t.Helper()
	args := append([]string{"worker-cwd", label}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sesh worker-cwd %s: %v\nstdout=%s\nstderr=%s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
