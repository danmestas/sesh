# orch e2e fixtures

Test fixtures for the orch-variant Tier 1 e2e suite
(`cli/e2e_orch_test.go`, build tag `orch_e2e`). The orch variant exists
to validate sesh's swarm workflow end-to-end through the real
subprocess chain — `orch-spawn` -> tmux -> claude — rather than the
mock variant's libfossil-direct path.

These fixtures let the orch variant run without a real interactive
`claude` binary. They are committed and bound; the recipes are bounded
shell sequences that touch only the fossil checkout cwd they run in.

## Files

- **`stub-claude.sh`** — non-interactive stand-in for `claude`. The
  test stages this on PATH as `claude` via a shim dir, so that
  `orch-spawn claude --sesh-session <label>` launches the stub
  instead. The stub ignores all CLI flags (including
  `--dangerously-skip-permissions` and `--append-system-prompt-file`),
  reads a recipe path from `$SESH_E2E_RECIPE` env var or falls back
  to `./.stub-claude-recipe` in its cwd, executes the recipe with
  shell, writes a `.stub-claude-done` marker, and exits.

- **`worker-mission.recipe`** — fossil-worker stand-in for the full
  mission loop test (T1.2). Creates `mission.txt`, runs `fossil add`,
  `fossil commit`. Output: the trunk advances by one commit.

  Recipes commit under the operator's natural `$USER`. `sesh up`
  registers `$USER` in the seeded project repo's fossil user table at
  seed time (sesh#77), so `fossil commit` from a checkout works
  without any `fossil user new` priming step.

- **`verifier-mission.recipe`** — fossil-verifier stand-in for T1.2.
  Reads the trunk via `fossil timeline`, writes a verdict file
  (`verifier-verdict.txt`) matching the accessory template. Output:
  the test reads the file back and asserts the verdict line is GO.

- **`fanout-alpha.recipe`**, **`fanout-beta.recipe`**,
  **`fanout-gamma.recipe`** — star-fanout test (T1.1). Each commits
  one distinct file from one session's checkout; the test then
  watches the other two checkouts observe it.

- **`hubrestart-pre.recipe`**, **`hubrestart-post.recipe`** —
  hub-restart test (T1.3). Pre commits before the test kills the
  central hub; post commits after. The recovery property under test
  is that both commits land in beta's view.

## How the test wires it up

For each test invocation, the Go code:

1. Locates this directory relative to the test file.
2. Builds a per-test shim dir with `claude -> stub-claude.sh` so
   `orch-spawn claude` resolves to the stub on PATH.
3. Writes the chosen recipe's absolute path to `.stub-claude-recipe`
   inside the fossil checkout dir (the cwd orch-spawn will land the
   stub in via `--sesh-session`). The pointer file is a single-line
   path; the stub follows it to find the actual recipe under
   `cli/testdata/orch_e2e/`. The file-based handoff sidesteps tmux's
   env-var stripping for new sessions / panes.
4. Invokes `orch-spawn claude --sesh-session <label> --headless
   --no-shim --no-fleet` with PATH=shim-dir:$PATH. orch-spawn places
   a tmux pane in the `orch-headless` session; the stub runs the
   recipe and exits.
5. Polls for `.stub-claude-done` and/or the recipe's side effects
   (file appearing in the checkout, fossil commit in the timeline).
6. Tears down: `tmux kill-pane -t <pane>` and `tmux kill-session -t
   orch-headless` if it owns the session.

## Operator override

To point the tests at a real `claude` binary or custom recipes:

```bash
SESH_E2E_CLAUDE=/path/to/real/claude \
SESH_E2E_RECIPE_DIR=/path/to/your/recipes \
go test -tags=orch_e2e ./cli/ -run TestE2E -v
```

The default behavior auto-resolves to this directory and the stub.
