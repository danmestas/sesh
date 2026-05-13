package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/danmestas/EdgeSync/hub"
	// Register the modernc/sqlite driver with libfossil's db package so
	// in-process hubs constructed in this test (TestCrossSessionAutosync)
	// can open their Fossil repos. Production wires this up in
	// cmd/sesh/main.go; the test binary doesn't include that file.
	_ "github.com/danmestas/libfossil/db/driver/modernc"
)

// TestPerSessionRepos verifies the per-session Fossil model: every
// session in a project owns its own repo at
// `.sesh/sessions/<label>.repo`, and the legacy shared
// `.sesh/project.repo` is not created. With the architectural pivot to
// per-session repos + NATS autosync (and away from a shared SQLite
// file), divergence of repo files is the new invariant — convergence
// happens at the publish-hook level, not the storage layer.
func TestPerSessionRepos(t *testing.T) {
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

	alpha, alphaStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)

	beta, betaStderr := startSesh(t, bin, home, project, "beta")
	defer killAndWait(t, beta, betaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "beta.json"), 15*time.Second)

	// Per-session repo files exist and are distinct.
	alphaRepo := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	betaRepo := filepath.Join(project, ".sesh", "sessions", "beta.repo")
	for _, p := range []string{alphaRepo, betaRepo} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected per-session repo at %s: %v", p, err)
		}
	}
	if sameFile(t, alphaRepo, betaRepo) {
		t.Errorf("alpha.repo and beta.repo resolve to the same inode — want distinct files")
	}

	// The legacy shared project.repo must NOT be created.
	legacy := filepath.Join(project, ".sesh", "project.repo")
	if _, err := os.Stat(legacy); err == nil {
		t.Errorf("legacy %s exists — pivot to per-session repos was incomplete", legacy)
	}
}

// TestSeedFromCwdForFirstSession verifies that the first sesh up in a
// fresh project (hub holds no commits) bootstraps its Fossil repo from
// the cwd's git worktree. With the per-session model, the first session
// is the only path through which the worktree snapshot enters the
// Fossil mesh; subsequent sessions clone from the hub (see
// TestSeedFromHubForSubsequentSession).
func TestSeedFromCwdForFirstSession(t *testing.T) {
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

	alpha, alphaStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)

	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("first session did not seed from cwd:\n%s", alphaStderr.String())
	}
	if line := findSlogLine(alphaStderr.String(), "fossil repo cloned from hub"); line != "" {
		t.Errorf("first session unexpectedly cloned from hub (hub should be empty): %s", line)
	}
}

// TestSeedFromHubForSubsequentSession verifies that once the hub has
// content (from a peer session's autosync), a new session bootstraps
// by cloning from the hub rather than re-seeding from cwd. Re-seeding
// from cwd would fork the history with two divergent root commits at
// the same content, defeating the point of having a shared
// fossil-sync subject.
func TestSeedFromHubForSubsequentSession(t *testing.T) {
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

	// Alpha: first session, seeds from cwd, propagates to hub.
	alpha, alphaStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", alphaStderr.String())
	}

	// Wait for autosync to propagate alpha's seed commit to the hub.
	hubRepo := filepath.Join(home, ".sesh", "hub.repo")
	if !waitForCheckins(t, hubRepo, 1, 15*time.Second) {
		t.Fatalf("hub.repo never received alpha's seed commit (count=%d)", countCheckins(t, hubRepo))
	}

	// Beta: spawns after hub has content. Should clone from hub, not
	// re-seed from cwd.
	beta, betaStderr := startSesh(t, bin, home, project, "beta")
	defer killAndWait(t, beta, betaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "beta.json"), 15*time.Second)

	if !waitForSlog(t, betaStderr, "fossil repo cloned from hub", 10*time.Second) {
		t.Fatalf("beta did not clone from hub:\n%s", betaStderr.String())
	}
	if line := findSlogLine(betaStderr.String(), "seeded fossil from worktree"); line != "" {
		t.Errorf("beta unexpectedly re-seeded from cwd; would fork history: %s", line)
	}

	// Beta's repo should match alpha's commit count (post-clone).
	betaRepo := filepath.Join(project, ".sesh", "sessions", "beta.repo")
	alphaRepo := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	wantCount := countCheckins(t, alphaRepo)
	if !waitForCheckins(t, betaRepo, wantCount, 5*time.Second) {
		t.Errorf("beta.repo has %d check-ins, want %d (alpha's count post-clone)",
			countCheckins(t, betaRepo), wantCount)
	}
}

// TestCrossSessionAutosync is the load-bearing test for the
// per-session model: a commit landing in one session's hub propagates
// to a peer session's hub in the same project via the EdgeSync NATS
// fossil-sync subject (keyed on the pinned project-code), not via
// shared SQLite. The architecture's whole point is that the publish
// hook fires natively from the in-process hub on every commit and
// peer hubs subscribed to the same subject pull the artifact in.
//
// Topology: two session-level hubs at distinct .sesh/sessions/<label>.repo
// paths, leaf-linked directly (B as leaf of A), both pinned to the
// same project-code. This is the minimum that exercises the
// publish→pull pipeline; in production sesh constructs A→central←B
// via the user-level hub at ~/.sesh/hub.repo, but the central is a
// passive collector — the wiring under test is the per-session
// publish/pull symmetry, not the leaf-mesh fan-out.
//
// Constructed in-process via EdgeSync's hub package rather than
// spawning sesh up subprocesses because injecting a commit into a
// running sesh up's per-session repo from the test process requires
// either the publish hook fire from THAT process (impossible across
// process boundaries — that's the spike's finding) or a sesh commit
// RPC verb that we deliberately deferred to a follow-up PR.
func TestCrossSessionAutosync(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const projectCode = "abcdef0123456789abcdef0123456789abcdef01"
	project := t.TempDir()
	sessionsDir := filepath.Join(project, ".sesh", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}

	// Session A hub.
	hubA, err := hub.NewHub(ctx, hub.Config{
		RepoPath:     filepath.Join(sessionsDir, "alpha.repo"),
		ServerName:   "session-alpha",
		NATSStoreDir: filepath.Join(sessionsDir, "alpha.messaging"),
		ProjectCode:  projectCode,
		NobodyCaps:   "gio",
	})
	if err != nil {
		t.Fatalf("session A hub: %v", err)
	}
	t.Cleanup(func() { _ = hubA.Stop() })
	go func() { _ = hubA.ServeHTTP(ctx) }()

	// Session B hub leaf-linked to A.
	hubB, err := hub.NewHub(ctx, hub.Config{
		RepoPath:     filepath.Join(sessionsDir, "beta.repo"),
		ServerName:   "session-beta",
		NATSStoreDir: filepath.Join(sessionsDir, "beta.messaging"),
		LeafUpstream: hubA.LeafURL(),
		ProjectCode:  projectCode,
		NobodyCaps:   "gio",
	})
	if err != nil {
		t.Fatalf("session B hub: %v", err)
	}
	t.Cleanup(func() { _ = hubB.Stop() })
	go func() { _ = hubB.ServeHTTP(ctx) }()

	// Wait for the leaf link to come up.
	waitFor(t, 10*time.Second, "leaf link B → A", func() bool {
		return hubA.NumLeafs() >= 1
	})

	// Commit a unique payload on session A's hub. Publishes natively
	// because it's the same process that owns A's repo handle.
	const fileName = "from-alpha.txt"
	payload := []byte("hello-from-alpha\n")
	if _, err := hubA.Commit(ctx, hub.CommitOpts{
		Files:   []hub.FileToCommit{{Name: fileName, Content: payload}},
		Message: "cross-session autosync probe",
		Author:  "test",
	}); err != nil {
		t.Fatalf("hubA.Commit: %v", err)
	}

	// Within the bounded window, session B's repo should hold the file.
	// Read goes through B's libfossil handle, which reflects whatever
	// the autosync pipeline pulled in. The 10s budget matches the
	// EdgeSync cross-leaf propagation test (TestCrossLeaf_SharedProjectCode_PropagatesCommit).
	waitFor(t, 10*time.Second, "session B sees from-alpha.txt at trunk", func() bool {
		got, readErr := hubB.Read(ctx, fileName)
		return readErr == nil && bytes.Equal(got, payload)
	})
}

// TestSubLeaf_NoIndependentGitSeed verifies that an edgesync sub-leaf
// running under a sesh does NOT seed its own Fossil from the cwd's git
// worktree. The sub-leaf's repo starts empty; if it receives commits,
// they come from the parent via NATS sync (verified by
// TestSubLeaf_SyncsFromSesh).
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

	// Parent: sesh up — seeds the per-session repo.
	parent, parentStderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, parent, parentStderr)
	statePath := filepath.Join(project, ".sesh", "sessions", "alpha.json")
	state := waitForURLs(t, statePath, 15*time.Second)
	if !waitForSlog(t, parentStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("parent never seeded:\n%s", parentStderr.String())
	}

	parentRepo := filepath.Join(project, ".sesh", "sessions", "alpha.repo")
	parentCount := countCheckins(t, parentRepo)
	if parentCount != 1 {
		t.Fatalf("parent alpha.repo has %d check-ins, want 1", parentCount)
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

	if !waitForSlog(t, &subStderr, "edgesync hub running", 10*time.Second) {
		t.Fatalf("subleaf never bound:\n%s", subStderr.String())
	}

	// Sub-leaf MUST NOT log a "seeded fossil from worktree" line. Only
	// sesh does git seeding; edgesync hub serve doesn't.
	time.Sleep(500 * time.Millisecond)
	if line := findSlogLine(subStderr.String(), "seeded fossil from worktree"); line != "" {
		t.Errorf("sub-leaf logged independent seed — should not happen: %s", line)
	}

	// Sub-leaf has NOT independently seeded. Either 0 (no sync) or
	// matches parent (sync happened). It must NEVER be a different
	// non-zero number, which would indicate an independent seed.
	subCount := countCheckins(t, subleafRepo)
	if subCount != 0 && subCount != parentCount {
		t.Errorf("sub-leaf check-ins = %d; want 0 (no sync) or %d (synced).",
			subCount, parentCount)
	}
}

// TestSubLeaf_SyncsFromSesh verifies that an edgesync sub-leaf spawned
// with --seed-from-upstream pointing at the parent sesh's fossil HTTP
// endpoint clones the parent's existing Fossil state (including the
// worktree seed) and stays in sync via NATS auto-publish.
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

	parentCount := countCheckins(t, filepath.Join(project, ".sesh", "sessions", "alpha.repo"))

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
// session's per-session repo propagate to ~/.sesh/hub.repo over
// EdgeSync's fossil-sync subject. With the per-session model the hub
// is a passive collector: every session commit fires its in-process
// publish hook, the hub subscribes to the same project-code subject,
// and the hub's own libfossil pulls the artifact in.
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

	want := countCheckins(t, filepath.Join(project, ".sesh", "sessions", "alpha.repo"))
	if !waitForCheckins(t, hubRepo, want, 15*time.Second) {
		t.Errorf("hub.repo did not converge to alpha.repo within 15s: hub=%d, alpha=%d",
			countCheckins(t, hubRepo), want)
	}
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

// waitForCheckins polls a Fossil repo until its check-in count reaches
// at least `want`, returning true on success and false on timeout. Used
// to bound autosync convergence tests so we surface "did the
// publish→pull pipeline actually fire" rather than racing a fixed
// sleep.
func waitForCheckins(t *testing.T, repoPath string, want int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if countCheckins(t, repoPath) >= want {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// waitFor calls fn periodically until it returns true or timeout
// elapses. On timeout, fails the test with the supplied label so the
// failure message points at the unmet condition.
func waitFor(t *testing.T, timeout time.Duration, label string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout (%s) waiting for: %s", timeout, label)
}

// sameFile reports whether two paths resolve to the same inode. With
// per-session repo files, this catches the regression case where a
// future change accidentally points two sessions at one file.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	infoA, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	infoB, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(infoA, infoB)
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
// repo or returns "" if not available.
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
	candidate := filepath.Join(cwd, "..", "..", name)
	abs, _ := filepath.Abs(candidate)
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
		return abs
	}
	return ""
}

var _ = fmt.Sprintf // silence import if helpers shrink
