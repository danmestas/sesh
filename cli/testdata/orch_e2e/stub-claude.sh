#!/usr/bin/env bash
# stub-claude.sh — non-interactive stand-in for `claude`, used by the
# orch-variant e2e tests (cli/e2e_orch_test.go) to drive a recipe inside
# a tmux pane that `orch-spawn` placed.
#
# Behavior:
#
#   1. Ignore every flag passed in. The real claude takes
#      --dangerously-skip-permissions, --append-system-prompt-file, etc.
#      We accept them and discard. The recipe controls what happens, not
#      the CLI flags.
#
#   2. Resolve the recipe path:
#        a. If $SESH_E2E_RECIPE is set, use it.
#        b. Else, read ./.stub-claude-recipe in cwd. This is the path
#           the e2e tests actually use, because tmux's existing server
#           strips most env vars from new panes (update-environment is
#           limited to DISPLAY/SSH_AUTH_SOCK/etc by default). A file in
#           cwd is the robust handoff. The test writes the file into
#           the fossil checkout before calling orch-spawn.
#
#   3. Execute each non-empty, non-comment line of the recipe as a shell
#      command in the current cwd. orch-spawn has already cd'd us into
#      the fossil checkout via `--sesh-session <label>` resolution.
#
#   4. On success, write a marker file ./.stub-claude-done containing
#      "ok" + the timestamp. On any command failure, write the failing
#      line + exit code to .stub-claude-done and exit non-zero.
#
#   5. Exit. Do NOT block on `read` or attempt to keep the pane alive
#      — the test will tmux kill-pane us when it's done observing
#      side effects.
#
# Why a script and not a Go binary:
#
#   The recipe primitives we drive are shell-flavoured (`fossil add`,
#   `fossil commit`, `echo > file`). Implementing a recipe interpreter
#   in Go would duplicate the shell. The test environment already
#   requires bash + fossil + tmux to even reach this stub; one more
#   bash script is no new dependency.
#
# Tier-1 safety:
#
#   The stub never touches paths outside its cwd (the fossil checkout)
#   or whatever the recipe explicitly references. Recipes themselves
#   are committed, reviewable, and bounded — they don't `rm -rf` or
#   `os.Remove*` anything in .sesh/messaging/ or .sesh/sessions/.

set -u

# Pre-flight: stub-claude must be invoked with a recipe somewhere
# accessible. Without one we have nothing to do; rather than hang the
# pane forever (which would survive past the test's tmux kill-pane and
# leak), fail loud and fast.
RECIPE=""
if [ -n "${SESH_E2E_RECIPE:-}" ]; then
    RECIPE="$SESH_E2E_RECIPE"
elif [ -f "./.stub-claude-recipe" ]; then
    # The pointer file's single line is the absolute path to the
    # recipe to run. This indirection (vs. dropping the recipe content
    # itself into the checkout) keeps the source-of-truth file in
    # cli/testdata/orch_e2e/ and means stub-claude's error messages
    # name the committed fixture, not a per-test copy.
    POINTER=$(head -1 ./.stub-claude-recipe | tr -d '[:space:]')
    if [ -n "$POINTER" ]; then
        RECIPE="$POINTER"
    fi
fi

DONE_MARKER="./.stub-claude-done"

if [ -z "$RECIPE" ]; then
    printf 'stub-claude: no recipe (set SESH_E2E_RECIPE or write ./.stub-claude-recipe)\n' >&2
    printf 'no-recipe %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DONE_MARKER" 2>/dev/null || true
    exit 2
fi

if [ ! -f "$RECIPE" ]; then
    printf 'stub-claude: recipe not found: %s\n' "$RECIPE" >&2
    printf 'recipe-missing %s %s\n' "$RECIPE" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DONE_MARKER" 2>/dev/null || true
    exit 2
fi

printf 'stub-claude: executing recipe %s in %s\n' "$RECIPE" "$(pwd)" >&2

# Source the recipe as a bash script. Lets recipes use multi-line
# constructs (heredocs, `{ ...; } > file`, command substitutions) the
# same way a real claude-worker shell session would. We don't use `set
# -e` inside the recipe because some lines are intentionally fallible
# (e.g. `fossil user new <name> ... || true` — adding an existing user
# is an error that recipes ignore).
#
# `bash -e` would stop at the first failing line, but that's
# incompatible with the `|| true` pattern recipes use. Instead, the
# recipe is responsible for its own error semantics; the stub captures
# the final exit code and reports it via the done marker.
bash "$RECIPE"
rc=$?
if [ "$rc" -ne 0 ]; then
    printf 'stub-claude: recipe %s exited non-zero (rc=%d)\n' "$RECIPE" "$rc" >&2
    printf 'fail rc=%d recipe=%s\n' "$rc" "$RECIPE" > "$DONE_MARKER"
    exit "$rc"
fi

printf 'ok %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DONE_MARKER"
printf 'stub-claude: recipe complete\n' >&2
exit 0
