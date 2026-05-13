# Scoped memory on sesh

A multi-agent workflow has at least five natural visibility levels for
shared state — wider than "this project" or "this session" alone covers,
and narrower than "everything on this hub." This document specifies the
scope conventions, bucket naming, TTL policy, and storage selection so
agents on the mesh can share working memory cleanly across whichever
scope fits.

Sesh does not enforce these conventions; following them lets multiple
agent systems on the same hub coexist without trampling each other's
state.

## The five scopes

| Scope        | Who sees it                                    | Lifetime                          | Example content                                                |
| ------------ | ---------------------------------------------- | --------------------------------- | -------------------------------------------------------------- |
| **Hub**      | All sessions and projects on this machine      | Forever                           | User preferences, machine-wide configs, cross-project indexes  |
| **Project**  | All sessions in one project                    | Until explicit delete             | Project plan, code style guide, domain glossary                |
| **Session**  | All agents inside one `sesh up`                | Until session exits               | Local working state, agent process roster                      |
| **Workflow** | All agents participating in one trace          | TTL after last write (24h default) | Plan for this trace, intermediate findings, this trace's task list |
| **Agent**    | One agent process                              | Process lifetime                  | Private scratchpad, in-progress computation                    |

Workflow scope is the interesting one — it cuts across sessions. A
generator and verifier living in different sessions share a workflow
because they share a trace-id. With only project/session scopes you
cannot model this; workflow scope is keyed by the W3C `trace-id` from
the [message envelope spec](./message-envelope.md).

## Bucket naming

Buckets are deterministic so agents can derive the name from context
without a lookup table:

```
sesh_hub                              hub scope
sesh_project_<project>                project scope
sesh_session_<project>_<session>      session scope
sesh_workflow_<trace-id-8hex>         workflow scope
sesh_agent_<role>_<agent-id>          agent scope
```

NATS JetStream KV bucket names accept only `[a-zA-Z0-9_-]` — dots
aren't allowed. The convention uses underscore as the separator.
When deriving bucket names, sanitize user-supplied identifiers by
replacing dots and hyphens with underscores (e.g., a project named
`my-app` becomes `my_app` in the bucket name; a session-id like
`myproj.alpha` becomes `myproj_alpha`).

The identifiers come from the same sources sesh already uses: cwd
basename for project, `--session=` flag for session, the publisher's
identity for agent. Workflow id is the first 8 hex chars of the W3C
trace-id from the incoming `traceparent` header — unique enough in
practice and keeps bucket names readable.

## Connection target — which NATS to talk to

Each `sesh up` runs an embedded NATS server with **its own JetStream
domain** (storage under `<cwd>/.sesh/sessions/<label>.messaging/`). The
hub also runs its own JetStream domain (`~/.sesh/messaging/`). NATS
leaf nodes connect the message-routing layer, but JetStream **storage
is per-domain** — a KV bucket created on one server's JetStream is
invisible to clients connected to a different server.

So the connection target matters for whether your scope is actually
shared:

| Scope             | Connect to                                | Why                                       |
| ----------------- | ----------------------------------------- | ----------------------------------------- |
| hub               | hub's NATS URL (`~/.sesh/hub.nats.url`)   | Hub is the shared store                   |
| project           | hub's NATS URL                            | Shared across sessions in the project     |
| workflow          | hub's NATS URL                            | Shared across sessions in the trace       |
| session           | session's `nats_url` from state JSON      | Local durable state, session-private      |
| agent             | session's `nats_url` from state JSON      | Local durable state, agent-private        |

Sesh publishes the hub's client NATS URL at `~/.sesh/hub.nats.url`
(written atomically when the hub binds, removed on shutdown). Clients
that need hub/project/workflow-scoped KV connect there; clients that
want session/agent-scoped state stay on the session's URL from the
session JSON.

The reference CLI `sesh-ops` does this routing automatically based on
the `--scope` flag. Hand-rolled clients should follow the same rule.

## TTL policy

Configured at bucket creation. JetStream KV supports both per-bucket
max-age and per-key TTL.

| Scope    | Bucket TTL         | Per-key TTL | Cleanup trigger                                          |
| -------- | ------------------ | ----------- | -------------------------------------------------------- |
| Hub      | none               | optional    | Manual `kv prune`                                        |
| Project  | none               | optional    | Manual `kv prune`                                        |
| Session  | 1h after last write | optional   | Graceful session exit deletes; TTL covers crashes        |
| Workflow | 24h after last write | optional  | TTL only — workflows have no explicit "done" signal      |
| Agent    | process lifetime   | n/a         | Agent deletes on graceful exit; orphans expire via TTL   |

The TTL is a backstop, not the primary cleanup. Agents should delete
their own scope when they exit cleanly. TTLs handle the crash cases.

## Storage selection

Three substrate-level stores; pick by data shape, not by scope.

| Data shape                                                    | Store                  | Notes                                                |
| ------------------------------------------------------------- | ---------------------- | ---------------------------------------------------- |
| Structured state, frequent updates, small (< 1MB per value)   | JetStream KV           | CAS, watchers, history (configurable)                |
| Append-only events (decisions, audit log)                     | JetStream stream       | One-shot publish, replay by consumer                 |
| Large blobs (documents, datasets, generated assets)           | JetStream Object Store | Versioned, streamed, separate from message path      |
| Content-addressed artifacts (code, research outputs)          | Fossil repo            | Per-session + hub, sync'd across leaves by EdgeSync  |

Stream and Object Store buckets follow the same naming with a different
prefix:

```
sesh_events_<scope>_<scope-id>    append-only event stream
sesh_blobs_<scope>_<scope-id>     versioned object store
```

For Fossil: every session has its own repo at
`<cwd>/.sesh/sessions/<label>.repo`. The hub has its own repo at
`~/.sesh/hub.repo` and acts as a passive collector / mirror. Use
Fossil when the artifact has identity (a commit) and when other
agents should be able to read it at a specific revision.

When the first `sesh up` of a project runs in a git worktree, that
session's Fossil is seeded with the worktree as a single initial
commit (see the [README](../README.md#worktree-seeded-into-fossil)
for `--seed` modes). Subsequent sessions detect the hub already has
content and clone from the hub instead of re-seeding from the
worktree, so per-session repos start in convergent state.

**Cross-process Fossil sync works** via NATS rather than shared
SQLite. Each session's in-process hub fires the EdgeSync publish
hook natively on commit; the hub at `~/.sesh/` mirrors via the same
fossil-sync subject; peer sessions subscribed to the subject pull
the commit into their own repos. Sesh threads a deterministic
project-code through `hub.Config.ProjectCode` so all participants
land on the same subject. Sub-leaves spawned via `edgesync hub
serve --leaf-upstream=... --seed-from-upstream=$FOSSIL_URL` clone
the parent's Fossil state and inherit its project-code, so they
stay synced via NATS auto-publish. See the
[README](../README.md#how-cross-process-fossil-sync-works) for the
sub-leaf spawn recipe.

## Lifecycle responsibility

| Bucket scope | Who creates                              | Who deletes                                  |
| ------------ | ---------------------------------------- | -------------------------------------------- |
| Hub          | First writer (idempotent create)         | Operator (explicit)                          |
| Project      | First writer in the project              | Operator or project-level cleanup script     |
| Session      | First agent on first write               | Graceful session exit; session TTL on crash  |
| Workflow     | First agent in the trace                 | TTL only                                     |
| Agent        | Agent process at startup                 | Agent process at exit; agent TTL on crash    |

The conventions work without tooling — agents create and delete buckets
directly via `nats kv add` / `nats kv rm`. A reference CLI for the
common lifecycle operations will live in
[`sesh-ops`](https://github.com/danmestas/sesh-ops).

## Usage

### Workflow plan

```sh
# Workflow root agent generates a plan, stores in workflow scope
trace_id_short=${traceparent:3:8}     # 8 hex chars from "00-<trace-id>-..."
bucket=sesh_workflow_$trace_id_short

nats kv add $bucket --ttl=24h
nats kv put $bucket plan '{"phase":"research","next":["draft","review"]}'

# Downstream agent reads the plan
nats kv get $bucket plan

# Downstream agent watches for changes
nats kv watch $bucket plan
```

### Session-scoped scratchpad

```sh
project=$(basename $(pwd) | tr .- _)   # sanitize: dots/hyphens → underscore
session=alpha
bucket=sesh_session_${project}_${session}

nats kv add $bucket --ttl=1h
nats kv put $bucket roster '{"agents":["orchestrator","researcher"]}'
```

### Hub-wide preference

```sh
nats kv add sesh_hub
nats kv put sesh_hub default_max_attempts 3
```

### Cross-scope: small structured state, large blob

```sh
# Big artifact in Fossil
fossil commit -m "research notes" notes.md
revid=$(fossil info | awk '/^checkout:/{print $2}')

# Pointer in scoped KV
nats kv put $bucket findings "{\"rev\":\"$revid\",\"path\":\"notes.md\"}"

# Announce via NATS so watchers react now (rather than poll)
nats pub workflow.update.findings "{\"rev\":\"$revid\"}"
```

## Edge cases

- **Workflow bucket expired but a late hop publishes.** The write
  succeeds (KV recreates the bucket if absent) but readers may have
  moved on. Convention: workflow scope is not a durable store. Promote
  anything you need long-term to project or hub scope before the TTL
  elapses.
- **Concurrent writers to the same key.** JetStream KV CAS forces one
  to lose. Resolution is application-specific — overwrite, merge, or
  escalate. Convention: include `metadata.owner` on keys with
  single-writer semantics so contests are visible.
- **Schema migration.** Bump the `v` field on the value. Readers
  tolerate higher versions (don't write back); writers refuse to
  downgrade. Same pattern the envelope spec uses.
- **Bucket sprawl.** Workflow buckets accumulate if TTLs are missing.
  Sibling `sesh-ops` will provide `kv prune --stale=7d`. Manually:
  iterate `nats kv ls`, check `info` for `last_update`, delete the
  stale ones.

## Observability

```sh
nats kv ls                                # all buckets
nats kv info sesh_workflow_4bf92f35       # bucket config + key count
nats kv ls-keys sesh_workflow_4bf92f35    # keys in a bucket
nats kv get <bucket> <key>                # current value
nats kv history <bucket> <key>            # version history (if enabled)
nats kv watch <bucket>                    # tail change events
```

## Further reading

- [Message envelope](./message-envelope.md) — trace-id binding for workflow scope
- [Task management](./task-management.md) — structured records stored in scoped memory
- [Coordination patterns](./coordination-patterns.md) — Pattern 6 (shared state) uses these scopes
- [NATS JetStream KV docs](https://docs.nats.io/nats-concepts/jetstream/key-value-store)
