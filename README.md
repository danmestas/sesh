# sesh

Session-aware leaf wrapper for [EdgeSync](https://github.com/danmestas/EdgeSync).

Owns the **session** and **agent** vocabulary on top of EdgeSync's neutral NATS+fossil hub substrate. EdgeSync stays a sync engine; sesh adds:

- Session naming: `<project>-session-<id>` (id defaults to a time-prefixed random label, override with `--session=<label>`)
- Lockfile guard against same-machine session-name collisions
- Disk layout under `~/.sesh/sessions/<project>/<session>/`

## Layering

```
sesh                 ← user-facing CLI (up/down/hub) — session vocabulary, ~/.sesh
  └─ EdgeSync/hub    ← NATS+fossil substrate (in-process server, leaf solicit)
       └─ libfossil  ← repo primitives
```

Dependency arrow goes one way: sesh depends on EdgeSync, never the reverse.

## Commands

- `sesh up --session=<label>` — bring a session up. Cwd-derived project name. Foreground; blocks until SIGINT. Auto-spawns the hub if none is running.
- `sesh down --session=<label>` — SIGINT the matching `sesh up` and wait for it to exit.
- `sesh hub serve` — run the hub daemon directly. Normally auto-spawned by `sesh up`; visible for power users. `--keepalive` keeps it running past the last leaf disconnect (default: exit when last session closes).

## Quick start

```sh
# In a project directory
cd ~/projects/myproject

# Start a session — hub is auto-spawned on first invocation
sesh up --session=morning

# In another shell, look at what's running
cat ~/.sesh/hub.url                              # hub's NATS leaf URL
cat .sesh/sessions/morning.json                  # {"pid":..,"nats_url":..,"leaf_url":..}

# End the session — hub auto-shuts down if this was the last session
sesh down --session=morning
```

### Attaching to a running session

A live `sesh up` publishes its NATS client URL and leafnode listener URL in
`.sesh/sessions/<label>.json`. Sub-leaves and clients dial those without
grepping logs.

```sh
# EdgeSync leaf node under this session
LEAF=$(jq -r .leaf_url < .sesh/sessions/morning.json)
edgesync hub serve --leaf-upstream="$LEAF"

# NATS client under this session
NATS=$(jq -r .nats_url < .sesh/sessions/morning.json)
nats --server="$NATS" sub '>'
```

## Lifecycle model

- One **hub** per user (at `~/.sesh/`). Singleton. Auto-spawned by the first `sesh up`; auto-shuts down when the last leaf disconnects (unless `--keepalive`).
- Many **session leaves** per project. Each `sesh up` opens one leaf connection to the hub via the `nats-leaf://` URL written to `~/.sesh/hub.url` by the hub at startup.
- Session identity = `<cwd-basename>-session-<label>`. Project name is derived from the working directory.
- Same-name collision detection is local: O_EXCL on `<cwd>/.sesh/sessions/<label>.json`. A second `sesh up` with the same label refuses to start, naming the holder PID.

## Disk layout

```
~/.sesh/
├── hub.url           ← hub's NATS leaf URL (O_EXCL by hub at bind)
├── hub.repo          ← fossil repo (persistent across hub restarts)
├── messaging/        ← JetStream storage (persistent)
└── hub.log           ← stderr from auto-spawned hub

<cwd>/.sesh/sessions/
├── <label>.json      ← {pid, nats_url, leaf_url} — claimed PID-only via O_EXCL,
│                       URLs filled in once the embedded hub binds its ports;
│                       file removed on graceful exit
├── <label>.repo      ← per-session fossil leaf repo
└── <label>.messaging/  ← per-session JetStream storage
```

## Coordination patterns

The mesh is a neutral transport — any well-known multi-agent coordination
pattern maps onto its primitives. See
[`docs/coordination-patterns.md`](docs/coordination-patterns.md) for
generator–verifier, orchestrator–subagent, hierarchical multi-tier, agent
teams, message bus, and shared-state/blackboard patterns wired against
NATS subjects, JetStream, and the Fossil repo.

For correlation and tracing across hops, agents should follow the
[message envelope spec](docs/message-envelope.md) — NATS headers carrying
W3C `traceparent` (OpenTelemetry-compatible) plus optional sesh metadata.

For shared state across agents, see
[`docs/scoped-memory.md`](docs/scoped-memory.md) (five scopes — hub,
project, session, workflow, agent — with bucket conventions and TTL
policy) and
[`docs/task-management.md`](docs/task-management.md) (task schema and
pull protocol built on scoped memory).

## Status

Spike. Designed during a 2026-05-11 brainstorm; see commit messages for the design rationale captured inline. Out of scope today:

- Coord/lease registry (use the hub's connection state, not a KV bucket)
- Cross-machine session teleport
- Historical session log persistence to JetStream
- Agent-tier 3rd-level naming
