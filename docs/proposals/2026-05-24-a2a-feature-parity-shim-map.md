# A2A Feature-Parity Shim Map

**Date:** 2026-05-24
**Status:** Brainstorm phase 2 (complementary to `2026-05-24-synadia-protocol-extensions-brainstorm.md` — do not supersede it)
**Goal:** Inventory every A2A operation, data object, and behavior so we can spec what Synadia needs to add to enable a complete (not lossy) shim Synadia ↔ A2A.

## How to read this doc

The prior brainstorm produced a *tiered* extension menu (Tier 1: events + shutdown + task-id header, Tier 2: task ops, Tier 3: artifacts/AgentCard). That was a "what should we ship next" exercise.

This doc is the *exhaustive* exercise: what does Synadia need so a shim can losslessly translate every A2A interaction? The tiered menu is a subset of this map. They should land in priority order from the prior brainstorm; this doc tells us the *destination*.

---

## Section 1 — A2A's complete surface inventory

### 1.1 Operations (11 methods)

| # | Method | Transport hint | Sync/stream |
|---|---|---|---|
| 1 | `SendMessage` | unary | sync |
| 2 | `SendStreamingMessage` | SSE | stream |
| 3 | `GetTask` | unary | sync — returns Task snapshot |
| 4 | `ListTasks` | unary | sync — paginated |
| 5 | `CancelTask` | unary | sync |
| 6 | `SubscribeToTask` | SSE | stream — re-attach to in-progress task |
| 7 | `CreateTaskPushNotificationConfig` | unary | sync — register webhook |
| 8 | `GetTaskPushNotificationConfig` | unary | sync |
| 9 | `ListTaskPushNotificationConfigs` | unary | sync |
| 10 | `DeleteTaskPushNotificationConfig` | unary | sync |
| 11 | `GetExtendedAgentCard` | unary | sync — signed AgentCard |

### 1.2 Data model — every object

**`Task`** — `{id, contextId, status, artifacts[], history[], metadata}`
**`TaskStatus`** — `{state, message, timestamp}`
**`TaskState`** — enum: UNSPECIFIED, SUBMITTED, WORKING, COMPLETED (terminal), FAILED (terminal), CANCELED (terminal), REJECTED (terminal), INPUT_REQUIRED (interrupted), AUTH_REQUIRED (interrupted)
**`Message`** — `{messageId, contextId, taskId, role, parts[], metadata, extensions[], referenceTaskIds[]}`
**`Role`** — enum: USER, AGENT
**`Part`** — OneOf{`text`, `raw` (base64 bytes), `url`, `data` (arbitrary JSON)} + optional `metadata`, `filename`, `mediaType`
**`Artifact`** — `{artifactId, name, description, parts[], metadata, extensions[]}`
**`AgentCard`** — `{name, description, url, provider, version, documentationUrl, capabilities, securitySchemes, securityRequirements, defaultInputModes[], defaultOutputModes[], skills[], supportsAuthenticatedExtendedCard, extensions[], supportedInterfaces[], protocolVersions[], signatures[], iconUrl}`
**`AgentCapabilities`** — `{streaming, pushNotifications, stateTransitionHistory, extendedAgentCard, extensions[]}`
**`AgentSkill`** — `{id, name, description, tags[], examples[], inputModes[], outputModes[]}`
**`AgentExtension`** (in AgentCard) — `{uri, description, required, params}`
**`AgentProvider`** — `{name, url}`
**`SecurityScheme`** — multi-shape: OAuth2, APIKey, HTTP-Basic, HTTP-Bearer, mTLS
**`SecurityRequirement`** — references SecurityScheme by name
**`PushNotificationConfig`** — `{id, taskId, url, token, authentication}`
**`AuthenticationInfo`** — credentials for webhook delivery
**`TaskStatusUpdateEvent`** — `{taskId, contextId, status, final, metadata}`
**`TaskArtifactUpdateEvent`** — `{taskId, contextId, artifact, append, lastChunk, metadata}`
**`StreamResponse`** — wrapper OneOf{task, message, statusUpdate, artifactUpdate}
**`SendMessageRequest`** / **`SendMessageResponse`** — operation envelopes
**`ListTasksRequest`** — `{pageSize, pageToken, filter}`

### 1.3 Service parameters (headers)

| Header | Purpose |
|---|---|
| `A2A-Extensions` | Comma-separated extension URIs client wants active |
| `A2A-Version` | Protocol version (e.g. `0.3`, `1.0`) |

### 1.4 Error model

JSON-RPC base errors: -32700 JSONParseError, -32600 InvalidRequestError, -32601 MethodNotFoundError, -32602 InvalidParamsError, -32603 InternalError.

A2A-specific (-32001 to -32099):
- `TaskNotFoundError`
- `TaskNotCancelableError`
- `PushNotificationNotSupportedError`
- `UnsupportedOperationError`
- `ContentTypeNotSupportedError`
- `AuthenticationRequiredError`
- `AuthorizationFailedError`
- `InvalidAgentResponseError`
- `VersionNotSupportedError`
- `ExtendedAgentCardNotConfiguredError`

### 1.5 Behaviors that span objects

- **Multi-turn tasks** — a single Task can receive multiple Messages over its lifetime (not just one prompt-response pair)
- **Context grouping** — `contextId` groups multiple tasks into one conversation
- **Reference messages** — a Message can `referenceTaskIds` to bring prior task outputs into context
- **History retention** — Task carries `history[]` of Messages; `historyLength` param controls how much is returned
- **Re-attach to stream** — after disconnect, client can SubscribeToTask to resume the event stream
- **Push notification fan-out** — task updates fire to N registered webhooks
- **Capability negotiation** — client reads AgentCard, picks operations the agent supports
- **Per-skill modalities** — each skill declares its input/output modes (overrides agent defaults)
- **AgentCard signing** — JCS-canonical JSON Web Signature for cross-org trust
- **Extension declaration** — agents publish supported extension URIs in AgentCard; clients send `A2A-Extensions` header to activate
- **Authentication flow** — AUTH_REQUIRED state + out-of-band credential delivery (or in-band via extension)
- **Input flow** — INPUT_REQUIRED state for human-in-the-loop turns
- **Versioned migration** — major version bumps for breaking changes, deprecated names retained until next major

---

## Section 2 — Synadia v0.3 current surface (recap)

### 2.1 Verbs

- `agents.prompt.<agent>.<owner>.<name>` — prompt + chunked reply
- `agents.hb.<agent>.<owner>.<name>` — periodic heartbeats
- `agents.status.<agent>.<owner>.<name>` — status query
- `$SRV.INFO.agents` — NATS micro discovery
- Coord additions (#91/#94): `agents.prompt.<m>.<p>.<s>.<role>[.<inst>]`, `agents.report.<m>.<p>.<s>.>`

### 2.2 Envelope

Request: plain UTF-8 text OR `{prompt, attachments[{filename, content (base64)}]}`
Reply chunks: `{type:status|response|query, data}` + empty terminator
Headers: `Nats-Service-Error-Code` (400/500) for cancellation/error

### 2.3 Behaviors

- Single round-trip per prompt
- No task identity; reply inbox IS the task scope
- Disconnect = stream lost
- Permissions / human-in-loop via §7 `{type:query}` mid-stream chunks
- Shutdown signaled outbound only (§8.6 empty heartbeat)
- Metadata key/value bag on the micro service (agent, owner, session, role, class, machine, project_id, protocol_version)

### 2.4 What's already shimmable today (best case)

A2A `SendMessage` (text-only, simple) → Synadia `prompt` ✓
A2A `SendStreamingMessage` (text-only) → Synadia `prompt` with chunks ✓ — but A2A's stream events (statusUpdate/artifactUpdate) have no Synadia analog
A2A `CancelTask` → Synadia error-code header — partial (caller-only, no by-id)
A2A AgentCard (basic shape) → translatable from `$SRV.INFO.agents` metadata — but no skills, capabilities, security schemes structure

Everything else is unmappable today.

---

## Section 3 — Complete gap matrix (every A2A surface → Synadia status)

### 3.1 Operations

| A2A | Synadia today | Gap | Synadia work needed |
|---|---|---|---|
| SendMessage | `agents.prompt.*` request | Envelope shape (Message vs free text) | **HIGH** — Message envelope |
| SendStreamingMessage | `agents.prompt.*` chunks | Chunk types (Task/Message/statusUpdate/artifactUpdate) | **HIGH** — chunk union |
| GetTask | — | Task identity + persistence | **HIGH** — new verb + state |
| ListTasks | — | Task registry | **HIGH** — new verb + indexing |
| CancelTask | header-based | by-id cancel | **MEDIUM** — new verb |
| SubscribeToTask | — | History retention + re-attach | **HIGH** — new verb + retention |
| CreateTaskPushNotificationConfig | NATS sub is the webhook | Translation layer | **LOW** — shim handles |
| Get/List/Delete TaskPushNotificationConfig | — | Webhook registry | **LOW** — shim handles |
| GetExtendedAgentCard | $SRV.INFO partial | Signed structured card | **MEDIUM** — new verb + spec |

### 3.2 Data objects

| A2A object | Synadia today | Gap | Synadia work |
|---|---|---|---|
| Task | None | No task concept | New first-class type |
| TaskStatus / TaskState | Implicit | No state machine | New explicit lifecycle |
| Message | Free-text + attachments | No Message envelope | Replace prompt envelope |
| Role (USER/AGENT) | Implicit (request=USER, reply=AGENT) | No marker | Add field |
| Part (text/raw/url/data) | text + base64 attachments only | No url, no data, no per-part mediaType | Extend |
| Artifact | None | No named outputs | New |
| AgentCard | $SRV.INFO metadata (flat) | Structural | New verb |
| AgentCapabilities | None | None | Part of AgentCard |
| AgentSkill | None | None | Part of AgentCard |
| AgentExtension | metadata bag | URI-typed, declared in card | Standardize |
| AgentProvider | None | None | Part of AgentCard |
| SecurityScheme | NATS auth (out of band) | Inline declaration | Informational in AgentCard |
| PushNotificationConfig | NATS sub | Shim layer | Shim handles |
| TaskStatusUpdateEvent | None | Stream event | New chunk type or event verb |
| TaskArtifactUpdateEvent | None | Stream event | New chunk type or event verb |
| StreamResponse wrapper | Untyped chunk dispatch | Type discrimination | Standardize chunk union |
| Extension URIs | metadata bag | URI-typed | Standardize |

### 3.3 Behaviors

| A2A behavior | Synadia today | Gap | Synadia work |
|---|---|---|---|
| Task identity | None | Critical foundation | Add task-id |
| Multi-turn within task | One round-trip per prompt | Task accepts N messages | Multi-turn task lifecycle |
| Context grouping | None | None | Add context-id |
| Message references | None | None | Add reference field |
| History retention | None | None | Per-task chunk store |
| Stream re-attach | None | Disconnect = lost | History + subscribe verb |
| Push notification fan-out | NATS native (subscribers) | Shim translation | Shim handles |
| Capability advertisement | flat metadata | Structured AgentCard | New spec |
| Per-skill modalities | None | None | Part of AgentCard |
| AgentCard signing | None | JCS+JWS | Optional, cross-org |
| Extension activation | None | `A2A-Extensions` header | Add headers |
| AUTH_REQUIRED | None | Explicit state | Add state + event |
| INPUT_REQUIRED | §7 query chunks (informal) | Explicit state | Formalize as state |
| Versioning | `protocol_version` metadata | Header-based negotiation | Add `A2A-Version` header |
| Error code mapping | Nats-Service-Error-Code 400/500 | A2A-specific codes | Map errors |

---

## Section 4 — Required Synadia additions, classified

### 4.1 New subjects (verbs)

```
agents.task.get.<agent>.<owner>.<name>                  # GetTask
agents.task.list.<agent>.<owner>.<name>                 # ListTasks
agents.task.cancel.<agent>.<owner>.<name>               # CancelTask (by id)
agents.task.subscribe.<agent>.<owner>.<name>            # SubscribeToTask
agents.card.get.<agent>.<owner>.<name>                  # GetAgentCard (basic)
agents.card.extended.<agent>.<owner>.<name>             # GetExtendedAgentCard (signed)
agents.event.<event-type>.<agent>.<owner>.<name>        # statusUpdate, artifactUpdate, lifecycle
agents.shutdown.<agent>.<owner>.<name>                  # graceful shutdown (sesh-specific bonus)
agents.notify.create.<agent>.<owner>.<name>             # PushNotificationConfig CRUD
agents.notify.get.<agent>.<owner>.<name>
agents.notify.list.<agent>.<owner>.<name>
agents.notify.delete.<agent>.<owner>.<name>
```

(Coord-subject expansion for events: `agents.event.<type>.<m>.<p>.<s>.<role>.<inst>` so observers can filter by role.)

### 4.2 New envelope structure (Synadia v0.4 prompt envelope)

```json
{
  "messageId": "msg-uuid",
  "taskId": "task-uuid",         // NEW — first-class
  "contextId": "ctx-uuid",       // NEW — group tasks
  "role": "USER",                // NEW
  "parts": [                     // RENAMED from attachments + extended
    {"text": "..."},
    {"raw": "<b64>", "filename": "a.png", "mediaType": "image/png"},
    {"url": "https://...", "mediaType": "video/mp4"},
    {"data": {...}, "mediaType": "application/json"}
  ],
  "metadata": {...},
  "extensions": ["https://example.com/ext/v1"],     // NEW — activate extensions
  "referenceTaskIds": ["task-uuid-2"]              // NEW — reference prior tasks
}
```

### 4.3 New chunk discriminator (Synadia v0.4 stream chunks)

Replace `{type:status|response|query, data}` with A2A-aligned union:

```json
// One of:
{"task": {...Task object...}}                  // initial / snapshot
{"message": {...Message object...}}             // one-shot reply
{"statusUpdate": {...TaskStatusUpdateEvent...}} // state transition
{"artifactUpdate": {...TaskArtifactUpdateEvent...}} // artifact emitted
// + empty terminator (existing)
```

Old `{type:status,data:ack}` becomes `{statusUpdate: {status: {state: SUBMITTED}}}`.
Old `{type:response,text}` becomes `{message: {role:AGENT, parts:[{text:...}]}}`.
Old `{type:query}` becomes `{statusUpdate: {status: {state: INPUT_REQUIRED, message: {...}}}}`.

### 4.4 New headers (NATS message headers — already supported on the wire)

| Header | Purpose | Required? |
|---|---|---|
| `A2A-Version` | Protocol version client speaks | optional |
| `A2A-Extensions` | Comma-separated extension URIs to activate | optional |
| `Sesh-Task-Id` | Task identity (also in envelope; header for ergonomics) | when known |
| `Sesh-Context-Id` | Context identity | when known |
| `Sesh-Envelope-Version` | `v1` (current) or `v2` (A2A-shaped) | optional; default v1 |

### 4.5 New behaviors agents must support

- **Track tasks in memory** — at minimum, retain active tasks + their chunk history for re-subscribe
- **Multi-message-per-task** — accept new Messages bound to existing task-id
- **Publish lifecycle events** — emit `agents.event.task_state.*` on every state transition
- **Serve AgentCard** — respond to `agents.card.get.*` with structured card built from in-process knowledge
- **Honor extensions header** — when `A2A-Extensions` includes an extension the agent declares, behave per that extension's spec
- **Map errors** — translate internal errors to A2A error codes in `Nats-Service-Error-Code` header

### 4.6 What the shim handles (does NOT need Synadia work)

- **Push notification webhooks** — shim subscribes to NATS, POSTs HTTP webhook on behalf of agent
- **Webhook config persistence** — shim has its own registry; agent doesn't need to know about webhooks
- **AgentCard signing** — shim can sign on behalf of agents (for cross-org A2A clients)
- **HTTP/JSON-RPC binding** — shim is the HTTP server; agents stay NATS-native
- **Auth scheme translation** — A2A bearer/OAuth → NATS auth (handled at NATS connection level)

---

## Section 5 — Shim architecture sketch

```
A2A client (HTTP+JSON-RPC)
        │
        ▼
┌─────────────────────────┐
│   sesh-a2a-shim         │  ← new binary
│  ─────────────────────  │
│  HTTP/REST listener     │
│  JSON-RPC dispatcher    │
│  SSE streamer           │
│  Webhook fan-out        │
│  AgentCard signer       │
│  Task registry (mirror) │
└──────────┬──────────────┘
           │  NATS req/reply on agents.* subjects
           ▼
┌─────────────────────────┐
│   sesh hub (NATS)       │
│  ─────────────────────  │
│  Existing v0.3 + v0.4   │
│  verbs                  │
└──────────┬──────────────┘
           │
           ▼
┌─────────────────────────┐
│  Synadia v0.4 agent     │
│  (claude/pi/omp/...)    │
│  ─────────────────────  │
│  Speaks Sesh-Envelope-  │
│  Version: v2            │
└─────────────────────────┘
```

**Shim responsibilities:**
1. Listen on HTTP, accept A2A JSON-RPC
2. For each A2A method, translate to NATS request on the right subject
3. For streaming: open NATS subscription, fan out chunks as SSE events
4. For push notifications: register own NATS subscription, POST HTTP to webhook URLs
5. AgentCard: query `agents.card.get.*`, optionally sign (JCS+JWS), serve at `/.well-known/agent.json`
6. Error code translation
7. Auth: terminate HTTP auth at shim; NATS auth at NATS layer

**Shim depends on Synadia v0.4 having:**
- Every verb listed in §4.1
- The v2 envelope from §4.2
- The chunk union from §4.3
- Headers from §4.4
- Behaviors from §4.5

If any are missing, that A2A feature is lossy (shim can fake-respond or 501).

---

## Section 6 — Lossiness matrix (if we ship less than full parity)

| A2A feature | Without Synadia v0.4 change | Shim behavior |
|---|---|---|
| SendMessage (text-only) | OK with v0.3 envelope | Pass-through |
| SendMessage (binary parts, url parts, structured data parts) | Synadia has attachments[] only | Lossy — shim has to inline-fetch URL parts, drop structured data, base64-encode bytes into attachments |
| Streaming events (statusUpdate, artifactUpdate) | No analog | Shim synthesizes from chunk text-stream; loses state machine richness |
| GetTask | No task id | Shim invents id, can't actually fetch — 501 |
| ListTasks | No registry | 501 |
| CancelTask by id | No id | 501 (caller-only cancel works via header but isn't externally cancellable) |
| SubscribeToTask (re-attach) | No history | 501 |
| Push notifications | NATS subs | Shim handles entirely |
| AgentCard (basic) | Build from $SRV.INFO | Lossy — no skills, no security schemes, no signing |
| AgentCard signing | Not in Synadia | Shim can sign on behalf |
| AUTH_REQUIRED | No state | Shim maps to generic auth error |
| INPUT_REQUIRED | §7 chunks | Shim maps query chunks → INPUT_REQUIRED status |
| Multi-turn tasks | One round-trip per prompt | 501 — task can't accept second message |
| Context grouping | No contextId | Shim ignores; loses correlation |

**TL;DR — without v0.4, the shim is a 60% solution.** Lossy on structured data, broken on task-id-dependent operations (4 of 11 ops, plus all history), no real artifacts.

---

## Section 7 — Strategy alternatives (3)

### Approach A — Big-bang Synadia v0.4 = A2A-on-NATS

Wholesale adopt A2A data model: Task, Message, Part, Artifact, AgentCard. Map every A2A op to NATS. Shim becomes near-trivial 1:1 translation.

- **Pros:** Shim is dumb (translation only, no synthesis). Maximally A2A-compatible. Future cross-protocol interop straightforward.
- **Cons:** Breaking change for all 5 sesh-channels adapters. Lots of work all at once. Bakes A2A's design choices into Synadia (e.g., Message/Part shape) — what if A2A v2 changes?
- **Effort:** 4-6 weeks adapter work + 2 weeks shim + ~3 weeks Synadia spec updates.

### Approach B — Layered: v0.3.1 additions + intelligent shim

Add minimal Synadia surface (task-id, lifecycle events, AgentCard, shutdown — the prior brainstorm's Tier 1). Shim does heavy lifting for everything else: Message/Part shape translation, artifact synthesis from text chunks, webhook fan-out, signing.

- **Pros:** Backward compatible. Existing adapters keep working. Ships in waves (Tier 1 → Tier 2 → ...).
- **Cons:** Shim is fat and complex. Some A2A features remain lossy permanently (e.g., structured data parts, true re-attach without per-task history retention).
- **Effort:** 1-2 weeks per Tier wave + 4-6 weeks shim.

### Approach C — Hybrid: versioned envelopes coexist

Define `Sesh-Envelope-Version: v2` that's A2A-shaped. Adapters declare via AgentCard which versions they speak. v1 (current) and v2 coexist on same agents. Shim is 1:1 for v2 adapters, lossy for v1.

- **Pros:** Existing adapters keep working unchanged. New adapters can opt into full A2A shape. Eventually deprecate v1.
- **Cons:** Most complex to maintain (two envelope shapes everywhere). Bifurcates the adapter ecosystem during transition. Shim has two code paths.
- **Effort:** 3-4 weeks Synadia spec (both envelopes) + 2 weeks shim + adapters migrate at own pace.

### My recommendation (after the user picks the foundational question below)

I lean **Approach C (hybrid)** for two reasons:
1. Sesh-channels adapters shipped only weeks ago; breaking them is high-cost.
2. v2 envelope can be designed to converge with A2A *exactly*, so the shim for v2 agents is trivial. Lossy-only for v1 adapters that haven't migrated yet — an acceptable transitional cost.

But this depends on the foundational scope question (see §8).

---

## Section 8 — One foundational question before proceeding

The brainstorming skill says one question at a time, multiple choice preferred. The most decisive single question that gates the approach choice:

> **Who's the primary consumer of the shim? Specifically, do we need to be A2A-compatible for *outside-the-mesh* clients (Google's reference SDK, third-party agents, cross-org integrations), or just for *inside-the-mesh* tooling (sesh-ops, sesh mesh CLI, internal dashboards) that happens to like A2A's data model?**

The user's answer drives:
- **Outside-the-mesh:** must implement signed AgentCard, webhook push notifications, JSON-RPC over HTTP. Full A2A parity is the bar. Approach A or B.
- **Inside-the-mesh:** can skip signing, webhooks, HTTP binding. Just need shape compatibility. Approach C is fine.
- **Both eventually:** start with C, layer outside-the-mesh later.

This question gates strategy — without an answer, the trade-offs in §7 don't sort cleanly.
