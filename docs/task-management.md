# Task management on sesh

A task record is a structured value in a scoped-memory KV bucket. This
document specifies the schema, state machine, and **pull protocol** that
agents follow to coordinate work without inventing their own task
schemas (which subtly disagree and don't interop).

The whole protocol is convention plus JetStream KV's atomic operations.
See [scoped-memory.md](./scoped-memory.md) for the bucket model this
builds on, and [message-envelope.md](./message-envelope.md) for the
trace-id binding that ties workflow tasks to a single trace.

A reference CLI implementation lives in
[`sesh-ops`](https://github.com/danmestas/sesh-ops); raw `nats kv`
operations are sufficient to participate.

## Where tasks live

Tasks are stored in a KV bucket named:

```
sesh_tasks_<scope>_<scope-id>
```

Following the [scoped-memory](./scoped-memory.md) convention
(underscore separator; dots and hyphens in scope-ids sanitized to
underscore because NATS KV bucket names disallow them). Most common
scope is workflow (`sesh_tasks_workflow_4bf92f35`) — tasks associated
with one trace. Project or session scope is fine for longer-lived
work plans.

Each task's KV key is its ID (a ULID). The KV value is the task record
described below.

## Record schema (v1)

```json
{
  "id": "01HXX...",                          
  "v": 1,                                    
  "title": "Migrate auth service",
  "description": "Optional longer text",
  "status": "pending",                       
  "puller": null,                            
  "pulled_at": null,                         
  "due_at": null,                            
  "depends_on": [],                          
  "priority": 0,                             
  "attempts": 0,                             
  "max_attempts": 3,                         
  "created_at": "2026-05-11T18:00:00Z",
  "created_by": "orchestrator:agent-001",
  "updated_at": "2026-05-11T18:00:00Z",
  "result": null,                            
  "metadata": {}                             
}
```

Field meanings:

| Field           | Type     | Purpose                                                            |
| --------------- | -------- | ------------------------------------------------------------------ |
| `id`            | ULID     | Stable identifier; also the KV key                                 |
| `v`             | int      | Schema version (currently 1)                                       |
| `title`         | string   | Short human-readable name                                          |
| `description`   | string   | Optional longer context                                            |
| `status`        | enum     | See state machine below                                            |
| `puller`        | string?  | `role:agent-id` of the current puller, or null                     |
| `pulled_at`     | ISO8601? | When the current pull started                                      |
| `due_at`        | ISO8601? | When the current pull expires unless extended                      |
| `depends_on`    | string[] | Task IDs that must be `completed` before this one can be pulled    |
| `priority`      | int      | Higher integers pulled first                                       |
| `attempts`      | int      | Incremented on each pull                                           |
| `max_attempts`  | int      | After this many failed pulls, status sticks at `failed`            |
| `created_at`    | ISO8601  | When the task was created                                          |
| `created_by`    | string   | `role:agent-id` of the creator                                     |
| `updated_at`    | ISO8601  | Last modification time                                             |
| `result`        | object?  | Populated on `completed` (success payload) or `failed` (reason)    |
| `metadata`      | object   | Free-form agent-specific data                                      |

## State machine

```
                       ┌──────────────────────┐
                       ▼                      │
   pending ──pull──▶ in_progress ──complete──▶│  completed
                       │                      │
                       ├──fail (attempts < max)──▶ pending
                       │
                       ├──fail (attempts >= max)──▶ failed
                       │
                       ├──block──▶ blocked ──unblock──▶ in_progress
                       │
                       └──due_at lapses (no extension)──▶ pending  (sweeper)

   any non-terminal ──cancel──▶ cancelled
```

Terminal states: `completed`, `failed`, `cancelled`. Non-terminal:
`pending`, `in_progress`, `blocked`.

## Pull protocol

Agents pull tasks from the queue, work on them, and either complete or
fail. While working, the agent extends `due_at` periodically to keep the
task from being kicked back to `pending` by the sweeper.

### 1. Choose a pullable task

A task is pullable when:

- `status == "pending"`
- Every task in `depends_on` is `completed`
- `attempts < max_attempts`

Select the candidate with highest `priority`, oldest `created_at`
breaking ties.

### 2. Pull (atomic claim)

CAS update — only succeeds if the task is still `pending` at the
revision the puller read:

```sh
# Pseudocode
new=$(jq '.status="in_progress" 
        | .puller="researcher:agent-042" 
        | .pulled_at="'$(date -u +%FT%TZ)'"
        | .due_at="'$(date -u -v+30S +%FT%TZ)'"
        | .attempts+=1
        | .updated_at="'$(date -u +%FT%TZ)'"' <<<"$record")

nats kv put $bucket $task_id "$new" --revision=$prev_rev
```

If the CAS fails (revision moved), another agent pulled the task — pick
the next candidate. No retries on CAS failure; just move on.

### 3. Extend (keep the deadline ahead)

While working, push `due_at` forward periodically. Typical pattern:
extend every 10s with a 30s deadline, so two missed extensions = lapse.

```sh
new=$(jq '.due_at="'$(date -u -v+30S +%FT%TZ)'"
        | .updated_at="'$(date -u +%FT%TZ)'"' <<<"$current")
nats kv put $bucket $task_id "$new" --revision=$rev
```

A CAS failure during extension means another writer modified the task —
most likely the sweeper kicked it back to `pending`. The puller's claim
is no longer valid; stop working and discard any partial result.

### 4. Complete or fail

```sh
# On success
new=$(jq '.status="completed"
        | .result={"output":"..."}
        | .due_at=null
        | .updated_at="'$(date -u +%FT%TZ)'"' <<<"$record")

# On failure — sticks at "failed" if out of attempts, otherwise back to "pending"
new=$(jq 'if .attempts >= .max_attempts then .status="failed" else .status="pending" end
        | .puller=null | .pulled_at=null | .due_at=null
        | .result={"error":"timed out"}
        | .updated_at="'$(date -u +%FT%TZ)'"' <<<"$record")

nats kv put $bucket $task_id "$new" --revision=$rev
```

## Sweeper

A long-running loop (an orchestrator can run one as a background
goroutine, or `sesh-ops` provides one) periodically scans for
`in_progress` tasks whose `due_at` has lapsed and resets them:

```sh
# Every 10s
for key in $(nats kv ls-keys $bucket); do
  record=$(nats kv get $bucket $key --raw)
  status=$(jq -r .status <<<"$record")
  due=$(jq -r .due_at <<<"$record")
  if [[ $status == "in_progress" && $due < $(date -u +%FT%TZ) ]]; then
    new=$(jq '.status="pending" | .puller=null | .pulled_at=null | .due_at=null
            | .updated_at="'$(date -u +%FT%TZ)'"' <<<"$record")
    nats kv put $bucket $key "$new" --revision=$rev    # CAS-protected
  fi
done
```

Multiple sweepers are safe — CAS ensures only one succeeds per task.

## Watching for changes

NATS KV publishes change events. Subscribe to react in real time:

```sh
nats kv watch sesh_tasks_workflow_4bf92f35
```

Common watcher patterns:

- **Orchestrator** watches; on `status=completed`, decides what's next.
- **Dependency releaser** watches; on a task completing, scans for
  tasks whose `depends_on` is now satisfied (no automatic cascade — the
  releaser is a convention, not a schema feature).
- **Notification** sends Slack on `failed` or unusual extension counts.

## Dependencies

A task may declare prerequisites:

```json
{ "id": "deploy", "depends_on": ["build", "test"] }
```

Pullers MUST verify dependencies are `completed` before pulling. The
schema doesn't enforce this — convention plus the pullers' code does.

No automatic state cascade: when `build` completes, `deploy` doesn't
automatically move. The next puller scan picks it up because the
dependency check now passes.

## Retries

When a task fails and `attempts < max_attempts`, the failing puller
resets status to `pending` so the next puller picks it up. After
`max_attempts`, status sticks at `failed` and `result` records why.

Useful convention: append per-attempt failure summaries to
`metadata.attempt_log` so postmortems can see why each attempt failed.

## Idempotency

Task creation is idempotent on `id`:

```sh
# Create only if the task doesn't already exist
nats kv create $bucket $task_id "$record"
# create fails with EEXIST if the key exists; the existing record wins
```

For repeated workflows (the same trace re-running, or a recurring job),
generate IDs from deterministic inputs (e.g., hash of `trace_id +
step_name`) and you naturally deduplicate.

## Example flow

A small workflow: build → test (depends on build) → deploy (depends on
test).

```sh
bucket=sesh_tasks_workflow_4bf92f35

# Orchestrator creates the tasks
for task in build test deploy; do
  case $task in
    build)  deps='[]' ;;
    test)   deps='["build"]' ;;
    deploy) deps='["test"]' ;;
  esac
  nats kv create $bucket $task "$(jq -n \
    --arg id "$task" --argjson deps "$deps" \
    '{id:$id, v:1, title:$id, status:"pending",
      puller:null, pulled_at:null, due_at:null,
      depends_on:$deps, priority:0, attempts:0, max_attempts:3,
      created_at:now|todateiso8601, created_by:"orchestrator:001",
      updated_at:now|todateiso8601, result:null, metadata:{}}')"
done

# Worker pool sees build is pullable; one pulls and runs it; completes.
# test becomes pullable (build is completed). One puller pulls it; completes.
# deploy becomes pullable. Pulled, completed.
# Orchestrator watches for deploy.status == "completed" to terminate the workflow.
```

## Further reading

- [Scoped memory](./scoped-memory.md) — where tasks live
- [Message envelope](./message-envelope.md) — trace-id propagation
- [`sesh-ops`](https://github.com/danmestas/sesh-ops) — reference CLI
- [NATS KV watchers](https://docs.nats.io/nats-concepts/jetstream/key-value-store/kv_walkthrough#watching-for-changes)
