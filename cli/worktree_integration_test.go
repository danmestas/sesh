package cli_test

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/danmestas/EdgeSync/hub" // SQL driver registration for the test's vvar probe.
	libfossil "github.com/danmestas/libfossil"
	libfossildb "github.com/danmestas/libfossil/db"
)

// TestSeshWorktree_CreatesCheckout_FromSessionRepo is the happy path for
// the worktree subcommand under --scope=session (the default). It brings
// up a session, runs `sesh worktree <label>`, and asserts:
//
//   - the stdout is a single line containing the absolute checkout path,
//   - .sesh/checkouts/<label>/.fslckout exists (libfossil's checkout marker),
//   - the checkout's vvar 'repository' row points at .sesh/sessions/<label>.repo,
//   - the trunk-tip files extracted into the checkout actually appear on
//     disk (the regression case where Create succeeds but Extract is
//     skipped would leave an empty dir despite a populated repo).
func TestSeshWorktree_CreatesCheckout_FromSessionRepo(t *testing.T) {
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

	out := mustRunWorktree(t, bin, home, project, "alpha")
	if out == "" {
		t.Fatalf("worktree printed empty stdout")
	}
	if !filepath.IsAbs(out) {
		t.Errorf("worktree output %q is not absolute", out)
	}
	expected := filepath.Join(project, ".sesh", "checkouts", "alpha")
	if got, err := filepath.EvalSymlinks(out); err == nil {
		if want, _ := filepath.EvalSymlinks(expected); want != got {
			t.Errorf("worktree output = %q; want %q (post-symlink-resolution)", got, want)
		}
	}

	marker := filepath.Join(expected, ".fslckout")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected .fslckout at %s: %v", marker, err)
	}

	repoVVar, err := readVVarRepository(marker)
	if err != nil {
		t.Fatalf("read vvar 'repository': %v", err)
	}
	wantRepo := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	wantResolved, _ := filepath.EvalSymlinks(wantRepo)
	gotResolved, _ := filepath.EvalSymlinks(repoVVar)
	if gotResolved != wantResolved {
		t.Errorf("vvar 'repository' = %q; want %q", repoVVar, wantRepo)
	}

	// The git worktree's seed put hello.txt at the trunk tip. Extract
	// should have materialized it inside the checkout dir.
	if _, err := os.Stat(filepath.Join(expected, "hello.txt")); err != nil {
		t.Errorf("expected hello.txt inside checkout dir: %v", err)
	}
}

// TestSeshWorktree_Idempotent re-runs `sesh worktree` against an existing
// checkout and confirms the operation is a no-op: the stdout matches, no
// files inside the checkout are clobbered. A scratch file written between
// the two runs survives.
func TestSeshWorktree_Idempotent(t *testing.T) {
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

	first := mustRunWorktree(t, bin, home, project, "alpha")

	// Drop a sentinel into the checkout. Idempotent re-materialization
	// must NOT clobber unrelated files in the dir.
	sentinel := filepath.Join(first, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("survive-me\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	second := mustRunWorktree(t, bin, home, project, "alpha")
	if first != second {
		t.Errorf("second worktree output = %q; want %q (paths must match)", second, first)
	}

	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel after second worktree: %v", err)
	}
	if string(got) != "survive-me\n" {
		t.Errorf("sentinel contents = %q; want %q (idempotent run clobbered uncommitted file)",
			string(got), "survive-me\n")
	}
}

// TestSeshWorktree_ForceRecreate asserts the destructive path: a
// `--force-recreate` invocation removes the checkout dir before
// re-materializing. A sentinel file in the dir does NOT survive. The
// backing repo (the .repo file) is untouched: the trunk's check-in
// count remains the same before and after.
func TestSeshWorktree_ForceRecreate(t *testing.T) {
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

	first := mustRunWorktree(t, bin, home, project, "alpha")
	sentinel := filepath.Join(first, "doomed.txt")
	if err := os.WriteFile(sentinel, []byte("nuked\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	wantCheckins := countCheckins(t, repoPath)

	second := mustRunWorktree(t, bin, home, project, "alpha", "--force-recreate")
	if first != second {
		t.Errorf("path changed across --force-recreate: %q vs %q", first, second)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sentinel survived --force-recreate; expected ErrNotExist, got: %v", err)
	}

	// Trunk state preserved: the .repo file's check-in count is the
	// same before and after the recreate. Acceptance gate for the
	// tier-1 safety rule "RemoveAll touches only the per-label checkout dir."
	if got := countCheckins(t, repoPath); got != wantCheckins {
		t.Errorf("trunk check-ins after --force-recreate = %d; want %d", got, wantCheckins)
	}
}

// TestSeshWorktree_AutosyncEnabled confirms the backing repo's 'autosync'
// config row is set to '1' after a worktree invocation. Idempotent: a
// second call leaves it at '1'.
func TestSeshWorktree_AutosyncEnabled(t *testing.T) {
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

	mustRunWorktree(t, bin, home, project, "alpha")

	repoPath := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	got, err := readRepoConfig(repoPath, "autosync")
	if err != nil {
		t.Fatalf("read autosync config: %v", err)
	}
	if got != "1" {
		t.Errorf("autosync config = %q; want %q", got, "1")
	}

	// Second call: still '1'. SetConfig is an UPSERT — re-running must
	// not flip to a different value.
	mustRunWorktree(t, bin, home, project, "alpha")
	got2, err := readRepoConfig(repoPath, "autosync")
	if err != nil {
		t.Fatalf("read autosync config (2nd): %v", err)
	}
	if got2 != "1" {
		t.Errorf("autosync after 2nd run = %q; want %q", got2, "1")
	}
}

// TestSeshWorktree_ScopeProject mirrors _FromSessionRepo but uses the
// shared project.repo. The session starts with --scope=project so the
// shared file gets created; the checkout's vvar must point at the
// shared path, not the per-session one.
func TestSeshWorktree_ScopeProject(t *testing.T) {
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

	out := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")
	expected := filepath.Join(project, ".sesh", "checkouts", "alpha")
	if got, _ := filepath.EvalSymlinks(out); got != "" {
		want, _ := filepath.EvalSymlinks(expected)
		if got != want {
			t.Errorf("worktree output = %q; want %q", got, want)
		}
	}

	marker := filepath.Join(expected, ".fslckout")
	repoVVar, err := readVVarRepository(marker)
	if err != nil {
		t.Fatalf("read vvar: %v", err)
	}
	wantRepo := filepath.Join(project, ".sesh", "project.repo")
	wantResolved, _ := filepath.EvalSymlinks(wantRepo)
	gotResolved, _ := filepath.EvalSymlinks(repoVVar)
	if gotResolved != wantResolved {
		t.Errorf("vvar 'repository' = %q; want %q (--scope=project must use shared repo)",
			repoVVar, wantRepo)
	}
}

// TestSeshWorktree_ErrorIfSessionNotUp asserts the operator-facing
// error path when `sesh worktree` is invoked before `sesh up`. The
// command must exit non-zero and the stderr must mention `sesh up`
// so the operator knows the fix.
func TestSeshWorktree_ErrorIfSessionNotUp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()

	cmd := exec.Command(bin, "worktree", "alpha")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("worktree against missing session unexpectedly succeeded; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sesh up") {
		t.Errorf("error stderr lacks 'sesh up' hint; got: %s", stderr.String())
	}
}

// TestSeshWorktree_PropagatesEdit_ViaAutosync is the SWARM ACCEPTANCE
// GATE for issue #64. Two sessions share --scope=project; both run
// `sesh worktree`. A commit landing in alpha's checkout must surface
// in beta's checkout within 10s, via the autosync pipeline.
//
// The commit path here uses libfossil's Checkout.Checkin directly,
// against the shared project.repo file. SQLite's WAL serializes
// concurrent writers, so beta sees the new commit on its next libfossil
// read (no NATS round-trip needed — same physical file under
// --scope=project). The test confirms the cohabitation works end-to-end
// and that the checkout dirs reflect trunk-tip after an update.
//
// If this fails, the worktree subcommand still works for single-
// operator use; the failure points at autosync / cross-checkout
// propagation and a follow-up issue is the correct path (PR body has
// the link template).
func TestSeshWorktree_PropagatesEdit_ViaAutosync(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (swarm acceptance)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	alpha, alphaStderr := startSeshArgs(t, bin, home, project, "alpha", "--scope=project")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", alphaStderr.String())
	}

	beta, betaStderr := startSeshArgs(t, bin, home, project, "beta", "--scope=project")
	defer killAndWait(t, beta, betaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "beta.json"), 15*time.Second)

	alphaWT := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")
	betaWT := mustRunWorktree(t, bin, home, project, "beta", "--scope=project")

	// Write a brand-new file inside alpha's checkout, then commit it
	// through the shared project.repo. We bypass the `fossil` CLI binary
	// (sesh's contract is libfossil-only) by opening the repo + checkout
	// in-process and using the Go Checkin API.
	const newName = "from-alpha-worktree.txt"
	const payload = "hello-swarm\n"
	if err := os.WriteFile(filepath.Join(alphaWT, newName), []byte(payload), 0o644); err != nil {
		t.Fatalf("write %s into alpha checkout: %v", newName, err)
	}

	repoPath := filepath.Join(project, ".sesh", "project.repo")
	if err := commitViaLibfossil(repoPath, alphaWT, newName, "swarm propagation probe"); err != nil {
		t.Fatalf("commit via libfossil into alpha's checkout: %v", err)
	}

	// Within the bounded window, beta's checkout (after an update
	// against the shared repo) must see the new file. The test does the
	// update step explicitly — worker-side `fossil update` polling is
	// orch's responsibility (Slice 4+), not sesh's. Bonus test: if the
	// update step itself fails, that's the gap we file upstream.
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := updateViaLibfossil(repoPath, betaWT); err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if _, err := os.Stat(filepath.Join(betaWT, newName)); err == nil {
			return // success
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("beta's checkout did not see %s within 10s (last update err: %v)", newName, lastErr)
}

// mustRunWorktree runs `sesh worktree <label> [extra...]` and returns
// the trimmed stdout (the absolute checkout path). Fails the test if
// the subcommand exits non-zero.
func mustRunWorktree(t *testing.T, bin, home, project, label string, extra ...string) string {
	t.Helper()
	args := append([]string{"worktree", label}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sesh worktree %s: %v\nstdout=%s\nstderr=%s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// readVVarRepository peeks at a .fslckout's 'repository' vvar row. Used
// by the integration tests to verify that the checkout dir is bound to
// the expected backing repo path. Mirrors the helper used by
// WorktreeCmd's idempotency probe (worktree_vvar.go) but lives in the
// _test file so the test can stay in the external cli_test package
// without exporting a probe API.
func readVVarRepository(dbPath string) (string, error) {
	drv := libfossildb.RegisteredDriver()
	if drv == nil {
		return "", errors.New("no libfossil SQLite driver registered")
	}
	db, err := sql.Open(drv.Name, dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var val string
	err = db.QueryRow(`SELECT value FROM vvar WHERE name = 'repository'`).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return val, err
}

// readRepoConfig returns the value of a single key from a libfossil
// repo's config table. Used by the autosync-enabled test to verify the
// flag landed. We open the SQLite DB directly rather than going through
// libfossil.Open so the test stays self-contained and doesn't trigger
// any of the Repo lifecycle hooks (which would be misleading in a
// read-only assertion).
func readRepoConfig(repoPath, key string) (string, error) {
	drv := libfossildb.RegisteredDriver()
	if drv == nil {
		return "", errors.New("no libfossil SQLite driver registered")
	}
	db, err := sql.Open(drv.Name, repoPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var val string
	err = db.QueryRow(`SELECT value FROM config WHERE name = ?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return val, err
}

// commitViaLibfossil opens repoPath, opens the checkout at checkoutDir,
// adds the named file to version tracking, and creates a check-in. The
// caller must have already written the file to disk inside checkoutDir.
//
// Used by the swarm propagation test to inject a commit through the
// shared project.repo without shelling out to the `fossil` CLI binary
// (sesh's contract is libfossil-only). The repo and checkout handles
// are closed before returning so the underlying SQLite file is fully
// released for the peer-side update.
func commitViaLibfossil(repoPath, checkoutDir, fileName, message string) error {
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return err
	}
	defer repo.Close()
	co, err := repo.OpenCheckout(checkoutDir, libfossil.CheckoutOpenOpts{})
	if err != nil {
		return err
	}
	defer co.Close()
	if _, err := co.Add([]string{fileName}); err != nil {
		return err
	}
	if _, _, err := co.Checkin(libfossil.CheckoutCommitOpts{
		Message: message,
		User:    "swarm-test",
	}); err != nil {
		return err
	}
	return nil
}

// updateViaLibfossil opens repoPath + the checkout and calls Update to
// merge any new trunk commits into the working dir. Bounds the moment
// where peer checkouts see the new file in the swarm propagation test.
func updateViaLibfossil(repoPath, checkoutDir string) error {
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return err
	}
	defer repo.Close()
	co, err := repo.OpenCheckout(checkoutDir, libfossil.CheckoutOpenOpts{})
	if err != nil {
		return err
	}
	defer co.Close()
	if err := co.Update(libfossil.UpdateOpts{}); err != nil {
		return err
	}
	return nil
}
