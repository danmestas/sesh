# Goal management on sesh

A goal is a structured value in a scoped-memory KV bucket that captures
a **persistent objective** an agent (or a fleet of agents) is pursuing
across many turns, tasks, and conversation boundaries. Goals are the
long-horizon companion to [tasks](./task-management.md): tasks are
atomic units of work in a queue; a goal is the durable intent that may
spawn dozens of tasks toward a single outcome.

The whole protocol is convention plus JetStream KV's atomic operations,
exactly like task management. See [scoped-memory.md](./scoped-memory.md)
for the bucket model this builds on, [task-management.md](./task-management.md)
for the task protocol goals compose with, and
[message-envelope.md](./message-envelope.md) for the trace-id binding.

Operator-facing commands live in [`sesh-ops`](https://github.com/danmestas/sesh-ops);
raw `nats kv` operations are sufficient to participate.

## When to use a goal vs a task

| Use a **goal** when… | Use a **task** when… |
| --- | --- |
| The objective spans many turns or many tasks | The work is one atomic unit |
| You want autonomous agentic pursuit across context boundaries | You want a queue of work for a pool of workers |
| Completion is judged (model audits or operator confirms) | Completion is a fact (the worker finished or didn't) |
| You need a token / wall-clock budget for the whole effort | You need a per-attempt deadline that retries on lapse |
| One owner is responsible across cold starts | Many workers may claim and complete |

Goals and tasks **compose**: a goal owns a set of tasks. The goal
persists; tasks come and go as the goal owner decomposes work.

## Where goals live

Goals are stored in a KV bucket named:

```
sesh_goals_<scope>_<scope-id>
```

Following the [scoped-memory](./scoped-memory.md) convention
(underscore separator; dots and hyphens in scope-ids sanitized to
underscore because NATS KV bucket names disallow them).

**Default scope is `project`** for long-running agentic work — goals
typically outlive any single trace and benefit from project-level
durability (no TTL). Workflow scope is valid for short-horizon goals
bound to one trace's lifetime (24h TTL). Hub / session / agent scopes
are unusual and discouraged for goals.

| Scope | Bucket | Use when… |
| --- | --- | --- |
| `project` | `sesh_goals_project_<project>` | Default — long-running agentic objectives that may span days |
| `workflow` | `sesh_goals_workflow_<trace-id-8hex>` | Goal lifetime equals one trace's lifetime |

Each goal's KV key is its ID (a ULID). The KV value is the record
described below.

Connection target follows the [scoped-memory routing rule](./scoped-memory.md#connection-target--which-nats-to-talk-to):
goals in project or workflow scope live on the hub's NATS URL
(`~/.sesh/hub.nats.url`).

## Record schema (v1)

```json
{
  "id": "01HXX...",
  "v": 1,
  "objective": "Migrate the auth service to OAuth2",
  "status": "pursuing",
  "owner": "orchestrator:agent-001",
  "token_budget": 200000,
  "used_tokens": 0,
  "wall_clock_budget_sec": null,
  "started_at": "2026-05-13T10:00:00Z",
  "updated_at": "2026-05-13T10:00:00Z",
  "completed_at": null,
  "trace_id": null,
  "tasks": [],
  "checkpoints": [],
  "result": null,
  "metadata": {}
}
```

Field meanings:

| Field                    | Type      | Purpose                                                              |
| ------------------------ | --------- | -------------------------------------------------------------------- |
| `id`                     | ULID      | Stable identifier; also the KV key                                   |
| `v`                      | int       | Schema version (currently 1)                                         |
| `objective`              | string    | Human-readable statement of what the goal is                         |
| `status`                 | enum      | See state machine below                                              |
| `owner`                  | string    | `role:agent-id` of the agent pursuing this goal                      |
| `token_budget`           | int?      | Maximum tokens the goal may consume across all turns; null = no cap  |
| `used_tokens`            | int       | Running total of tokens spent; CAS-updated each turn                 |
| `wall_clock_budget_sec`  | int?      | Maximum wall-clock seconds since `started_at`; null = no cap         |
| `started_at`             | ISO8601   | When the goal entered `pursuing`                                     |
| `updated_at`             | ISO8601   | Last modification time                                               |
| `completed_at`           | ISO8601?  | When the goal entered a terminal state                               |
| `trace_id`               | string?   | W3C trace-id this goal is bound to, if any (workflow-scope goals)    |
| `tasks`                  | string[]  | Best-effort denormalized list of task IDs linked to this goal        |
| `checkpoints`            | object[]  | Optional progress markers; free-form per harness                     |
| `result`                 | object?   | Populated in terminal states (`achieved` payload or abandon reason)  |
| `metadata`               | object    | Free-form agent-specific data                                        |

The `tasks[]` array is **best-effort denormalized**. The authoritative
linkage is each task's `metadata.goal_id` (see [Tasks linked to a goal](#tasks-linked-to-a-goal)
below). If they diverge, trust the tasks.

## State machine

```
                                       ┌────── resume ────┐
                                       ▼                  │
   (create) ──▶ pursuing ──pause──▶ paused                  │
                  │ ▲                                       │
                  │ └────────── resume ───────────────────┘
                  │
                  ├── complete ──▶ achieved      (terminal)
                  │
                  ├── exceed-budget ──▶ budget_limited  (terminal; system sets)
                  │
                  ├── abandon ──▶ unmet           (terminal; operator sets)
                  │
                  └── clear ──▶ (record deleted)
```

Terminal states: `achieved`, `unmet`, `budget_limited`. Non-terminal:
`pursuing`, `paused`.

State semantics:

| Status            | Meaning                                                            |
| ----------------- | ------------------------------------------------------------------ |
| `pursuing`        | Active; the owner is making progress (or about to) on this turn    |
| `paused`          | Active but suspended; will not auto-continue until resumed         |
| `achieved`        | Owner audited completion and confirmed the objective is met        |
| `unmet`           | Operator abandoned the goal without completion                     |
| `budget_limited`  | `used_tokens` >= `token_budget` (or wall-clock exhausted)          |

## Lifecycle protocol

### 1. Create (idempotent)

The owner creates a goal at the start of a long-horizon pursuit.
Convention: **one `pursuing` goal per owner at a time** — before
creating, scan the bucket and confirm no other record with
`owner == self && status == "pursuing"` exists.

```sh
hub_url=$(cat ~/.sesh/hub.nats.url)
project_id=$(basename "$(pwd)" | tr .- _)
bucket="sesh_goals_project_${project_id}"

goal_id=$(ulid)
nats --server "$hub_url" kv create "$bucket" "$goal_id" "$(jq -n \
  --arg id "$goal_id" \
  --arg obj "Migrate the auth service to OAuth2" \
  --arg owner "orchestrator:agent-001" '
  {id:$id, v:1, objective:$obj, status:"pursuing", owner:$owner,
   token_budget:200000, used_tokens:0, wall_clock_budget_sec:null,
   started_at:now|todateiso8601, updated_at:now|todateiso8601,
   completed_at:null, trace_id:null,
   tasks:[], checkpoints:[], result:null, metadata:{}}')"
```

`kv create` (not `put`) fails with EEXIST if the goal-id collides —
desirable for deterministic IDs derived from `hash(owner + objective)`.

### 2. Pause (operator / runtime only)

The model is forbidden from pausing its own goal (asymmetric tool
surface; see below). Operators or the runtime call:

```sh
record=$(nats --server "$hub_url" kv get "$bucket" "$goal_id" --raw)
rev=$(nats   --server "$hub_url" kv revision "$bucket" "$goal_id")
new=$(jq --arg now "$(date -u +%FT%TZ)" \
       '.status="paused" | .updated_at=$now' <<<"$record")
nats --server "$hub_url" kv put "$bucket" "$goal_id" "$new" --revision="$rev"
```

Common pause triggers: user interrupted the conversation, runtime
detected unexpected state, operator wants to inspect intermediate
results before resumption.

### 3. Resume (operator / runtime only)

```sh
record=$(nats --server "$hub_url" kv get "$bucket" "$goal_id" --raw)
rev=$(nats   --server "$hub_url" kv revision "$bucket" "$goal_id")
new=$(jq --arg now "$(date -u +%FT%TZ)" \
       '.status="pursuing" | .updated_at=$now' <<<"$record")
nats --server "$hub_url" kv put "$bucket" "$goal_id" "$new" --revision="$rev"
```

### 4. Complete (model-callable; the only model-side transition)

The model calls this when its completion audit concludes the objective
is met. Per the asymmetric tool surface, this is the **only** status
transition the model can perform.

```sh
record=$(nats --server "$hub_url" kv get "$bucket" "$goal_id" --raw)
rev=$(nats   --server "$hub_url" kv revision "$bucket" "$goal_id")
new=$(jq --arg now "$(date -u +%FT%TZ)" \
        --argjson r '{"output":"Auth service now uses OAuth2; tests green."}' '
       .status="achieved"
       | .completed_at=$now
       | .updated_at=$now
       | .result=$r' <<<"$record")
nats --server "$hub_url" kv put "$bucket" "$goal_id" "$new" --revision="$rev"
```

The substrate **does not enforce** that completion was actually
warranted — that's the harness's job via the continuation prompt
("perform a completion audit before declaring achieved"). Layered
multi-agent verification (generator-verifier pattern from
[coordination-patterns.md](./coordination-patterns.md)) is available
for goals that need stronger guarantees.

### 5. Abandon (operator only)

When the operator decides the goal is no longer worth pursuing:

```sh
new=$(jq --arg now "$(date -u +%FT%TZ)" \
        --argjson r '{"reason":"requirements changed; deferred to next quarter"}' '
       .status="unmet" | .completed_at=$now | .updated_at=$now | .result=$r' <<<"$record")
nats --server "$hub_url" kv put "$bucket" "$goal_id" "$new" --revision="$rev"
```

### 6. Clear (operator only)

Hard-delete the record:

```sh
nats --server "$hub_url" kv del "$bucket" "$goal_id"
```

Clearing terminates the goal without preserving history. Prefer
`abandon` if you want a paper trail.

## Asymmetric tool surface (convention)

The substrate stores records; agents can technically write any
transition. The asymmetry is enforced at the **harness's tool
registration layer**, mirroring Codex's design:

| Operation | Model-side tool | Operator / runtime |
| --- | --- | --- |
| `get_goal` | yes (read-only) | yes |
| `create_goal` | yes | yes |
| `update_goal(status="complete")` | yes (this transition only) | yes |
| `pause` / `resume` | **no** | yes |
| `abandon` / `clear` | **no** | yes |
| `account_tokens` (CAS counter) | **no** | yes (runtime) |

The harness registers only the allowed three operations as
model-facing tools. Operator and runtime go through `sesh-ops` or raw
KV. This is convention, not substrate-enforced — substrate-side ACLs
(per-role NATS users with scoped subject permissions) are a deferred
hardening.

## Token accounting and budget enforcement

After every model turn, the harness reports the tokens consumed and
the runtime updates `used_tokens` via CAS:

```sh
record=$(nats --server "$hub_url" kv get "$bucket" "$goal_id" --raw)
rev=$(nats   --server "$hub_url" kv revision "$bucket" "$goal_id")
new=$(jq --arg n "$tokens_this_turn" --arg now "$(date -u +%FT%TZ)" '
       .used_tokens += ($n|tonumber)
       | .updated_at=$now' <<<"$record")
nats --server "$hub_url" kv put "$bucket" "$goal_id" "$new" --revision="$rev"
```

On CAS failure, re-read and retry — the counter is monotonic and
contention is rare (the goal owner is the typical writer).

After each write, the runtime checks:

```
if used_tokens >= token_budget:
    transition status to "budget_limited"
elif wall_clock_budget_sec set and (now - started_at) > budget:
    transition status to "budget_limited"
elif used_tokens >= 0.75 * token_budget:
    publish budget_warning event (optional)
```

The transition to `budget_limited` is system-side and terminates the
continuation loop in the harness. Operators can `abandon` or extend the
budget by transitioning back to `pursuing` after raising
`token_budget`.

## Tasks linked to a goal

To prevent task-pool clashing (one goal's owner pulling tasks meant
for another goal), tasks carry a `metadata.goal_id` field that links
them to their parent goal.

### Tagging a task with a goal

When the goal owner enqueues a task to be worked toward this goal,
include `goal_id` in the task's metadata:

```sh
task_bucket="sesh_tasks_workflow_${trace_short}"
task_id=$(ulid)
nats --server "$hub_url" kv create "$task_bucket" "$task_id" "$(jq -n \
  --arg id "$task_id" --arg title "Run OAuth2 integration tests" \
  --arg goal_id "$goal_id" '
  {id:$id, v:1, title:$title, status:"pending",
   puller:null, pulled_at:null, due_at:null,
   depends_on:[], priority:5, attempts:0, max_attempts:3,
   created_at:now|todateiso8601, created_by:"orchestrator:agent-001",
   updated_at:now|todateiso8601, result:null,
   metadata:{goal_id:$goal_id}}')"
```

Concurrently, append the task ID to the goal's `tasks[]` (best-effort
denormalization):

```sh
record=$(nats --server "$hub_url" kv get "$bucket" "$goal_id" --raw)
rev=$(nats   --server "$hub_url" kv revision "$bucket" "$goal_id")
new=$(jq --arg t "$task_id" '.tasks += [$t]' <<<"$record")
nats --server "$hub_url" kv put "$bucket" "$goal_id" "$new" --revision="$rev"
```

### Pull discipline (puller side)

When choosing a pullable task, agents filter by goal context:

| Agent context | Pulls tasks where… |
| --- | --- |
| Pursuing goal `G` | `task.metadata.goal_id == G` OR `task.metadata.goal_id == null` |
| Not pursuing any goal | `task.metadata.goal_id == null` |
| Goal-agnostic worker pool (explicitly opted in) | Any task, regardless of `goal_id` |

This is the convention that prevents goal-A's worker from poaching
goal-B's task. A "free pool" task (no `goal_id`) is shared by all
goals.

```sh
# Filter when scanning task bucket
nats --server "$hub_url" kv ls-keys "$task_bucket" | while read k; do
  rec=$(nats --server "$hub_url" kv get "$task_bucket" "$k" --raw)
  task_goal=$(jq -r '.metadata.goal_id // ""' <<<"$rec")
  status=$(jq    -r .status <<<"$rec")
  if [[ "$status" == "pending" ]] && \
     [[ -z "$task_goal" || "$task_goal" == "$my_goal_id" ]]; then
    # candidate — proceed with depends_on check + CAS claim
    :
  fi
done
```

### Cleanup on goal termination

When a goal enters a terminal state (`achieved` / `unmet` /
`budget_limited`), its linked tasks do **not** automatically cascade.
Operators decide:

- **Most common**: leave tasks; if they're still pending and another
  goal can use them, that goal's owner can re-tag (`metadata.goal_id =
  new_goal_id`) and claim them.
- **Cancel pending tasks**: operator runs `sesh-ops goal cleanup-tasks
  <goal-id>` which transitions all `pending` linked tasks to
  `cancelled`.
- **Hard delete**: operator runs `sesh-ops goal clear <goal-id>
  --with-tasks` which deletes the goal and its `pending` / `in_progress`
  tasks.

The substrate defaults to leaving the tasks in place — explicit
opt-in to cascading.

## Watching for changes

NATS KV publishes change events. Subscribe to react in real time:

```sh
nats --server "$hub_url" kv watch "$bucket"
nats --server "$hub_url" kv watch "$bucket" "$goal_id"     # one record
```

Common watcher patterns:

- **Orchestrator UI** watches the bucket; renders goal state in real
  time so the operator sees progress without polling.
- **Budget enforcer** watches; when `used_tokens >= 0.75 *
  token_budget`, sends a warning to the operator.
- **Audit logger** watches; appends every state transition to a
  durable log (JetStream stream, structured log).

### Optional lifecycle subjects

For watchers that want filtered subjects rather than tailing the whole
bucket, the harness may publish narrower lifecycle events:

```
sesh.goals.<scope>.<scope-id>.<goal-id>.created
sesh.goals.<scope>.<scope-id>.<goal-id>.status_changed
sesh.goals.<scope>.<scope-id>.<goal-id>.budget_warning
sesh.goals.<scope>.<scope-id>.<goal-id>.completed
sesh.goals.<scope>.<scope-id>.<goal-id>.cleared
```

These are convention, not required. KV watch is sufficient for most
cases.

## Sweeper

A long-running loop scans for goals whose budget has elapsed and
transitions them to `budget_limited`. `sesh-ops` provides one;
operators can also run their own.

```sh
# Every 30s
for goal_id in $(nats kv ls-keys $bucket); do
  rec=$(nats kv get $bucket $goal_id --raw)
  status=$(jq -r .status <<<"$rec")
  used=$(jq   -r .used_tokens <<<"$rec")
  budget=$(jq -r .token_budget <<<"$rec")
  wcb=$(jq    -r .wall_clock_budget_sec <<<"$rec")
  started=$(jq -r .started_at <<<"$rec")

  if [[ $status == "pursuing" ]]; then
    if [[ "$budget" != "null" && $used -ge $budget ]] || \
       [[ "$wcb" != "null" && $(now) -gt $((started_epoch + wcb)) ]]; then
      new=$(jq '.status="budget_limited" | .completed_at=now|todateiso8601 | .updated_at=now|todateiso8601' <<<"$rec")
      nats kv put $bucket $goal_id "$new" --revision=$rev    # CAS-protected
    fi
  fi
done
```

Multiple sweepers are safe — CAS ensures only one succeeds per goal.

## Idempotency

Goal creation is idempotent on `id`:

```sh
nats kv create $bucket $goal_id "$record"
# create fails with EEXIST if the key exists; the existing record wins
```

For deterministic resumption (operator wants to resume the same goal
across restarts), generate IDs from `hash(owner + objective + start_date)`
or similar — naturally deduplicates.

## What sesh does NOT provide

To keep the substrate clean and harness-agnostic, sesh explicitly does
**not** ship the following — they belong in each harness (orch, codex
CLI, claude-code, custom agent runtime) that drives a model:

- **The continuation engine.** Hooks into turn lifecycle (TurnStarted /
  ToolCompleted / TurnFinished / TaskAborted / ThreadResumed), decides
  whether to re-call the model, injects the continuation prompt. Sesh
  has no concept of "turn" or "model invocation".
- **The `continuation.md` prompt content.** Opinionated agent-behavior
  policy ("perform a completion audit before declaring achieved",
  "choose the next concrete action"). May vary per harness or per
  model. Harness owns this.
- **Token measurement.** Each model API counts tokens differently. The
  harness reads from its model SDK and reports the integer to the
  sesh CAS counter.
- **Interrupt detection.** "User typed control text → pause" is UX
  policy. Harness-side.
- **Model tool registration.** The asymmetric tool surface (model can
  only call `update_goal(complete)`) is enforced by each harness's
  tool registration. Sesh defines the convention; harnesses enforce.

Reference continuation prompts and continuation-engine recipes will
appear in each harness's own docs (e.g. orch's `multi-executor-workers.md`
specifies how its executors wire the loop).

## Operator interface — sesh-ops

Operator and system commands wrap the raw protocol. `sesh-ops` provides:

```
sesh-ops goal create --objective "..." [--budget=N] [--wall-clock=Ns] [--scope=project|workflow]
sesh-ops goal get [--id=<goal-id>] [--owner=<role:id>]
sesh-ops goal list [--status=pursuing|paused|achieved|unmet|budget_limited] [--owner=<role:id>]
sesh-ops goal status <goal-id>
sesh-ops goal pause <goal-id>
sesh-ops goal resume <goal-id>
sesh-ops goal complete <goal-id> [--result='{...}']     # operator-confirmed completion
sesh-ops goal abandon <goal-id> --reason "..."
sesh-ops goal clear <goal-id> [--with-tasks]
sesh-ops goal account <goal-id> <tokens>                # system-only; usually runtime-driven
sesh-ops goal link-task <goal-id> <task-id>             # tag task + update goal.tasks[]
sesh-ops goal unlink-task <goal-id> <task-id>
sesh-ops goal cleanup-tasks <goal-id>                   # cancel pending linked tasks
sesh-ops goal sweep [--budget-warning-threshold=0.75]   # one-shot budget enforcement pass
sesh-ops goal sweep --daemon                            # long-running sweeper loop
```

Some of these are operator-facing (`create`, `pause`, `resume`,
`abandon`, `clear`). Others are system-facing and called by the
harness's continuation runtime (`account`, `sweep`). The CLI is the
canonical place for the asymmetric-tool-surface enforcement: operator
flags expose all transitions; the harness's model-facing tool wrappers
expose only `update_goal(complete)`.

## One active per owner

To avoid multi-active-goal confusion, sesh enforces by convention:
**at most one `pursuing` goal per `owner` at a time**.

The harness's `create_goal` tool implementation MUST scan the bucket
for existing pursuing goals owned by the caller and refuse creation
(or transition the existing one) if found. `sesh-ops goal create` does
this scan by default.

Operators can override via `sesh-ops goal create --allow-multiple` if a
genuine multi-pursuit use case exists.

## Example flow: long-running migration

A migration owner spins up a goal, decomposes into tasks, the model
pursues across many turns with budget accounting, and audits
completion.

```sh
hub_url=$(cat ~/.sesh/hub.nats.url)
project_id=$(basename "$(pwd)" | tr .- _)

# 1. Operator creates the goal
sesh-ops goal create \
  --objective "Migrate auth service from JWT to OAuth2" \
  --budget=500000 \
  --owner="orchestrator:agent-001"
# → returns goal_id=01HXX...

# 2. Goal owner decomposes into tasks
for step in research design implement test deploy; do
  sesh-ops task create --bucket=workflow \
    --title="auth-oauth2: $step" \
    --goal-id=01HXX...
done

# 3. Workers pursuing goal 01HXX claim tasks; agents not pursuing any
#    goal don't see these (filtered by metadata.goal_id).

# 4. Each turn, the harness's continuation engine:
#    - reads goal state via get_goal()
#    - injects continuation.md
#    - re-calls the model
#    - reports used_tokens via sesh-ops goal account

# 5. Model decides the objective is achieved:
#    - performs the completion audit per continuation.md
#    - calls update_goal({status: "complete"})
#    - CAS write flips status to "achieved", populates result

# 6. The harness's continuation engine sees status=achieved on the next
#    poll, stops the loop, surfaces the result to the operator.

sesh-ops goal status 01HXX...
# objective:  Migrate auth service from JWT to OAuth2
# status:     achieved
# owner:      orchestrator:agent-001
# tokens:     387412 / 500000 (77%)
# wall-clock: 4h 23m
# tasks:      5 linked (5 completed, 0 cancelled)
# result:     {"output":"OAuth2 deployed; JWT path deprecated"}
```

## Edge cases

- **Owner crash mid-pursuit.** The goal record persists; status stays
  `pursuing`. On reconnect, the owner reads its goal, the runtime
  reattaches the continuation engine. No sweeper kicks `pursuing` goals
  back (unlike tasks) because the goal has no per-turn deadline — only
  budget bounds.
- **Operator wants to extend budget mid-pursuit.** Pause, raise
  `token_budget`, resume. Sweeper will not re-trigger as long as
  `used_tokens < token_budget`.
- **Two owners trying to create the same objective.** Use deterministic
  IDs (`hash(objective + project)`) plus `kv create` — second creator
  fails with EEXIST and reads the existing record instead.
- **Goal pursued from a cf-worker that scales to zero.** The KV record
  is durable; the continuation engine inside the Worker reads goal
  state on each cold start and continues. Token accounting via CAS
  handles multiple worker instances if the runtime parallelizes.
- **Goal that wants multi-agent verification.** Layer the
  generator-verifier pattern from
  [coordination-patterns.md](./coordination-patterns.md) on top: the
  pursuing agent submits a completion claim, a verifier agent audits,
  only the verifier transitions to `achieved`.

## Schema migration

Bump the `v` field on new fields. Readers tolerate higher versions
(don't write back unknown fields); writers refuse to downgrade. Same
pattern as task and envelope specs.

A planned v2 candidate: making `goal_id` a first-class field on tasks
(currently in `metadata.goal_id`). Until then, treat the metadata
convention as the contract.

## Further reading

- [Scoped memory](./scoped-memory.md) — where goals live; bucket
  naming and connection routing.
- [Task management](./task-management.md) — the task protocol goals
  compose with; CAS pattern goals reuse.
- [Coordination patterns](./coordination-patterns.md) — multi-agent
  patterns goals plug into (Orchestrator–subagent for goal owners,
  generator-verifier for audited completion).
- [Message envelope](./message-envelope.md) — trace-id binding for
  workflow-scoped goals.
- [`sesh-ops`](https://github.com/danmestas/sesh-ops) — reference CLI
  for the goal commands above.
- [NATS KV watchers](https://docs.nats.io/nats-concepts/jetstream/key-value-store/kv_walkthrough#watching-for-changes)
