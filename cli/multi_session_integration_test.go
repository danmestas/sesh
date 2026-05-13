package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMultiSession_SharedProjectRepo verifies the project-level Fossil
// model: multiple sessions in the same cwd share `.sesh/project.repo`,
// the first session seeds it from the worktree, subsequent sessions
// open the existing repo without re-seeding. Counts check-in events
// directly from SQLite to confirm there is exactly one seed commit.
func TestMultiSession_SharedProjectRepo(t *testing.T) {
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

	repoPath := filepath.Join(project, ".sesh", "project.repo")

	// 1. Alpha first: should seed.
	alpha, alphaStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha did not seed:\n%s", alphaStderr.String())
	}

	// 2. Beta second: should NOT seed; should log pre-existed.
	beta, betaStderr := startSesh(t, bin, home, project, "beta")
	defer killAndWait(t, beta, betaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "beta.json"), 15*time.Second)
	if !waitForSlog(t, betaStderr, "fossil repo pre-existed", 10*time.Second) {
		t.Fatalf("beta did not skip seed:\n%s", betaStderr.String())
	}
	if found := findSlogLine(betaStderr.String(), "seeded fossil from worktree"); found != "" {
		t.Errorf("beta should not have seeded; got: %s", found)
	}

	// 3. Repo file at the project level exists; per-session repo paths do not.
	if _, err := os.Stat(repoPath); err != nil {
		t.Fatalf("project repo missing: %v", err)
	}
	for _, label := range []string{"alpha", "beta"} {
		stale := filepath.Join(project, ".sesh", "sessions", label+".repo")
		if _, err := os.Stat(stale); err == nil {
			t.Errorf("unexpected per-session repo at %s — should be project-level only", stale)
		}
	}

	// 4. Exactly one check-in commit (the seed).
	if got := countCheckins(t, repoPath); got != 1 {
		t.Errorf("project.repo has %d check-ins, want 1 (seed only)", got)
	}
}

// TestSubLeaf_NoIndependentGitSeed verifies that an edgesync sub-leaf
// running under a sesh does NOT seed its own Fossil from the cwd's git
// worktree. The sub-leaf's repo starts empty; if it receives commits,
// they come from the parent via NATS sync (verified by
// TestSubLeaf_SyncsViaProjectCode).
//
// What this test verifies (HARD): the sub-leaf does not log a seed
// line. Only sesh does git seeding; edgesync hub serve doesn't.
func TestSubLeaf_NoIndependentGitSeed(t *testing.T) {
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
	edgesyncBin := buildEdgeSync(t)
	if edgesyncBin == "" {
		t.Skip("EdgeSync binary unavailable (set EDGESYNC_BINARY or place sibling repo at ../EdgeSync)")
	}

	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	// Parent: sesh up — seeds the project.repo.
	parent, parentStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, parent, parentStderr)
	statePath := filepath.Join(project, ".sesh", "sessions", "alpha.json")
	state := waitForURLs(t, statePath, 15*time.Second)
	if !waitForSlog(t, parentStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("parent never seeded:\n%s", parentStderr.String())
	}

	parentRepo := filepath.Join(project, ".sesh", "project.repo")
	parentCount := countCheckins(t, parentRepo)
	if parentCount != 1 {
		t.Fatalf("parent project.repo has %d check-ins, want 1", parentCount)
	}

	// Sub-leaf: edgesync hub serve --leaf-upstream=<parent leaf URL>.
	subleafDir := t.TempDir()
	subleafRepo := filepath.Join(subleafDir, "subleaf.repo")
	sub := exec.Command(edgesyncBin, "hub", "serve",
		"--repo="+subleafRepo,
		"--leaf-upstream="+state.LeafURL,
		"--http-port=0", "--nats-client-port=0", "--nats-leaf-port=0")
	var subStderr syncBuf
	sub.Stdout = &subStderr
	sub.Stderr = &subStderr
	if err := sub.Start(); err != nil {
		t.Fatalf("start subleaf: %v", err)
	}
	defer killAndWait(t, sub, &subStderr)

	// Wait for sub-leaf to bind.
	if !waitForSlog(t, &subStderr, "edgesync hub running", 10*time.Second) {
		t.Fatalf("subleaf never bound:\n%s", subStderr.String())
	}

	// Sub-leaf MUST NOT log a "seeded fossil from worktree" line. Only
	// sesh does git seeding; edgesync hub serve doesn't.
	time.Sleep(500 * time.Millisecond)
	if line := findSlogLine(subStderr.String(), "seeded fossil from worktree"); line != "" {
		t.Errorf("sub-leaf logged independent seed — should not happen: %s", line)
	}

	// Hard assertion: sub-leaf has NOT independently seeded. Either 0
	// (no sync happened) or matches parent (sync happened). It must
	// NEVER be a different non-zero number, which would indicate the
	// sub-leaf created its own seed.
	subCount := countCheckins(t, subleafRepo)
	if subCount != 0 && subCount != parentCount {
		t.Errorf("sub-leaf check-ins = %d; want 0 (no sync) or %d (synced). A different non-zero value would indicate an independent seed.", subCount, parentCount)
	}
}

// TestSubLeaf_SyncsFromSesh verifies that an edgesync sub-leaf spawned
// with --seed-from-upstream pointing at the parent sesh's fossil HTTP
// endpoint clones the parent's existing Fossil state (including the
// worktree seed) and stays in sync via NATS auto-publish.
//
// --seed-from-upstream serves two roles at once: it backfills the
// commits that happened before the sub-leaf joined, AND it inherits
// the parent's project-code so subsequent commits land on the same
// fossil-sync subject.
//
// Requires EdgeSync#159 (CLI flag) + libfossil v0.6.1
// (CreateOpts.ProjectCode).
func TestSubLeaf_SyncsFromSesh(t *testing.T) {
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
	edgesyncBin := buildEdgeSync(t)
	if edgesyncBin == "" {
		t.Skip("EdgeSync binary unavailable")
	}

	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	parent, parentStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, parent, parentStderr)
	state := waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, parentStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("parent never seeded")
	}

	// Sub-leaf needs to clone the parent's existing state (the seed
	// commit happened before the sub-leaf joined the sync subject — pure
	// auto-publish wouldn't backfill). --seed-from-upstream pulls the
	// parent's state via HTTP xfer and inherits the parent's
	// project-code, so subsequent commits propagate via NATS auto-publish.
	if state.FossilURL == "" {
		t.Fatalf("parent state missing fossil_url (got %+v)", state)
	}

	subleafDir := t.TempDir()
	subleafRepo := filepath.Join(subleafDir, "subleaf.repo")
	sub := exec.Command(edgesyncBin, "hub", "serve",
		"--repo="+subleafRepo,
		"--leaf-upstream="+state.LeafURL,
		"--seed-from-upstream="+state.FossilURL,
		"--http-port=0", "--nats-client-port=0", "--nats-leaf-port=0")
	var subStderr syncBuf
	sub.Stdout = &subStderr
	sub.Stderr = &subStderr
	if err := sub.Start(); err != nil {
		t.Fatalf("start subleaf: %v", err)
	}
	defer killAndWait(t, sub, &subStderr)
	if !waitForSlog(t, &subStderr, "edgesync hub running", 10*time.Second) {
		t.Fatalf("subleaf never bound")
	}

	parentCount := countCheckins(t, filepath.Join(project, ".sesh", "project.repo"))

	// Sync should converge within a few seconds via auto-publish.
	deadline := time.Now().Add(15 * time.Second)
	var subCount int
	for time.Now().Before(deadline) {
		subCount = countCheckins(t, subleafRepo)
		if subCount == parentCount {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("sub-leaf did not converge to parent within 15s: sub=%d, parent=%d", subCount, parentCount)
}

// TestHub_AccumulatesProjectCommits verifies that commits made on a
// session's project.repo propagate to ~/.sesh/hub.repo over EdgeSync's
// fossil-sync subject. Coordination works because sesh pins a
// per-project project-code (seeded from hostname + project name on
// first run; read from .sesh/project-code thereafter) and passes it
// via hub.Config.ProjectCode + the SESH_PROJECT_CODE env var to the
// spawned hub, so both repos subscribe to the same sync subject.
//
// Requires EdgeSync's cross-leaf fossil sync (#157 / merged 2026-05-12)
// AND libfossil v0.6.1 (CreateOpts.ProjectCode).
func TestHub_AccumulatesProjectCommits(t *testing.T) {
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

	cmd, stderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, cmd, stderr)
	_ = waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, stderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("never seeded:\n%s", stderr.String())
	}

	hubRepo := filepath.Join(home, ".sesh", "hub.repo")
	if _, err := os.Stat(hubRepo); err != nil {
		t.Fatalf("hub.repo missing at %s: %v", hubRepo, err)
	}

	want := countCheckins(t, filepath.Join(project, ".sesh", "project.repo"))

	deadline := time.Now().Add(15 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = countCheckins(t, hubRepo)
		if got == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("hub.repo did not converge to project.repo within 15s: hub=%d, project=%d", got, want)
}

// --- helpers ---

// startSesh launches `sesh up --session=<label>` in project with HOME
// set, returning the cmd and a buffered stderr.
func startSesh(t *testing.T, bin, home, project, session string) (*exec.Cmd, *syncBuf) {
	t.Helper()
	cmd := exec.Command(bin, "up", "--session="+session)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = os.Stderr
	stderr := &syncBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh up --session=%s: %v", session, err)
	}
	return cmd, stderr
}

func killAndWait(t *testing.T, cmd *exec.Cmd, _ *syncBuf) {
	t.Helper()
	if cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-done
	}
}

func waitForSlog(t *testing.T, sb *syncBuf, needle string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if findSlogLine(sb.String(), needle) != "" {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// countCheckins counts type='ci' events in a Fossil repo (SQLite file).
// Returns 0 if the file doesn't exist yet.
func countCheckins(t *testing.T, repoPath string) int {
	t.Helper()
	if _, err := os.Stat(repoPath); err != nil {
		return 0
	}
	cmd := exec.Command("sqlite3", repoPath,
		"SELECT count(*) FROM event WHERE type='ci'")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// SQLite may transiently fail if the writer is mid-transaction.
		// Return 0 so the polling loop retries.
		t.Logf("sqlite3 (transient?): %v\n%s", err, stderr.String())
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil {
		return 0
	}
	return n
}

// buildEdgeSync builds the edgesync binary from a sibling ../EdgeSync
// repo or returns "" if not available. Mirrors resolveSeshBinary's
// approach.
func buildEdgeSync(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("EDGESYNC_BINARY"); env != "" {
		return env
	}
	siblingPath := siblingRepo(t, "EdgeSync")
	if siblingPath == "" {
		return ""
	}
	bin := filepath.Join(t.TempDir(), "edgesync")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/edgesync")
	cmd.Dir = siblingPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("edgesync build failed: %v\n%s", err, out)
		return ""
	}
	return bin
}

func siblingRepo(t *testing.T, name string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// from cli/ → repo root → parent → sibling
	candidate := filepath.Join(cwd, "..", "..", name)
	abs, _ := filepath.Abs(candidate)
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
		return abs
	}
	return ""
}

var _ = fmt.Sprintf // silence import if helpers shrink
