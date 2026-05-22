# Agent Role & Class Registration — Phase 3 (orch-spawn) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `orch-spawn` exports `SESH_ROLE` and `SESH_CLASS` (in addition to the existing `ORCH_ROLE`) into spawned worker processes, so the adapters (Phase 2) inherit them automatically when launched by orch.

**Architecture:** orch-spawn (a **Bash** script at `~/projects/orch/bin/orch-spawn`) already parses `--role`, `--outfit`, `--cut` flags and derives an effective role: explicit `--role` wins; outfit `stasi` or cut matching `wait-watch|spy-on-*` defaults role to `observer`; otherwise `worker`. The role is currently passed to the `orch-agent-shim` process at line 401 but NOT exported to the spawned worker's shell environment. This plan adds two exports to the worker's environment block: `SESH_ROLE` (= the derived role) and `SESH_CLASS` (= `observer` for the same outfit/cut conditions that already trigger an observer role; otherwise `active`).

**Tech Stack:** Bash (orch-spawn is a shell script, not Go). Integration tests live under `~/projects/orch/test/test-orch-spawn-*.sh`.

**Source proposal:** `docs/proposals/2026-05-21-agent-role-registration.md` (Phase 3, lines 134-135)

**Depends on:** Phase 1 (sesh core) and Phase 2 (at least one adapter that reads the env vars). Without those, orch-spawn exports the env vars into a vacuum.

**Cross-repo note:** orch-spawn lives in `~/projects/orch/` — a sibling repo, not in sesh. This plan must be executed in that repo with PRs against `orch/main`.

---

## File Structure

**Modify:**
- `~/projects/orch/bin/orch-spawn` — Bash script. Touch points:
  - Lines 156-170 (current role-derivation block): also derive `CLASS` from the same outfit/cut conditions
  - Line 401 (shim invocation): pass `SESH_ROLE` and `SESH_CLASS` into the shim's env so the worker inherits them
  - Lines 115-152 (flag parsing): add an optional `--class active|observer` override
- `~/projects/orch/docs/orch-spawn.md` (or equivalent docs file) — document new env vars and the optional `--class` flag

**Test:**
- `~/projects/orch/test/test-orch-spawn-sesh-env.sh` — new integration script asserting `SESH_ROLE` / `SESH_CLASS` land in the worker's environment

---

## Task 1: Derive CLASS alongside ROLE

The script already derives `ROLE` at lines 156-170 (`stasi` outfit or `wait-watch|spy-on-*` cut → `observer`, else `worker`, unless `--role` overrides). Add a parallel `CLASS` variable derived from the same conditions, plus a new `--class` flag for explicit override.

**Files:**
- Modify: `~/projects/orch/bin/orch-spawn` (flag-parse block 115-152, role-derivation block 156-170)

- [ ] **Step 1: Read the current role-derivation block**

Run: `sed -n '115,170p' ~/projects/orch/bin/orch-spawn`

Confirm:
- The `case "$1" in` flag-parse block (lines 115-152)
- The role-derivation block — likely a sequence of `if`/`elif` that sets `ROLE=observer` for stasi/spy-on cases.

- [ ] **Step 2: Add `--class` flag parsing**

In the `case "$1" in` block (lines 115-152), insert a new arm alongside `--role`:

```bash
        --class)
            shift
            CLASS_OVERRIDE="$1"
            shift
            ;;
        --class=*)
            CLASS_OVERRIDE="${1#--class=}"
            shift
            ;;
```

After the loop ends (just before the role-derivation block), validate the override:

```bash
if [[ -n "${CLASS_OVERRIDE:-}" && "$CLASS_OVERRIDE" != "active" && "$CLASS_OVERRIDE" != "observer" ]]; then
    printf 'orch-spawn: --class must be "active" or "observer", got %q\n' "$CLASS_OVERRIDE" >&2
    exit 2
fi
```

- [ ] **Step 3: Derive CLASS alongside ROLE**

Immediately after the existing role-derivation block (after line ~170), add:

```bash
# CLASS derivation mirrors ROLE's outfit/cut conditions: stasi outfit or
# spy-on-* / wait-watch cut → observer, else active. Explicit --class wins.
# Reference: docs/proposals/2026-05-21-agent-role-registration.md (sesh repo).
if [[ -n "${CLASS_OVERRIDE:-}" ]]; then
    CLASS="$CLASS_OVERRIDE"
elif [[ "$ROLE" == "observer" ]]; then
    # If the role-derivation block landed on observer, the class matches.
    CLASS="observer"
else
    CLASS="active"
fi
```

This keeps a single conditional chain (the existing role logic) as the source of truth — `CLASS=observer` iff `ROLE=observer`, unless `--class` overrides. No duplicated outfit-name matching.

- [ ] **Step 4: Sanity-check the script parses**

Run: `bash -n ~/projects/orch/bin/orch-spawn`

Expected: no syntax errors.

- [ ] **Step 5: Checkpoint**

Commit (in orch repo): `feat(orch-spawn): derive CLASS alongside ROLE; accept --class override`.

---

## Task 2: Export SESH_ROLE and SESH_CLASS into the worker's shim env

orch-spawn currently passes `ORCH_ROLE` to `orch-agent-shim` at line 401. Add `SESH_ROLE` and `SESH_CLASS` to the same env block so they propagate to the worker's shell.

**Files:**
- Modify: `~/projects/orch/bin/orch-spawn` line 401 (shim invocation)

- [ ] **Step 1: Read the current shim invocation**

Run: `sed -n '395,415p' ~/projects/orch/bin/orch-spawn`

Identify the exact line that invokes the shim with `ORCH_ROLE`. It will look something like:

```bash
ORCH_ROLE="$ROLE" ORCH_SESH_BIN="$SESH_BIN" exec orch-agent-shim ...
```

or use `env -i` / explicit `export` statements.

- [ ] **Step 2: Add SESH_ROLE and SESH_CLASS to the invocation**

Extend the prefix to include the two new vars:

```bash
ORCH_ROLE="$ROLE" \
SESH_ROLE="$ROLE" \
SESH_CLASS="$CLASS" \
ORCH_SESH_BIN="$SESH_BIN" \
exec orch-agent-shim ...
```

Keep `ORCH_ROLE` for legacy consumers per the proposal.

- [ ] **Step 3: Sanity-check**

Run: `bash -n ~/projects/orch/bin/orch-spawn`

Expected: no syntax errors.

- [ ] **Step 4: Checkpoint**

Commit: `feat(orch-spawn): export SESH_ROLE and SESH_CLASS into worker shim env`.

---

## Task 3: Integration test — env vars land in worker shell

**Files:**
- Create: `~/projects/orch/test/test-orch-spawn-sesh-env.sh`

Pattern after the existing `test-orch-spawn-*.sh` scripts in that directory.

- [ ] **Step 1: Locate a representative existing test for reference**

Run: `ls ~/projects/orch/test/test-orch-spawn-*.sh | head -5`

Read one (`cat <path>`) to confirm the test harness convention — how panes are spawned, how worker env is read back, what assertion style is used (`grep -q`, `assert_contains`, etc.).

- [ ] **Step 2: Write the new test script**

File: `~/projects/orch/test/test-orch-spawn-sesh-env.sh`

```bash
#!/usr/bin/env bash
# Asserts that orch-spawn exports SESH_ROLE and SESH_CLASS into the
# worker's environment, derived from --outfit/--cut/--role and the new
# --class flag. Mirrors docs/proposals/2026-05-21-agent-role-registration.md
# (sesh repo) Phase 3.
set -euo pipefail

# Adopt the same test setup pattern as sibling test-orch-spawn-*.sh scripts.
# This is a sketch; copy the exact session-setup boilerplate from a working
# sibling script before submitting.

source "$(dirname "$0")/common.sh"  # if a shared helper exists; else inline

case_defaults_to_worker_active() {
    spawn_worker --outfit engineer --cut implementing
    role=$(read_worker_env SESH_ROLE)
    class=$(read_worker_env SESH_CLASS)
    [[ "$role" == "worker" ]] || die "SESH_ROLE = $role, want worker"
    [[ "$class" == "active" ]] || die "SESH_CLASS = $class, want active"
}

case_stasi_outfit_yields_observer() {
    spawn_worker --outfit stasi
    role=$(read_worker_env SESH_ROLE)
    class=$(read_worker_env SESH_CLASS)
    [[ "$role" == "observer" ]] || die "stasi SESH_ROLE = $role, want observer"
    [[ "$class" == "observer" ]] || die "stasi SESH_CLASS = $class, want observer"
}

case_explicit_class_override() {
    spawn_worker --outfit engineer --class observer
    class=$(read_worker_env SESH_CLASS)
    [[ "$class" == "observer" ]] || die "--class observer SESH_CLASS = $class, want observer"
}

case_invalid_class_rejected() {
    if spawn_worker --outfit engineer --class passive 2>/dev/null; then
        die "--class passive should have failed"
    fi
}

case_defaults_to_worker_active
case_stasi_outfit_yields_observer
case_explicit_class_override
case_invalid_class_rejected

echo "PASS"
```

`spawn_worker` and `read_worker_env` are placeholders for the actual orch test-harness helpers. Replace them with whatever a sibling test-orch-spawn script uses (likely `tmux send-keys` + `tmux capture-pane` + `printenv` round-trip, or a direct `env`-piped invocation).

- [ ] **Step 3: Run the integration test**

Run: `cd ~/projects/orch && bash test/test-orch-spawn-sesh-env.sh`

Expected: PASS — prints `PASS`.

- [ ] **Step 4: Checkpoint**

Commit: `test(orch-spawn): integration test for SESH_ROLE / SESH_CLASS export and --class override`.

---

## Task 4: Update orch-spawn docs

**Files:**
- Modify: `~/projects/orch/docs/orch-spawn.md` (the file referenced by `sesh/docs/orch-bridge.md`)

- [ ] **Step 1: Find the existing env-export documentation**

Run: `cd ~/projects/orch && grep -n 'ORCH_ROLE\|ORCH_SESH_BIN' docs/`

- [ ] **Step 2: Add SESH_ROLE / SESH_CLASS to the env-export table**

Append rows:

```markdown
| `SESH_ROLE`  | Set to the same value as `ORCH_ROLE` (the derived role). Consumed by sesh adapters per `docs/proposals/2026-05-21-agent-role-registration.md` (sesh repo). |
| `SESH_CLASS` | `observer` when role-derivation lands on `observer` (stasi outfit / `wait-watch` cut / `spy-on-*` cut), else `active`. Override with `--class`. Drives coordination-subject routing on the sesh side. |
```

- [ ] **Step 3: Add a `--class` row to the flag table**

```markdown
| `--class active|observer` | Optional. Overrides the outfit/cut-derived class. Drives `SESH_CLASS` in the worker env. Invalid values cause orch-spawn to exit 2. |
```

- [ ] **Step 4: Checkpoint**

Commit (in orch repo): `docs(orch-spawn): document SESH_ROLE / SESH_CLASS exports and --class flag`.

---

## Task 5: End-to-end against sesh + claude-nats-channel

**Files:** (no edits — verification only)

**Pre-req:** Phase 1 plan merged in sesh; Phase 2 Task 1-3 merged for claude-nats-channel.

- [ ] **Step 1: Spawn a default worker via orch-spawn**

Run:

```bash
sesh up rc-e2e-orch
orch-spawn --outfit engineer --cut implementing --session rc-e2e-orch claude-code
```

Wait ~3s.

- [ ] **Step 2: Inspect the session JSON**

Run: `jq '.agents' ~/projects/sesh/.sesh/sessions/rc-e2e-orch.json`

Expected: at least one agent with `"role": "worker"`, `"class": "active"` (or whatever the engineer outfit's role-derivation produces).

- [ ] **Step 3: Spawn a stasi observer and verify**

Run: `orch-spawn --outfit stasi --session rc-e2e-orch claude-code`

Wait ~3s. Run: `jq '.agents | map(select(.class == "observer"))' ~/projects/sesh/.sesh/sessions/rc-e2e-orch.json`

Expected: at least one entry with `class=observer`.

- [ ] **Step 4: Test explicit --class override**

Run: `orch-spawn --outfit engineer --class observer --session rc-e2e-orch claude-code`

Wait ~3s. Verify another `class=observer` entry appears.

- [ ] **Step 5: Tear down**

Run: `sesh down rc-e2e-orch`

- [ ] **Step 6: Checkpoint**

If everything passes, open the PR against `orch/main`. The PR body should reference both this plan and the sesh proposal, and include the verified `jq` output.

---

## Acceptance

- [x] orch-spawn derives `CLASS` alongside the existing `ROLE` derivation (stasi outfit / spy-on cut → observer, else active). → Task 1
- [x] orch-spawn accepts `--class active|observer` override flag. → Task 1
- [x] orch-spawn exports `SESH_ROLE` and `SESH_CLASS` into the worker's shim env (alongside existing `ORCH_ROLE`). → Task 2
- [x] Integration test asserts env vars land correctly under default, stasi, --class-override, and invalid-class scenarios. → Task 3
- [x] Docs updated. → Task 4
- [x] End-to-end test confirms env vars surface in sesh session JSON via the adapter. → Task 5

---

## Out of Scope

- **Phase 1 (sesh core):** `2026-05-22-agent-role-class-phase1-sesh.md`.
- **Phase 2 (adapters):** `2026-05-22-agent-role-class-phase2-adapters.md`.
- **Phase 4 (sesh up --exec):** gated on sesh#89; no plan written yet.
- **Multi-class outfits:** outfit→class mapping starts with just `spy → observer`. Expanding to other observer-class outfits (e.g., `auditor`, `watcher`) is a future change, not part of this plan.
