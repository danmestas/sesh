//go:build orch_e2e

// Tier 1 end-to-end tests — orch-spawn variant.
//
// Build with: go test -tags=orch_e2e ./cli/ -run 'TestE2E' -v
//
// This file carries the same three test names as cli/e2e_test.go (the
// mock variant), but the "worker" and "verifier" phases are driven via
// real `orch-spawn` invocations rather than direct libfossil calls.
// Where the mock variant proves sesh's leaf-link plumbing through the
// libfossil API surface, this variant proves the same properties
// through the real subprocess chain: orch-spawn -> tmux -> claude
// (here, stub-claude).
//
// Default behavior — no env vars required:
//
//   The tests auto-resolve fixtures in cli/testdata/orch_e2e/. A
//   per-test PATH shim stages stub-claude.sh as `claude`, so
//   `orch-spawn claude --sesh-session <label>` lands the stub in the
//   fossil checkout. The stub reads a recipe (committed under the
//   same fixtures dir), executes it with shell, writes a done marker,
//   and exits. Each test then polls for the recipe's side effects
//   (file appearing in the trunk timeline, peer checkouts converging)
//   and asserts the load-bearing property.
//
//   `orch-spawn` must be on PATH, `tmux` must be on PATH, `fossil`
//   must be on PATH. If any of those are missing the test SKIPs with
//   a specific message naming what's absent. CI runners that don't
//   have orch installed will skip cleanly; orch-workstation hosts
//   run the full suite.
//
// Env-var overrides (for pointing at real claude or custom recipes):
//
//   SESH_E2E_ORCH_BIN   — path to orch-spawn (default: looks up
//                         "orch-spawn" on PATH).
//   SESH_E2E_CLAUDE     — path to a claude-compatible binary or stub.
//                         Default: cli/testdata/orch_e2e/stub-claude.sh
//                         (auto-resolved relative to this test file).
//   SESH_E2E_RECIPE_DIR — directory containing the *.recipe fixtures.
//                         Default: cli/testdata/orch_e2e/.
//
// Recipe handoff:
//
//   tmux's existing server strips most env vars from new panes
//   (update-environment is limited to DISPLAY / SSH_AUTH_SOCK / etc.
//   by default), so we cannot rely on $SESH_E2E_RECIPE propagating
//   from `go test` through orch-spawn into the tmux pane. The test
//   writes the chosen recipe path to .stub-claude-recipe inside the
//   fossil checkout dir before invoking orch-spawn. The stub reads
//   that file first when $SESH_E2E_RECIPE is unset, which is the
//   common case for these tests.
//
// What still needs to be true for the tests to RUN (vs. SKIP) even
// after this scaffold lands:
//
//   1. orch-spawn on PATH (or SESH_E2E_ORCH_BIN set).
//   2. tmux on PATH (orch-spawn requires it).
//   3. fossil on PATH (recipes call `fossil add` / `fossil commit`).
//
// The orch-agent-shim NATS-bus registration is suppressed via
// --no-shim; the fleet doctrine system-prompt file is suppressed via
// --no-fleet. Neither is needed to drive the worker recipe.

package cli_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// orchEnv holds the resolved binary + fixture paths for one orch-e2e
// test invocation. requireOrchEnv populates it from env vars (when set)
// or auto-resolved defaults (when not), and a t.Skip ends the test if
// any prerequisite is genuinely missing.
type orchEnv struct {
	orchBin   string // path to orch-spawn
	claudeBin string // path to claude-compatible binary (stub or real)
	recipeDir string // directory holding *.recipe fixtures
	seshBin   string // path to the test-built sesh binary (for ORCH_SESH_BIN)
}

// requireOrchEnv resolves the orch-spawn driver, the stub-claude
// (or real claude) path, and the recipe directory, then verifies tmux
// and fossil are on PATH. Skips the test on the first missing piece
// with a specific message.
func requireOrchEnv(t *testing.T, seshBin string) orchEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (E2E orch variant)")
	}

	orchBin := os.Getenv("SESH_E2E_ORCH_BIN")
	if orchBin == "" {
		var err error
		orchBin, err = exec.LookPath("orch-spawn")
		if err != nil {
			t.Skip("orch-spawn not on PATH and SESH_E2E_ORCH_BIN unset — install orch from ~/projects/orch or set SESH_E2E_ORCH_BIN")
		}
	}
	if _, err := os.Stat(orchBin); err != nil {
		t.Skipf("orch-spawn at %s: %v", orchBin, err)
	}

	claudeBin := os.Getenv("SESH_E2E_CLAUDE")
	if claudeBin == "" {
		claudeBin = fixturesPath(t, "stub-claude.sh")
	}
	if _, err := os.Stat(claudeBin); err != nil {
		t.Skipf("SESH_E2E_CLAUDE / auto-resolved stub at %s: %v", claudeBin, err)
	}

	recipeDir := os.Getenv("SESH_E2E_RECIPE_DIR")
	if recipeDir == "" {
		recipeDir = fixturesDir(t)
	}
	if fi, err := os.Stat(recipeDir); err != nil || !fi.IsDir() {
		t.Skipf("SESH_E2E_RECIPE_DIR / auto-resolved %s: %v", recipeDir, err)
	}

	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH (orch-spawn requires tmux to place worker panes)")
	}
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH (worker recipes call fossil add / fossil commit)")
	}

	return orchEnv{
		orchBin:   orchBin,
		claudeBin: claudeBin,
		recipeDir: recipeDir,
		seshBin:   seshBin,
	}
}

// fixturesDir returns the absolute path to cli/testdata/orch_e2e, the
// directory that ships with this test file. We use runtime.Caller to
// locate this file's path because `go test` invokes the test from
// the package's directory, but in some build/test harnesses the cwd
// is configurable and we want to be robust against both.
func fixturesDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot resolve test file path")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "orch_e2e")
}

// fixturesPath returns fixturesDir/name.
func fixturesPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(fixturesDir(t), name)
}

// makeClaudeShim creates a per-test shim dir containing a `claude`
// symlink pointing at env.claudeBin. Returns the shim dir. Caller is
// responsible for prepending it to PATH when invoking orch-spawn.
//
// Why a symlink and not an exec wrapper: orch-spawn's WRAP runs
// `claude --dangerously-skip-permissions ...` literally. A symlink is
// the cheapest way to make "claude" resolve to our stub script
// without rewriting orch-spawn's WRAP construction.
func makeClaudeShim(t *testing.T, env orchEnv) string {
	t.Helper()
	shim := t.TempDir()
	link := filepath.Join(shim, "claude")
	if err := os.Symlink(env.claudeBin, link); err != nil {
		t.Fatalf("symlink claude -> %s: %v", env.claudeBin, err)
	}
	return shim
}

// stageRecipe writes the recipe path to .stub-claude-recipe in the
// fossil checkout so stub-claude.sh can find it without relying on
// env-var propagation through tmux. The recipe filename is resolved
// against env.recipeDir.
//
// We write the absolute path of the recipe (not the contents) so the
// stub's error messages name the source-of-truth fixture file, not a
// per-test copy.
func stageRecipe(t *testing.T, env orchEnv, checkoutDir, recipeFile string) {
	t.Helper()
	src := filepath.Join(env.recipeDir, recipeFile)
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("recipe %s: %v", src, err)
	}
	dst := filepath.Join(checkoutDir, ".stub-claude-recipe")
	if err := os.WriteFile(dst, []byte(src+"\n"), 0o644); err != nil {
		t.Fatalf("stage recipe at %s: %v", dst, err)
	}
}

// orchSpawnHeadless invokes orch-spawn in headless mode with the given
// session label, PATH-shimmed to point at the stub claude. Returns the
// pane id (orch-spawn's stdout) and the headless tmux session name we
// chose. The caller is responsible for tearing down the pane via
// killOrchPane and the session via killTmuxSession.
//
// We pass --no-fleet (no fleet-doctrine prompt file required), --no-shim
// (no NATS-bus shim required), and --headless (no host tmux session
// required). The --sesh-session resolution shells out to `sesh
// worker-cwd`; we point at the test-built sesh via ORCH_SESH_BIN.
func orchSpawnHeadless(t *testing.T, env orchEnv, shimDir, project, label, tmuxSession string) (paneID string) {
	t.Helper()
	cmd := exec.Command(env.orchBin, "claude",
		"--sesh-session", label,
		"--headless",
		"--no-shim",
		"--no-fleet",
	)
	cmd.Dir = project
	// PATH=shim:$PATH so `claude` resolves to stub-claude.sh inside
	// the tmux pane. ORCH_HEADLESS_SESSION names the headless tmux
	// session per-test (default `orch-headless` would collide across
	// tests and with the operator's real orch session). ORCH_SESH_BIN
	// points at the test-built sesh binary so worker-cwd resolves
	// against the project's .sesh/sessions state.
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ORCH_HEADLESS_SESSION="+tmuxSession,
		"ORCH_SESH_BIN="+env.seshBin,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("orch-spawn claude --sesh-session %s: %v\nstdout=%s\nstderr=%s",
			label, err, stdout.String(), stderr.String())
	}
	pane := strings.TrimSpace(stdout.String())
	if pane == "" {
		t.Fatalf("orch-spawn returned empty pane id; stderr=%s", stderr.String())
	}
	t.Logf("orch-spawn placed %s for session=%s (stderr=%s)", pane, label, stderr.String())
	return pane
}

// waitForStubDone polls for the stub's done marker inside the
// checkout dir, returning the marker's contents on success or "" on
// timeout. The contents start with "ok " on success or "fail " on
// recipe failure.
func waitForStubDone(checkoutDir string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	marker := filepath.Join(checkoutDir, ".stub-claude-done")
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			return string(data)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return ""
}

// killOrchPane runs `tmux kill-pane -t <pane>`. Best-effort; logs but
// does not fail the test on error (the test may have already failed
// for a real reason and we don't want a noisy teardown to mask it).
func killOrchPane(t *testing.T, pane string) {
	t.Helper()
	cmd := exec.Command("tmux", "kill-pane", "-t", pane)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("tmux kill-pane %s: %v (out=%s)", pane, err, out)
	}
}

// killTmuxSession runs `tmux kill-session -t <session>`. Best-effort.
func killTmuxSession(t *testing.T, session string) {
	t.Helper()
	cmd := exec.Command("tmux", "kill-session", "-t", session)
	_ = cmd.Run() // session may already be gone
}

// uniqueTmuxSession returns a per-test headless tmux session name. The
// name is derived from t.Name() (replacing characters tmux dislikes)
// plus the process PID. This avoids collisions between parallel test
// runs and the operator's real `orch-headless` session.
func uniqueTmuxSession(t *testing.T) string {
	t.Helper()
	safe := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("sesh-e2e-%s-%d", safe, os.Getpid())
}

// TestE2E_ThreeWorkers_StarFanout (orch variant) — three sessions
// (alpha/beta/gamma) sharing one project repo via --scope=project,
// each driving a real `orch-spawn claude --sesh-session <label>` that
// runs the matching fanout-*.recipe. After each commit, the test
// polls the other two peer checkouts for the file. Final materialize
// proves all three files landed at trunk HEAD.
//
// Property under test: the leaf-link plumbing carries cross-leaf
// commit announces through the real claude-launch surface, not just
// through libfossil-direct injection.
func TestE2E_ThreeWorkers_StarFanout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	env := requireOrchEnv(t, bin)
	shim := makeClaudeShim(t, env)

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

	tmuxSession := uniqueTmuxSession(t)
	defer killTmuxSession(t, tmuxSession)

	type round struct {
		commitFrom string
		checkout   string
		recipe     string
		fileName   string
		payload    string
		peers      []struct{ name, checkout string }
	}
	rounds := []round{
		{
			commitFrom: "alpha",
			checkout:   alphaWT,
			recipe:     "fanout-alpha.recipe",
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
			recipe:     "fanout-beta.recipe",
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
			recipe:     "fanout-gamma.recipe",
			fileName:   "fanout-from-gamma.txt",
			payload:    "gamma-was-here\n",
			peers: []struct{ name, checkout string }{
				{"alpha", alphaWT},
				{"beta", betaWT},
			},
		},
	}

	for _, r := range rounds {
		stageRecipe(t, env, r.checkout, r.recipe)
		pane := orchSpawnHeadless(t, env, shim, project, r.commitFrom, tmuxSession)
		defer killOrchPane(t, pane)

		marker := waitForStubDone(r.checkout, 30*time.Second)
		if marker == "" {
			t.Fatalf("%s stub never wrote .stub-claude-done in %s", r.commitFrom, r.checkout)
		}
		if !strings.HasPrefix(marker, "ok ") {
			t.Fatalf("%s stub failed: %s", r.commitFrom, marker)
		}

		// Recipe wrote+committed via the `fossil` CLI. Confirm the file
		// landed in the committer's own checkout first (sanity), then
		// poll the other two peers.
		if got, err := os.ReadFile(filepath.Join(r.checkout, r.fileName)); err != nil {
			t.Fatalf("%s own checkout missing %s: %v", r.commitFrom, r.fileName, err)
		} else if string(got) != r.payload {
			t.Fatalf("%s own checkout %s = %q; want %q", r.commitFrom, r.fileName, string(got), r.payload)
		}
		for _, p := range r.peers {
			if !awaitPeerSees(p.checkout, repoPath, r.fileName, r.payload, 15*time.Second) {
				t.Fatalf("%s's checkout did not see %s's commit (%s) within 15s",
					p.name, r.commitFrom, r.fileName)
			}
		}
	}

	// Final trunk-state assertion: materialize from alpha, all three
	// files land. A regression that broke a single leaf-link direction
	// mid-test would still pass the per-round assertions if a recovery
	// loop papered over the gap; the final materialize closes that hole.
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

// TestE2E_FullMissionLoop (orch variant) — the worker phase is a real
// `orch-spawn claude --sesh-session alpha` invocation running
// worker-mission.recipe; the verifier phase is a separate orch-spawn
// running verifier-mission.recipe. The test asserts:
//
//   - the worker recipe lands mission.txt at trunk HEAD,
//   - the verifier's verdict file is present and contains
//     "Verdict: GO" + "Recommendation to operator: merge"
//     (matching the fossil-verifier accessory template),
//   - sesh materialize --git-add stages mission.txt in the project's
//     git index.
//
// This is the first test that exercises EVERY swarm slice end-to-end
// through the real orch subprocess chain.
func TestE2E_FullMissionLoop(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	env := requireOrchEnv(t, bin)
	shim := makeClaudeShim(t, env)

	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	alpha, alphaStderr := startSeshArgs(t, bin, home, project, "alpha", "--scope=project")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", alphaStderr.String())
	}

	// Slice 2 + Slice 5: worktree and worker-cwd must agree. orch-spawn's
	// --sesh-session resolution calls `sesh worker-cwd`, so these two
	// values agreeing is the contract that lets orch-spawn land the
	// stub in the right cwd.
	checkout := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")
	cwd := mustRunWorkerCwd(t, bin, home, project, "alpha", "--scope=project")
	if eq, err := samePath(checkout, cwd); err != nil {
		t.Fatalf("samePath(checkout, cwd): %v", err)
	} else if !eq {
		t.Errorf("worker-cwd (%q) != worktree (%q)", cwd, checkout)
	}

	tmuxSession := uniqueTmuxSession(t)
	defer killTmuxSession(t, tmuxSession)

	// Worker phase: real orch-spawn drives stub-claude through
	// worker-mission.recipe. Recipe writes mission.txt, fossil-adds it,
	// fossil-commits with a fixed message.
	const missionFile = "mission.txt"
	stageRecipe(t, env, checkout, "worker-mission.recipe")
	workerPane := orchSpawnHeadless(t, env, shim, project, "alpha", tmuxSession)
	defer killOrchPane(t, workerPane)

	workerMarker := waitForStubDone(checkout, 30*time.Second)
	if workerMarker == "" {
		t.Fatalf("worker stub never wrote .stub-claude-done in %s", checkout)
	}
	if !strings.HasPrefix(workerMarker, "ok ") {
		t.Fatalf("worker stub failed: %s", workerMarker)
	}

	// The recipe just committed via the `fossil` CLI inside the
	// checkout. The same checkout also stores the post-commit file in
	// its working dir, so we can read it back directly.
	missionPath := filepath.Join(checkout, missionFile)
	gotPayload, err := os.ReadFile(missionPath)
	if err != nil {
		t.Fatalf("worker phase: %s never landed: %v", missionFile, err)
	}
	const wantPayload = "mission accomplished\n"
	if string(gotPayload) != wantPayload {
		t.Fatalf("worker phase: %s = %q; want %q", missionFile, gotPayload, wantPayload)
	}

	// Verifier phase: replace the recipe + done marker, kill the
	// worker pane, spawn a fresh stub-claude that runs
	// verifier-mission.recipe. We have to clear the done marker first
	// — the worker's leftover marker would short-circuit our poll.
	if err := os.Remove(filepath.Join(checkout, ".stub-claude-done")); err != nil {
		t.Fatalf("clear worker done marker: %v", err)
	}
	stageRecipe(t, env, checkout, "verifier-mission.recipe")
	verifierPane := orchSpawnHeadless(t, env, shim, project, "alpha", tmuxSession)
	defer killOrchPane(t, verifierPane)

	verifierMarker := waitForStubDone(checkout, 30*time.Second)
	if verifierMarker == "" {
		t.Fatalf("verifier stub never wrote .stub-claude-done in %s", checkout)
	}
	if !strings.HasPrefix(verifierMarker, "ok ") {
		t.Fatalf("verifier stub failed: %s", verifierMarker)
	}

	verdict, err := os.ReadFile(filepath.Join(checkout, "verifier-verdict.txt"))
	if err != nil {
		t.Fatalf("verifier verdict file missing: %v", err)
	}
	verdictStr := string(verdict)
	t.Logf("verifier verdict:\n%s", verdictStr)
	if !strings.Contains(verdictStr, "Verdict: GO") {
		t.Errorf("verifier verdict missing 'Verdict: GO':\n%s", verdictStr)
	}
	if !strings.Contains(verdictStr, "Recommendation to operator: merge") {
		t.Errorf("verifier verdict missing 'Recommendation to operator: merge':\n%s", verdictStr)
	}

	// Operator phase: materialize with --git-add into the project's git
	// worktree, then assert via `git diff --cached` that mission.txt is
	// staged. The recipe also wrote verifier-verdict.txt and
	// timeline-snapshot.txt into the checkout — neither is committed
	// to the trunk (the verifier recipe doesn't `fossil add` them), so
	// they should NOT show up in the materialize output.
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

	// Round-trip sanity: `sesh down alpha` cleanly tears down.
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

// TestE2E_HubRestart_MidMission (orch variant) — alpha commits
// pre-restart.txt via orch-spawn, then the test kills the central
// user-wide hub mid-mission, then alpha commits post-restart.txt via
// orch-spawn. Both files must land on beta's checkout, proving the
// leaf-side reconnect-or-respawn cycle survives a hub kill under the
// real subprocess chain (not just libfossil-direct injection).
//
// Note on hub recovery: sesh's `sesh up` is what spawns the user-wide
// hub on first-touch. If sesh does not auto-respawn the hub when it
// dies, this test will hang at the post-restart commit's propagation
// step. The remediation per CLAUDE.md is to file a separate sesh
// issue, NOT to add a manual respawn in this test.
func TestE2E_HubRestart_MidMission(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH (used to resolve hub PID)")
	}

	bin := buildSesh(t)
	env := requireOrchEnv(t, bin)
	shim := makeClaudeShim(t, env)

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

	tmuxSession := uniqueTmuxSession(t)
	defer killTmuxSession(t, tmuxSession)

	// Baseline: alpha commits via orch-spawn, beta sees within 15s.
	const preFile = "pre-restart.txt"
	const prePayload = "before-the-storm\n"
	stageRecipe(t, env, alphaWT, "hubrestart-pre.recipe")
	prePane := orchSpawnHeadless(t, env, shim, project, "alpha", tmuxSession)
	defer killOrchPane(t, prePane)
	if mk := waitForStubDone(alphaWT, 30*time.Second); !strings.HasPrefix(mk, "ok ") {
		t.Fatalf("pre-restart stub failed or timed out: %q", mk)
	}
	if !awaitPeerSees(betaWT, repoPath, preFile, prePayload, 15*time.Second) {
		t.Fatalf("baseline failed: beta did not see %s within 15s", preFile)
	}

	// Locate hub PID via hub.url + lsof. Same mechanism as the mock
	// variant — see readHubPID in e2e_test.go for the rationale.
	hubURLPath := filepath.Join(home, ".sesh", "hub.url")
	hubPID, hubPort, err := readHubPID(hubURLPath)
	if err != nil {
		t.Fatalf("resolve hub PID: %v", err)
	}
	t.Logf("hub PID = %d (port %s)", hubPID, hubPort)
	if hubPID <= 0 {
		t.Fatalf("hub PID resolution returned %d", hubPID)
	}

	if err := syscall.Kill(hubPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill hub PID %d: %v", hubPID, err)
	}
	t.Logf("hub PID %d killed; waiting for leaf-side recovery", hubPID)
	time.Sleep(3 * time.Second)

	// Post-restart commit via orch-spawn. The done marker from the pre
	// recipe is still in the checkout; clear it so the wait pole below
	// sees the post recipe's marker.
	if err := os.Remove(filepath.Join(alphaWT, ".stub-claude-done")); err != nil {
		t.Fatalf("clear pre-restart done marker: %v", err)
	}
	const postFile = "post-restart.txt"
	const postPayload = "after-the-storm\n"
	stageRecipe(t, env, alphaWT, "hubrestart-post.recipe")
	postPane := orchSpawnHeadless(t, env, shim, project, "alpha", tmuxSession)
	defer killOrchPane(t, postPane)
	if mk := waitForStubDone(alphaWT, 30*time.Second); !strings.HasPrefix(mk, "ok ") {
		t.Fatalf("post-restart stub failed or timed out: %q", mk)
	}
	if !awaitPeerSees(betaWT, repoPath, postFile, postPayload, 20*time.Second) {
		t.Fatalf("HUB-RESTART RECOVERY GAP: beta did not see %s within 20s of post-restart commit. "+
			"Likely cause: sesh did not auto-respawn the user-wide hub. "+
			"This is a real finding — file a separate sesh issue for hub respawn-on-demand, "+
			"do NOT add a workaround here.", postFile)
	}

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

// readHubPID — duplicate of the same helper in the mock variant
// (e2e_test.go). The two variants are mutually exclusive at build time
// (build tags `!orch_e2e` vs `orch_e2e`), so each variant carries the
// helpers it needs. Promoting to a shared no-tag helpers file is the
// natural next move if a third consumer arrives; today there are only
// these two, and the Ousterhout-helper-promotion gate prefers
// duplication until that third consumer materializes.
func readHubPID(hubURLPath string) (int, string, error) {
	data, err := os.ReadFile(hubURLPath)
	if err != nil {
		return 0, "", fmt.Errorf("read hub.url: %w", err)
	}
	urlStr := strings.TrimSpace(string(data))
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
	var pid int
	if _, err := fmt.Sscanf(lines[0], "%d", &pid); err != nil {
		return 0, port, fmt.Errorf("parse lsof PID %q: %v", lines[0], err)
	}
	return pid, port, nil
}
