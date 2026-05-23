# Notes for Oh My Pi — MCP discovery exclusion mechanism

> **Status:** Operator-discretion notes. This file is **not** filed
> upstream — per the sesh CLAUDE.md no-third-party-filing rule, Claude
> Code subagents do not open issues at `can1357/oh-my-pi` or
> `oh-my-pi/pi-coding-agent`. The notes are kept here so the operator
> can choose to file (or skip) themselves.

## Observed gap

When `mcp.discoveryMode: true` is set in OMP's config (the documented
default for plugin-style MCP discovery), OMP autoloads **every** entry
in any `.mcp.json` reachable from its working directory. There is no
exclusion mechanism: an operator who runs claude-code and OMP in the
same workspace has no way to tell OMP "ignore these specific MCP
entries — they're for claude, not for you".

This is most painful when claude-code's `.mcp.json` includes a
long-running channel (e.g. `claude-nats-channel`). OMP discovers the
entry, spawns its own instance of the channel server, and the second
instance registers on the bus as a phantom duplicate of claude's
agent. See sesh's
[`docs/plans/2026-05-22-integration-fix-F5-omp-mcp-discovery.md`](plans/2026-05-22-integration-fix-F5-omp-mcp-discovery.md)
for the full root-cause analysis.

## Suggested fixes

Either of these would close the gap:

1. **Allow-list / deny-list config key.** A new `mcp.exclude:
   [<server-name>, ...]` (or symmetrically `mcp.allowlist:
   [<server-name>, ...]`) filter that runs against the discovered
   `mcpServers` map before instantiation. Operators co-locating with
   other MCP clients can list the server names that belong to a
   different client.

2. **Per-`.mcp.json`-file exclusion marker.** A top-level key in the
   `.mcp.json` itself — e.g. `{"x-clients": ["claude-code"]}` — that
   OMP honors as "only load this file if my client id matches". This
   needs a cross-client convention to be useful (a marker only OMP
   reads is half-broken), but it puts the discipline at the right
   layer.

## Current workaround (no upstream change needed)

The sesh integration rig demonstrates the two-part fix:

1. **Place claude's MCP config outside the workspace.** Launch claude
   with `--strict-mcp-config --mcp-config /path/to/file.json` where
   the path is not under any directory OMP would walk during
   discovery. The rig uses `/opt/claude.mcp.json` (sibling to
   `/workspace`).
2. **Set `NATS_CHANNEL_STRICT=1`** on claude-nats-channel so a phantom
   spawn (operator misconfiguration, future regression) becomes a
   loud `exit 2` rather than a silent `<name>-2` duplicate registration.

Both pieces are documented in `test/integration/README.md` (F5
section) and `sesh-channels/claude-nats-channel/README.md#strict-mode`.

## If the operator decides to file upstream

The bug report can reference the rig as a reproduction:
`github.com/danmestas/sesh/test/integration` builds a container with
claude + OMP co-located and shows the phantom-duplicate failure mode
when `NATS_CHANNEL_STRICT` is unset. Then point at this notes file
and the F5 plan for the proposed `mcp.exclude` shape.
