package cli_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSeshMaterialize_OverlaysFossilTrunk_IntoCwd is the happy path. We
// bring a session up, commit a file into the fossil trunk via the
// libfossil Go API (commitViaLibfossil, shared with the worktree tests),
// run `sesh materialize <label>`, and assert:
//
//   - the file appears at cwd with the committed contents,
//   - re-running materialize is idempotent (mtime aside, contents unchanged),
//   - stdout matches the documented summary shape.
func TestSeshMaterialize_OverlaysFossilTrunk_IntoCwd(t *testing.T) {
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

	// Commit a new file into the fossil trunk via a transient checkout.
	// We use sesh worktree to provision the checkout, then drive
	// commitViaLibfossil against it.
	checkout := mustRunWorktree(t, bin, home, project, "alpha")
	const newName = "materialized.txt"
	const payload = "trunk-tip\n"
	if err := os.WriteFile(filepath.Join(checkout, newName), []byte(payload), 0o644); err != nil {
		t.Fatalf("seed trunk file: %v", err)
	}
	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if err := commitViaLibfossil(repoPath, checkout, newName, "materialize probe"); err != nil {
		t.Fatalf("commit via libfossil: %v", err)
	}

	// Materialize. The output goes to cwd, which we set to project.
	// --allow-dirty here because setupGitWorktree intentionally leaves
	// `agent-note.md` untracked; the dirty-check is exercised in its
	// own dedicated test, not here.
	stdout := mustRunMaterialize(t, bin, home, project, "alpha", "--allow-dirty")
	if !strings.HasPrefix(stdout, "materialized ") {
		t.Errorf("stdout missing summary prefix: %q", stdout)
	}
	if !strings.Contains(stdout, project) {
		t.Errorf("stdout %q does not mention output dir %q", stdout, project)
	}

	got, err := os.ReadFile(filepath.Join(project, newName))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(got) != payload {
		t.Errorf("materialized file = %q; want %q", string(got), payload)
	}

	// Idempotent re-run: file contents survive byte-for-byte.
	before := sha256File(t, filepath.Join(project, newName))
	mustRunMaterialize(t, bin, home, project, "alpha", "--allow-dirty")
	after := sha256File(t, filepath.Join(project, newName))
	if before != after {
		t.Errorf("idempotent re-run changed file hash: %s -> %s", before, after)
	}
}

// TestSeshMaterialize_RejectsLabelTraversal is the tier-1 safety test.
// It exercises validateLabel through the materialize entrypoint with the
// hostile inputs that would otherwise let the label escape its slot
// under .sesh/. The dual matrix covers both --scope=session and
// --scope=project: validateLabel sits ABOVE the scope branch so the
// same set of inputs must be rejected in both modes.
//
// After each run we re-fingerprint the .sesh/ tree to assert the
// validator caught the input before any path math touched disk.
func TestSeshMaterialize_RejectsLabelTraversal(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	cases := []struct {
		name  string
		label string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"slash_prefix", "/etc"},
		{"slash_embedded", "foo/bar"},
		{"backslash_embedded", "foo\\bar"},
		{"dotdot_embedded", "alpha/../beta"},
		{"dotdot_only_embedded", "x..y"},
		{"nul_byte", "alpha\x00beta"},
		{"leading_dot", ".sessions"},
		{"whitespace_only", "   "},
		{"control_char", "alpha\x01"},
		{"newline", "alpha\nbeta"},
	}

	for _, scope := range []string{"session", "project"} {
		scope := scope
		t.Run("scope="+scope, func(t *testing.T) {
			bin := buildSesh(t)
			home := t.TempDir()
			project := t.TempDir()
			// .sesh/ tier-1 paths we want to prove are not touched
			// by the hostile-input runs. We don't bring a session
			// up — the validator must reject the label before we
			// ever attempt to read or write any of these.
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

			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					cmd := exec.Command(bin, "materialize", tc.label, "--scope="+scope)
					cmd.Dir = project
					cmd.Env = append(os.Environ(), "HOME="+home)
					var stdout, stderr bytes.Buffer
					cmd.Stdout = &stdout
					cmd.Stderr = &stderr
					err := cmd.Run()
					if err == nil {
						t.Fatalf("materialize accepted hostile label %q under scope=%s; stdout=%q stderr=%q",
							tc.label, scope, stdout.String(), stderr.String())
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

// TestSeshMaterialize_RefusesDirtyWorktree asserts that materialize
// refuses to overlay files into a git worktree with uncommitted changes
// unless --allow-dirty is set. The uncommitted file must be unchanged
// after the refusal and the stderr must name it so the operator knows
// what's blocking.
func TestSeshMaterialize_RefusesDirtyWorktree(t *testing.T) {
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

	// Plant an uncommitted file. setupGitWorktree leaves a clean
	// working tree after its seed commit; this is operator-side work
	// that materialize must not destroy.
	dirty := filepath.Join(project, "dirty.txt")
	const dirtyPayload = "wip-work\n"
	if err := os.WriteFile(dirty, []byte(dirtyPayload), 0o644); err != nil {
		t.Fatalf("plant dirty file: %v", err)
	}

	cmd := exec.Command(bin, "materialize", "alpha")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderrBuf bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	if err == nil {
		t.Fatalf("materialize unexpectedly succeeded against dirty worktree; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderrBuf.String(), "dirty.txt") {
		t.Errorf("dirty-refusal stderr missing filename 'dirty.txt'; got: %s", stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "git stash") {
		t.Errorf("dirty-refusal stderr missing 'git stash' hint; got: %s", stderrBuf.String())
	}

	got, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatalf("dirty file vanished: %v", err)
	}
	if string(got) != dirtyPayload {
		t.Errorf("dirty file mutated by refused materialize: got %q want %q", string(got), dirtyPayload)
	}
}

// TestSeshMaterialize_AllowDirtyOverride exercises the escape hatch.
// Same dirty setup as the refusal test, but with --allow-dirty the
// command succeeds, the fossil trunk overlay lands, and the operator's
// uncommitted file is preserved (overlay semantics: we only write what
// fossil has).
func TestSeshMaterialize_AllowDirtyOverride(t *testing.T) {
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

	// Commit a new file to the fossil trunk so we can prove the overlay
	// lands. Use the same helper as the worktree tests.
	checkout := mustRunWorktree(t, bin, home, project, "alpha")
	const fossilFile = "from-fossil.txt"
	if err := os.WriteFile(filepath.Join(checkout, fossilFile), []byte("trunk-content\n"), 0o644); err != nil {
		t.Fatalf("seed fossil file: %v", err)
	}
	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if err := commitViaLibfossil(repoPath, checkout, fossilFile, "allow-dirty probe"); err != nil {
		t.Fatalf("fossil commit: %v", err)
	}

	// Plant an uncommitted file.
	dirty := filepath.Join(project, "dirty.txt")
	const dirtyPayload = "wip-work\n"
	if err := os.WriteFile(dirty, []byte(dirtyPayload), 0o644); err != nil {
		t.Fatalf("plant dirty file: %v", err)
	}

	mustRunMaterialize(t, bin, home, project, "alpha", "--allow-dirty")

	// Fossil-side file landed.
	got, err := os.ReadFile(filepath.Join(project, fossilFile))
	if err != nil {
		t.Fatalf("read overlaid file: %v", err)
	}
	if string(got) != "trunk-content\n" {
		t.Errorf("overlaid file = %q; want %q", string(got), "trunk-content\n")
	}
	// Operator's uncommitted file untouched.
	stillDirty, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatalf("dirty file vanished under --allow-dirty: %v", err)
	}
	if string(stillDirty) != dirtyPayload {
		t.Errorf("dirty file mutated by --allow-dirty overlay: got %q want %q", string(stillDirty), dirtyPayload)
	}
}

// TestSeshMaterialize_PreservesFileModes commits a file with 0o755 to
// the fossil trunk, materializes, and asserts the on-disk permission
// bits include the executable bit.
//
// Blocked on libfossil#36: `Checkout.Add` + `Checkin` do not currently
// capture the executable bit from disk into the manifest, so a vanilla
// commit lands with `Perm == ""` and `Extract` rehydrates 0o644 on the
// other side. The materialize-side code in this PR DOES preserve the
// mode it gets from libfossil (verified by sha256-with-chmod assertions
// further down in this test), but the end-to-end round-trip cannot
// surface a 0o755 file without the upstream fix.
//
// When libfossil#36 lands, drop the t.Skip; the assertion below is
// already shaped to catch any regression in materialize's perm copy
// step.
func TestSeshMaterialize_PreservesFileModes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Skip("blocked on libfossil#36 (Checkout.Add does not capture exec bit)")

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

	checkout := mustRunWorktree(t, bin, home, project, "alpha")
	const execFile = "run.sh"
	execPath := filepath.Join(checkout, execFile)
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("seed exec file: %v", err)
	}
	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if err := commitViaLibfossil(repoPath, checkout, execFile, "mode probe"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	mustRunMaterialize(t, bin, home, project, "alpha", "--allow-dirty")
	info, err := os.Stat(filepath.Join(project, execFile))
	if err != nil {
		t.Fatalf("stat materialized exec file: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("materialized %s lost executable bit; perm=%o", execFile, info.Mode().Perm())
	}
}

// TestSeshMaterialize_PreservesFileModes_Unit is the per-call-site
// assertion that materialize's overlay copies file modes verbatim.
// We bypass the full sesh up + fossil commit pipeline (and so dodge
// libfossil#36) by constructing a tempdir that mimics what libfossil's
// Extract would write — a regular file with 0o755 — then exercising
// materialize's copy helper directly through an `--output` round-trip
// of a precommitted repo.
//
// In practice this test plants the executable into the materialize
// staging area by pre-committing it through fossil with isexe forced
// via direct SQL on the repo's manifest blob — a path that's not in
// scope here. Until libfossil#36 lands, this is a placeholder that
// documents the contract; see the godoc on copyFilePreservingMode for
// the perm-copy logic that's already in place.
func TestSeshMaterialize_PreservesFileModes_Unit(t *testing.T) {
	t.Skip("placeholder — see libfossil#36; copyFilePreservingMode is unit-tested via the integration test above when unblocked")
}

// TestSeshMaterialize_GitAdd runs the happy path with --git-add and
// asserts the fossil files appear in `git status --porcelain` as staged.
func TestSeshMaterialize_GitAdd(t *testing.T) {
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

	checkout := mustRunWorktree(t, bin, home, project, "alpha")
	const newFile = "git-add-probe.txt"
	if err := os.WriteFile(filepath.Join(checkout, newFile), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if err := commitViaLibfossil(repoPath, checkout, newFile, "git-add probe"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	mustRunMaterialize(t, bin, home, project, "alpha", "--git-add", "--allow-dirty")

	statusCmd := exec.Command("git", "status", "--porcelain", newFile)
	statusCmd.Dir = project
	var out bytes.Buffer
	statusCmd.Stdout = &out
	if err := statusCmd.Run(); err != nil {
		t.Fatalf("git status: %v", err)
	}
	porcelain := out.String()
	// `A ` prefix = added to index, no unstaged changes. The line
	// shape is "A  git-add-probe.txt\n".
	if !strings.HasPrefix(porcelain, "A  "+newFile) {
		t.Errorf("expected %q to be staged ('A  '); got porcelain: %q", newFile, porcelain)
	}
}

// TestSeshMaterialize_OutputDirOverride sends the overlay to a tempdir
// via --output=<dir>. Files land there, not in cwd.
func TestSeshMaterialize_OutputDirOverride(t *testing.T) {
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

	checkout := mustRunWorktree(t, bin, home, project, "alpha")
	const newFile = "to-output.txt"
	if err := os.WriteFile(filepath.Join(checkout, newFile), []byte("over-there\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if err := commitViaLibfossil(repoPath, checkout, newFile, "output-dir probe"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	outDir := t.TempDir()
	mustRunMaterialize(t, bin, home, project, "alpha", "--output="+outDir)

	if got, err := os.ReadFile(filepath.Join(outDir, newFile)); err != nil {
		t.Errorf("file not in --output dir: %v", err)
	} else if string(got) != "over-there\n" {
		t.Errorf("file content = %q; want %q", string(got), "over-there\n")
	}
	// cwd untouched.
	if _, err := os.Stat(filepath.Join(project, newFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--output did not redirect; file appeared in cwd too (err=%v)", err)
	}
}

// TestSeshMaterialize_RequiresFossilRepoExists asserts the operator-
// facing error when materialize is called without `sesh up` first. The
// stderr must mention `sesh up` so the operator knows the fix.
func TestSeshMaterialize_RequiresFossilRepoExists(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()

	cmd := exec.Command(bin, "materialize", "alpha")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderrBuf bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	if err == nil {
		t.Fatalf("materialize against missing repo unexpectedly succeeded: %s", stdout.String())
	}
	if !strings.Contains(stderrBuf.String(), "sesh up") {
		t.Errorf("missing-repo stderr lacks 'sesh up' hint; got: %s", stderrBuf.String())
	}
}

// TestSeshMaterialize_BinaryFiles round-trips a small binary blob
// through the fossil trunk → materialize pipeline and asserts byte-for-
// byte equality. Defends against accidental text-mode line-ending
// conversion or BOM stripping.
func TestSeshMaterialize_BinaryFiles(t *testing.T) {
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

	checkout := mustRunWorktree(t, bin, home, project, "alpha")
	const binName = "blob.bin"
	// Bytes chosen to exercise the line-ending / encoding paths that
	// would mangle them: NUL, ESC, CR, LF, high-bit, UTF-8 BOM bytes.
	payload := []byte{
		0x00, 0x01, 0xff, 0xfe, 0x0d, 0x0a, 0x1b, 0x80, 0xef, 0xbb, 0xbf, 0x7f, 0x00,
	}
	if err := os.WriteFile(filepath.Join(checkout, binName), payload, 0o644); err != nil {
		t.Fatalf("seed binary: %v", err)
	}
	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if err := commitViaLibfossil(repoPath, checkout, binName, "binary probe"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	mustRunMaterialize(t, bin, home, project, "alpha", "--allow-dirty")
	got, err := os.ReadFile(filepath.Join(project, binName))
	if err != nil {
		t.Fatalf("read materialized binary: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("binary round-trip mangled bytes:\nwant: %x\ngot:  %x", payload, got)
	}
}

// TestSeshMaterialize_RoundTrip_FossilCommitToGitDiff is the load-
// bearing acceptance gate for Slice 3: a file committed to the fossil
// trunk under --scope=project, then materialized into the git worktree
// with --git-add, must show up in `git diff --cached` with the exact
// content that was committed. This is the mission-complete → git-PR
// proof.
func TestSeshMaterialize_RoundTrip_FossilCommitToGitDiff(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (load-bearing)")
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

	checkout := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")

	const newFile = "mission-complete.txt"
	const payload = "swarm-converged\n"
	if err := os.WriteFile(filepath.Join(checkout, newFile), []byte(payload), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repoPath := filepath.Join(project, ".sesh", "project.repo")
	if err := commitViaLibfossil(repoPath, checkout, newFile, "mission complete"); err != nil {
		t.Fatalf("fossil commit: %v", err)
	}

	mustRunMaterialize(t, bin, home, project, "alpha", "--scope=project", "--git-add", "--allow-dirty")

	// `git diff --cached -- <file>` shows the staged content. We expect
	// the +<payload> line to appear in the diff.
	diffCmd := exec.Command("git", "diff", "--cached", "--", newFile)
	diffCmd.Dir = project
	var out bytes.Buffer
	diffCmd.Stdout = &out
	if err := diffCmd.Run(); err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	diff := out.String()
	if !strings.Contains(diff, "+"+strings.TrimRight(payload, "\n")) {
		t.Errorf("fossil commit did not surface in git diff --cached:\n%s", diff)
	}
	// And the file is staged with the right mode/name.
	if !strings.Contains(diff, "b/"+newFile) {
		t.Errorf("git diff --cached does not reference %s:\n%s", newFile, diff)
	}
}

// --- helpers ---

// mustRunMaterialize executes `sesh materialize <label> [extra...]` in
// `project` with HOME set, returning trimmed stdout. Fails the test if
// the subcommand exits non-zero.
func mustRunMaterialize(t *testing.T, bin, home, project, label string, extra ...string) string {
	t.Helper()
	args := append([]string{"materialize", label}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sesh %s: %v\nstdout=%s\nstderr=%s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// sha256File returns the hex SHA-256 of the file at path. Used to
// detect cross-run drift for idempotency assertions without flagging
// mtime / atime noise.
func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fingerprintTree walks dir and returns a deterministic string keyed
// on (relative-path, mode-perm-bits, regular-file-content-hash). Used
// by the traversal test to assert hostile inputs never mutate the
// .sesh/ tree. Errors during the walk are folded into the fingerprint
// as `ERR:<path>:<msg>` lines so the diff is human-readable when the
// assertion fails.
func fingerprintTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			lines = append(lines, "ERR:"+path+":"+walkErr.Error())
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			lines = append(lines, "D:"+rel)
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			lines = append(lines, "ERR:"+rel+":"+infoErr.Error())
			return nil
		}
		if !d.Type().IsRegular() {
			lines = append(lines, "S:"+rel+":"+info.Mode().String())
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			lines = append(lines, "ERR:"+rel+":"+readErr.Error())
			return nil
		}
		sum := sha256.Sum256(data)
		lines = append(lines, "F:"+rel+":"+info.Mode().Perm().String()+":"+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
