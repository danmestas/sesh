# `sesh-ref-agent` — the executable spec

`sesh-ref-agent` is sesh's reference Synadia Agent Protocol v0.3 agent.
It is the executable counterpart to
[`docs/synadia-agents-on-sesh.md`](./synadia-agents-on-sesh.md) (the
contract doc): every future test, plugin, or Go SDK should validate its
behavior against this binary.

The agent is intentionally minimal:

- **Echo only.** Each `prompt` is split into UTF-8-safe chunks and sent
  back as `response` chunks. No model, no memory, no side effects.
- **No attachments.** Declares `attachments_ok: false`; requests with
  `attachments[]` are rejected with `400`.
- **No mid-stream queries.** Agents in production may emit `query`
  chunks; the reference agent never does.

The point is to prove the contract is implementable and to give
upstream callers (TS/Python SDKs, future Go SDK) something concrete to
test against.

## Quick start

```sh
# Inside a `sesh up` session — picks up SESH_SESSION and ~/.sesh/hub.url.
sesh-ref-agent

# Standalone against any NATS broker:
NATS_URL=nats://127.0.0.1:4222 sesh-ref-agent --agent=demo

# Verify it registered:
nats req '$SRV.INFO.agents' '' --replies=0 --timeout=2s

# Drive it:
nats req agents.prompt.echo.$USER 'hello, world' --replies=0 --timeout=2s
```

## Configuration

The CLI surface is one optional flag. Everything else is environment-derived.

| Source                        | Sets             | Default                              |
|-------------------------------|------------------|--------------------------------------|
| `--agent=<name>`              | `metadata.agent` | `echo`                               |
| `$SESH_OWNER`                 | `metadata.owner` | `$USER`, else `os/user.Current()`    |
| `$SESH_SESSION`               | `metadata.session` | unset → session-less subjects     |
| `$NATS_URL`                   | broker URL       | see below                            |
| `.sesh/sessions/<label>.json` | broker URL (`nats_url` field) | walked up from CWD         |
| `~/.sesh/hub.url`             | broker URL       | last-resort fallback                 |

`Config.Interval` defaults to 30s (Synadia §8.2 recommended cadence).
The reference agent doesn't expose this on the CLI — heartbeats every
30s match the spec's recommendation and there is no production reason
to tune it.

## What it actually does (Synadia §12 conformance map)

Each row maps a Synadia §12 agent-side requirement to its
implementation. Line numbers refer to `internal/refagent/agent.go`.

| §12 item                                                                                                              | Implementation                                                                                                                |
|-----------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------|
| Register as NATS micro service with `name = "agents"`                                                                | `register()` → `micro.AddService(..., Name: serviceName)` (`serviceName = "agents"`)                                          |
| Declare `metadata.agent`, `metadata.owner`, `metadata.protocol_version = "0.3"`; add `metadata.session` when session-aware | `register()` builds the metadata map; `session` is conditional on `cfg.Session != ""`                                |
| Register `prompt` endpoint with queue group `"agents"` and metadata `max_payload`, `attachments_ok`                  | `register()` → `svc.AddEndpoint(promptEndpoint, ...)` with `WithEndpointQueueGroup("agents")` + `WithEndpointMetadata(...)`   |
| Register `status` endpoint with queue group `"agents"`; reply with §8.3 heartbeat-shaped payload                     | `register()` → second `AddEndpoint`; handler is `handleStatus` (builds via `heartbeatPayload`)                                |
| Accept both JSON envelopes and plain-text shorthand on `prompt`                                                       | `parsePrompt` — JSON detection on first non-whitespace byte (`{`); plain-text otherwise                                       |
| Reject malformed envelopes, empty payloads, invalid base64, oversize, attachments when `attachments_ok=false` with `400` | `parsePrompt` returns errors; `handlePrompt` translates to `req.Error("400", "malformed_request", body)` + terminator     |
| Tolerate and preserve unknown envelope fields                                                                         | `parsePrompt` decodes only `prompt` + `attachments`; unknown fields fall on the floor without error                            |
| Emit `{"type":"status","data":"ack"}` as first chunk, before any latency-inducing work                                | `handlePrompt` publishes `ackChunk()` to the reply subject before `parsePrompt`                                               |
| Emit response stream per §6: typed chunks in order, zero-byte headerless terminator                                  | `handlePrompt` publishes `responseChunk(...)` per `splitUTF8` fragment, then `req.Respond(nil)` for the terminator             |
| Publish heartbeats on `agents.hb.<agent>.<owner>.<name>` at configured cadence with all §8.3 fields                  | `Run` ticker fires `publishHeartbeat`; payload built by `heartbeatPayload`                                                    |
| Respond to `$SRV.PING.agents` and `$SRV.INFO.agents` via micro service framework                                     | Provided by `nats.go/micro` — no agent code required                                                                          |
| Issue mid-stream queries per §7 (conditional)                                                                         | **Not implemented** — reference agent is echo-only by design (see contract doc, "Non-goals")                                  |
| Use `respondError` per §9; `Nats-Service-Error-Code` from §9.2 taxonomy                                              | `req.Error("400", ...)` — sets both `Nats-Service-Error-Code` and `Nats-Service-Error` headers via the micro framework         |

## File layout

```
cmd/sesh-ref-agent/main.go    — flag parsing + signal handling (~50 LOC)
internal/refagent/agent.go    — Config, Run, all internals       (~450 LOC)
internal/refagent/agent_test.go — unit + behavior tests          (~500 LOC)
test/wire-compat/wire-compat.ts — upstream TS-SDK driver
test/wire-compat/run.sh       — runner: broker + agent + tsx
```

`internal/` rather than `pkg/` is deliberate. The two-symbol surface
(`Config`, `Run`) is fine for the reference, but no third-party
stability promise is made until a second Go consumer exists and a real
SDK is extracted.

## Test layers

Three layers, in order of speed:

1. **Unit tests against Appendix B.** Byte-for-byte assertions on the
   chunk encodings (B.4, B.6, B.9), the heartbeat shape (B.11), and the
   error wire format (B.10 derivative). These tests run in milliseconds
   and have no external dependencies.

2. **Behavior tests with embedded NATS.** A fresh `nats-server`
   instance per test, driven by `Run` in-process. Covers
   `$SRV.INFO.agents` shape, leading-ack ordering, malformed → 400,
   status endpoint payload, heartbeat cadence, clean shutdown.

3. **Wire-compat against the upstream TS SDK.** The
   `@synadia-ai/agents` TypeScript SDK — which knows nothing about
   sesh — discovers the running agent via `Agents.discover()`, drives
   the prompt endpoint, and asserts ack-first + content match +
   terminator. Run via `test/wire-compat/run.sh` (no CI yet; sesh has
   no `.github/workflows/`).

```sh
# Unit + behavior:
go test ./internal/refagent/

# Wire-compat:
test/wire-compat/run.sh
```

## What's intentionally absent

- **Stateful conversation memory** — the Python reference agent has a
  per-session deque; the sesh reference doesn't, because the contract
  doesn't require it and statelessness keeps the spec proof minimal.
- **Attachment handling** — `attachments_ok: false`. A real harness
  with model integration adds this.
- **Mid-stream queries (§7)** — see contract doc, "Non-goals."
- **A pluggable handler interface** — `Run` does echo and nothing else.
  When sesh grows a real Go SDK, the prompt handler will become a
  callback; for now there is no second consumer to inform that
  abstraction.

## Worked example: `$SRV.INFO.agents`

Running `sesh-ref-agent --agent=echo` as user `alice` inside session
`demo` against a 1MB-max-payload broker produces:

```json
{
  "name": "agents",
  "id": "VRGYZ5C8U2H38LBQ4NXWPK",
  "version": "0.1.0",
  "description": "echo reference agent (sesh-ref-agent)",
  "metadata": {
    "agent": "echo",
    "owner": "alice",
    "session": "demo",
    "protocol_version": "0.3"
  },
  "endpoints": [
    {
      "name": "prompt",
      "subject": "agents.prompt.echo.alice.demo",
      "queue_group": "agents",
      "metadata": {
        "max_payload": "1MB",
        "attachments_ok": "false"
      }
    },
    {
      "name": "status",
      "subject": "agents.status.echo.alice.demo",
      "queue_group": "agents"
    }
  ]
}
```

This shape is verified by `TestServiceRegistration` in `agent_test.go`.

## Further reading

- [`docs/synadia-agents-on-sesh.md`](./synadia-agents-on-sesh.md) — the
  contract this agent implements.
- Synadia Agent Protocol v0.3 — upstream spec at `core-protocol.md`.
- `internal/refagent/agent.go` — the implementation. The whole module
  is ~450 LOC; reading it end-to-end is the fastest way to understand
  the protocol.
