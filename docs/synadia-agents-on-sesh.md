# Synadia agents on sesh

An agent running inside a sesh session participates in
[Synadia Agent Protocol v0.3](https://github.com/synadia-io/agent-sdk-docs)
discovery by registering as a NATS micro service. This document is the
contract — every requirement cites a Synadia § number so a reader who knows
the upstream spec can cross-reference directly. No SDK is shipped from sesh;
agents bring `@synadia-ai/*` (TypeScript/Python) or a future Go equivalent.

Sesh does not enforce this contract; following it lets agents on the mesh be
discovered via `$SRV.INFO.agents` without per-consumer protocol negotiation.

---

## 1. Identity

Agents register using the Synadia §3.2 required metadata table. All four
fields are compulsory unless marked conditional.

| Field              | Source                                                        | Required              |
|--------------------|---------------------------------------------------------------|-----------------------|
| `agent`            | Agent's own identifier (e.g. `claude-code`, `pi`, `worker-001`) | Yes                |
| `owner`            | `$SESH_OWNER` env if set, else `$USER`                        | Yes                   |
| `session`          | `$SESH_SESSION` env (the `sesh up --session=<label>` label)  | When session-aware    |
| `protocol_version` | `"0.3"`                                                       | Yes                   |

`session` MAY be omitted or set to `"default"` for session-less harnesses
(§3.2). `owner` MUST match the 4th token of every endpoint subject (§2).

The NATS micro service framework assigns a per-instance opaque `id`
(e.g. `VMKS6MHK71PCPWGY38A7N5`). That value is the `instance_id` in
heartbeat payloads (§8.3). It is not echoed in the metadata object — callers
read it from `$SRV.INFO` (§3.4).

The service's top-level fields follow §3.1:

| Field         | Value                                                                      |
|---------------|----------------------------------------------------------------------------|
| `name`        | `"agents"` — the discovery filter used by `$SRV.PING.agents`              |
| `version`     | Semver of the harness implementation (not the protocol), e.g. `"1.4.0"`   |
| `description` | Human-readable string surfaced by `nats micro list` / `nats micro info`   |
| `metadata`    | Object per the table above                                                 |

---

## 2. NATS connection

Agents discover the bus URL in this priority order:

1. `$NATS_URL` environment variable (set by the caller or launch script).
2. `.sesh/sessions/<label>.json` → `nats_url` field — walk up from CWD until
   found.
3. `~/.sesh/hub.url` — the hub's leaf URL, written at hub startup.

Connection failure after exhausting all three is a startup error; the agent
MUST NOT silently proceed with no bus.

The JSON file at `.sesh/sessions/<label>.json` also carries `leaf_url` and
`fossil_url` — agents that need Fossil or sub-leaf access read from the same
file. `$SESH_SESSION` (the label) is set by `sesh up` and names the JSON
file.

---

## 3. Subjects

Sesh strongly recommends the Synadia §2 verb-first default. The three
canonical subjects for a session-aware agent are:

| Purpose    | Subject pattern                                     | §    |
|------------|-----------------------------------------------------|------|
| Prompt     | `agents.prompt.<agent>.<owner>.<session>`           | §2   |
| Status     | `agents.status.<agent>.<owner>.<session>`           | §8.7 |
| Heartbeat  | `agents.hb.<agent>.<owner>.<session>`               | §8.1 |

Token rules (§2.2): tokens MUST NOT begin with `$`; SHOULD use only
`a`–`z`, `0`–`9`, `-`, `_`; each token SHOULD be 1–63 characters; full
subject SHOULD stay under 256 characters. Sanitize user-supplied identifiers
(replace `.` with `-`).

Verb-first subjects are recommended, not required. Agents MAY choose other
subject layouts, but the subjects they actually register are the
authoritative source — callers MUST read the subject from `$SRV.INFO` rather
than constructing it from identity tokens (§12 caller checklist).

Discovery via `$SRV.PING.agents` / `$SRV.INFO.agents` works regardless of
subject layout because the service `name` is always `"agents"` (§4).

---

## 4. Endpoints

Two endpoints are required (§3, §8.7). Both MUST use queue group `"agents"`.

| Endpoint | Required | Queue group | Endpoint metadata                                              | §        |
|----------|----------|-------------|----------------------------------------------------------------|----------|
| `prompt` | Yes      | `"agents"`  | `max_payload` (from NATS `INFO.max_payload`), `attachments_ok` (agent-declared) | §3, §3.3 |
| `status` | Yes      | `"agents"`  | none required                                                  | §8.7     |

### 4.1 `prompt` endpoint

The queue group `"agents"` MUST be set explicitly — framework defaults differ
across SDK implementations and break interoperability (§3.3). Multiple
physical instances sharing the same `agent`/`owner`/`session` identity use
the same endpoint subjects; the queue group load-balances across them (§3.4).

Endpoint metadata (§2.1):

- `max_payload` — read from the NATS server's `INFO.max_payload` at connect
  time and echoed verbatim (e.g. `"1MB"`). Callers use this to enforce
  request size limits before publishing (§5.4).
- `attachments_ok` — boolean; declared by the agent. Agents that cannot
  process binary attachments set `false`; callers that see `false` MUST NOT
  send attachments (§5.4).

In `@nats-io/services` (TypeScript):

```ts
svc.addEndpoint("prompt", {
  subject: `agents.prompt.${agent}.${owner}.${session}`,
  queue:   "agents",
  metadata: { max_payload: "1MB", attachments_ok: true },
  handler: promptHandler,
});
```

### 4.2 `status` endpoint

Registered with queue group `"agents"`. The request body is currently
reserved — agents MUST ignore it (§8.7). The reply MUST be a §8.3
heartbeat-shaped payload built fresh per request (see §6 below).

---

## 5. Request envelope

The `prompt` endpoint accepts two wire shapes (§5.1, §5.3):

**Plain-text shorthand** — body is a UTF-8 string:

```
summarize the attached report
```

Parsed as `{ "prompt": "<body>" }`.

**JSON envelope** — body is a JSON object:

```json
{
  "prompt":      "summarize the attached report",
  "attachments": [{ "name": "report.pdf", "data": "<base64>" }],
  "metadata":    { "traceparent": "00-..." }
}
```

Agents MUST accept both forms (§5.3) and MUST tolerate unknown envelope
fields (§5.6). Rejection rules (§9.2):

| Condition                                                    | Code |
|--------------------------------------------------------------|------|
| Invalid envelope, empty payload, invalid base64             | 400  |
| Attachment present but `attachments_ok=false`               | 400  |
| Request exceeds `max_payload`                               | 400  |
| Authentication missing                                       | 401  |
| Caller authenticated but not authorized                      | 403  |
| Not found                                                    | 404  |
| Conflict with current agent state                           | 409  |
| Rate limited                                                 | 429  |
| Internal error                                               | 500  |

---

## 6. Streaming contract

The `prompt` endpoint responds to the caller's reply subject with a sequence
of typed JSON chunks terminated by a zero-byte headerless message (§6).

### 6.1 Chunk shape (Synadia §6.2)

```json
{ "type": "<type>", "data": <value> }
```

| Chunk type  | `data` shape          | When                                      | §    |
|-------------|----------------------|-------------------------------------------|------|
| `status`    | `"ack"`              | MUST be first, before any latency work    | §6.4 |
| `response`  | string or object     | Content fragment                          | §6.3 |
| `query`     | object               | Mid-stream clarification request (§7)     | §7.1 |

### 6.2 Stream lifecycle

1. **Ack** — emit `{"type":"status","data":"ack"}` immediately on the reply
   subject, before doing any latency-inducing work (§6.4).
2. **Chunks** — emit `{"type":"response","data":"..."}` fragments in order
   (§6.3).
3. **Mid-stream queries** — if the agent needs clarification, emit
   `{"type":"query","data":{...}}` and await a reply on the query's own
   reply subject (§7.1–7.3).
4. **Terminator** — publish a zero-byte body with no NATS headers (§6.5).
5. **Errors** — if an error occurs, set `Nats-Service-Error-Code` and
   `Nats-Service-Error` headers; a body MAY carry JSON with `error` +
   `message` fields. Error message precedes the terminator (§9.3).

Unknown chunk types MUST be silently ignored by callers; the stream
continues (§6.6).

### 6.3 Cancellation (§6.7)

Callers signal cancellation by letting the reply subject expire (no active
subscriber). Agents SHOULD monitor the reply subject and abort when it
disappears.

---

## 7. Liveness

Agents MUST publish heartbeats and MUST respond to `status` requests (§8).

### 7.1 Heartbeat pub/sub (§8.1–§8.3)

Subject: `agents.hb.<agent>.<owner>.<session>` (no queue group — pub/sub).

Recommended cadence: **30 s** (§8.2). Payload (§8.3):

```json
{
  "agent":       "claude-code",
  "owner":       "aconnolly",
  "session":     "synadia-com-2",
  "instance_id": "VMKS6MHK71PCPWGY38A7N5",
  "ts":          "2026-04-28T14:23:01Z",
  "interval_s":  30
}
```

| Field         | Type   | Required             | Notes                                    |
|---------------|--------|----------------------|------------------------------------------|
| `agent`       | string | Yes                  | Matches §3.2 metadata                    |
| `owner`       | string | Yes                  | Matches §3.2 metadata                    |
| `session`     | string | When session-aware   | Matches §3.2 metadata                    |
| `instance_id` | string | Yes                  | Framework-assigned service `id` (§3.4)   |
| `ts`          | string | Yes                  | RFC 3339 UTC                             |
| `interval_s`  | number | Yes                  | Heartbeat cadence in seconds             |

Observers key liveness on `instance_id` and consider an instance offline
after 3× `interval_s` of silence (§8.2).

**Convergent liveness with task pullers.** Sesh task-puller status events
(`sesh.task.*.*.events`) carry the same six §8.3 fields (`agent`, `owner`,
`session`, `instance_id`, `ts`, `interval_s`) plus a task-specific tail
(`event`, `task_id`, `due_at`). A liveness tracker subscribed to both
`agents.hb.*.*.*` and `sesh.task.*.*.events` can handle both with one
parser, keying on `instance_id`. See
[`docs/task-management.md § Status events`](./task-management.md#status-events)
for the full field table and worked example.

### 7.2 Shutdown heartbeat (§8.6)

Before a graceful shutdown agents SHOULD publish one final heartbeat with an
empty payload to the same heartbeat subject, signalling immediate offline.

### 7.3 Status endpoint reply (§8.7)

The `status` endpoint MUST reply with a freshly built §8.3 payload on every
request — same JSON schema as the periodic heartbeat. Callers MAY feed the
reply into the same liveness tracker (keyed on `instance_id`).

The same builder SHOULD produce both the periodic heartbeat and the status
reply to keep them in lockstep (§8.7.1). Errors during payload construction
MUST be returned as `Nats-Service-Error-Code: 500` (§8.7.1).

---

## 8. Sesh-specific guidance (informative)

These conventions are not required by the Synadia protocol but ensure clean
coexistence with other sesh agents on the mesh.

**Project code** — a stable identifier for the project derived at first
`sesh up` and persisted at `<cwd>/.sesh/project-code`. Available as
`$SESH_PROJECT_CODE` to spawned processes. Use it to scope JetStream bucket
names (see [`docs/scoped-memory.md`](./scoped-memory.md)).

**Scoped memory** — agents sharing state on the bus SHOULD use the five-scope
bucket naming convention from [`docs/scoped-memory.md`](./scoped-memory.md):
`sesh_session_<project>_<session>` for within-session state,
`sesh_workflow_<trace-id-8hex>` for cross-session workflow state.

**Trace propagation** — requests arriving at the `prompt` endpoint MAY carry
a `traceparent` header (W3C) or a `traceparent` field in the JSON envelope.
Agents SHOULD propagate it outbound per
[`docs/message-envelope.md`](./message-envelope.md). A plan to canonicalize
this as Synadia §5 upstream is tracked in sesh#51.

**Hub does not register an `agents` service** — the sesh hub is a
substrate, not an agent. Only harnesses running inside a session register
under `name = "agents"`. This is a locked decision for v1.

---

## 9. Worked example: `$SRV.INFO.agents` response

A `claude-code` instance in session `synadia-com-2` owned by `aconnolly`
returns the following on `$SRV.INFO.agents` (Synadia Appendix B.12):

```json
{
  "name": "agents",
  "id": "VMKS6MHK71PCPWGY38A7N5",
  "version": "0.3.0",
  "description": "Claude Code — synadia-com-2",
  "metadata": {
    "agent": "claude-code",
    "owner": "aconnolly",
    "session": "synadia-com-2",
    "protocol_version": "0.3"
  },
  "endpoints": [
    {
      "name": "prompt",
      "subject": "agents.prompt.claude-code.aconnolly.synadia-com-2",
      "queue_group": "agents",
      "metadata": {
        "max_payload": "1MB",
        "attachments_ok": true
      }
    },
    {
      "name": "status",
      "subject": "agents.status.claude-code.aconnolly.synadia-com-2",
      "queue_group": "agents"
    }
  ]
}
```

Notes on this shape:

- `id` is the framework-assigned instance id — the §8.3 `instance_id`.
- `version` is the harness's own semver string (per Synadia §3.1 — the
  agent's implementation version), not the protocol version. The protocol
  version lives in `metadata.protocol_version`.
- `metadata.session` is present because `claude-code` is session-aware.
- `endpoints[].queue_group` is `"agents"` for both endpoints — reported by
  the micro service framework and visible to callers without extra work.
- `attachments_ok` is a boolean per Synadia §2.1.

A `status` request/reply pair for the same instance (Appendix B.11a):

```
Request  → agents.status.claude-code.aconnolly.synadia-com-2  (empty body)

Reply    ← {"agent":"claude-code","owner":"aconnolly","session":"synadia-com-2",
             "instance_id":"VMKS6MHK71PCPWGY38A7N5",
             "ts":"2026-04-28T14:23:01Z","interval_s":30}
```

---

## 10. Synadia §12 conformance map

One line per agent-side item in the §12 checklist:

| §12 item                                                                                         | sesh mapping                                                                           | Status   |
|--------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------|----------|
| Register as NATS micro service with `name = "agents"`                                           | §1 above — mandatory                                                                   | Required |
| Declare `metadata.agent`, `metadata.owner`, `metadata.protocol_version = "0.3"`; add `metadata.session` when session-aware | §1 above; `session` = `$SESH_SESSION`                           | Required |
| Register `prompt` endpoint with queue group `"agents"` and metadata `max_payload`, `attachments_ok` | §4.1 above; `max_payload` from NATS `INFO`; `attachments_ok` agent-declared       | Required |
| Register `status` endpoint with queue group `"agents"`; reply with §8.3 heartbeat-shaped payload | §4.2 and §7.3 above                                                                  | Required |
| Accept both JSON envelopes and plain-text shorthand on `prompt`                                  | §5 above                                                                               | Required |
| Reject malformed envelopes, empty payloads, invalid base64, oversize requests, attachments when `attachments_ok=false` with `400` | §5, error table                                        | Required |
| Tolerate and preserve unknown envelope fields                                                    | §5 above (§5.6)                                                                        | Required |
| Emit `{"type":"status","data":"ack"}` as first chunk, before any latency-inducing work          | §6.2 above                                                                             | Required |
| Emit response stream per §6: typed chunks in order, zero-byte headerless terminator             | §6 above                                                                               | Required |
| Publish heartbeats on `agents.hb.<agent>.<owner>.<name>` at configured cadence with all §8.3 fields | §7.1 above; recommended cadence 30 s                                              | Required |
| Respond to `$SRV.PING.agents` and `$SRV.INFO.agents` via micro service framework               | Provided automatically by `@nats-io/services` / equivalent SDK                         | Required |
| Issue mid-stream queries per §7 when used                                                        | §6.2 above (query chunk)                                                               | Conditional |
| Use `respondError` per §9; `Nats-Service-Error-Code` from §9.2 taxonomy                         | §5 error table above                                                                   | Required |

---

## Further reading

- [`docs/synadia-comparison.md`](./synadia-comparison.md) — layer map and rationale for adopting Synadia §3
- [`docs/orch-bridge.md`](./orch-bridge.md) — historical context: the ad-hoc `orch.*` subjects this contract supersedes
- [`docs/scoped-memory.md`](./scoped-memory.md) — five-scope bucket naming for shared state
- [`docs/task-management.md`](./task-management.md) — task pull protocol for work distribution
- [`docs/message-envelope.md`](./message-envelope.md) — W3C traceparent propagation via NATS headers
- Synadia Agent Protocol v0.3 — upstream spec at `core-protocol.md`
- Synadia Appendix B.12 — byte-level `$SRV.INFO.agents` wire example
