//go:build orch_e2e

// Tier 1 end-to-end tests — orch-spawn variant.
//
// Build with: go test -tags=orch_e2e ./cli/ -run 'TestE2E' -v
//
// This file carries the same three test names as cli/e2e_test.go (the
// mock variant), but the "worker" and "verifier" phases are driven via
// real `orch-spawn` invocations rather than direct libfossil calls.
// Status: SCAFFOLD ONLY. Each test currently t.Skip's with a description
// of what would need to land to make it runnable. The orch variant is
// acceptable as a scaffold because:
//
//  - orch is under active development (the --sesh-session flag in
//    orch#139 is recent and the recipe-driver story is not yet stable).
//  - Running orch-spawn from a Go test today requires either (a) an
//    interactive claude binary in a tmux pane that the test can drive
//    via orch-tell, or (b) a stub claude binary that runs a recipe and
//    exits. Neither is available in a form a non-interactive `go test`
//    run can rely on.
//  - The mock variant in cli/e2e_test.go covers the same load-bearing
//    properties through the sesh-side surface (worktree, materialize,
//    worker-cwd, autosync). The gap the orch variant would close is
//    proving the same properties through the real subprocess chain.
//
// To make this file runnable, the following would need to land:
//
//   1. A non-interactive driver for orch-spawn — either a documented
//      recipe-file format that orch-spawn can run head-less, or a way
//      to point orch-spawn at a stub claude binary that just runs
//      shell commands and exits. The orch-driver skill at
//      ~/.claude/skills/orch-driver/ documents the interactive driver
//      surface; the head-less surface doesn't exist yet.
//
//   2. A NATS bus available on localhost — orch-tell publishes on
//      NATS; the test would need to either start one or assume one is
//      running. The mock variant avoids this by not invoking orch-tell
//      at all.
//
//   3. tmux available on PATH — orch-spawn places workers in tmux
//      panes. CI runners (GitHub Actions linux-x86) have tmux
//      available; macOS runners typically do not without homebrew.
//
//   4. claude binary available on PATH OR a documented stub. CI
//      environments do not have claude installed; production-orch
//      workstations do.
//
// Env vars consumed by this file when not skipping:
//
//   SESH_E2E_ORCH_BIN — path to orch-spawn (default: looks up "orch-spawn"
//                       on PATH).
//   SESH_E2E_CLAUDE   — path to a claude-compatible binary or stub. If
//                       unset, every test in this file SKIPs with a
//                       message naming this variable.
//   SESH_E2E_RECIPE_DIR — directory containing worker-recipe.sh and
//                         verifier-recipe.sh. If unset, tests SKIP.
//
// Until those land this file's job is documentation + structural
// scaffolding: same test names as the mock variant, same workflow
// shape, t.Skip with a specific message so when the missing pieces
// arrive the tests can be filled in without re-deriving the
// architecture.

package cli_test

import (
	"os"
	"os/exec"
	"testing"
)

// TestE2E_ThreeWorkers_StarFanout (orch variant) — see mock variant in
// cli/e2e_test.go for the property under test and assertion shape. The
// orch-variant difference: each worker is a real `orch-spawn claude
// --sesh-session <label> --accessory fossil-worker` subprocess running
// a recipe that writes a file and calls `fossil commit`.
func TestE2E_ThreeWorkers_StarFanout(t *testing.T) {
	requireOrchEnv(t)
	t.Skip("orch-spawn driving not yet wired; see header for status and required env vars")
}

// TestE2E_FullMissionLoop (orch variant) — see mock variant in
// cli/e2e_test.go. The orch-variant difference: the worker phase is a
// real `orch-spawn claude --sesh-session alpha --accessory
// fossil-worker` invocation running a recipe that does
// `fossil add mission.txt && fossil commit -m "implement"`; the
// verifier phase is `orch-spawn claude --sesh-session alpha
// --accessory fossil-verifier` running a recipe that runs `fossil
// timeline` and emits the GO verdict to stdout. The test captures the
// stdout and asserts the verdict matches the fossil-verifier accessory
// template.
func TestE2E_FullMissionLoop(t *testing.T) {
	requireOrchEnv(t)
	t.Skip("orch-spawn driving not yet wired; see header for status and required env vars")
}

// TestE2E_HubRestart_MidMission (orch variant) — see mock variant in
// cli/e2e_test.go. Workers are real orch-spawned subprocesses; the
// hub-kill step is identical to the mock variant. The hard part of
// this test under orch is that the worker recipes need to survive the
// hub restart — if a worker is mid-`fossil commit` when the hub dies,
// the commit may succeed locally but never propagate. The mock variant
// dodges this by sequencing commits explicitly; the orch variant would
// need to either accept that risk or add retry logic to the recipes.
func TestE2E_HubRestart_MidMission(t *testing.T) {
	requireOrchEnv(t)
	t.Skip("orch-spawn driving not yet wired; see header for status and required env vars")
}

// requireOrchEnv skips the test if any of the env vars or binaries
// required to run the orch variant are unavailable. Even when this
// helper passes, the tests above still t.Skip because the recipe-driver
// piece is not yet in place; the env check is here so future
// implementations don't have to re-derive the gate.
func requireOrchEnv(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (E2E orch variant)")
	}

	orchBin := os.Getenv("SESH_E2E_ORCH_BIN")
	if orchBin == "" {
		var err error
		orchBin, err = exec.LookPath("orch-spawn")
		if err != nil {
			t.Skip("orch-spawn not on PATH and SESH_E2E_ORCH_BIN unset")
		}
	}
	if _, err := os.Stat(orchBin); err != nil {
		t.Skipf("orch-spawn at %s: %v", orchBin, err)
	}

	if os.Getenv("SESH_E2E_CLAUDE") == "" {
		t.Skip("SESH_E2E_CLAUDE unset (need claude binary or stub for orch-spawn to launch)")
	}
	if os.Getenv("SESH_E2E_RECIPE_DIR") == "" {
		t.Skip("SESH_E2E_RECIPE_DIR unset (need worker-recipe.sh + verifier-recipe.sh)")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH (orch-spawn requires tmux)")
	}
}
