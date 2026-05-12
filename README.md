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

## Worktree seeded into Fossil

When `sesh up` runs in a git worktree, the project's Fossil repo
(`<cwd>/.sesh/project.repo`) gets seeded with the current worktree as
a single initial commit. The repo is **shared by every session in the
same project** — all sessions read and write the same Fossil trunk.
Only the first `sesh up` in a project seeds it; subsequent sessions
open the existing repo and stack their commits on top.

The git worktree itself is untouched. Agents work in Fossil; a human
or an explicit `sesh promote` (TODO) decides which Fossil commits get
applied back to the git working tree.

Seed mode is set by `--seed`:

- `all` (default): tracked + untracked-but-not-gitignored files
- `tracked`: only files in the git index
- `none`: skip seeding (Fossil starts empty)

Skipped automatically when cwd isn't a git worktree, when the Fossil
repo already has content from a prior session, or with `--seed=none`.
Sesh's own `.sesh/` runtime state is never seeded.

Recommended: add `.sesh/` to your `.gitignore` so git doesn't notice
the sesh runtime state.

### How cross-process Fossil sync works

Same-project sessions share the `.sesh/project.repo` file directly
(SQLite handles concurrent opens) — no network sync needed for
intra-project commits; readers see writes immediately.

For **hub.repo** at `~/.sesh/`: sesh derives a deterministic
project-code per project (`sha1("sesh:" + hostname + ":" + project)`)
and passes it via `hub.Config.ProjectCode` plus `SESH_PROJECT_CODE`
env to the spawned hub. Both the project's repo and the hub's repo
subscribe to the same EdgeSync fossil-sync subject, so commits
propagate over NATS auto-publish. Verified by
`TestHub_AccumulatesProjectCommits`.

For **sub-leaves** (an `edgesync hub serve --leaf-upstream=...`
spawned under a sesh): use `--seed-from-upstream=$FOSSIL_URL` where
`FOSSIL_URL` comes from the parent session's state JSON
(`.sesh/sessions/<label>.json` → `.fossil_url`). The sub-leaf clones
the parent's Fossil over HTTP at startup (catching the seed commit
and any prior agent commits) and inherits the parent's project-code,
so subsequent commits land via NATS auto-publish. Verified by
`TestSubLeaf_SyncsFromSesh`.

```sh
# Attach an edgesync sub-leaf under the running session "morning"
LEAF=$(jq -r .leaf_url   < .sesh/sessions/morning.json)
HTTP=$(jq -r .fossil_url < .sesh/sessions/morning.json)

edgesync hub serve \
  --repo=./.subleaf.repo \
  --leaf-upstream="$LEAF" \
  --seed-from-upstream="$HTTP"
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
