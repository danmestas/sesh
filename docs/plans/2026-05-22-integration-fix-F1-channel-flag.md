# F1 — Inbound prompt does not trigger an agent turn

**Date:** 2026-05-22
**Status:** AFK-ready (one operator decision deferred — see bottom)
**Severity:** P0 — gates rig cases 03 / 04 / 05 / 06
**Primary owner:** sesh-channels (`claude-nats-channel`) — documentation + launch helper
**Secondary owner:** sesh rig (`test/integration/entrypoint.sh`) — wire the launch flag

## Root cause (function and line precise)

Claude Code 2.1.126 segregates MCP servers into two classes:

1. **Tool providers (default).** Loaded via `--mcp-config` / `.mcp.json`. The model can call tools registered by the server. **`notifications/claude/channel` notifications from these servers are silently dropped.**
2. **Channels.** Loaded via either `--channels <name>` (production allowlist; org‑policy + claude.ai‑auth gated) or `--dangerously-load-development-channels <name>` (local-dev override). Only channels are permitted to push inbound prompts into the running session.

Direct evidence pulled from `~/.local/share/claude/versions/2.1.126` (Mach-O strings, single binary):

```
--channels <servers...>                MCP servers whose channel notifications (inbound push)
                                        should register this session. Space-separated server names.
--dangerously-load-development-channels <servers...>
                                        Load channel servers not on the approved allowlist.
                                        For local channel development only. Shows a confirmation
                                        dialog at startup.

"server: entries need --dangerously-load-development-channels"
"is not on the approved channels allowlist (use --dangerously-load-development-channels for local dev)"
"server did not declare claude/channel capability"
"channels feature is not currently available"
"channels requires claude.ai authentication (run /login)"
"channels not enabled by org policy (set channelsEnabled: true in managed settings)"
"Channels:"
"Listening for channel messages from: "
"Inbound messages will be silently dropped"
"Channel notifications registered"
"Channel notifications skipped: "
"Channel gate says skip:"
"notifications/claude/channel"
```

`server.ts:586-589` declares the `experimental.claude/channel` capability handshake, but the capability is *necessary* not *sufficient*: claude-code's gate (`tengu_mcp_channel_gate` in the binary) also requires the explicit allowlist entry. With no `--channels nats` / `--dangerously-load-development-channels nats` flag, the server passes the capability handshake but fails the gate, so `notifications/claude/channel` are dropped — exactly the observed symptom:

- Caller publishes → `nc.request("agents.prompt.cc.<owner>.smoke-test")` reaches `claude-nats-channel/server.ts:781 handleNatsMessage`.
- §6.4 ack chunk publishes successfully at `server.ts:832-834` (start of `setInterval(ackTimer)`).
- `mcp.notification({ method: 'notifications/claude/channel', ... })` at `server.ts:852` returns successfully — the MCP SDK delivered it on stdio.
- Inside claude-code: `Channel notifications skipped: unmatched` (in the rig logs we'd see this with `--debug mcp` enabled — currently they're not). Model never gets the inbound, never starts a turn, never calls `reply`.
- Ack-timer fires every 30 s until the caller's 90 s timeout.

The roundtrip test at `claude-nats-channel/scripts/roundtrip-test.ts:37-49` does NOT reproduce real claude-code behavior — it constructs a `new Client({ name: 'fake-claude' })` and manually calls `mcp.callTool('reply', ...)` from the test, so the channel/notification gate is never crossed. The test proves the server can encode/decode chunks; it does not prove an inbound notification drives a real session.

### What `--dangerously-load-development-channels` does (verified from the binary)

At startup, when present:

1. Claude renders a `WARNING: Loading development channels` dialog with three lines and three options:
   - `I am using this for local development` → accept
   - `Exit` → exit
   - `accept` / `exit` are the values (binary string table)
2. On accept, the named server is added to the channels-registered set. The `tengu_mcp_channel_gate` logs `registered`. `notifications/claude/channel` from that server start routing to the channel inbound dispatcher (`tengu_mcp_channel_message` telemetry).
3. The dialog requires interactive accept (no settings key skips it — `skipDangerousModePermissionPrompt` is a different gate, for the bypass-permissions dialog only). In non-TTY mode, claude refuses to render the dialog and exits.

Cross-references:

- `~/projects/sesh-channels/claude-nats-channel/server.ts:580-601` — MCP server construction with the `experimental.claude/channel` capability (necessary but insufficient).
- `~/projects/sesh-channels/claude-nats-channel/server.ts:780-865` — `handleNatsMessage` flow.
- `~/projects/sesh-channels/claude-nats-channel/scripts/roundtrip-test.ts:37-49` — the test that does NOT exercise the channel gate.
- `~/projects/synadia-agents/agents/claude-code/README.md:43` — upstream installation instruction `claude --dangerously-load-development-channels plugin:nats-channel@synadia-plugins`. The sesh-channels README copy at `~/projects/sesh-channels/claude-nats-channel/README.md` quotes this but launches via `/plugin install` (which automates the flag); operators driving claude-code directly with `--mcp-config` lose the auto-wiring.
- Claude binary at `~/.local/share/claude/versions/2.1.126`, string offsets 199700-199900 (channel gate), 202590-202650 (CLI flag parsing), 225650-225660 (help string), 161580-161600 (managed-settings keys).

## Alternatives considered

### Option A — Pass `--dangerously-load-development-channels nats` and auto-accept the dev-channels dialog

Adds one flag to the rig's `claude` invocation and extends the FIFO auto-feed to dismiss the new dialog (in addition to the bypass-permissions dialog).

**Interface complexity:** small — one CLI flag, ~6 extra FIFO lines.
**Blast radius:** rig-only (`test/integration/entrypoint.sh`). No code change to `claude-nats-channel`.
**Reversibility:** trivial — revert the flag.
**Risk:** the dev-channels dialog text/order may shift across claude-code versions. The rig already has this fragility for the bypass-permissions dialog (FIFO feeds `2\n` after 6 s); adding a second timed feed compounds the brittleness.

### Option B — Install `claude-nats-channel` as a Claude Code plugin and launch via `/plugin install`

Mirrors the upstream `synadia-agents` distribution model: register a marketplace, `/plugin install`, then `claude --dangerously-load-development-channels plugin:nats-channel@<marketplace>`. The plugin installation pre-confirms the channel registration.

**Interface complexity:** larger — requires running `/plugin marketplace add` + `/plugin install` inside an interactive claude session at image-build time, or shipping a pre-populated `~/.claude/plugins/` tree. Both involve significant claude-code internals the rig doesn't otherwise touch.
**Blast radius:** rig-build + likely changes to `claude-nats-channel/package.json` / `.claude-plugin/`. Crosses into upstream territory.
**Reversibility:** medium — still requires deletion of plugin state and reversion of build steps.
**Risk:** brittleness of the install dance is at least as bad as the dialog auto-feed.

### Option C — Wait for Claude to receive a user message, then trampoline the prompt as a user turn (in-channel-server workaround)

If `notifications/claude/channel` is silently dropped without the flag, an alternative is to mint a "user message" inside `claude-nats-channel` via a different MCP surface — e.g., a `tools/list_changed` ping that nudges the model. This was investigated and ruled out: there is no MCP method that synthesizes a *user turn* in claude-code from an MCP server's side; the only inputs that drive turns are (a) human stdin/`-p` prompt, (b) channel notification (gated as above), or (c) the `--brief` flag's `SendUserMessage` tool (which is a tool the model calls, not a tool that pushes input).

**Interface complexity:** high — would require either changes inside claude-code or abuse of an undocumented MCP path.
**Blast radius:** large — likely a new external API surface.
**Reversibility:** poor.

### Chosen approach — Option A

Cheapest. Operates at the rig boundary. No upstream changes. The rig is the *only* consumer that needs unattended-mode automation of the dev-channels dialog; operators running interactively will just click through it. We also ship a small docs note in `claude-nats-channel/README.md` so other operators don't rediscover this gap.

## Operator decisions deferred

**Decision F1.1 — Channel-name convention for the dev-channels flag.** The flag accepts the MCP server name as registered in `--mcp-config` (`nats` in the current rig — see `test/integration/config/claude.mcp.json`). Either:

- (a) Keep `nats` (current name; matches upstream synadia adapter convention for the plugin-name `nats-channel`).
- (b) Rename to `sesh-channel` / `sesh-nats` / `agents` for clarity.

This is a **taste** decision (axis 1 in the policy). Pick one before AFK dispatch. The plan defaults to (a) `nats` so no rename is needed. To change, update `test/integration/config/claude.mcp.json#mcpServers.<name>` and the flag arg in `entrypoint.sh`.

## AFK-ready plan

### Task 1 — Failing test: rig reproduction

**File:** `/Users/dmestas/projects/sesh/test/integration/harness/cases/03-prompt-claude.ts`
**Status:** already failing (case 03). No change. This is the red signal for Task 2.

### Task 2 — Update `entrypoint.sh` launch flags + FIFO

**File:** `/Users/dmestas/projects/sesh/test/integration/entrypoint.sh`

Locate the claude-side launcher block (currently at lines 124-132) and replace with the following. Two changes: (a) add `--dangerously-load-development-channels nats` to the `claude` invocation, (b) extend the FIFO auto-feed with a second `2\n` to dismiss the dev-channels dialog after the bypass-permissions dialog clears.

```bash
# Hold both FIFOs open by writing zero bytes forever. For claude we also
# auto-feed two "2\n" inputs:
#   1. The "Yes, I accept" choice in the Bypass-Permissions warning dialog
#      that --dangerously-skip-permissions raises on first run.
#   2. The "I am using this for local development" choice in the
#      Loading-Development-Channels warning dialog that
#      --dangerously-load-development-channels raises every run.
# Both dialogs' first option is "1" (the safe choice) and second is the
# accept choice we want; we hit the down-arrow then enter, encoded as "2\n",
# twice with a delay between them so claude has time to render the next one.
(
  # First dialog: bypass-permissions (~6s into startup).
  sleep 6
  printf '2\n'
  # Second dialog: dev-channels (~2s after the first is accepted).
  sleep 4
  printf '2\n'
  sleep infinity
) > /tmp/claude.fifo &
( sleep infinity > /tmp/omp.fifo )    &

(
  export SESH_ROLE=implementer
  export SESH_CLASS=active
  echo "[claude-side] SESH_SESSION=$SESH_SESSION SESH_ROLE=$SESH_ROLE SESH_CLASS=$SESH_CLASS HOME=$HOME PATH=$PATH NATS_URL=$NATS_URL" >&2
  # `--strict-mcp-config` + `--mcp-config` skips the .mcp.json auto-discovery
  # path entirely (which would otherwise present a 1/2/3 trust dialog the
  # first time claude sees a new MCP server in the project). The explicit
  # config we pass is treated as operator-supplied and pre-trusted.
  #
  # `--dangerously-load-development-channels nats` opts the `nats` MCP server
  # into Claude Code's "channel" gate, which is what makes
  # `notifications/claude/channel` from the server actually drive a model
  # turn. Without it, the notifications are silently dropped by the gate
  # ("tengu_mcp_channel_gate" in claude's telemetry) and the rig sees only
  # the channel's §6.4 ack chunks until the caller times out. See
  # docs/plans/2026-05-22-integration-fix-F1-channel-flag.md for the root
  # cause writeup.
  exec script -qfc "claude --dangerously-skip-permissions --strict-mcp-config --mcp-config /opt/claude.mcp.json --dangerously-load-development-channels nats" /dev/null < /tmp/claude.fifo
) > /var/log/claude.log 2>&1 &
CLAUDE=$!
echo "[exec-wrapper] claude pid=$CLAUDE" >&2
```

### Task 3 — Add a `--debug=mcp` capture under failure for fast post-mortems

To prevent the next reviewer from having to do the same multi-day investigation, append `--debug=mcp` when an environment variable is set. Add this conditional just before the `exec script -qfc` line in Task 2's wrapper:

```bash
  CLAUDE_DEBUG_FLAGS=""
  if [ "${RIG_DEBUG_MCP:-}" = "1" ]; then
    CLAUDE_DEBUG_FLAGS="--debug=mcp"
  fi
  exec script -qfc "claude --dangerously-skip-permissions --strict-mcp-config --mcp-config /opt/claude.mcp.json --dangerously-load-development-channels nats ${CLAUDE_DEBUG_FLAGS}" /dev/null < /tmp/claude.fifo
```

Document `RIG_DEBUG_MCP=1` in `test/integration/README.md` (see Task 5).

### Task 4 — Update the claude-nats-channel README with the launch-flag requirement

**File:** `/Users/dmestas/projects/sesh-channels/claude-nats-channel/README.md`

Add a new "Launching claude-code with the channel enabled" subsection just after Quick Setup step 5 (current "Send a prompt." section). This is for operators who run claude-code directly with `--mcp-config` rather than via `/plugin install`:

```markdown
## Launching claude-code with the channel enabled

If you launch Claude Code via `/plugin install` (Quick Setup steps 1-3), the
plugin marketplace pre-registers `nats-channel` as an approved channel and
the inbound notification path "just works".

If you launch Claude Code directly with `--mcp-config` pointing at this
server's config (e.g., in containerized / CI environments), you must
*also* pass one of:

- `--channels <server-name>` — production mode; requires claude.ai
  authentication and the channel name to be on your org's
  `allowedChannelPlugins` allowlist (managed settings).
- `--dangerously-load-development-channels <server-name>` — local-dev
  mode; shows a one-time confirmation dialog at startup.

`<server-name>` is the key under `mcpServers` in your `--mcp-config`
file (e.g., `nats` for the standard installation).

Without either flag, the channel's MCP `notifications/claude/channel`
notifications are silently dropped by Claude Code's channel gate, and
the model never starts a turn in response to an inbound NATS prompt —
the channel still emits its §6.4 ack chunks, so callers see acks but no
response, until they time out.

This requirement is enforced inside the claude-code binary at the
`tengu_mcp_channel_gate` telemetry boundary; the MCP server's
`experimental.claude/channel` capability handshake (declared in
`server.ts:586-589`) is necessary but not sufficient.
```

### Task 5 — Update the rig README

**File:** `/Users/dmestas/projects/sesh/test/integration/README.md` — if absent, add this section; if present, append.

```markdown
## Claude Code channel-enablement (F1 workaround)

The rig launches `claude` with `--dangerously-load-development-channels nats`
in addition to `--strict-mcp-config --mcp-config /opt/claude.mcp.json`. The
former is what makes Claude Code treat the `nats` MCP server as a *channel*
(inbound-push enabled) instead of a plain tool provider. Without the flag,
`notifications/claude/channel` from the channel server are silently dropped
by claude-code, and case 03/05/06 hang.

The rig also auto-feeds two `2\n` inputs over the claude FIFO:

1. Bypass-Permissions warning dialog (~6 s after launch)
2. Loading-Development-Channels warning dialog (~10 s after launch)

To debug the channel gate, set `RIG_DEBUG_MCP=1` in the rig's environment;
`--debug=mcp` is then appended to the claude invocation and the relevant
gate decisions appear in `/var/log/claude.log`.
```

### Task 6 — Re-run the rig and confirm green

**Command:**

```bash
cd /Users/dmestas/projects/sesh/test/integration
./scripts/run.sh  # whichever launcher the rig ships; falls back to `docker compose up` if absent
```

Then read `/var/artifacts/results.txt`. The expected outcome after F1 lands:

- Case 03 (`03-prompt-claude`) — PASS. Response contains "SUCCESS" within 60 s.
- Case 05 (`05-attachment`) — PASS. Response contains "10".
- Case 06 (`06-cross-adapter`) — PASS. Both legs return non-empty replies.
- Case 04 (`04-prompt-omp`) — orthogonal; tracked by F2. If F2 hasn't landed yet, case 04 may still fail; see F2 plan.

### Task 7 — Commit the rig change

One commit, one rig-side fix:

```bash
cd /Users/dmestas/projects/sesh
git checkout -b feat/integration-rig-f1-channel-flag
git add test/integration/entrypoint.sh test/integration/README.md
git commit -m "fix(test/integration): pass --dangerously-load-development-channels to enable claude channel gate (closes F1)"
```

Open a PR; do NOT push to main directly.

### Task 8 — Commit the sesh-channels README change

```bash
cd /Users/dmestas/projects/sesh-channels
git checkout -b docs/claude-channel-launch-flag
git add claude-nats-channel/README.md
git commit -m "docs(claude-nats-channel): document --dangerously-load-development-channels requirement for direct --mcp-config launches"
```

Open a PR.

## Dependencies

- None for the rig change (Tasks 2-7). Lands independently.
- Tasks 4 / 8 (sesh-channels README) is documentation only; ships in parallel.
- F2 (omp session env-var) is orthogonal — F1 fixes claude (case 03/05/06), F2 fixes the OMP-side session-label propagation that case 04 depends on. Land in either order.

## Optional follow-ups (not part of this AFK plan)

- File an upstream feedback at anthropic/claude-code (third-party repo per CLAUDE.md) suggesting that the dev-channels dialog be either skippable via a managed-settings key (similar to `skipDangerousModePermissionPrompt`) OR that a non-TTY mode auto-accept it when `--dangerously-load-development-channels` is explicitly passed (the operator has already opted-in). **Per the upstream-investigation-before-filing skill + the no-third-party-filing policy, this is an operator-only decision — surface it, do not file it.**
- Capture `--debug=mcp` output unconditionally in CI mode (so future channel-gate regressions surface in `results.txt`).
