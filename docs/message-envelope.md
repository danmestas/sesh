# Message envelope spec

Sesh ships every coordination pattern over NATS subjects (see
[coordination-patterns.md](./coordination-patterns.md)). For a single
orchestrator dispatching to one or two subagents, raw payloads are
enough. For anything else — message-bus pipelines, multi-tier
hierarchies, agent teams running for hours — you need to be able to
reconstruct "what caused what" after the fact.

This document specifies a small metadata convention agents follow when
publishing on the mesh. Sesh does not enforce it; following it makes
debugging across runs tractable.

## Decisions

- **Metadata travels in NATS headers**, not wrapped around the payload.
  Payloads stay binary-clean (LLM prompts, JSON bodies, blobs — whatever
  the agent wants). Tools like `nats sub -H` already render headers
  inline.
- **Trace and span context uses the W3C trace-context standard
  (`traceparent` header)**. Free interop with the OpenTelemetry
  Collector, Jaeger, Honeycomb, and every other tracing backend that
  understands W3C. No custom format to learn.

## Header set

```
traceparent      00-<trace-id-32hex>-<parent-id-16hex>-<flags-2hex>   REQUIRED
Sesh-Role        orchestrator | researcher | verifier | ...           optional
Sesh-Task-Id     <ulid or app-defined id>                             optional
Sesh-Attempt     <int>                                                optional
Sesh-Envelope    1                                                    optional
```

### traceparent (REQUIRED)

W3C trace-context, format `<version>-<trace-id>-<parent-id>-<flags>`:

- `<version>` — `00` (the only published version)
- `<trace-id>` — 32 lowercase hex chars (128-bit). Same value across
  every hop of a logical workflow.
- `<parent-id>` — 16 lowercase hex chars (64-bit). The span-id of the
  publisher creating *this* message. Receivers treat it as the parent
  span of whatever work they do in response.
- `<flags>` — `01` if sampled (recorded for tracing), `00` if not.

Example: `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`

W3C also defines `tracestate` for vendor-specific data; agents may set
it if their backend cares, but it's not required by this spec.

### Sesh-Role (optional)

Free-form string naming what the publisher does in the workflow. Useful
for filtering traces by participant. Recommended: lowercase, single
word.

### Sesh-Task-Id (optional)

Application-level identifier for the logical unit of work. Distinct
from `trace-id` (which is for the *trace*) — a single task may produce
multiple traces (retries, sub-workflows) and a single trace may span
multiple tasks (e.g. a workflow that processes a batch). ULID is a good
default; UUID v4 also fine.

### Sesh-Attempt (optional)

Integer retry counter. Increment on each redelivery. Helps consumers
detect duplicates and tracing backends distinguish attempts of the same
task.

### Sesh-Envelope (optional)

Spec version. Currently `1`. Lets the spec evolve without breaking
deployed agents — until v2 exists, omitting it is fine.

## Lifecycle

### Workflow root

The agent that starts a workflow generates both ids:

```sh
trace_id=$(openssl rand -hex 16)              # 32 hex chars
span_id=$(openssl rand -hex 8)                # 16 hex chars
traceparent="00-${trace_id}-${span_id}-01"

nats pub orchestrator.dispatch '<payload>' \
    -H "traceparent:${traceparent}" \
    -H "Sesh-Role:orchestrator"
```

### Intermediate hop

An agent that receives a message, does work, and publishes downstream:

1. Read incoming `traceparent`. Parse out the trace-id.
2. Generate a new span-id for this hop.
3. Build outgoing `traceparent` with the same trace-id and the new
   span-id (which becomes the parent of whatever the downstream
   creates):

```sh
# Pseudocode — actual extraction depends on your client / library
incoming_trace_id=<parsed from incoming traceparent>
my_span_id=$(openssl rand -hex 8)
outgoing="00-${incoming_trace_id}-${my_span_id}-01"

nats pub researcher.dispatch '<payload>' \
    -H "traceparent:${outgoing}" \
    -H "Sesh-Role:researcher"
```

### Terminal hop

If you don't publish downstream, log the incoming `traceparent`
alongside whatever you did. The trace ends naturally — no outgoing
header to set.

## Sampling

Default to always-on (`flags=01`) until volume becomes a problem. Once
it does, the standard pattern is sample-by-trace-id at the root: the
root agent decides sampled or not, and every downstream honors the flag
from the incoming `traceparent`. Don't second-guess the flag at later
hops — that produces broken traces with missing spans.

## Observability tooling

NATS headers survive through JetStream and across leaf-node hops. To
build an end-to-end trace view:

1. Run a tracing consumer subscribed to a wide subject (`>` or your
   workflow's root) that reads incoming messages and converts the NATS
   `traceparent` header into an OpenTelemetry span.
2. Export to the OTel Collector or directly to Jaeger / Honeycomb.

A starter "tail every published message, convert traceparent to OTLP
span, ship to collector" is roughly 50 lines of Go using `nats.go` plus
`go.opentelemetry.io/otel/sdk`.

## Minimum compliant agent

To participate in tracing an agent only needs to:

1. **On publish** — set `traceparent`. If it received one upstream,
   reuse the same trace-id and generate a fresh span-id. If it's the
   workflow root, generate both.
2. **On receive** — read `traceparent`, log it with whatever output the
   agent produces, and forward correctly on any downstream publishes.

Everything else (`Sesh-Role`, `Sesh-Task-Id`, `Sesh-Attempt`,
`Sesh-Envelope`) is optional metadata. The required behavior is: don't
break the trace.

## Further reading

- [W3C Trace Context specification](https://www.w3.org/TR/trace-context/)
- [OpenTelemetry NATS instrumentation](https://opentelemetry.io/ecosystem/registry/?language=go&component=instrumentation)
- [NATS message headers](https://docs.nats.io/nats-concepts/subjects#message-headers)
