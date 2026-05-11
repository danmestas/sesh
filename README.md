# sesh

Session-aware leaf wrapper for [EdgeSync](https://github.com/danmestas/EdgeSync).

Owns the **session** and **agent** vocabulary on top of EdgeSync's neutral NATS+fossil hub substrate. EdgeSync stays a sync engine; sesh adds:

- Session naming: `<project>-session-<id>` (id defaults to a time-prefixed random label, override with `--session=<label>`)
- Lockfile guard against same-machine session-name collisions
- Disk layout under `~/.sesh/sessions/<project>/<session>/`

## Layering

```
sesh                 ← session/agent vocabulary, lockfiles, ~/.sesh/ layout
  └─ EdgeSync/hub    ← NATS+fossil substrate (in-process server, leaf solicit)
       └─ libfossil  ← repo primitives
```

Dependency arrow goes one way: sesh depends on EdgeSync, never the reverse.

## Quick start

`sesh` is a single binary that both serves the hub substrate (re-exported from EdgeSync) and runs session leaves:

```sh
# 1. Start the hub (EdgeSync's HubCmd, embedded). Prints its NATS URLs.
sesh hub serve

# 2. Run a session leaf — auto-generated session id
sesh leaf serve \
  --upstream=nats-leaf://127.0.0.1:7422 \
  --hub-nats=nats://127.0.0.1:4222 \
  --project=alpha

# 3. Run a named session — local lockfile + cross-machine lease both guard
sesh leaf serve \
  --upstream=nats-leaf://127.0.0.1:7422 \
  --hub-nats=nats://127.0.0.1:4222 \
  --project=alpha --session=morning
```

`--upstream` is where the leaf solicits its NATS connection (the hub's leafnode listener).
`--hub-nats` is the hub's client NATS URL — used for session-lease coordination on the hub's JetStream KV.

## Coordination

The `coord/` package manages session leases in a JetStream KV bucket named `sessions` on the hub:

- **Claim** is atomic (`kv.Create` fails if the key exists). The error names the current owner.
- **Renew** on a ticker (default 10s; bucket TTL is 30s). Verifies the caller is still the owner.
- **Release** on graceful shutdown. Lost processes' leases auto-expire via TTL.

The local lockfile at `~/.sesh/state/<project>/sessions/<label>.lock` is a fast-fail check for same-machine collisions; the coord lease is what enforces uniqueness across machines.

## Status

Spike. Sessions teleporting across machines (graceful handoff with stash + lease transfer) is the next piece.
