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

```sh
# 1. Start an EdgeSync hub on this machine (provides the NATS leafnode endpoint)
edgesync hub serve

# 2. Run a session leaf — auto-generated session id, no lockfile
sesh leaf serve --upstream=nats-leaf://127.0.0.1:7422 --project=alpha

# 3. Run a named session — single-machine lockfile guards against collision
sesh leaf serve --upstream=nats-leaf://127.0.0.1:7422 --project=alpha --session=morning
```

## Status

Spike. Cross-machine collision detection (lease registry on the hub) is the next
piece — see `docs/` once that lands.
