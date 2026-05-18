package cli_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil"
)

// TestSwarmTBD_ConflictResolution_OperatorHandoff is the stretch test for
// the conflict-resolution path that #71 carves out from #70's Slice 7.
// Two real `sesh up` subprocesses cohabit one project repo via
// `--scope=project`, each provisioning its own libfossil checkout. Each
// worker commits divergent edits to shared.txt on its own branch. The
// test then attempts repo.Merge to consolidate the two leaves into trunk.
// The two sub-tests have DIFFERENT scopes — read carefully:
//
//   - Sub-test "trivial_disjoint_lines" is close to a production
//     assertion. Alpha and beta edit different, non-overlapping line
//     ranges of shared.txt on separate branches. libfossil.Repo.Merge
//     auto-resolves via three-way text-merge. Materializing the merged
//     tip surfaces BOTH workers' contributions via the same
//     `sesh materialize` path a real consumer would use. The round-trip
//     through the Go API, the on-disk repo, and the materialize CLI is
//     exactly what production code paths exercise.
//
//   - Sub-test "judgment_same_line_branch_merge_api" is a libfossil
//     API-CONTRACT assertion, NOT a production worker-protocol
//     assertion. Alpha and beta edit the SAME line range on separate
//     branches. libfossil.Repo.Merge returns a *MergeConflictError
//     naming shared.txt. The test asserts that error shape on the
//     branch-merge surface specifically. See the long comment below
//     about why this is NOT the production conflict path workers will
//     hit, and what would be needed to test that production path.
//
// Why the judgment sub-test uses Repo.Merge and not the worker's
// trunk-commit-with-autosync-pull path:
//
// The fossil-worker accessory's production conflict path is:
//
//  1. Worker commits a change to trunk via `co.Checkin`.
//  2. Autosync pulls a peer's conflicting trunk commit.
//  3. libfossil calls `Checkout.Update` to merge peer's change into
//     the local checkout.
//  4. Working-tree files now contain conflict markers
//     (`<<<<<<<` / `>>>>>>>`), per libfossil's internal Update at
//     internal/checkout/update.go:124-128 (which bumps `conflicts++`,
//     writes the file with markers, and emits a diagnostic) and the
//     docstring at internal/checkout/update.go:177-178 ("performing
//     3-way merge where needed to preserve local modifications").
//  5. The next `co.Checkin` would commit those marker-bearing files
//     unless the worker surfaces the conflict and stops.
//
// So the internal capability — 3-way merge with conflict markers in the
// working tree — EXISTS in libfossil today. The gap is the public Go
// API surface:
//
//   - libfossil/checkout.go:125-135 wraps the internal Update and
//     returns plain `error`. The internal function's `(filesWritten,
//     filesRemoved, conflicts int, err error)` quadruple collapses to a
//     single `error`, discarding the conflict count. A Go caller
//     cannot distinguish "succeeded clean" from "succeeded but wrote
//     conflict markers into the working tree" from "failed to apply at
//     all" — all three return either `nil` or a generic wrapped error
//     with no typed surface.
//
//   - libfossil's `Repo.Merge` (branch-merge), in contrast, DOES expose
//     *MergeConflictError as a typed error with conflicted file paths.
//
// Therefore the only conflict surface this test CAN assert from Go is
// the branch-merge API contract on Repo.Merge — not the production
// trunk-commit-with-autosync-pull conflict path workers will actually
// hit. The judgment sub-test below is an API-contract test against the
// surface libfossil currently exposes, not a worker-protocol test.
//
// A proper worker-path test would pump `Checkout.Update` directly,
// scan the working tree for conflict markers, and assert that the next
// `Checkin` either errors with a surfaceable shape or that an inspect-
// before-commit hook fires. That test is currently inexpressible
// against the public libfossil API. The shape gap is tracked at
// danmestas/libfossil#37 ("feat: expose conflict info on
// Checkout.Update"). When that lands, a sibling sub-test should be
// added here covering the worker path.
//
// Failure modes this catches:
//   - Disjoint-line text-merge regresses to whole-file "ours/theirs"
//     picker (the trivial sub-test would lose one side's edits).
//   - Repo.Merge's same-line conflict stops returning
//     *MergeConflictError (the judgment sub-test would see a clean
//     merge that silently drops a side's work — strictly worse than
//     surfacing the conflict). This is the branch-merge API contract,
//     not the worker conflict surface.
//   - The materialize-after-merge round-trip loses the branch-merge
//     commit (the trivial sub-test's materialize assertion would fail).
func TestSwarmTBD_ConflictResolution_OperatorHandoff(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (swarm conflict resolution)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	t.Run("trivial_disjoint_lines", testSwarmConflict_TrivialDisjointLines)
	t.Run("judgment_same_line_branch_merge_api", testSwarmConflict_JudgmentSameLine_BranchMergeAPI)
}

// swarmConflictHarness sets up two sesh up subprocesses sharing one
// project repo via --scope=project, provisions both checkouts, and
// commits a shared baseline of shared.txt on trunk. Both checkouts are
// at the same trunk RID when the harness returns; the scenarios then
// branch off from this baseline.
type swarmConflictHarness struct {
	bin      string
	home     string
	project  string
	repoPath string
	alphaWT  string
	betaWT   string
}

// sharedTxtBaseline is the 10-line baseline both conflict scenarios start
// from. Line ranges:
//   - Scenario A (trivial): alpha-branch edits lines 1–3, beta-branch
//     edits lines 5–7. Lines 4 and 8–10 are the buffer that gives the
//     text-merge enough unchanged-context to align.
//   - Scenario B (judgment): both edit line 5 to different content.
const sharedTxtBaseline = "line-1\nline-2\nline-3\nline-4\nline-5\nline-6\nline-7\nline-8\nline-9\nline-10\n"

func newSwarmConflictHarness(t *testing.T) *swarmConflictHarness {
	t.Helper()
	h := &swarmConflictHarness{
		bin:     buildSesh(t),
		home:    t.TempDir(),
		project: t.TempDir(),
	}
	setupGitWorktree(t, h.project)

	alpha, alphaStderr := startSeshArgs(t, h.bin, h.home, h.project, "alpha", "--scope=project")
	t.Cleanup(func() { killAndWait(t, alpha, alphaStderr) })
	waitForURLs(t, filepath.Join(h.project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, alphaStderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", alphaStderr.String())
	}

	beta, betaStderr := startSeshArgs(t, h.bin, h.home, h.project, "beta", "--scope=project")
	t.Cleanup(func() { killAndWait(t, beta, betaStderr) })
	waitForURLs(t, filepath.Join(h.project, ".sesh", "sessions", "beta.json"), 15*time.Second)

	h.alphaWT = mustRunWorktree(t, h.bin, h.home, h.project, "alpha", "--scope=project")
	h.betaWT = mustRunWorktree(t, h.bin, h.home, h.project, "beta", "--scope=project")
	h.repoPath = filepath.Join(h.project, ".sesh", "project.repo")

	// Establish a shared baseline on trunk: alpha commits shared.txt;
	// beta picks it up. Both checkouts are at the same trunk RID with
	// shared.txt content equal to sharedTxtBaseline before the scenario-
	// specific divergence starts.
	if err := os.WriteFile(filepath.Join(h.alphaWT, "shared.txt"),
		[]byte(sharedTxtBaseline), 0o644); err != nil {
		t.Fatalf("write baseline shared.txt in alpha checkout: %v", err)
	}
	if err := commitViaLibfossil(h.repoPath, h.alphaWT, "shared.txt",
		"swarm baseline shared.txt"); err != nil {
		t.Fatalf("baseline commit via libfossil: %v", err)
	}
	if !awaitPeerSees(h.betaWT, h.repoPath, "shared.txt", sharedTxtBaseline, 10*time.Second) {
		t.Fatalf("beta's checkout did not see the baseline shared.txt within 10s")
	}

	return h
}

// testSwarmConflict_TrivialDisjointLines is Scenario A: alpha edits
// lines 1-3 on branch "alpha-edits", beta edits lines 5-7 on branch
// "beta-edits". A subsequent Repo.Merge("alpha-edits", "trunk") followed
// by Repo.Merge("beta-edits", "trunk") (or vice versa) yields a clean
// trunk tip containing both halves.
func testSwarmConflict_TrivialDisjointLines(t *testing.T) {
	h := newSwarmConflictHarness(t)

	alphaContent := "ALPHA-1\nALPHA-2\nALPHA-3\nline-4\nline-5\nline-6\nline-7\nline-8\nline-9\nline-10\n"
	betaContent := "line-1\nline-2\nline-3\nline-4\nBETA-5\nBETA-6\nBETA-7\nline-8\nline-9\nline-10\n"

	commitOnBranch(t, h.repoPath, h.alphaWT, "shared.txt",
		"alpha-edits", "alpha edits lines 1-3", alphaContent)
	commitOnBranch(t, h.repoPath, h.betaWT, "shared.txt",
		"beta-edits", "beta edits lines 5-7", betaContent)

	// Fold alpha-edits into trunk first, then beta-edits into trunk.
	// Both should be clean three-way text-merges.
	if _, _, err := repoMerge(h.repoPath, "alpha-edits", "trunk",
		"merge alpha-edits into trunk", "swarm-test"); err != nil {
		t.Fatalf("merge alpha-edits → trunk (expected clean): %v", err)
	}
	if _, _, err := repoMerge(h.repoPath, "beta-edits", "trunk",
		"merge beta-edits into trunk", "swarm-test"); err != nil {
		t.Fatalf("merge beta-edits → trunk (expected clean): %v", err)
	}

	// Materialize the post-merge trunk tip; both halves must be present.
	outDir := t.TempDir()
	mustRunMaterialize(t, h.bin, h.home, h.project, "alpha",
		"--scope=project", "--output="+outDir)
	final, err := os.ReadFile(filepath.Join(outDir, "shared.txt"))
	if err != nil {
		t.Fatalf("read materialized shared.txt: %v", err)
	}
	for _, want := range []string{"ALPHA-1", "ALPHA-2", "ALPHA-3", "BETA-5", "BETA-6", "BETA-7"} {
		if !strings.Contains(string(final), want) {
			t.Errorf("materialized trunk HEAD missing %q\n--- file ---\n%s", want, final)
		}
	}
	// No conflict markers should have leaked through the auto-merge.
	if strings.Contains(string(final), "<<<<<<<") ||
		strings.Contains(string(final), ">>>>>>>") {
		t.Errorf("materialized trunk HEAD contains conflict markers (expected clean merge):\n%s", final)
	}
}

// testSwarmConflict_JudgmentSameLine_BranchMergeAPI is Scenario B and is
// scoped as a libfossil API-contract assertion. Alpha edits line 5 to
// "ALPHA-EDIT" on branch "alpha-edits"; beta edits the SAME line to
// "BETA-EDIT" on branch "beta-edits". The first merge into trunk
// succeeds (it's a fast-forward from the baseline). The second merge
// must return *MergeConflictError naming shared.txt on the Repo.Merge
// (branch-merge) surface — that is the surface libfossil currently
// exposes to Go callers, and the only conflict-shape this test can
// assert today.
//
// This is NOT the production worker conflict path. The fossil-worker
// accessory's conflict path is `Checkout.Update` writing conflict
// markers into the working tree after an autosync pull; that path's
// public Go API at libfossil/checkout.go:125-135 returns plain `error`
// and currently swallows the conflict signal. See the parent test's
// godoc and danmestas/libfossil#37 for the gap. When that lands, add a
// sibling sub-test exercising Checkout.Update + next-Checkin.
func testSwarmConflict_JudgmentSameLine_BranchMergeAPI(t *testing.T) {
	h := newSwarmConflictHarness(t)

	alphaContent := "line-1\nline-2\nline-3\nline-4\nALPHA-EDIT\nline-6\nline-7\nline-8\nline-9\nline-10\n"
	betaContent := "line-1\nline-2\nline-3\nline-4\nBETA-EDIT\nline-6\nline-7\nline-8\nline-9\nline-10\n"

	commitOnBranch(t, h.repoPath, h.alphaWT, "shared.txt",
		"alpha-edits", "alpha edits line 5", alphaContent)
	commitOnBranch(t, h.repoPath, h.betaWT, "shared.txt",
		"beta-edits", "beta edits line 5", betaContent)

	// First merge: alpha-edits → trunk. Should be clean (single
	// divergent line vs ancestor; no other side has changed it yet).
	if _, _, err := repoMerge(h.repoPath, "alpha-edits", "trunk",
		"merge alpha-edits into trunk", "swarm-test"); err != nil {
		t.Fatalf("merge alpha-edits → trunk (expected clean): %v", err)
	}

	// Second merge: beta-edits → trunk. Beta also touched line 5; the
	// three-way merge against the ancestor cannot pick a winner.
	_, _, err := repoMerge(h.repoPath, "beta-edits", "trunk",
		"merge beta-edits into trunk", "swarm-test")
	if err == nil {
		t.Fatal("merge beta-edits → trunk: expected *MergeConflictError, got nil (the conflict surface vanished)")
	}
	if !errors.Is(err, libfossil.ErrMergeConflict) {
		t.Errorf("err = %v, want errors.Is ErrMergeConflict", err)
	}
	var mce *libfossil.MergeConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("err = %v, want errors.As *MergeConflictError (the operator-readable surface)", err)
	}
	if len(mce.Files) != 1 || mce.Files[0] != "shared.txt" {
		t.Errorf("conflict files = %v, want [shared.txt]", mce.Files)
	}

	// The test stops here. Resolution — picking ALPHA-EDIT, BETA-EDIT, or
	// something else — is operator territory. By not resolving, this
	// test asserts the discipline: sesh's machinery surfaces the
	// conflict and stops. An autonomous-resolution mode would be a
	// distinct test that explicitly opts in.
}

// commitOnBranch opens the repo and checkout, writes content into
// fileName, switches the checkout to a new branch named branchName, and
// commits. Used by the conflict scenarios to produce two divergent
// branches from a shared trunk baseline without going through the CLI.
//
// Why not commitViaLibfossil + a separate branch-create call: the
// CheckoutCommitOpts.Branch field is libfossil's documented way to fork
// a new branch off the current checkout in one atomic call. Using it
// avoids the race window where the worker is on the wrong branch
// between create and commit.
func commitOnBranch(t *testing.T, repoPath, checkoutDir, fileName, branchName, message, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(checkoutDir, fileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s into %s: %v", fileName, checkoutDir, err)
	}
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("open repo %s: %v", repoPath, err)
	}
	defer repo.Close()
	co, err := repo.OpenCheckout(checkoutDir, libfossil.CheckoutOpenOpts{})
	if err != nil {
		t.Fatalf("open checkout %s: %v", checkoutDir, err)
	}
	defer co.Close()
	if _, err := co.Add([]string{fileName}); err != nil {
		t.Fatalf("add %s: %v", fileName, err)
	}
	if _, _, err := co.Checkin(libfossil.CheckoutCommitOpts{
		Message: message,
		User:    "swarm-test",
		Branch:  branchName,
	}); err != nil {
		t.Fatalf("checkin on branch %s: %v", branchName, err)
	}
}

// repoMerge opens repoPath and calls Repo.Merge, returning the merged
// commit's (rid, uuid) on success or the libfossil error verbatim.
// The repo handle is closed before returning so subsequent operations
// (additional Merge calls, materialize) see a fully-released SQLite
// file.
func repoMerge(repoPath, srcBranch, dstBranch, message, user string) (int64, string, error) {
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return 0, "", err
	}
	defer repo.Close()
	return repo.Merge(srcBranch, dstBranch, message, user)
}
