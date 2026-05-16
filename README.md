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

## Synadia Agent Protocol

Agents running inside a sesh session register as NATS micro services under
`name = "agents"`, making them discoverable via `$SRV.INFO.agents` with no
per-consumer protocol negotiation. See
[`docs/synadia-agents-on-sesh.md`](docs/synadia-agents-on-sesh.md) for the
full presence contract (identity, connection, subjects, endpoints, streaming,
and liveness), and [`docs/sesh-ref-agent.md`](docs/sesh-ref-agent.md) for the
executable spec — a minimal Go agent (`cmd/sesh-ref-agent/`) that the contract
is validated against.

## Commands

- `sesh up --session=<label> [--scope=session|project]` — bring a session up. Cwd-derived project name. Foreground; blocks until SIGINT. Auto-spawns the hub if none is running. See [Fossil scope](#fossil-scope-session-vs-project) for `--scope`.
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

When `sesh up` runs in a git worktree, the session's Fossil repo
(`<cwd>/.sesh/sessions/<label>.repo`) gets seeded with the current
worktree as a single initial commit. Each session owns its own repo
— same-project sessions converge via NATS autosync rather than a
shared SQLite file. Only the **first** session in a fresh project
seeds from the worktree; later sessions detect the hub already has
content (from the first session's commit propagating through
autosync) and clone from the hub instead, so per-session repos start
in convergent state.

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

## Fossil scope: session vs project

`sesh up` accepts `--scope=session|project` to control where the
session's Fossil repo lives on disk. The default is `session` — the
PR #20 model where every session owns its own repo at
`.sesh/sessions/<label>.repo` and cross-session convergence happens
via NATS autosync on the shared project-code subject. `--scope=project`
opts into a single shared file at `.sesh/project.repo` for cases where
synchronous cross-session visibility matters more than the autosync
robustness story.

| Mode                  | Repo path                                    | Cross-session writes                  | Contention                                                       |
| --------------------- | -------------------------------------------- | ------------------------------------- | ---------------------------------------------------------------- |
| `session` (default)   | `<cwd>/.sesh/sessions/<label>.repo`          | Eventual via NATS autosync (~0.24s)   | None — every session is the sole writer to its own file          |
| `project` (opt-in)    | `<cwd>/.sesh/project.repo`                   | Synchronous via shared SQLite WAL     | Writers serialize at `BEGIN IMMEDIATE`; queued via `busy_timeout`                  |

Both modes coexist in the same project. A `--scope=session` session
and a `--scope=project` session can run side-by-side and still
exchange commits via the shared project-code autosync subject — the
publish-hook wiring is scope-agnostic. The JetStream message store
stays per-session in both modes (each `sesh up` runs its own embedded
NATS server with its own on-disk store).

```sh
# Default — per-session repo; same as before this flag existed.
sesh up --session=alpha

# Opt in to the shared file. All same-project sessions launched with
# --scope=project write to .sesh/project.repo directly.
sesh up --session=beta --scope=project

# Mixed-scope is fine — autosync still propagates across the
# project-code subject between the two repo files.
sesh up --session=alpha --scope=session   # writes .sesh/sessions/alpha.repo
sesh up --session=beta  --scope=project   # writes .sesh/project.repo
```

When to pick which:

- **`session` (default)** — almost always the right choice. No SQLite
  contention; convergence latency under typical load is well below a
  human round-trip; survives the case of a session crashing
  mid-commit without affecting peers' repos.
- **`project`** — when you genuinely need a single shared file (e.g.,
  external tooling that opens `.sesh/project.repo` directly and
  expects to see commits from all sessions instantly without an
  autosync hop). Trade off: concurrent commits queue on the SQLite
  write lock, and one badly-behaved session affects the file every
  other session is reading.

### How cross-process Fossil sync works

Each same-project session owns its own `.sesh/sessions/<label>.repo`
and the hub at `~/.sesh/hub.repo` is a passive collector / mirror.
Commits propagate session-to-session via the EdgeSync fossil-sync
subject keyed on the project-code (pinned at
`<cwd>/.sesh/project-code` on first run): a commit landing in
session A's in-process hub fires the publish hook natively, the hub
picks it up over NATS, and peer sessions subscribed to the same
subject pull it into their own repos. No shared SQLite file, no
cross-process write coordination.

The project-code is derived as `sha1("sesh:" + hostname + ":" +
project)` on first run and persisted, so it survives hostname
changes (VM clones, container migration). It's passed via
`hub.Config.ProjectCode` plus the `SESH_PROJECT_CODE` env var to the
spawned hub. Verified by `TestHub_AccumulatesProjectCommits` and
`TestCrossSessionAutosync`.

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

For **worker processes** that need to read or write Fossil state,
the supported pattern is HTTP clone-push via `$fossil_url`:

```sh
HTTP=$(jq -r .fossil_url < .sesh/sessions/morning.json)

# Worker bootstrap (once per worker)
fossil clone "$HTTP" /tmp/worker.repo
fossil open /tmp/worker.repo --workdir /tmp/work
fossil user default worker --repo /tmp/worker.repo
fossil settings autosync on --repo /tmp/worker.repo

# Per-commit
cd /tmp/work
fossil add notes.md && fossil commit -m "..."   # autosync pushes back
```

Workers must **not** `fossil open` the session repo at
`.sesh/sessions/<label>.repo` directly — commits made via that path
land locally but do not propagate. The clone-push pattern via
`$fossil_url` is the supported way; EdgeSync's auto-publish on the
HTTP xfer push handler is what carries the commit to peers (see
`TestCrossLeaf_HTTPPush_PropagatesCommit` upstream).

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
policy),
[`docs/task-management.md`](docs/task-management.md) (task schema and
pull protocol built on scoped memory), and
[`docs/goal-management.md`](docs/goal-management.md) (long-horizon
goal records, hierarchical composition, and task linkage — the
durable-intent companion to tasks).

## Status

Spike. Designed during a 2026-05-11 brainstorm; see commit messages for the design rationale captured inline. Out of scope today:

- Coord/lease registry (use the hub's connection state, not a KV bucket)
- Cross-machine session teleport
- Historical session log persistence to JetStream
- Agent-tier 3rd-level naming
