# Synadia Agent Protocol — Extensions Brainstorm

**Date:** 2026-05-24
**Status:** Brainstorm (not a proposal — exploration to inform a future proposal)
**Reference protocols:** [A2A v1.0.0](https://a2a-protocol.org/latest/specification/), [ACP](https://agentcommunicationprotocol.dev/) (merged into A2A 2026), [MCP 2025-06-18](https://modelcontextprotocol.io/)

## Goal

Map the Synadia Agent Protocol v0.3's current surface against the two leading agent-interop protocols (A2A and ACP — now consolidated), identify gaps, and brainstorm what verbs / events / shapes are worth adding. Output is a ranked menu of extensions with sesh-specific rationale, not a decided plan.

## Section 1 — Current Synadia surface

Verbs / subject hierarchies in v0.3 today:

| Subject | Purpose | Pattern |
|---|---|---|
| `agents.prompt.<agent>.<owner>.<name>` | Send a prompt; receive streamed reply chunks | Request/reply with mandatory `{type:status,data:ack}` leading chunk + zero-or-more `{type:response,data:...}` + empty-body terminator (§6.4–§6.5) |
| `agents.hb.<agent>.<owner>.<name>` | Publish-only periodic heartbeat (§8.2 cadence 30s) | Publish |
| `agents.status.<agent>.<owner>.<name>` | On-demand status query — replies with hb-shaped payload (§8.7) | Request/reply |
| `$SRV.INFO.agents` | NATS micro discovery — returns full metadata + endpoints for all responders | Request/reply |
| `agents.hb.<agent>.<owner>.<name>` final-heartbeat | Empty body = "I'm going down now" (§8.6) | Publish |

**Coordination subjects added by sesh #91/#94 (not yet upstream Synadia):**

| Subject | Purpose |
|---|---|
| `agents.prompt.<machine>.<project>.<session>.<role>` | 6-token role-pool prompt (queue group = role name; work-stealing across active workers in pool) |
| `agents.prompt.<machine>.<project>.<session>.<role>.<inst>` | 7-token direct addressing of one instance |
| `agents.report.<machine>.<project>.<session>.>` | Observer-class subscribe; explicitly excluded from `prompt.*` per coord-tier design |

**In-stream control:**
- §6.7 cancellation via `Nats-Service-Error-Code` headers
- §7 mid-stream `{type:query,data:{...}}` chunks for permission prompts / interactive negotiation
- §8.6 shutdown signaled via final empty heartbeat

**Attachments:** carried inside the prompt envelope as base64 in `attachments[]`. **No separate attachment verb** — user's mental model of "attachments verb" is actually inline-on-prompt today.

## Section 2 — A2A protocol surface (Google A2A v1.0.0)

**A2A is JSON-RPC / gRPC / REST + SSE — NOT NATS subject-based. Mapping is conceptual.**

### A2A core operations

| Operation | JSON-RPC method | Purpose |
|---|---|---|
| `SendMessage` | unary | Send a message, get a synchronous response |
| `SendStreamingMessage` | stream (SSE) | Send a message, stream back chunks (Task, Message, status updates, artifact updates) |
| `GetTask` | unary | Retrieve a task's current state by ID |
| `ListTasks` | unary | List all tasks the agent has (with filtering) |
| `CancelTask` | unary | Request cancellation of an in-flight task |
| `SubscribeToTask` | stream | Re-attach to an in-progress task's event stream (after disconnect) |
| `CreateTaskPushNotificationConfig` | unary | Configure a webhook for off-bus task updates |
| `Get/List/Delete TaskPushNotificationConfig` | unary | Webhook config management |
| `GetExtendedAgentCard` | unary | Discovery — returns the signed AgentCard with skills/capabilities |

### A2A Task lifecycle (the big idea)

Every interaction is a **Task** with an explicit state machine:

```
TASK_STATE_SUBMITTED
  ↓
TASK_STATE_WORKING ←──┐
  ↓                   │
  ├──→ TASK_STATE_COMPLETED (terminal)
  ├──→ TASK_STATE_FAILED (terminal)
  ├──→ TASK_STATE_CANCELED (terminal)
  ├──→ TASK_STATE_REJECTED (terminal — agent refused)
  ├──→ TASK_STATE_INPUT_REQUIRED → (waits for caller) → WORKING
  └──→ TASK_STATE_AUTH_REQUIRED → (waits for credentials) → WORKING
```

**Streaming event types** on the SSE channel:
- `Task` (initial + final snapshots)
- `Message` (one-shot reply)
- `TaskStatusUpdateEvent` (state transition)
- `TaskArtifactUpdateEvent` (artifact added/updated)

### A2A data model

- **Task** — persistent, identifiable, has state
- **Message** — one turn of conversation
- **Artifact** — named output (file, structured data) separate from message text
- **Part** — message content fragment (text, file ref, structured data, embedded UI)
- **AgentCard** — JCS-canonicalized signed JSON describing the agent's identity + capabilities + skills + auth requirements
- **Extension** — namespaced custom additions (think OpenAPI's `x-` headers)

### A2A authentication

Standard web patterns (bearer tokens, OAuth). In-task auth handled via `TASK_STATE_AUTH_REQUIRED` + out-of-band credential delivery.

## Section 3 — Gap analysis: Synadia vs A2A

| A2A concept | Synadia v0.3 equivalent | Gap severity |
|---|---|---|
| `SendMessage` (unary) | `nats req agents.prompt...` with no streaming | ✓ Conceptually covered |
| `SendStreamingMessage` (SSE) | `agents.prompt` chunked replies | ✓ Same idea, different transport |
| `GetTask` (by id) | **None** — no task id concept | ✗ Big gap |
| `ListTasks` | **None** | ✗ |
| `CancelTask` (by id) | Cancellation via `Nats-Service-Error-Code` header on the reply subject | ⚠ Partial — only works for the in-flight stream you're holding; no "cancel this task from a different client" |
| `SubscribeToTask` (re-attach) | **None** — disconnect = lost stream | ✗ Big gap |
| `PushNotificationConfig` (webhooks) | NATS is the bus → any client can subscribe | ✓ NATS makes this trivial; no equivalent needed |
| `GetExtendedAgentCard` (signed) | `$SRV.INFO.agents` returns metadata, no signing | ⚠ Partial — sesh has rich metadata via #91/#94 but no canonical Card concept |
| `TaskState` (explicit lifecycle) | Implicit in chunk flow (ack → responses → terminator) | ⚠ Partial — works for happy path; weak for interrupted/rejected states |
| `TASK_STATE_INPUT_REQUIRED` | §7 mid-stream query chunks | ⚠ Partial — informal; client SDK has to recognize query chunks as "the agent is blocked" |
| `TASK_STATE_AUTH_REQUIRED` | **None** | ✗ |
| `TASK_STATE_REJECTED` | Error reply via Nats-Service-Error-Code 400/500 | ⚠ Partial — distinguishes "I rejected" from "I crashed" weakly |
| `Artifact` (named outputs) | **None** — outputs are textual chunks only | ✗ Big gap for binary/file outputs |
| `Extension` (namespaced custom) | Custom metadata keys via §5.6/§12 | ✓ Covered |
| Lifecycle hook events (Stop, PreToolUse...) | Filesystem markers in orch; not on the bus | ✗ Sesh-side gap |

## Section 4 — Sesh-specific verbs the user asked about

### `shutdown` verb

Today: agent goes down → publishes empty-body heartbeat (§8.6). That's outbound-only. There's no inbound "please shut down" request from the operator.

**A2A doesn't have this either** — agents shut down by their own logic, not by external command.

**Sesh value:** high. Operators routinely want to drain a worker (`sesh down`, `sesh-ops kill <agent>`). Currently they have to find the OS process or use orch's tmux integration. A bus verb would centralize this.

**Proposed:** `agents.shutdown.<agent>.<owner>.<name>` — request/reply. Agent acks the request, sets internal shutdown flag, drains in-flight, emits final-heartbeat, exits. Optional `mode` field: `graceful` (drain) vs `immediate` (just stop).

### `interrupt` verb

Distinct from `cancel`. Cancel = stop this specific task. Interrupt = pause-and-redirect this agent without ending its session.

**A2A doesn't have a distinct interrupt** — closest is INPUT_REQUIRED + a new message on the existing task.

**Sesh value:** medium. Operator wants to interject into a running agent ("wait, do X first"). Today they'd have to cancel + re-prompt or use the agent's own TUI.

**Proposed:** `agents.interrupt.<agent>.<owner>.<name>` with a payload that becomes a high-priority next prompt. Agent's adapter raises a §7-style query / inbound channel notification. Could also be modeled as just "send a prompt with a `priority:interrupt` extension flag" — that may be simpler.

### `hook status` verb (lifecycle events)

Today: orch's lifecycle hooks (Stop, PreToolUse, SessionStart, etc.) ran as filesystem-marker scripts. They've been migrated to the Synadia Agent Protocol shim path (per orch#94), but **not formalized as bus verbs** — each adapter handles them differently.

**A2A models these as `TaskStatusUpdateEvent` on the streaming channel.**

**Sesh value:** high. Today there's no way to subscribe to "give me every agent's PreToolUse events on the mesh." Useful for: auditing tool use, security monitoring, automating approvals, building observability dashboards.

**Proposed:** `agents.event.<event-type>.<agent>.<owner>.<name>` — publish-only. Event types: `started`, `stopped`, `paused`, `resumed`, `tool_use_pre`, `tool_use_post`, `session_start`, `session_stop`, `attention_needed`, plus a `custom.<adapter>.<name>` escape hatch. Payloads vary by event; common envelope has `timestamp`, `agent_id`, `event_type`, `data`.

## Section 5 — Tier ranking of all proposed extensions

### Tier 1 — High value, low disruption, additive

**A. `agents.event.*` namespace — lifecycle events**
- New publish-only subject hierarchy
- Replaces orch's filesystem markers with first-class bus events
- Subscribers: observability tools, security auditors, the planned `sesh mesh watch`
- Backward-compatible (purely additive)
- **Recommend first**

**B. `agents.shutdown.*` — graceful shutdown request**
- One new request/reply endpoint per agent micro-service
- Sesh-ops `kill` and `sesh down` become bus-driven instead of process-driven
- Mirrors §8.6 shutdown semantics but inbound
- Small surface, big UX win

**C. Task identity (`X-Task-Id` header on existing `prompt`)**
- No new subject; add a NATS header `Sesh-Task-Id` (or `A2A-Task-Id` for cross-compat) populated by the caller
- Adapters echo it on every reply chunk
- Enables future task-by-id operations without changing the prompt subject
- Lays the foundation for D-F below

### Tier 2 — Bigger lift, real value

**D. `agents.task.cancel.<agent>.<owner>.<name>.<task-id>` — cancel by id**
- Cancel a task initiated by a different client (e.g., operator sees a runaway in `sesh mesh`, cancels it)
- Requires task identity (C) to exist
- Maps directly to A2A `CancelTask`

**E. `agents.task.subscribe.<agent>.<owner>.<name>.<task-id>` — re-attach**
- Reconnect to an in-progress task's reply stream after a disconnect
- Adapter has to retain the chunk history for some retention window
- Maps to A2A `SubscribeToTask`
- **Hardest piece — needs adapter-side state retention**

**F. `agents.task.get.<agent>.<owner>.<name>.<task-id>` — task snapshot**
- Returns current state + accumulated chunks
- Read-only; cheaper than subscribe
- Maps to A2A `GetTask`

### Tier 3 — A2A parity work, valuable but optional

**G. `agents.artifact.*` — named outputs separate from reply chunks**
- For binary / file / structured outputs that aren't just text streams
- "Generate a PDF and put it in an artifact" model
- Could lean on a shared object store or pass URIs in the artifact payload

**H. Agent Card / capability advertisement**
- Structured replacement for `$SRV.INFO.agents.metadata`
- Adopt A2A AgentCard JSON shape (signed via JCS) for cross-protocol interop
- Even if sesh doesn't sign, the *shape* is useful for tooling

**I. Explicit task state machine on the bus**
- Publish `TaskStatusUpdateEvent` analog on `agents.event.task_state.*`
- Maps A2A's TaskState directly
- Requires task identity (C)

### Tier 4 — Defer / probably not

**J. AUTH_REQUIRED lifecycle** — Synadia already has §7 query chunks; a separate state isn't worth the complexity
**K. Webhook push config** — NATS is the bus; subscribers ARE the push mechanism. A2A's webhook model is for non-bus deployments
**L. `interrupt` as a separate verb** — likely better modeled as `prompt` with a `priority:interrupt` extension. Avoid adding a verb for what's really a flag on existing verb

## Section 6 — Design tensions

### Tension 1: Subject explosion vs. unified prompt envelope

Adding `event.*`, `task.*`, `shutdown.*`, `artifact.*` quadruples the subject namespace.

**Alternative:** keep one `agents.*.<agent>.<owner>.<name>` shape with a `verb` field in the envelope.

**Tradeoff:** NATS-native is to use subjects (cheap, wildcards work). Envelope-fields are easier to evolve but harder to subscribe-by-type.

**Recommend:** subject-per-verb. Synadia's existing shape sets the precedent.

### Tension 2: Coordination subjects (#91/#94) interact with the new verbs

The 6/7-token coord subjects are for role-pool routing on prompts. Do they apply to events/shutdown/tasks too?

- **Events** should ALSO use coord-subject form (`agents.event.<type>.<machine>.<project>.<session>.<role>.<inst>`) so observers can filter the same way
- **Shutdown** is per-instance; the 5-token form is right
- **Task** is per-instance per-task; 6 or 7 + task-id

Need a consistent rule: which verbs get coord-token expansion, which stay flat 5-token.

**Recommend:** verbs that have role-pool semantics (prompt, event-class-broadcast) get the expanded form. Per-instance ops (shutdown, status, task) stay 5-token. Document the rule.

### Tension 3: Task identity is a one-way door

Once we add `Sesh-Task-Id` to the protocol, every adapter has to support it. Removing it later would be breaking.

**Mitigation:** start with header (`Sesh-Task-Id`) rather than path token. Headers are easier to deprecate. Path tokens are baked into subject hierarchies.

### Tension 4: Upstream vs sesh-fork

A2A is becoming the consolidated standard (ACP merged in 2026). Synadia's protocol is its own thing.

**Option a:** Add verbs as sesh-specific extensions, keep Synadia compatibility (additive only).
**Option b:** Propose Synadia v0.4 with our additions, push upstream.
**Option c:** Implement an A2A↔Synadia bridge alongside, let the two coexist.

**Recommend:** (a) for now. The sesh extensions become part of `@agent-ops/sesh-channels`'s contract. If they prove themselves, propose upstream as v0.4.

### Tension 5: Sesh-specific vs cross-tool

Lifecycle hooks (PreToolUse, etc.) are claude-code-specific terminology. Other agents (OMP, gemini, codex) have their own lifecycle vocabularies.

**Recommend:** define a CANONICAL set on the bus (`started`, `stopped`, `tool_use_pre`, etc.) and let each adapter map its native events. Custom events go under `agents.event.custom.<adapter>.<name>` as an escape hatch.

## Section 7 — Recommendation for a first PR

**Ship the Tier 1 trio (A + B + C) together as one cohesive Synadia v0.3 extension:**

1. **`agents.event.*` publish-only verb** with a canonical event type vocabulary + `custom.<adapter>.<name>` escape
2. **`agents.shutdown.<agent>.<owner>.<name>` request/reply** for graceful drain
3. **`Sesh-Task-Id` NATS header** on every `prompt` request and reply chunk, enforced by the SDK

Defer Tier 2 (task.cancel/subscribe/get) until task-id has bedded in.

This gives sesh:
- Bus-observable lifecycle (replaces orch's filesystem markers)
- Operator-driven shutdown without process-hunting
- Foundation for task-id operations without committing to all of them at once

Surface cost: 2 new verbs + 1 header. Compatible with existing v0.3 (purely additive). Foundation for A2A interop later.

## Open questions for the operator

These are the calls that need a human:

1. **Should `agents.event.*` go through the coord-subject expansion (6/7-token machine/project/session/role tokens)?** If yes, observers can filter events by role exactly like they filter prompts. If no, events stay flat 5-token and observers filter post-receipt.

2. **Is the `Sesh-Task-Id` header sesh-specific or worth proposing upstream as `A2A-Task-Id`?** Pushing to Synadia v0.4 takes time and political effort; staying sesh-specific is faster but creates a fork.

3. **Should `shutdown` accept a `force` mode?** Force = "don't drain, exit now". Adapters may not all support graceful drain. Could just leave it as best-effort and document.

4. **Should we adopt the A2A AgentCard format for `$SRV.INFO.agents` payload?** Big rewrite, big interop win. Or just document the existing metadata shape and call it "sesh's agent card."

5. **What's the lifecycle event vocabulary?** I proposed: `started`, `stopped`, `paused`, `resumed`, `tool_use_pre`, `tool_use_post`, `session_start`, `session_stop`, `attention_needed`. Probably 1-2 of these are wrong; need adapter-author input.

## Out of scope (deliberately)

- Re-implementing A2A on NATS (`a2a-on-nats` would be a separate project)
- Push notification webhooks (NATS subscribers are the webhooks)
- Artifact transport (Tier 3 — bigger discussion)
- AgentCard signing (cross-org auth not yet a sesh problem)
- Multi-machine federation as a verb (federation should be a NATS leaf-node concern, not protocol)
