package cli_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/EdgeSync/hub"
	// modernc/sqlite driver registration is provided by the sibling
	// multi_session_integration_test.go's blank import; package cli_test
	// inherits it. Re-importing here is redundant.
)

// TestScope_Session_Default verifies that omitting --scope leaves the
// PR #20 per-session model unchanged: the repo lands at
// .sesh/sessions/<label>.repo and no shared .sesh/project.repo is
// created. This is a regression sentinel for the default; the rest of
// the per-session behavior is exercised by TestPerSessionRepos.
func TestScope_Session_Default(t *testing.T) {
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

	cmd, stderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, cmd, stderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)

	sessionRepo := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if _, err := os.Stat(sessionRepo); err != nil {
		t.Fatalf("expected per-session repo at %s (default scope): %v", sessionRepo, err)
	}
	projectRepo := filepath.Join(project, ".sesh", "project.repo")
	if _, err := os.Stat(projectRepo); err == nil {
		t.Errorf("unexpected %s — default --scope should be session, not project", projectRepo)
	}
}

// TestScope_Project verifies that --scope=project creates the shared
// .sesh/project.repo and skips the per-session repo file entirely.
func TestScope_Project(t *testing.T) {
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

	cmd, stderr := startSeshArgs(t, bin, home, project, "alpha",
		"--scope=project")
	defer killAndWait(t, cmd, stderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)

	projectRepo := filepath.Join(project, ".sesh", "project.repo")
	if _, err := os.Stat(projectRepo); err != nil {
		t.Fatalf("expected shared repo at %s under --scope=project: %v\nstderr:\n%s",
			projectRepo, err, stderr.String())
	}
	sessionRepo := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	if _, err := os.Stat(sessionRepo); err == nil {
		t.Errorf("unexpected per-session %s under --scope=project", sessionRepo)
	}
}

// TestScope_InvalidValue verifies kong's enum tag rejects unknown
// values before any side effects. Operators get a clear error naming
// the valid set, not a runtime failure deep in repo bootstrap.
func TestScope_InvalidValue(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()

	cmd := exec.Command(bin, "up", "--session=alpha", "--scope=bogus")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit for --scope=bogus, got success.\noutput:\n%s", out)
	}
	body := string(out)
	// Kong's "must be one of" enum message is the contract. Don't
	// pin the exact wording; just verify the offending value and the
	// flag name surface so the operator can debug.
	if !strings.Contains(body, "scope") || !strings.Contains(body, "bogus") {
		t.Errorf("error message should name --scope and the bogus value; got:\n%s", body)
	}
}

// TestScope_Mixed_Coexistence verifies two sessions in the same
// project running in different scopes coexist cleanly: alpha at
// --scope=session lands at .sesh/sessions/alpha.repo, beta at
// --scope=project lands at .sesh/project.repo, both files exist and
// are distinct inodes. Cross-propagation is exercised by waiting for
// alpha's seed commit to reach beta's shared repo via NATS autosync
// on the shared project-code subject — the wiring is scope-agnostic.
func TestScope_Mixed_Coexistence(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not on PATH")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	// Alpha first (session scope, default). Seeds from cwd because
	// hub is empty.
	alpha, alphaStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", alphaStderr.String())
	}

	// Wait for alpha's seed commit to propagate to the user-level
	// hub at ~/.sesh/hub.repo. Beta will bootstrap from there.
	hubRepo := filepath.Join(home, ".sesh", "hub.repo")
	if !waitForCheckins(t, hubRepo, 1, 15*time.Second) {
		t.Fatalf("hub never received alpha's seed (count=%d)", countCheckins(t, hubRepo))
	}

	// Beta opts into project scope.
	beta, betaStderr := startSeshArgs(t, bin, home, project, "beta",
		"--scope=project")
	defer killAndWait(t, beta, betaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "beta.json"), 15*time.Second)

	alphaRepo := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	projectRepo := filepath.Join(project, ".sesh", "project.repo")

	for _, p := range []string{alphaRepo, projectRepo} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to exist (mixed-scope): %v", p, err)
		}
	}
	if sameFile(t, alphaRepo, projectRepo) {
		t.Errorf("alpha.repo and project.repo resolve to the same inode — mixed-scope must keep distinct files")
	}
	// Sentinel: the legacy non-project session repo for beta must NOT
	// exist when beta is project-scoped.
	betaSessionRepo := filepath.Join(project, ".sesh", "sessions", "beta.repo")
	if _, err := os.Stat(betaSessionRepo); err == nil {
		t.Errorf("unexpected %s — beta is --scope=project, should not own a session repo", betaSessionRepo)
	}

	// Cross-scope propagation: alpha's seed should land in beta's
	// shared repo via NATS autosync on the shared project-code, the
	// same path TestSeedFromHubForSubsequentSession exercises for
	// pure-session sessions.
	if !waitForCheckins(t, projectRepo, 1, 15*time.Second) {
		t.Errorf("project.repo never received alpha's seed via autosync (count=%d)",
			countCheckins(t, projectRepo))
	}
}

// TestScope_Project_ConcurrentCommits exercises the SQLite contention
// path that --scope=project reintroduces: two libfossil handles on
// .sesh/project.repo issue commits concurrently. Without BEGIN
// IMMEDIATE, the second writer's first INSERT returns SQLITE_BUSY
// immediately because SQLite's deadlock-avoidance bypasses
// busy_timeout on SHARED→RESERVED upgrade races. The libfossil
// modernc/ncruces drivers now prepend _txlock=immediate to the DSN
// (libfossil#33), so concurrent writers serialize at BEGIN where
// busy_timeout applies.
//
// In-process via hub.NewHub (mirrors TestCrossSessionAutosync) because
// driving concurrent commits from subprocesses requires a sesh commit
// RPC we deliberately don't have.
func TestScope_Project_ConcurrentCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".sesh", "sessions"), 0o755); err != nil {
		t.Fatalf("mkdir .sesh: %v", err)
	}
	const projectCode = "abcdef0123456789abcdef0123456789abcdef02"
	sharedRepo := filepath.Join(project, ".sesh", "project.repo")

	// Hub A: first opener creates and bootstraps the repo. Subsequent
	// libfossil opens on the same file inherit busy_timeout via
	// hub.applySQLiteTuning on their own connection pools.
	hubA, err := hub.NewHub(ctx, hub.Config{
		RepoPath:     sharedRepo,
		ServerName:   "scope-project-A",
		NATSStoreDir: filepath.Join(project, ".sesh", "sessions", "A.messaging"),
		ProjectCode:  projectCode,
		NobodyCaps:   "gio",
	})
	if err != nil {
		t.Fatalf("hub A on shared repo: %v", err)
	}
	t.Cleanup(func() { _ = hubA.Stop() })
	go func() { _ = hubA.ServeHTTP(ctx) }()

	// Hub B opens the same SQLite file. Leaf-links to A so they share
	// a NATS plane; the contention we're testing happens at the
	// SQLite layer regardless.
	hubB, err := hub.NewHub(ctx, hub.Config{
		RepoPath:     sharedRepo,
		ServerName:   "scope-project-B",
		NATSStoreDir: filepath.Join(project, ".sesh", "sessions", "B.messaging"),
		LeafUpstream: hubA.LeafURL(),
		ProjectCode:  projectCode,
		NobodyCaps:   "gio",
	})
	if err != nil {
		t.Fatalf("hub B on shared repo: %v", err)
	}
	t.Cleanup(func() { _ = hubB.Stop() })
	go func() { _ = hubB.ServeHTTP(ctx) }()

	// Fire two commits in lockstep. A small barrier maximises
	// overlap on the SQLite write path; without busy_timeout one
	// transaction would return SQLITE_BUSY (5).
	var startBarrier sync.WaitGroup
	startBarrier.Add(1)
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)

	go func() {
		defer wg.Done()
		startBarrier.Wait()
		_, err := hubA.Commit(ctx, hub.CommitOpts{
			Files:   []hub.FileToCommit{{Name: "from-A.txt", Content: []byte("a\n")}},
			Message: "concurrent A",
			Author:  "test",
		})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		startBarrier.Wait()
		_, err := hubB.Commit(ctx, hub.CommitOpts{
			Files:   []hub.FileToCommit{{Name: "from-B.txt", Content: []byte("b\n")}},
			Message: "concurrent B",
			Author:  "test",
		})
		errs <- err
	}()

	startBarrier.Done()
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil {
			continue
		}
		// Surface SQLITE_BUSY explicitly so a future regression
		// (e.g., busy_timeout reverted upstream) gives a debuggable
		// failure rather than a generic write error.
		if isSQLiteBusy(err) {
			t.Fatalf("concurrent commit failed with SQLITE_BUSY — busy_timeout is unset or too low: %v", err)
		}
		t.Fatalf("concurrent commit failed: %v", err)
	}
}

// isSQLiteBusy reports whether err is or wraps a SQLite SQLITE_BUSY
// (code 5). Used by TestScope_Project_ConcurrentCommits to give a
// clean failure message when busy_timeout regresses.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	// modernc/sqlite returns *sqlite.Error with .Code() == 5, but
	// without importing the driver here we match the public string.
	s := err.Error()
	if strings.Contains(s, "SQLITE_BUSY") {
		return true
	}
	if strings.Contains(s, "database is locked") {
		return true
	}
	if inner := errors.Unwrap(err); inner != nil {
		return isSQLiteBusy(inner)
	}
	return false
}

// startSeshArgs is startSesh + extra CLI args. Used by scope tests
// which need to pass --scope=project alongside --session=<label>.
func startSeshArgs(t *testing.T, bin, home, project, session string, extra ...string) (*exec.Cmd, *syncBuf) {
	t.Helper()
	args := append([]string{"up", "--session=" + session}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = os.Stderr
	stderr := &syncBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh up %s: %v", strings.Join(args, " "), err)
	}
	return cmd, stderr
}

