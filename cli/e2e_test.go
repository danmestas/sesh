//go:build !orch_e2e

package cli_test

// Tier 1 end-to-end tests — mock variant.
//
// These tests stand in for the "real" Tier 1 suite from
// /tmp/sesh-e2e-test-plan.md. They exercise the same three properties as
// the orch variant (cli/e2e_orch_test.go), but the "worker" is the test
// code itself driving the project repo via the libfossil Go API rather
// than a real `orch-spawn claude --sesh-session <label> --accessory
// fossil-worker` invocation. The mock variant carries the load-bearing
// assertions and runs in normal CI; the orch variant validates the same
// properties through the real subprocess chain (claude + tmux + recipe)
// once orch can be driven non-interactively from a Go test.
//
// Three properties under test:
//
//   - T1.1 (TestE2E_ThreeWorkers_StarFanout): a star topology of three
//     sessions sharing one project repo via --scope=project propagates
//     every worker's commit to every peer within bounded time. The
//     EdgeSync layer's TestCrossLeaf_ThreeHubs_CommitAnnouncePropagatesToAll
//     proves the multi-responder .sync race fix at the hub layer; this
//     test proves sesh's leaf-link plumbing carries the property end-to-
//     end through the sesh up subprocesses and the per-project repo.
//
//   - T1.2 (TestE2E_FullMissionLoop): the full implement → verify →
//     materialize → git-stage loop. First test that exercises EVERY
//     slice — Slices 1, 2, 3, 5, 6 together — end-to-end. If it passes,
//     the swarm workflow is real, not just six unit-tested primitives.
//
//   - T1.3 (TestE2E_HubRestart_MidMission): the swarm survives a kill
//     of the central user-wide hub mid-mission. Commits made AFTER the
//     hub respawns must still propagate to peers. Proves leaf-side
//     resilience against the operator-laptop-suspend / network-blip
//     failure mode that would otherwise silently break autosync.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestE2E_ThreeWorkers_StarFanout brings up three `sesh up` subprocesses
// (alpha, beta, gamma) sharing one project repo via `--scope=project`,
// then rotates through three commit rounds — each worker commits a
// distinct file in turn, and the other two must observe it within a
// bounded window. The test is symmetric in all three directions, so a
// regression that breaks any single leaf-link direction surfaces here.
//
// Why three instead of two: the two-worker convergence case is already
// covered by TestSwarmTBD_TwoWorkers_ConvergeOnSharedTrunk. Three is the
// minimum that exercises a multi-responder .sync at the sesh leaf layer.
// EdgeSync's TestCrossLeaf_ThreeHubs_CommitAnnouncePropagatesToAll proves
// the hub-layer race fix; this test proves the sesh subprocess wiring
// inherits that fix.
func TestE2E_ThreeWorkers_StarFanout(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (E2E star fanout)")
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

	gamma, gammaStderr := startSeshArgs(t, bin, home, project, "gamma", "--scope=project")
	defer killAndWait(t, gamma, gammaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "gamma.json"), 15*time.Second)

	alphaWT := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")
	betaWT := mustRunWorktree(t, bin, home, project, "beta", "--scope=project")
	gammaWT := mustRunWorktree(t, bin, home, project, "gamma", "--scope=project")

	repoPath := filepath.Join(project, ".sesh", "project.repo")

	type round struct {
		commitFrom string
		checkout   string
		fileName   string
		payload    string
		peers      []struct{ name, checkout string }
	}
	rounds := []round{
		{
			commitFrom: "alpha",
			checkout:   alphaWT,
			fileName:   "fanout-from-alpha.txt",
			payload:    "alpha-was-here\n",
			peers: []struct{ name, checkout string }{
				{"beta", betaWT},
				{"gamma", gammaWT},
			},
		},
		{
			commitFrom: "beta",
			checkout:   betaWT,
			fileName:   "fanout-from-beta.txt",
			payload:    "beta-was-here\n",
			peers: []struct{ name, checkout string }{
				{"alpha", alphaWT},
				{"gamma", gammaWT},
			},
		},
		{
			commitFrom: "gamma",
			checkout:   gammaWT,
			fileName:   "fanout-from-gamma.txt",
			payload:    "gamma-was-here\n",
			peers: []struct{ name, checkout string }{
				{"alpha", alphaWT},
				{"beta", betaWT},
			},
		},
	}

	for _, r := range rounds {
		if err := os.WriteFile(filepath.Join(r.checkout, r.fileName), []byte(r.payload), 0o644); err != nil {
			t.Fatalf("write %s into %s checkout: %v", r.fileName, r.commitFrom, err)
		}
		if err := commitViaLibfossil(repoPath, r.checkout, r.fileName,
			"swarm "+r.commitFrom+" fanout commit"); err != nil {
			t.Fatalf("%s commit via libfossil: %v", r.commitFrom, err)
		}
		for _, p := range r.peers {
			if !awaitPeerSees(p.checkout, repoPath, r.fileName, r.payload, 10*time.Second) {
				t.Fatalf("%s's checkout did not see %s's commit (%s) within 10s",
					p.name, r.commitFrom, r.fileName)
			}
		}
	}

	// Final trunk-state assertion: materialize from alpha, all three files
	// land. If any leaf-link broke mid-test and a recovery loop papered
	// over the gap, the round assertions above would pass but the final
	// trunk state would be missing a file.
	outDir := t.TempDir()
	mustRunMaterialize(t, bin, home, project, "alpha",
		"--scope=project", "--output="+outDir)
	for _, r := range rounds {
		got, err := os.ReadFile(filepath.Join(outDir, r.fileName))
		if err != nil {
			t.Errorf("materialized output missing %s: %v", r.fileName, err)
			continue
		}
		if string(got) != r.payload {
			t.Errorf("materialized %s = %q; want %q", r.fileName, string(got), r.payload)
		}
	}
}

// TestE2E_FullMissionLoop walks the full swarm workflow: worker commits
// a file, verifier reads the timeline and emits a GO verdict matching
// the fossil-verifier accessory template, operator materializes with
// --git-add, and the file shows up in `git diff --cached`.
//
// In the mock variant the worker and verifier are the test code itself.
// In the orch variant (e2e_orch_test.go) they are real claude
// subprocesses launched via orch-spawn with --accessory fossil-worker
// and --accessory fossil-verifier respectively. The assertion shape is
// identical because both variants drive the same on-disk surface
// (project.repo and the materialized git worktree).
//
// This is the first test that exercises EVERY slice end-to-end: Slice 1
// (autosync), Slice 2 (worktree), Slice 3 (materialize), Slice 5
// (worker-cwd), Slice 6 (verifier template), Slice 7 (the convergence
// gate). If it passes, the swarm workflow is real — not just six
// independently-tested primitives that happen to live in the same repo.
func TestE2E_FullMissionLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (E2E full mission loop)")
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

	// Slice 2 + Slice 5: provision the checkout via `sesh worktree`, then
	// also resolve via `sesh worker-cwd` to assert they agree (the contract
	// orch-spawn relies on).
	checkout := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")
	cwd := mustRunWorkerCwd(t, bin, home, project, "alpha", "--scope=project")
	if eq, err := samePath(checkout, cwd); err != nil {
		t.Fatalf("samePath(checkout, cwd): %v", err)
	} else if !eq {
		t.Errorf("worker-cwd (%q) != worktree (%q)", cwd, checkout)
	}

	// Worker phase: stand-in for `orch-spawn claude --sesh-session alpha
	// --accessory fossil-worker` running a recipe that writes mission.txt
	// and runs `fossil add mission.txt && fossil commit -m "implement"`.
	// The mock implementation lands the same on-disk state directly via
	// libfossil, which is exactly what the worker would do via the fossil
	// CLI under the hood.
	const missionFile = "mission.txt"
	const missionPayload = "mission accomplished\n"
	if err := os.WriteFile(filepath.Join(checkout, missionFile), []byte(missionPayload), 0o644); err != nil {
		t.Fatalf("worker phase write %s: %v", missionFile, err)
	}
	repoPath := filepath.Join(project, ".sesh", "project.repo")
	if err := commitViaLibfossil(repoPath, checkout, missionFile, "implement mission"); err != nil {
		t.Fatalf("worker phase commit: %v", err)
	}

	// Verifier phase: stand-in for `orch-spawn claude --sesh-session alpha
	// --accessory fossil-verifier` running a recipe that runs
	// `fossil timeline` and emits the GO verdict to stdout per the
	// accessory template. The mock implementation reads the trunk via
	// libfossil and emits a verdict string matching the template at
	// wardrobe/accessories/fossil-verifier/accessory.md.
	//
	// We don't bother updating the checkout before reading — the worker
	// just committed against it in the line above, so it's already at
	// trunk HEAD. We DO read the file back through the checkout to prove
	// the verifier-side view sees what the worker committed; if autosync
	// were silently dropping commits the read would return ENOENT.
	gotPayload, err := os.ReadFile(filepath.Join(checkout, missionFile))
	if err != nil {
		t.Fatalf("verifier phase: read %s back from checkout: %v", missionFile, err)
	}
	if string(gotPayload) != missionPayload {
		t.Fatalf("verifier phase: %s = %q; want %q", missionFile, gotPayload, missionPayload)
	}
	verdict := renderMockGoVerdict("alpha", missionFile)
	if !strings.Contains(verdict, "Verdict: GO") {
		t.Errorf("verifier verdict missing 'Verdict: GO' line:\n%s", verdict)
	}
	if !strings.Contains(verdict, "Recommendation to operator: merge") {
		t.Errorf("verifier verdict missing 'Recommendation to operator: merge':\n%s", verdict)
	}
	t.Logf("verifier verdict:\n%s", verdict)

	// Operator phase: materialize with --git-add into a tmp git worktree
	// that already has a baseline commit (setupGitWorktree did that on
	// `project`). The materialize subcommand operates against cwd by
	// default; passing --output redirects it to a separate dir, which is
	// what we want here so the test owns the destination state.
	//
	// We re-use the project dir for materialize so the staged file lands
	// in the git index, then assert via `git diff --cached`. --allow-dirty
	// because setupGitWorktree leaves agent-note.md untracked.
	mustRunMaterialize(t, bin, home, project, "alpha",
		"--scope=project", "--git-add", "--allow-dirty")

	diffCmd := exec.Command("git", "diff", "--cached", "--name-only")
	diffCmd.Dir = project
	var diffOut bytes.Buffer
	diffCmd.Stdout = &diffOut
	if err := diffCmd.Run(); err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	staged := diffOut.String()
	if !strings.Contains(staged, missionFile) {
		t.Errorf("git diff --cached --name-only missing %s; got: %s", missionFile, staged)
	}

	// Round-trip sanity: `sesh down alpha` cleanly tears down. The full
	// loop's last step is the operator's signal that the mission is over;
	// we exercise it here so the test fails on regressions in the
	// shutdown path (e.g. a leaked session file).
	if err := alpha.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT alpha: %v", err)
	}
	doneCh := make(chan struct{})
	go func() {
		_, _ = alpha.Process.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Errorf("alpha did not exit within 10s of SIGINT")
		_ = alpha.Process.Signal(syscall.SIGKILL)
		<-doneCh
	}
	// State file should be reaped.
	statePath := filepath.Join(project, ".sesh", "sessions", "alpha.json")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); errors.Is(err, fs.ErrNotExist) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("alpha session JSON still present after SIGINT: %s", statePath)
}

// TestE2E_HubRestart_MidMission proves the swarm survives a kill of the
// central user-wide hub mid-mission. The chain under test:
//
//  1. alpha + beta come up under --scope=project; both leaf-link to the
//     central user-wide hub at ~/.sesh/hub.url.
//  2. alpha commits pre-restart.txt; beta observes it (baseline that
//     the link is healthy).
//  3. Test kills the user-wide hub daemon (NOT the sesh up processes).
//     The expected behavior: leaf-side reconnect-or-spawn logic detects
//     hub-gone within ~3s and re-establishes the link.
//  4. alpha commits post-restart.txt; beta observes it within 15s. The
//     longer window reflects hub-recovery latency vs. the steady-state
//     10s budget the other tests use.
//  5. Materialize from alpha. Both files must be present.
//
// Note on hub recovery: sesh's current `sesh up` is what spawns the
// user-wide hub on first-touch (via ensureHubRunning → spawnHub). If
// the hub dies, the next call to a sesh-leaf-side operation that needs
// the central (e.g. a fresh ensureHubRunning, or autosync's reconnect)
// should re-trigger the spawn path. If sesh doesn't auto-recover today,
// this test will fail or hang on step 4. That outcome would be a real
// finding — see the header on this test in the orch variant for the
// remediation path (file a sesh issue, don't paper over).
func TestE2E_HubRestart_MidMission(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (E2E hub restart)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH (used to resolve hub PID)")
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
	repoPath := filepath.Join(project, ".sesh", "project.repo")

	// Baseline: alpha commits, beta sees within 10s. If THIS round fails,
	// the test failure is in basic propagation — not hub restart — and
	// the operator should look at swarm_tbd_integration_test.go first.
	const preFile = "pre-restart.txt"
	const prePayload = "before-the-storm\n"
	if err := os.WriteFile(filepath.Join(alphaWT, preFile), []byte(prePayload), 0o644); err != nil {
		t.Fatalf("write %s: %v", preFile, err)
	}
	if err := commitViaLibfossil(repoPath, alphaWT, preFile, "pre-restart commit"); err != nil {
		t.Fatalf("pre-restart commit: %v", err)
	}
	if !awaitPeerSees(betaWT, repoPath, preFile, prePayload, 10*time.Second) {
		t.Fatalf("baseline failed: beta did not see %s within 10s", preFile)
	}

	// Locate the user-wide hub PID via the hub.url file. hub.url contains
	// the leafnode URL the central is listening on; lsof -ti maps that
	// port to the PID. This is the only safe way to find the hub daemon
	// — it's detached via setsid, has no PID file, and is not a child of
	// the sesh up processes (they exec'd it with Release()).
	hubURLPath := filepath.Join(home, ".sesh", "hub.url")
	hubPID, hubPort, err := readHubPID(hubURLPath)
	if err != nil {
		t.Fatalf("resolve hub PID: %v", err)
	}
	t.Logf("hub PID = %d (port %s)", hubPID, hubPort)
	if hubPID <= 0 {
		t.Fatalf("hub PID resolution returned %d", hubPID)
	}

	// Kill ONLY the hub daemon. NOT the sesh up subprocesses.
	if err := syscall.Kill(hubPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill hub PID %d: %v", hubPID, err)
	}
	t.Logf("hub PID %d killed; waiting for leaf-side recovery", hubPID)

	// Brief settle window so the leaf processes notice the hub is gone.
	// The autoShutdownLoop on the hub side doesn't apply (we just killed
	// it); this is the LEAF-side recovery path. Empirically the leaf's
	// reconnect-or-respawn cycle should kick in within a few seconds.
	time.Sleep(3 * time.Second)

	// Post-restart commit: alpha commits, beta MUST see it within 15s.
	// The longer window vs. the baseline 10s reflects hub-recovery
	// latency: the leaf may need to re-spawn the central, the leaf-link
	// has to come up again, JetStream consumers have to re-subscribe.
	//
	// If this fails, the most likely real finding is that sesh doesn't
	// auto-respawn the user-wide hub when it dies mid-session. That's a
	// real gap and should be filed as a separate sesh issue, not papered
	// over with a manual respawn in this test.
	const postFile = "post-restart.txt"
	const postPayload = "after-the-storm\n"
	if err := os.WriteFile(filepath.Join(alphaWT, postFile), []byte(postPayload), 0o644); err != nil {
		t.Fatalf("write %s: %v", postFile, err)
	}
	if err := commitViaLibfossil(repoPath, alphaWT, postFile, "post-restart commit"); err != nil {
		t.Fatalf("post-restart commit: %v", err)
	}
	if !awaitPeerSees(betaWT, repoPath, postFile, postPayload, 15*time.Second) {
		t.Fatalf("HUB-RESTART RECOVERY GAP: beta did not see %s within 15s of post-restart commit. "+
			"Likely cause: sesh did not auto-respawn the user-wide hub. "+
			"This is a real finding — file a separate sesh issue for hub respawn-on-demand, "+
			"do NOT add a workaround here.", postFile)
	}

	// Materialize from alpha. Both files must be present in the trunk
	// state, regardless of which side of the restart they landed on.
	outDir := t.TempDir()
	mustRunMaterialize(t, bin, home, project, "alpha",
		"--scope=project", "--output="+outDir)
	for name, want := range map[string]string{
		preFile:  prePayload,
		postFile: postPayload,
	} {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Errorf("materialized output missing %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("materialized %s = %q; want %q", name, string(got), want)
		}
	}
}

// renderMockGoVerdict produces a GO verdict matching the fossil-verifier
// accessory's fixed template at
// wardrobe/accessories/fossil-verifier/accessory.md. The orch variant
// captures this string from a real claude subprocess's stdout; the mock
// variant renders it inline so the test can assert the verifier-side
// contract without driving claude.
//
// Kept in this file rather than swarm_test_helpers_test.go because the
// template is specific to the E2E suite — no other test consumes it
// today, and promoting it requires the second consumer to exist first
// (per the helper-promotion gate in #71).
func renderMockGoVerdict(sessionLabel, fileChecked string) string {
	return fmt.Sprintf(`Verifier report — sesh-session: %s, trunk HEAD: <mock>

Verdict: GO

Tests run:
- (mock: no project tests configured): skip

Lints/static checks:
- (mock: no project linters configured): clean

Acceptance criteria coverage (if known):
- [x] %s present at trunk HEAD with expected payload

Findings (only on NO-GO or INCONCLUSIVE):
- (none)

Recommendation to operator: merge
`, sessionLabel, fileChecked)
}

// readHubPID reads hub.url, extracts the port, and resolves the PID
// listening on that port via `lsof -ti :<port>`. Returns (pid, port, nil)
// on success.
//
// Why lsof and not /proc: macOS doesn't have /proc; lsof is the
// portable answer for "what PID owns this listening port" on the
// platforms sesh supports.
func readHubPID(hubURLPath string) (int, string, error) {
	data, err := os.ReadFile(hubURLPath)
	if err != nil {
		return 0, "", fmt.Errorf("read hub.url: %w", err)
	}
	urlStr := strings.TrimSpace(string(data))
	// hub.url is a leafnode URL like "nats-leaf://127.0.0.1:55432/".
	// Strip the scheme and trailing slash, then split on ':' to get the
	// port. We don't use net/url because the nats-leaf scheme isn't
	// always parseable by stdlib; the contents are well-known.
	parts := strings.SplitN(urlStr, "://", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("hub.url %q missing scheme separator", urlStr)
	}
	hostport := strings.TrimSuffix(parts[1], "/")
	colonIdx := strings.LastIndex(hostport, ":")
	if colonIdx < 0 {
		return 0, "", fmt.Errorf("hub.url hostport %q has no port", hostport)
	}
	port := hostport[colonIdx+1:]
	if port == "" {
		return 0, "", fmt.Errorf("hub.url hostport %q has empty port", hostport)
	}

	// lsof -ti :<port> prints one PID per line. -t = terse (PID only).
	// -i = filter by network address. We want the listener; lsof's -i
	// shows both listeners and any established connections on that port,
	// but the central hub is the only listener — peers connect, they
	// don't listen on this port. Sort uniq just in case.
	cmd := exec.Command("lsof", "-ti", ":"+port)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return 0, port, fmt.Errorf("lsof :%s: %v (stderr=%s)", port, err, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return 0, port, fmt.Errorf("lsof :%s returned no PIDs", port)
	}
	// Take the first PID. With sesh's architecture (only the central hub
	// listens on this port) there should be exactly one, but if there's
	// noise we pick the first deterministically.
	var pid int
	if _, err := fmt.Sscanf(lines[0], "%d", &pid); err != nil {
		return 0, port, fmt.Errorf("parse lsof PID %q: %v", lines[0], err)
	}
	return pid, port, nil
}
