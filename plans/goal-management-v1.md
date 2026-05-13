# Goal Management v1 — Implementation Plan

**Status:** Planning. Spec at [`docs/goal-management.md`](../docs/goal-management.md).
Ready for issue breakdown via `to-issues`.

**Summary:** Implement the goal-management substrate primitive across two
repos: `sesh` (spec doc + minimal housekeeping) and `sesh-ops` (the bulk of
the CLI + GoalManager module). EdgeSync and libfossil require zero changes
for v1.

## Repos affected

| Repo | v1 work | Why |
| --- | --- | --- |
| `sesh` | doc lives here (spec already drafted); minimal housekeeping | The spec is sesh's contribution. No new Go code needed in sesh itself. |
| `sesh-ops` | ~80% of implementation: GoalManager module + CLI commands + sweeper | Reference CLI implementation per the spec's "Operator interface — sesh-ops" section. |
| `EdgeSync` | **none** | Goals use NATS KV (already in hub). iroh/Fossil not involved. |
| `libfossil` | **none** | Goals don't touch Fossil. Records are NATS KV values. |

V2 candidates that *might* eventually touch EdgeSync are flagged at the
bottom — none block v1.

## Pre-flight assumptions

- `sesh-ops` repo exists at [github.com/danmestas/sesh-ops](https://github.com/danmestas/sesh-ops).
  If not yet bootstrapped, slice 0 (below) handles initial scaffold.
- sesh's hub is the primary NATS endpoint (`~/.sesh/hub.nats.url`).
- Tests run against a real `sesh up` hub, not mocks (per upstream
  convention; integration discipline matches the task-management
  implementation).
- Go 1.22+ is the toolchain (matches sesh's choice).

## Tracer-bullet slices

Each slice below is independently shippable and maps 1:1 to a GH issue.
Slice dependencies are listed explicitly. Slices 1–5 land in `sesh-ops`;
slice 6 lands in `sesh`.

---

### Slice 0 (conditional): sesh-ops bootstrap

**Title:** `chore: scaffold sesh-ops goal subcommand`

**Skip if:** `sesh-ops` already has a `goal` subcommand or a generic
subcommand-routing skeleton.

**Scope:**

- Confirm `sesh-ops` has its main entry point and a way to register new
  subcommand groups (e.g., cobra subcommand tree).
- Add a placeholder `sesh-ops goal` parent command that prints a usage
  stub.
- Wire the `goal` subcommand into the existing CLI dispatch.

**Files to touch:**

- `sesh-ops/cmd/goal.go` — parent command
- `sesh-ops/cmd/root.go` (or equivalent) — register the new subcommand
- `sesh-ops/README.md` — list `goal` in command surface

**Acceptance:**

- `sesh-ops goal --help` prints a usage block listing the eventual
  subcommands (even if each prints "not yet implemented").
- CI green; no functional behavior expected.

**Tests:** smoke test on `--help` output.

**Effort:** ~half day.

**Dependencies:** none.

---

### Slice 1: GoalManager module + CRUD commands

**Title:** `feat(goal): GoalManager module + CRUD CLI (create/get/list/status/clear)`

**Scope:**

- New Go package `sesh-ops/internal/goals` containing:
  - `Goal` struct matching v1 schema (all fields, JSON tags, validation
    via `go-playground/validator` or similar).
  - `Manager` type with a NATS connection + scope-routing helpers.
  - `Connect(scope, scopeID) → *nats.KeyValue` that picks the correct
    NATS URL per scope per the spec's routing rule:
    - `hub` / `project` / `workflow` → `~/.sesh/hub.nats.url`
    - `session` / `sub-leaf` / `agent` → session JSON's `nats_url`
  - `BucketName(scope, scopeID) → string` that sanitizes per the
    scoped-memory.md convention.
  - `Create(g Goal) error` — uses `kv.Create` (not `Put`) for
    idempotency; returns `ErrAlreadyExists` on EEXIST.
  - `Get(id) (Goal, uint64, error)` — returns record + revision.
  - `List(filter ListFilter) ([]Goal, error)` — supports status, owner,
    scope, root-only filters (root-only = `parent_goal_id == nil`).
  - `Delete(id) error`.
- CLI commands wired:
  - `sesh-ops goal create --objective="..." --scope=<scope>
    [--owner=<role:id>] [--budget=N] [--wall-clock=Ns] [--parent=<id>]
    [--allow-multiple-roots]`
  - `sesh-ops goal get [--id=<id>] [--owner=<role:id>]`
  - `sesh-ops goal list [--status=...] [--owner=...] [--scope=...]
    [--root-only]`
  - `sesh-ops goal status <id>` — formatted single-record summary
    (objective / status / owner / budget / wall-clock / tasks count).
  - `sesh-ops goal clear <id>` — `nats kv del`; no cascade in this slice.
- "One active root-goal per owner" scan-on-create logic:
  - Before `Create`, scan the target bucket for records matching
    `parent_goal_id == nil && owner == self && status == "pursuing"`.
  - If any found, refuse with a useful error including the existing
    goal's ID and objective.
  - `--allow-multiple-roots` flag bypasses the scan.
  - `--parent=<id>` flag skips the scan (sub-goals are unrestricted).

**Files to touch:**

- `sesh-ops/internal/goals/goal.go` — schema struct + validation
- `sesh-ops/internal/goals/manager.go` — NATS connection + routing
- `sesh-ops/internal/goals/bucket.go` — bucket name derivation,
  sanitization
- `sesh-ops/internal/goals/manager_test.go` — unit tests for routing,
  sanitization, validation
- `sesh-ops/cmd/goal_create.go`
- `sesh-ops/cmd/goal_get.go`
- `sesh-ops/cmd/goal_list.go`
- `sesh-ops/cmd/goal_status.go`
- `sesh-ops/cmd/goal_clear.go`
- `sesh-ops/test/integration/goal_crud_test.go`

**Acceptance:**

- `sesh-ops goal create --objective="X" --scope=project` returns a ULID;
  KV record present at `sesh_goals_project_<project>` with `status=pursuing`,
  `started_at` set.
- `sesh-ops goal list --scope=project --status=pursuing` shows the
  record.
- `sesh-ops goal get --id=<id>` returns the full JSON record.
- `sesh-ops goal status <id>` renders a human-readable summary.
- `sesh-ops goal clear <id>` removes the record; subsequent `get`
  returns `not found`.
- Attempting a second `goal create` for the same owner without
  `--allow-multiple-roots` is refused with a clear error.
- `--parent=<id>` creates a sub-goal even when the owner has an active
  root-goal.

**Tests:**

- Integration: spin up `sesh up --session=test` in a temp dir; run
  CRUD; assert KV state via `nats kv get` directly.
- Unit: bucket name sanitization (`my-app` → `my_app`, etc.); routing
  per scope; validation of required fields.
- Edge: idempotent `Create` (second call with same ID returns EEXIST).

**Effort:** ~3 days.

**Dependencies:** Slice 0 (or pre-existing CLI skeleton).

---

### Slice 2: State transitions

**Title:** `feat(goal): state transitions (pause/resume/complete/abandon)`

**Scope:**

- Implement state machine transitions per spec, using CAS-protected
  updates:
  - `Pause(id) error` — `pursuing → paused`.
  - `Resume(id) error` — `paused → pursuing`.
  - `Complete(id, result) error` — `pursuing → achieved`; sets
    `completed_at`, `result`.
  - `Abandon(id, reason, cascadeSubgoals bool) error` — non-terminal →
    `unmet`; sets `completed_at`, `result.reason`; optional cascade to
    pending sub-goals.
- Each transition validates the source state (refuses transitions from
  terminal states unless the operator explicitly forces).
- Each transition is CAS-protected: read revision, mutate, `kv.Update`
  with revision; on conflict, re-read and retry up to N times.
- CLI commands:
  - `sesh-ops goal pause <id>`
  - `sesh-ops goal resume <id>`
  - `sesh-ops goal complete <id> [--result='{"output":"..."}']`
  - `sesh-ops goal abandon <id> --reason="..." [--cascade-subgoals]`
- `--cascade-subgoals` flag walks `subgoals[]`, transitions any
  `pursuing` / `paused` children to `unmet` with reason
  `"parent abandoned: <parent-id>"`.

**Files to touch:**

- `sesh-ops/internal/goals/transitions.go` — state-machine logic
- `sesh-ops/internal/goals/transitions_test.go` — exhaustive state
  machine table
- `sesh-ops/cmd/goal_pause.go`
- `sesh-ops/cmd/goal_resume.go`
- `sesh-ops/cmd/goal_complete.go`
- `sesh-ops/cmd/goal_abandon.go`
- `sesh-ops/test/integration/goal_transitions_test.go`

**Acceptance:**

- All 5 status transitions per the state machine in the spec succeed
  end-to-end.
- Terminal-state transitions (e.g. `complete` on already-achieved) are
  refused with a clear error.
- `--cascade-subgoals` transitions only `pursuing` / `paused` children
  (not already-terminal ones).
- CAS contention: two concurrent `pause` calls — exactly one succeeds,
  the other reports CAS failure cleanly.

**Tests:**

- Integration: full lifecycle (create → pause → resume → complete →
  achieved); confirm `completed_at` and `result` set correctly.
- Integration: abandon-with-cascade across a 3-level hierarchy.
- Concurrency: spawn two transitions in parallel; assert exactly one
  succeeds.

**Effort:** ~2 days.

**Dependencies:** Slice 1.

---

### Slice 3: Token accounting + sweeper

**Title:** `feat(goal): token accounting CAS counter + budget sweeper`

**Scope:**

- `Account(id, tokens int) error` — CAS-protected increment of
  `used_tokens`. On budget exceed, automatically transitions status to
  `budget_limited` and sets `completed_at`.
- Budget enforcement checks both `token_budget` and
  `wall_clock_budget_sec` (latter compares `now - started_at`).
- `Sweep(opts SweepOpts) error` — one-shot pass:
  - Scan a target bucket (or all goal buckets) for records where
    `status == "pursuing"`.
  - If `used_tokens >= token_budget` or wall-clock exceeded, CAS-update
    to `budget_limited`.
  - Optional `--budget-warning-threshold=0.75` publishes a warning
    event on `sesh.goals.<scope>.<scope-id>.<goal-id>.budget_warning`
    when usage crosses the threshold (idempotent — won't republish if
    already warned).
- `Sweep --daemon` mode: runs `Sweep` every 30s with structured logs;
  graceful shutdown on SIGTERM.
- CLI commands:
  - `sesh-ops goal account <id> <tokens>`
  - `sesh-ops goal sweep [--budget-warning-threshold=0.75]
    [--scope=<scope>] [--scope-id=<id>]`
  - `sesh-ops goal sweep --daemon`

**Files to touch:**

- `sesh-ops/internal/goals/accounting.go` — CAS counter
- `sesh-ops/internal/goals/sweeper.go` — scan + transition
- `sesh-ops/internal/goals/sweeper_test.go`
- `sesh-ops/cmd/goal_account.go`
- `sesh-ops/cmd/goal_sweep.go`
- `sesh-ops/test/integration/goal_budget_test.go`

**Acceptance:**

- A goal with `token_budget=1000` transitions to `budget_limited` when
  `used_tokens` hits 1000 (set in one `Account` call OR cumulative
  across multiple).
- Wall-clock budget enforced: a goal with
  `wall_clock_budget_sec=3600` and `started_at` 2h ago is transitioned
  to `budget_limited` on the next sweep.
- Sweeper daemon runs continuously; multiple instances are CAS-safe (no
  duplicate transitions).
- Budget warning event published once per goal per threshold crossing.

**Tests:**

- Integration: create goal with budget=100; account 50 then 50; assert
  budget_limited.
- Integration: create goal with wall-clock=10s; sleep 11s; sweep;
  assert budget_limited.
- Concurrency: two sweepers running simultaneously; assert CAS-safety.

**Effort:** ~3 days.

**Dependencies:** Slice 2.

---

### Slice 4: Task linkage

**Title:** `feat(goal): task linkage (link-task/unlink-task/cleanup-tasks)`

**Scope:**

- `LinkTask(goalID, taskID) error`:
  - Reads the task record from the appropriate task bucket
    (`sesh_tasks_<scope>_<scope-id>`).
  - CAS-updates the task to set `metadata.goal_id = goalID`.
  - CAS-updates the goal to append `taskID` to `subgoals[].tasks[]`
    (well, `goal.tasks[]`).
  - If either update fails after retries, both are rolled back
    (best-effort — log a warning if rollback partially fails).
- `UnlinkTask(goalID, taskID) error` — inverse: clears
  `task.metadata.goal_id` and removes from `goal.tasks[]`.
- `CleanupTasks(goalID) error` — transitions all `pending` linked tasks
  to `cancelled` (per the task-management.md state machine).
- CLI:
  - `sesh-ops goal link-task <goal-id> <task-id>`
  - `sesh-ops goal unlink-task <goal-id> <task-id>`
  - `sesh-ops goal cleanup-tasks <goal-id>`
- Add `--goal-id=<id>` flag to existing `sesh-ops task create` (per the
  spec's pull-discipline section). Writes `metadata.goal_id` at create.
- Add `--with-tasks` flag to `goal clear` (forwards to CleanupTasks
  before deletion).

**Files to touch:**

- `sesh-ops/internal/goals/tasks.go` — link/unlink/cleanup helpers
- `sesh-ops/internal/goals/tasks_test.go`
- `sesh-ops/cmd/goal_link_task.go`
- `sesh-ops/cmd/goal_unlink_task.go`
- `sesh-ops/cmd/goal_cleanup_tasks.go`
- `sesh-ops/cmd/goal_clear.go` — extend with `--with-tasks`
- `sesh-ops/cmd/task_create.go` — extend with `--goal-id`
- `sesh-ops/test/integration/goal_tasks_test.go`

**Acceptance:**

- `sesh-ops task create --goal-id=<gid>` writes
  `task.metadata.goal_id = <gid>`.
- `sesh-ops goal link-task <gid> <tid>` updates both task and goal
  records consistently.
- `sesh-ops goal cleanup-tasks <gid>` cancels all pending linked tasks
  (verifiable via `sesh-ops task list --status=cancelled`).
- Pull discipline (documented, not enforced in code yet): the task-side
  filter ignores tasks where `metadata.goal_id != myGoalID` unless the
  filter is `null`. *Documented in slice 4; enforced in sesh-ops task
  list filters in a follow-on issue if needed.*

**Tests:**

- Integration: full link/unlink cycle; assert both ends consistent.
- Integration: cleanup-tasks transitions only pending tasks (not
  in_progress, not completed).
- Integration: `goal clear --with-tasks` deletes goal + cancels pending
  tasks.

**Effort:** ~2 days.

**Dependencies:** Slice 2. (Doesn't require slice 3 — independent
feature.)

---

### Slice 5: Hierarchy (parent / subgoal linkage + tree + cascade)

**Title:** `feat(goal): hierarchical composition (link-subgoal/tree/cascade)`

**Scope:**

- `LinkSubgoal(parentID, childID) error`:
  - Reads child record; CAS-updates `parent_goal_id = parentID`.
  - Reads parent record; CAS-updates `subgoals[] += childID`.
  - Best-effort rollback on partial failure (log + manual cleanup
    instructions).
- `UnlinkSubgoal(parentID, childID) error` — inverse.
- `Tree(rootID) (TreeNode, error)` — recursive walk:
  - Reads root goal; recursively reads each `subgoals[]` entry
    (potentially across scopes — sub-goals at different scopes are
    resolved by querying each scope).
  - Returns a tree structure; CLI renders as ASCII art with status +
    progress per node.
- Extend existing transitions:
  - `Clear(id, withSubgoals bool)` — if flag set, walks `subgoals[]`
    and recursively clears `pursuing` / `paused` children.
  - `Abandon(id, reason, cascadeSubgoals bool)` — already from slice 2;
    no new work here.
- Extend `Create` to accept `--parent=<id>`:
  - Validates parent exists.
  - Sets `parent_goal_id` on the new record.
  - Updates parent's `subgoals[]` (best-effort denormalization).
  - Bypasses the "one active root-goal per owner" scan.
- Extend `List` with `--root-only` (already specified in slice 1; if not
  implemented there, do it here).
- CLI:
  - `sesh-ops goal link-subgoal <parent-id> <child-id>`
  - `sesh-ops goal unlink-subgoal <parent-id> <child-id>`
  - `sesh-ops goal tree <root-id>`
  - `sesh-ops goal clear <id> --with-subgoals` (extends slice 4)

**Files to touch:**

- `sesh-ops/internal/goals/hierarchy.go` — link/unlink/tree
- `sesh-ops/internal/goals/hierarchy_test.go`
- `sesh-ops/cmd/goal_link_subgoal.go`
- `sesh-ops/cmd/goal_unlink_subgoal.go`
- `sesh-ops/cmd/goal_tree.go` — also handles ASCII rendering
- `sesh-ops/cmd/goal_create.go` — extend with `--parent`
- `sesh-ops/cmd/goal_clear.go` — extend with `--with-subgoals`
- `sesh-ops/test/integration/goal_hierarchy_test.go`

**Acceptance:**

- 3-level hierarchy creation works: hub-scope meta-goal, project-scope
  intermediate goal (parent=meta), session-scope leaf goal
  (parent=intermediate). Each record's `parent_goal_id` and parent's
  `subgoals[]` are consistent.
- `sesh-ops goal tree <meta-id>` renders the full tree with status +
  budget per node.
- `goal clear <meta-id> --with-subgoals` deletes the meta goal plus all
  `pursuing` / `paused` descendants (terminal sub-goals preserved).
- `goal abandon <meta-id> --cascade-subgoals` transitions all
  descendants to `unmet`.

**Tests:**

- Integration: 3-level hierarchy across 3 scopes; verify consistency.
- Integration: `tree` walks correctly across scopes (querying multiple
  buckets).
- Integration: cascade clear vs abandon — verify only intended children
  are affected.
- Edge: cyclic parent references rejected (a goal cannot be its own
  ancestor).

**Effort:** ~3 days.

**Dependencies:** Slices 1, 2, 4. (Builds on existing CRUD + transitions
+ task plumbing.)

---

### Slice 6: sesh spec housekeeping

**Title:** `docs(sesh): mark goal-management spec v1; add to README index`

**Scope:**

- In `sesh/README.md`: add `docs/goal-management.md` to the docs index
  alongside `coordination-patterns`, `scoped-memory`, `task-management`,
  `message-envelope`.
- Optional: add a one-line entry to a `CHANGELOG.md` if one exists, or
  a release note for the next sesh version.
- Verify the spec doc references match shipped sesh-ops behavior (after
  slices 1–5 land — this is a coordination check, not a code change).

**Files to touch:**

- `sesh/README.md`

**Acceptance:**

- `goal-management.md` is discoverable from the README index.
- Cross-links between docs work (already verified in the spec).

**Tests:** none — docs only.

**Effort:** ~half day.

**Dependencies:** Slices 1–5 (so the spec can be validated against
shipping behavior).

---

## Cross-cutting concerns

These apply across all slices and should be enforced in code review,
not as separate issues.

### Testing discipline

- **Integration tests use a real `sesh up` hub.** No mocks for NATS KV.
  Per the operator's standing feedback (don't mock substrate
  interactions), integration tests spin up a sesh hub in a temp dir,
  run the CLI commands, assert KV state via `nats kv get` directly.
- **Unit tests cover pure functions** (bucket name sanitization, schema
  validation, state machine transitions in isolation).
- **CI** runs both via `make test` (matches sesh-ops convention if any
  exists; otherwise establish it in slice 0).

### Error handling

- CAS failures → re-read, retry up to 3 times with jittered backoff,
  then surface as a clear `CASConflictError` for callers.
- EEXIST on `Create` → return `ErrAlreadyExists`; CLI prints a useful
  message including the conflicting ID.
- Missing bucket → either auto-create (for KV-side buckets that don't
  exist yet) or surface clearly. Match the sesh-ops convention from
  task ops.
- Validation errors → return structured errors with field names; CLI
  prints them as `field=value: reason`.

### Observability

- Structured logs (zerolog or slog) from sesh-ops for every state
  transition and CAS write.
- KV change events are automatic via NATS; no extra publishing needed.
- Optional lifecycle subjects (per spec) — skip in v1, add as a v2
  follow-on if a watcher needs subject-filtered streams.

### Documentation

- Each CLI command supports `--help` with a clear synopsis.
- Examples in help text reference the spec's example flow.
- After all slices land, generate a `sesh-ops/docs/goal-cli.md`
  consolidating command reference (optional but recommended).

---

## Slices that touch EdgeSync

**None in v1.** Goals use NATS KV; cross-network reach already works
via EdgeSync's iroh transport in single-machine hub deployments.

Following candidates are flagged in the spec for v2+ — they MIGHT touch
EdgeSync, but only if/when a concrete use case appears. File these as
deferred-roadmap issues, not v1 blockers:

### Deferred EdgeSync candidate A: cross-machine NATS clustering recipe

**Title:** `docs(edgesync): recipe for cross-machine NATS JetStream clustering`

**Why it's deferred:** Hub scope is per-machine. Two machines on iroh
leaf each have their own hub with separate JetStream KV stores. Truly
cross-machine global goals require NATS clustering with a shared
JetStream domain or a designated upstream hub that other machines leaf
into. The spec marks this out of scope for v1.

**If we ever ship this:**

- Likely a docs-only contribution to EdgeSync: a setup recipe for
  multi-machine NATS clusters with replicated JetStream.
- May not require code changes to EdgeSync — depends on whether NATS's
  built-in clustering is enough or whether EdgeSync needs to
  orchestrate cluster setup.
- Scope: low effort if pure docs; medium effort if EdgeSync needs to
  ship cluster-aware helpers.

**Trigger to revisit:** an operator describes a real cross-machine
"global meta-goal" use case where per-machine hub scope is insufficient.

### Deferred EdgeSync candidate B: per-role NATS user templates

**Title:** `feat(edgesync): per-role NATS user templates for substrate-side ACLs`

**Why it's deferred:** The asymmetric tool surface (model can only call
`update_goal(complete)`) is enforced at the harness's tool registration
layer. For stronger isolation in multi-tenant setups, per-role NATS
users with scoped subject permissions could enforce the asymmetry at
the wire layer.

**If we ever ship this:**

- EdgeSync would expose a `--role` or `--user-config` knob on leaf
  serve commands.
- Sesh-ops would provide `sesh-ops goal grant-role <role> <permissions>`
  for managing role-specific NATS users.
- Modest code in EdgeSync; modest CLI work in sesh-ops.

**Trigger to revisit:** a multi-tenant deployment or a security audit
flags the convention-only enforcement as insufficient.

---

## Slices that touch libfossil

**None.** Goals live exclusively in NATS KV. No Fossil interaction in
v1 or in any planned v2 candidate.

---

## v2 candidates within sesh-ops

These are flagged in the spec but not v1:

1. **First-class `goal_id` on tasks** — currently in
   `task.metadata.goal_id`. v2 bumps the task schema to `v=2` and
   promotes the field. Requires coordinated update of task-management.md
   spec, sesh-ops task ops, and any task consumers (e.g. orch). File as
   `feat(task): schema v2 with first-class goal_id` when v1 has shipped
   and consumers are ready.
2. **MCP server for the three model tools** — a standalone
   `sesh-goals-mcp` exposing `get_goal` / `create_goal` /
   `update_goal(complete)` for any MCP-capable harness. Could live in a
   new sister repo. File when at least one harness wants to consume
   goals via MCP (orch is the natural first consumer).
3. **First-class cascade-policy declaration** — add a `cascade` field
   to the goal record so watchers can read the desired cascade behavior
   instead of inferring from operator commands. File when at least one
   operator wires a non-default cascade policy and wants persistence.
4. **Indexed "current goal per owner" KV** — speeds up the
   "one active root-goal per owner" scan from O(N) to O(1). File when
   bucket sizes get large enough to make the scan noticeable (likely
   never for personal use).

---

## Issue conversion

When ready, run `/to-issues` against this file. Suggested split:

- Slices 0–5 → file at [github.com/danmestas/sesh-ops/issues](https://github.com/danmestas/sesh-ops/issues)
- Slice 6 → file at [github.com/danmestas/sesh/issues](https://github.com/danmestas/sesh/issues)
- Deferred EdgeSync candidates A & B → file at
  [github.com/danmestas/EdgeSync/issues](https://github.com/danmestas/EdgeSync/issues)
  with label `deferred-v2` or similar
- v2 candidates 1–4 → file at sesh-ops (or sesh, for spec changes) with
  label `roadmap-v2`

Recommended issue ordering for sequential implementation:

1. Slice 0 (if needed)
2. Slice 1 (CRUD — unblocks everything)
3. Slice 2 (transitions)
4. Slice 3 (accounting + sweeper) AND Slice 4 (task linkage) — can land
   in parallel
5. Slice 5 (hierarchy)
6. Slice 6 (sesh housekeeping)

Total estimated effort: **~2 weeks** of focused work for one engineer
across slices 0–5, plus a half-day for slice 6. Parallel work on slices
3 + 4 cuts wall-clock to ~10 days.

---

## Open questions to resolve before slice 1 lands

These don't block planning but should be answered before code starts.

1. **CLI framework**: sesh-ops uses what — cobra, urfave/cli, custom?
   Slice 0 (or slice 1 if skipped) should align with the existing
   choice.
2. **NATS client library**: which Go client — `nats.go` (canonical) or
   something else? Probably canonical; confirm.
3. **Test infrastructure**: does sesh-ops have a `make test-integration`
   target that spins up a sesh hub already? If yes, reuse. If no,
   slice 0 should establish one.
4. **Logging**: zerolog, slog, zap? Match sesh-ops convention.
5. **Output format**: JSON, plain text, table? `sesh-ops task` ops
   already have a convention — match it (probably plain text by
   default, `--json` flag for structured output).

If any of these need decision before issue filing, ask the operator;
otherwise default to "match existing sesh-ops style."

---

## Further reading

- [`docs/goal-management.md`](../docs/goal-management.md) — v1 spec.
- [`docs/task-management.md`](../docs/task-management.md) — task
  protocol goals compose with; same CAS patterns reused.
- [`docs/scoped-memory.md`](../docs/scoped-memory.md) — bucket naming
  and connection routing.
- [`docs/coordination-patterns.md`](../docs/coordination-patterns.md) —
  cross-references the patterns goals enable (notably orchestrator–
  subagent and hierarchical).
- [github.com/danmestas/sesh-ops](https://github.com/danmestas/sesh-ops) —
  the repo where slices 0–5 land.
