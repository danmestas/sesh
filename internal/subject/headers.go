package subject

// Protocol-version + capability + wire-mode header surface for v0.4
// outbound NATS messages. Every request, reply, and publish carries
// these headers so subscribers can dispatch (e.g. Synadia vs. A2A)
// without inferring intent from the subject path.
//
// Mirror set: sesh-channels/sdk/src/headers.ts. The header NAMES and
// LITERAL VALUES are part of the cross-stack wire contract — keep both
// sides byte-identical. headers_test.go pins the literals; the TS test
// suite pins the same set on its side.
const (
	// HeaderProtocolVersion identifies the sesh wire version. Producers
	// stamp every outbound message with CurrentProtocolVersion; consumers
	// SHOULD reject or warn on absent/unknown values, but MUST NOT fall
	// back to subject-path heuristics.
	HeaderProtocolVersion = "Sesh-Protocol-Version"

	// HeaderCapabilities is a comma-separated list of optional message
	// behaviors the producer supports. Used by the consumer to negotiate
	// reply shape (e.g. emit artifacts only when "artifacts" appears).
	HeaderCapabilities = "Sesh-Capabilities"

	// HeaderWire selects the message payload dialect carried inside the
	// envelope. The shim's A2A/Synadia dispatcher routes on this header
	// alone — never on the subject path. Optional; absent header means
	// default = WireSynadia (the Synadia agent-protocol message shape).
	HeaderWire = "Sesh-Wire"
)

const (
	// CurrentProtocolVersion is the wire version this build of sesh
	// emits. Stamped onto every outbound NATS message via
	// HeaderProtocolVersion. Bumping this requires a coordinated cross-
	// stack PR (Go + TS SDK + every adapter) — see the v0.4 cutover
	// plan for the structural template.
	CurrentProtocolVersion = "0.4"

	// DefaultCapabilities is the capability set advertised by stock
	// sesh adapters and shims when the operator hasn't narrowed the
	// set. Comma-separated list of A2A-aligned feature tokens; the
	// consumer parses it field-by-field.
	DefaultCapabilities = "messages,artifacts,cards"

	// WireSynadia is the default value for HeaderWire — message payload
	// follows the Synadia agent-protocol shape. Shim treats this as the
	// fallback when HeaderWire is absent.
	WireSynadia = "synadia"

	// WireA2A is the alternative value for HeaderWire — message payload
	// follows the A2A JSON-RPC shape carried inline on NATS. Shim's
	// header-based dispatcher routes A2A-tagged messages to the
	// JSON-RPC translator.
	WireA2A = "a2a"
)
