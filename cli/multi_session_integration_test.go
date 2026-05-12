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
// worktree. The sub-leaf's repo starts empty.
//
// What this test verifies (HARD): the sub-leaf does not log a seed
// line. Only sesh does git seeding; edgesync hub serve doesn't.
//
// What this test does NOT verify (yet — see TestSubLeaf_DoesNotSyncToday
// for the negative finding): the sub-leaf does NOT receive the parent's
// commits via NATS sync. EdgeSync's fossil sync is subscriber-only
// (no auto-publish on commit) and is keyed by per-repo project-code
// (so two repos with different project-codes subscribe to different
// subjects and can't reach each other). Fixing this is upstream
// EdgeSync work — see TestSubLeaf_DoesNotSyncToday.
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

// TestSubLeaf_DoesNotSyncToday documents an upstream EdgeSync
// limitation: Fossil sync over NATS does not auto-propagate commits
// between sub-leaves and parent sessions today.
//
// Reason (from EdgeSync hub/hub.go startFossilSyncSubscriber):
//
//  1. Each Fossil repo's sync subject is "<prefix>.<project-code>.sync"
//     where project-code is a per-repo UUID set at libfossil.Create()
//     time. The parent's repo, the sub-leaf's repo, and the hub's repo
//     each have different project-codes → different subjects.
//  2. The sync handler is subscribe-only: it dispatches incoming xfer
//     requests via Repo.HandleSync. There's no auto-publish of local
//     commits to the sync subject.
//
// So today, sub-leaves and the hub start empty and stay empty unless
// something explicitly publishes an xfer request matching the
// participants' shared project-code (which they don't share).
//
// Fix is upstream-EdgeSync work — see GitHub issue (filed alongside
// this test). The test asserts the current behavior so we'll notice
// when the upstream fix lands.
func TestSubLeaf_DoesNotSyncToday(t *testing.T) {
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
	if !waitForSlog(t, &subStderr, "edgesync hub running", 10*time.Second) {
		t.Fatalf("subleaf never bound")
	}

	parentCount := countCheckins(t, filepath.Join(project, ".sesh", "project.repo"))

	// Allow a generous settle window. If sync ever starts working
	// upstream, this test will start failing (and we'll know to
	// flip its assertion).
	time.Sleep(5 * time.Second)
	subCount := countCheckins(t, subleafRepo)

	if subCount == parentCount {
		t.Fatalf("sync now propagates from sesh to sub-leaf (sub=%d, parent=%d) — upstream EdgeSync fix has landed. Flip this test's assertion and remove the known-limitation doc.", subCount, parentCount)
	}
	if subCount != 0 {
		t.Errorf("sub-leaf has unexpected commits (%d) — neither empty nor synced. Investigate.", subCount)
	}
	t.Logf("confirmed: sub-leaf has 0 commits, parent has %d — sync does not propagate today (expected)", parentCount)
}

// TestHub_DoesNotAccumulateProjectCommitsToday — same finding as
// TestSubLeaf_DoesNotSyncToday but for the hub: commits made on a
// session's project.repo don't reach ~/.sesh/hub.repo via NATS sync
// today. Same root cause (project-code mismatch + no auto-publish).
//
// When the upstream EdgeSync fix lands, this test will fail; flip the
// assertion to "hub.repo eventually has parentCount commits" at that
// point.
func TestHub_DoesNotAccumulateProjectCommitsToday(t *testing.T) {
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

	// Generous settle window.
	want := countCheckins(t, filepath.Join(project, ".sesh", "project.repo"))
	time.Sleep(5 * time.Second)
	got := countCheckins(t, hubRepo)

	if got == want {
		t.Fatalf("hub.repo now syncs from project.repo (hub=%d, project=%d) — upstream EdgeSync fix has landed. Flip this test's assertion.", got, want)
	}
	if got != 0 {
		t.Errorf("hub.repo has unexpected commits (%d) — neither empty nor synced", got)
	}
	t.Logf("confirmed: hub.repo has 0 commits, project has %d — sync does not propagate today (expected)", want)
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
