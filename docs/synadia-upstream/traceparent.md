# Upstream draft — Contribution 1: W3C `traceparent` in NATS headers

**Target repo:** `synadia-ai/synadia-agent-sdk-docs`  
**Amends:** core-protocol.md — adds new §5.7 "Header propagation
(traceparent)" and a cross-reference in §6.2.  
**Backward compatibility:** fully additive; agents that do not set or read
`traceparent` are unaffected.  
**Appendix B numbering:** this draft claims **B.13, B.14, B.15** (Appendix B
currently runs B.1–B.12 plus B.11a; the artifact-by-reference draft claims
B.16–B.19). Numbering is contiguous on the assumption both drafts merge
together; if they merge in a different order, renumber at submission time.  
**Status:** draft, ready to copy-paste into an upstream PR.

---

## Motivation

Observability tools (OpenTelemetry Collector, Jaeger, Honeycomb, Datadog,
and every other W3C-compliant backend) can reconstruct a distributed trace
from a single header: `traceparent`. NATS has supported arbitrary message
headers since v2.2. Wiring the two together costs ~50 lines of SDK code
per language and surfaces every Synadia interaction as a span automatically,
without any schema change to the JSON envelope.

Sesh's `docs/message-envelope.md` already mandates `traceparent` for
messages it generates. This proposal standardises the behaviour at the
protocol level so any Synadia client or agent benefits.

---

## Proposed spec text

### §5.7  Header propagation (traceparent) (NEW)

Insert after §5.6 "Unknown fields":

---

#### 5.7  Header propagation (traceparent)

Callers and agents communicate trace context through NATS message headers.
The following header is defined by this protocol.

| Header       | Direction             | Required | Description                                           |
|--------------|-----------------------|----------|-------------------------------------------------------|
| `traceparent`| caller→agent, agent→agent | SHOULD | W3C Trace Context §3.2 format. Propagated by agents. |
| `tracestate` | caller→agent, agent→agent | MAY    | W3C Trace Context §3.3 vendor-specific state.         |

##### 5.7.1  `traceparent` format

The header value follows [W3C Trace Context version 00][w3c-tc]:

```
traceparent: 00-<trace-id>-<parent-id>-<flags>
```

| Field       | Size     | Encoding          | Description                                                   |
|-------------|----------|-------------------|---------------------------------------------------------------|
| `version`   | 1 byte   | 2 hex chars       | Always `00`.                                                  |
| `trace-id`  | 16 bytes | 32 lowercase hex  | Identifies the top-level logical operation. Constant across all hops in one request chain. |
| `parent-id` | 8 bytes  | 16 lowercase hex  | The span-id of the publisher creating *this* message. Receivers treat it as the parent of their own work. |
| `flags`     | 1 byte   | 2 hex chars       | `01` = sampled (trace is being recorded); `00` = not sampled. |

Example:

```
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
```

##### 5.7.2  Propagation rules

**Callers** (before publishing to the `prompt` endpoint):

1. If the caller already holds a `traceparent` from an outer context (e.g.,
   an HTTP server that received W3C headers from a browser), it SHOULD copy
   that value and update `parent-id` to its own span-id, keeping `trace-id`
   unchanged.
2. If no outer context exists, the caller SHOULD generate a fresh `trace-id`
   (128-bit random) and a `parent-id` (64-bit random) and set `flags` to
   `01` if tracing is enabled, `00` otherwise.
3. Callers that do not implement tracing MAY omit the header. Absence of the
   header is not an error.

**Agents** (when publishing response chunks to the caller's reply subject
and when forwarding a request to a downstream agent):

1. Agents SHOULD propagate `traceparent` on every outbound NATS message that
   belongs to the same logical operation.
2. The `trace-id` MUST remain unchanged.
3. The agent MUST replace `parent-id` with its own newly generated span-id
   for each outgoing message so that the downstream receiver can attribute
   work to the correct hop.
4. Agents that do not implement tracing MAY omit the header on outbound
   messages. If the inbound request carried no `traceparent`, the agent has
   no obligation to generate one.

**Relays and orchestrators** that fan a single caller request out to
multiple agents SHOULD propagate `traceparent` to each downstream `prompt`
call, updating `parent-id` for each fan-out message individually, so each
downstream span is correctly parented to the relay's span.

##### 5.7.3  No behaviour change in the absence of the header

Agents and callers MUST NOT treat a missing `traceparent` as an error.
All decisions gated on `traceparent` (e.g., whether to sample a log entry)
are strictly optional. The absence of the header is indistinguishable from
`flags=00` for the purposes of this spec.

##### 5.7.4  `tracestate` (optional)

W3C Trace Context also defines `tracestate` for vendor-specific key/value
pairs. Agents and callers MAY include it alongside `traceparent`.
Implementations that do not recognise `tracestate` MUST forward it
unchanged when they propagate `traceparent`.

[w3c-tc]: https://www.w3.org/TR/2021/REC-trace-context-1-20211123/

---

### Cross-reference update — §6.2  Chunk wrapper (EDIT)

Add the following paragraph after the existing chunk-wrapper table:

> **Header propagation.** When an agent publishes response chunks, it SHOULD
> include a `traceparent` NATS header on each chunk message, updated with a
> fresh `parent-id` (§5.7.2). Callers that collect chunks and relay them to
> a further downstream agent SHOULD propagate the header in turn.

---

## Worked wire example (Appendix B style)

### B.13  Request with `traceparent` header

Published by a caller to `agents.prompt.claude-code.aconnolly.synadia-com-2`:

```
NATS headers:
  traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01

Body:
{"prompt":"summarize the attached report"}
```

The caller chose `trace-id = 4bf92f3577b34da6a3ce929d0e0e4736` (fresh,
random) and `parent-id = 00f067aa0ba902b7` (its own span-id).

### B.14  Response chunk with `traceparent` propagated

The agent publishes a `response` chunk to the caller's reply subject
(`_INBOX.Xj7k9Q2pA`):

```
NATS headers:
  traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-a3ce929d0e0e4736-01

Body:
{"type":"response","data":"The report concludes that…"}
```

The agent kept `trace-id` unchanged, generated a fresh `parent-id`
(`a3ce929d0e0e4736`) representing its own processing span, and kept
`flags=01` (sampled).

### B.15  Agent-to-agent forwarding (relay pattern)

An orchestrator agent receives the caller's request, starts its own span
(`b2c3d4e5f6a7b8c9`), and forwards to a downstream agent at
`agents.prompt.pi.aconnolly.pi-dev-1`:

```
NATS headers:
  traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-b2c3d4e5f6a7b8c9-01

Body:
{"prompt":"extract key findings from section 3"}
```

The downstream agent's work is now correctly parented under the
orchestrator's span `b2c3d4e5f6a7b8c9`, which is itself a child of the
original caller's span `00f067aa0ba902b7`, all within the same
`trace-id`.

---

## SDK companion (informative)

The following TypeScript snippet demonstrates caller-side propagation for
the client SDK. It is informative; the precise API is SDK-specific.

```typescript
import { connect, headers } from "@nats-io/transport-node";
import { randomBytes } from "crypto";

function newTraceparent(): string {
  const traceId = randomBytes(16).toString("hex");
  const parentId = randomBytes(8).toString("hex");
  return `00-${traceId}-${parentId}-01`;
}

function childTraceparent(parent: string): string {
  // Keep trace-id, replace parent-id with a fresh span-id.
  const parts = parent.split("-");
  parts[2] = randomBytes(8).toString("hex");
  return parts.join("-");
}

async function callAgent(
  serverUrl: string,
  subject: string,
  prompt: string,
  incomingTraceparent?: string,
): Promise<void> {
  const nc = await connect({ servers: serverUrl });
  const h = headers();
  h.set(
    "traceparent",
    incomingTraceparent
      ? childTraceparent(incomingTraceparent)
      : newTraceparent(),
  );
  const reply = nc.subscribe(nc.inbox());
  nc.publish(subject, JSON.stringify({ prompt }), { headers: h, reply: reply.getSubject() });
  for await (const msg of reply) {
    if (!msg.data.length) break; // empty-payload terminator (§6.5)
    console.log(new TextDecoder().decode(msg.data));
  }
  await nc.drain();
}
```

A matching test (`client-sdk/typescript/tests/traceparent.test.ts`) that
verifies the round-trip is omitted here but should accompany the PR — it
publishes a message with a known `traceparent`, starts a loopback
subscriber, and asserts that the subscriber receives the same `trace-id`
with a *different* `parent-id`.

---

## References

- [W3C Trace Context Level 1](https://www.w3.org/TR/2021/REC-trace-context-1-20211123/) (W3C
  Recommendation, November 2021)
- [OpenTelemetry NATS instrumentation registry](https://opentelemetry.io/ecosystem/registry/?language=go&component=instrumentation)
- [NATS message headers](https://docs.nats.io/nats-concepts/subjects#message-headers)
