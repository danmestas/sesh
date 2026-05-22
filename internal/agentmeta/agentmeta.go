// Package agentmeta is the canonical home for agent role/class types and
// validation. Both internal/refagent (emit side) and cli/agent_watcher
// (parse side) import this package so the rules live in one file.
//
// Rules are mirrored verbatim from docs/proposals/2026-05-21-agent-role-registration.md.
// Adapters in other languages MUST port the same rules — see that proposal's
// "Canonical role/class rules" section.
package agentmeta

// AgentClass is the on-the-wire class enum. Defined as a string type so
// JSON marshaling matches the wire format, while still letting the
// compiler reject typo'd assignments at every call site that uses the
// constants.
type AgentClass string

const (
	ClassActive   AgentClass = "active"
	ClassObserver AgentClass = "observer"

	DefaultRole  string     = "worker"
	DefaultClass AgentClass = ClassActive
)
