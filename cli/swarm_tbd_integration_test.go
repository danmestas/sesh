package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSwarmTBD_TwoWorkers_ConvergeOnSharedTrunk is the load-bearing
// integration test for the fossil-as-trunk swarm workflow. It exercises
// Slices 1, 2, 3, and 5 together, end-to-end, with two real cross-process
// `sesh up` subprocesses cohabiting one project repo via `--scope=project`:
//
//   - Slice 1: EdgeSync multi-responder .sync race fix — both sessions stay
//     up against the shared project.repo while autosync wires the project-
//     code subject in the background.
//   - Slice 2: `sesh worktree <label>` — each session provisions its own
//     fossil checkout at `.sesh/checkouts/<label>/` against the SAME
//     backing project.repo.
//   - Slice 3: `sesh materialize <label>` — after both workers have
//     committed, materialize bridges the fossil trunk into a tmp git
//     worktree and the materialized state contains BOTH workers'
//     contributions.
//   - Slice 5: `sesh worker-cwd <label>` — the checkout-path lookup that
//     orch-spawn would use to anchor a worker process.
//
// Bidirectional propagation is the load-bearing assertion: alpha commits
// → beta sees within bounded time, THEN beta commits → alpha sees within
// bounded time. The single-direction case is already covered by
// TestSeshWorktree_PropagatesEdit_ViaAutosync; this test is the next step
// up, asserting symmetry.
//
// This is the test that would fail if Slices 1, 2, 3, or 5 regressed in
// a way the per-slice tests don't catch — e.g., a regression that
// breaks reverse propagation but leaves forward propagation working, or
// one that breaks materialize when the trunk contains commits from
// multiple distinct authors.
func TestSwarmTBD_TwoWorkers_ConvergeOnSharedTrunk(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (swarm two-worker convergence)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	// Bring both sesh up subprocesses live under --scope=project so they
	// share project.repo. They are real subprocesses, not in-process hubs:
	// the cross-process boundary is the whole point of this test relative
	// to TestCrossSessionAutosync (which is in-process and leaf-linked).
	alpha, alphaStderr := startSeshArgs(t, bin, home, project, "alpha", "--scope=project")
	defer killAndWait(t, alpha, alphaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", alphaStderr.String())
	}

	beta, betaStderr := startSeshArgs(t, bin, home, project, "beta", "--scope=project")
	defer killAndWait(t, beta, betaStderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "beta.json"), 15*time.Second)

	// Tier-1 fingerprint sanity: snapshot the per-session JetStream
	// stores and the sessions/ dir so we can confirm at end-of-test that
	// neither session's startup nor the cross-checkout commits clobbered
	// them outside the normal write paths. We don't compare equality
	// (the hubs append to these legitimately during the test); we just
	// assert they remain present and walkable. The destructive paranoia
	// is for future regressions where a checkout RemoveAll escapes its
	// slot.
	tier1Roots := []string{
		filepath.Join(project, ".sesh", "sessions"),
		filepath.Join(project, ".sesh", "sessions", "alpha.messaging"),
		filepath.Join(project, ".sesh", "sessions", "beta.messaging"),
	}
	for _, root := range tier1Roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("tier-1 root missing pre-test: %s: %v", root, err)
		}
	}

	// Provision worktrees via the CLI subcommand — exercises Slice 2 over
	// the process boundary.
	alphaWT := mustRunWorktree(t, bin, home, project, "alpha", "--scope=project")
	betaWT := mustRunWorktree(t, bin, home, project, "beta", "--scope=project")

	// Also exercise Slice 5: worker-cwd should resolve to the same path
	// the worktree subcommand produced. orch-spawn relies on this
	// indirection.
	alphaCwd := mustRunWorkerCwd(t, bin, home, project, "alpha", "--scope=project")
	betaCwd := mustRunWorkerCwd(t, bin, home, project, "beta", "--scope=project")
	if eq, err := samePath(alphaWT, alphaCwd); err != nil {
		t.Fatalf("samePath(alphaWT,alphaCwd): %v", err)
	} else if !eq {
		t.Errorf("worker-cwd alpha (%q) != worktree alpha (%q)", alphaCwd, alphaWT)
	}
	if eq, err := samePath(betaWT, betaCwd); err != nil {
		t.Fatalf("samePath(betaWT,betaCwd): %v", err)
	} else if !eq {
		t.Errorf("worker-cwd beta (%q) != worktree beta (%q)", betaCwd, betaWT)
	}

	repoPath := filepath.Join(project, ".sesh", "project.repo")

	// Direction 1: alpha worker commits in its checkout, beta worker sees
	// the file in its own checkout (after an update against the shared
	// repo) within 10s.
	const alphaFile = "from-alpha-worker.txt"
	const alphaPayload = "alpha-was-here\n"
	if err := os.WriteFile(filepath.Join(alphaWT, alphaFile), []byte(alphaPayload), 0o644); err != nil {
		t.Fatalf("write %s into alpha checkout: %v", alphaFile, err)
	}
	if err := commitViaLibfossil(repoPath, alphaWT, alphaFile, "swarm alpha commit"); err != nil {
		t.Fatalf("alpha commit via libfossil: %v", err)
	}

	if !awaitPeerSees(betaWT, repoPath, alphaFile, alphaPayload, 10*time.Second) {
		t.Fatalf("beta's checkout did not see alpha's commit (%s) within 10s", alphaFile)
	}

	// Direction 2: beta worker commits in ITS checkout, alpha worker sees
	// the file in its own checkout within 10s. This is the symmetry
	// assertion that single-direction tests don't provide.
	const betaFile = "from-beta-worker.txt"
	const betaPayload = "beta-was-here\n"
	if err := os.WriteFile(filepath.Join(betaWT, betaFile), []byte(betaPayload), 0o644); err != nil {
		t.Fatalf("write %s into beta checkout: %v", betaFile, err)
	}
	if err := commitViaLibfossil(repoPath, betaWT, betaFile, "swarm beta commit"); err != nil {
		t.Fatalf("beta commit via libfossil: %v", err)
	}

	if !awaitPeerSees(alphaWT, repoPath, betaFile, betaPayload, 10*time.Second) {
		t.Fatalf("alpha's checkout did not see beta's commit (%s) within 10s", betaFile)
	}

	// Round-trip materialization: after both workers committed, materialize
	// alpha's trunk into a tmp git worktree and assert BOTH workers'
	// contributions are present. This is the Slice 3 hook in the swarm
	// pipeline: trunk → operator-visible git diff.
	outDir := t.TempDir()
	mustRunMaterialize(t, bin, home, project, "alpha",
		"--scope=project", "--output="+outDir)

	for name, want := range map[string]string{
		alphaFile: alphaPayload,
		betaFile:  betaPayload,
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

	// Tier-1 paths still present at end-of-test. We don't byte-compare
	// (hubs legitimately wrote to messaging/) — just survival.
	for _, root := range tier1Roots {
		if _, err := os.Stat(root); err != nil {
			t.Errorf("tier-1 root vanished during test: %s: %v", root, err)
		}
	}
}
