# sesh vs Synadia Agent Protocol — deep comparison

Cross-reference of `synadia-ai/synadia-agents` and `synadia-ai/synadia-agent-sdk-docs` against sesh's current implementation and docs, with recommendations for adoption.

## Layer map — they are not competitors

```
┌─────────────────────────────────────────────────────────────┐
│ Application: agents wiring patterns                          │
├─────────────────────────────────────────────────────────────┤
│ Sesh conventions: scoped-memory · task · goal · envelope     │  sesh-unique
├─────────────────────────────────────────────────────────────┤
│ Synadia Agent Protocol: discover · prompt · stream · status  │  GAP in sesh
├─────────────────────────────────────────────────────────────┤
│ Sesh substrate: hub/leaf · sessions · Fossil sync            │  sesh-unique
├─────────────────────────────────────────────────────────────┤
│ NATS micro + JetStream                                       │
└─────────────────────────────────────────────────────────────┘
```

Sesh is a **runtime substrate**. Synadia is a **wire contract**. They are orthogonal — the comparison reveals one missing layer in the sandwich.

## What each implements

| Concern | sesh (today) | Synadia Agent Protocol |
|---|---|---|
| Transport | embedded NATS server, hub+leaf, auto-spawn | assumes a NATS server exists |
| Session lifecycle | `sesh up/down`, PID O_EXCL claim, state JSON | none — session is the agent process |
| Agent identity | none (sessions, not agents) | `metadata.{agent, owner, session, protocol_version, instance_id}` |
| Discovery | not standardized (per-session NATS URL in JSON) | `$SRV.INFO.agents` (micro framework) |
| Prompt/invoke shape | ad-hoc `orch.tell {pane, prompt}` | `prompt` endpoint, plain-text or `{prompt, attachments[]}` envelope, declared `max_payload`+`attachments_ok` |
| Streaming responses | ad-hoc `orch.events.<pane>` JSONL tail | typed chunks `{type:"response"\|"status"\|"query", data}`, mandatory leading `ack`, zero-byte terminator |
| Errors | ad-hoc | `Nats-Service-Error-Code` + 400/401/403/404/409/429/500 taxonomy |
| Mid-stream agent→caller queries | no | §7 `query` chunks with per-query reply subject |
| Liveness | only Fossil sync, no per-agent heartbeat | `agents.hb.{a}.{o}.{n}` pub/sub + `status` req/reply, 3×interval offline threshold |
| Load-balanced instances | session-claim is exclusive | queue group `"agents"` on prompt endpoint |
| Versioning | implicit | `protocol_version` in metadata, MAJOR.MINOR rules |
| Reference impl | none | `ReferenceAgent` per language, cross-SDK interop test |
| Plugins for existing harnesses | `docs/orch-bridge.md` (one, partial) | `agents/{claude-code, pi, openclaw, hermes, open-agent}` |
| Scoped memory | hub/project/session/workflow/agent buckets | not addressed |
| Durable task/goal records | task + goal v1 specs (CAS, sweeper, hierarchy) | not addressed |
| Distributed tracing | W3C `traceparent` in NATS headers | not addressed |
| Content-addressable artifacts | Fossil + autosync | inline base64 in envelope, §5.5 reserves future artifact endpoint |

The orch-bridge doc is the most diagnostic artifact: sesh has *already started reinventing a fragment of Synadia's protocol* (publish hooks for events/notify/stop, single subscriber for inbound prompts), explicitly noting the design constraints (NATS subjects can't contain `%`, `nats sub --translate` doesn't expose `$NATS_SUBJECT`) that Synadia solves cleanly with verb-first subjects and a typed chunk envelope.

## What sesh should adopt from Synadia

### 1. Make Synadia §3 + §6.4 + §8.7 the agent-presence contract inside a session

The smallest valuable slice. An "agent" running inside a sesh session registers as a NATS micro service named `agents`, advertises a `prompt` endpoint, emits the mandatory `ack` chunk, and exposes a `status` endpoint. With that alone:
- `nats req '$SRV.INFO.agents'` lists every agent across all sesh sessions
- The orch bridge's 4 ad-hoc subjects collapse to a discoverable typed surface
- Sesh doesn't need to ship SDKs — it just defines what agents inside its substrate do, and uses the existing `@synadia-ai/*` packages

### 2. Replace orch-bridge subjects with the Synadia wire

| Today | After |
|---|---|
| `orch.tell {pane, prompt}` | `agents.prompt.cc.<owner>.<pane>` (verb-first, session-scoped) |
| `orch.events.<pane>` JSONL | `response` chunks on the reply subject |
| `orch.notify.<pane>` | `query` chunks (§7) |
| `orch.stop.<pane>` | terminator + heartbeat liveness |

Gains: typed envelope, explicit attachment story, error taxonomy, queue-group load balancing, free discovery. Loses: nothing — every current orch use case maps cleanly.

### 3. Extend session state JSON with an `agents` array

Today `.sesh/sessions/<label>.json` has `{pid, nats_url, leaf_url, fossil_url}`. Add `agents: [{name, owner, instance_id, subject}]` so outside-the-mesh tooling can address agents inside a session without an `$SRV.INFO` round trip.

### 4. Ship a Go reference agent

Synadia's `ReferenceAgent` is the most cited artifact in the spec — "the authoritative on-the-wire counterpart." Sesh should ship a `cmd/sesh-ref-agent/` that registers via Synadia on a sesh hub. That single binary anchors every future test, plugin, and SDK that targets sesh.

### 5. Adopt the conformance-checklist pattern (§12)

Sesh's docs are excellent but scattered. A single "you are participating in a sesh mesh when…" checklist (covering envelope, scoped-memory bucket names, task pull protocol, plus Synadia §12 if adopted) would be the most valuable consolidation.

### 6. Borrow the leading-ack idea for task pull

Synadia §6.4 mandates an immediate `ack` so callers can reset inactivity timers before any latency-inducing work. Sesh's task pull (KV CAS) has the same hazard — watchers don't know if a slow agent is alive or hung. An immediate `claimed` event on a task-status subject after CAS success would solve this with the same trick.

### 7. Standardize liveness on Synadia's heartbeat payload (§8.3)

Sesh's task `due_at` extension and Synadia's heartbeat both answer "is this actor alive?" Converging the payload shape (`agent, owner, session, instance_id, ts, interval_s`) lets one tracker watch task pullers and prompt-handlers simultaneously.

## What Synadia should learn from sesh (and how sesh can lead)

Each is a published-doc-level contribution sesh can offer upstream:

1. **W3C `traceparent` propagation.** Synadia has no trace context. Sesh's message-envelope spec is a ~50-line upstream PR that gives free OpenTelemetry interop. Recommend it for §5 or a new §13.

2. **Artifact-by-reference via Fossil.** Synadia §5.5 reserves "future artifact endpoint." Sesh has the answer sitting in EdgeSync/Fossil. A `metadata.artifact_url` advertising the session's Fossil HTTP URL lets agents pass commit hashes instead of base64 blobs, dodging `max_payload`.

3. **Scoped memory conventions.** Synadia doesn't define where agent state lives. Sesh's 5-scope bucket model (hub/project/session/workflow/agent) plus the deterministic name derivation is a clean answer to "what context does an agent have access to."

4. **Goal/task records with CAS pull.** Synadia is purely request/reply + heartbeat. Sesh's durable, queryable, hierarchical goal/task primitives are what fleets actually need once more than one workflow is in flight.

5. **Auto-spawn substrate.** Synadia assumes you have a NATS server. Sesh's "first sesh up auto-spawns the embedded hub and writes its URL where everyone can find it" is the ergonomic equivalent of `claude code plugin install` — recommend it as a "deployment patterns" appendix to the spec.

## Two failure modes to avoid

1. **Don't fold Synadia into sesh** — they are different layers. If sesh ships a *competing* agent protocol, orch-bridge replays at scale.
2. **Don't ignore sesh's distinctive primitives when adopting Synadia.** A sesh+Synadia agent should get scopes, goals, tasks, traceparent, and Fossil *because they sit above the Synadia wire*, not because they're folded into it.

## Proposed first-PR roadmap

| Slice | Scope | Why it's first |
|---|---|---|
| 1 | Reference doc: "Synadia Agent Protocol on a sesh hub" — wire-compat assertion, sample `$SRV.INFO.agents` response from a sesh session | Zero code; aligns the team |
| 2 | Go reference agent (`cmd/sesh-ref-agent/`) registering via Synadia on a sesh hub | Anchors every future test |
| 3 | Extend session JSON with `agents[]`; bridge external `$SRV.INFO` queries through hub | Outside-the-mesh discovery, no protocol change |
| 4 | Migrate orch-bridge to Synadia wire (`prompt` + chunks); leave shims for backward compat | Retires the ad-hoc subjects |
| 5 | Upstream PRs to `synadia-agent-sdk-docs`: traceparent §, artifact-by-reference §, deployment-patterns appendix | Sesh becomes a named topology in the spec |

## References

- `~/references/synadia-agents/` — full SDK + plugin monorepo (TS + Python)
- `~/references/synadia-agent-sdk-docs/core-protocol.md` — v0.3.0 spec, 870 lines
- `docs/orch-bridge.md` — sesh's current ad-hoc bridge (the gap this comparison closes)
- `docs/message-envelope.md`, `docs/scoped-memory.md`, `docs/task-management.md`, `docs/goal-management.md`, `docs/coordination-patterns.md` — the sesh-unique layer
