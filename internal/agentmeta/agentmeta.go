// Package agentmeta is the canonical home for agent role/class types and
// validation. Both internal/refagent (emit side) and cli/agent_watcher
// (parse side) import this package so the rules live in one file.
//
// Rules are mirrored verbatim from docs/proposals/2026-05-21-agent-role-registration.md.
// Adapters in other languages MUST port the same rules — see that proposal's
// "Canonical role/class rules" section.
package agentmeta

// AgentClass is the on-the-wire class value. Defined as a string type so
// JSON marshaling matches the wire format. It is pure display metadata —
// carried in $SRV.INFO and heartbeats so `sesh mesh` can show and filter
// it — and is NOT consulted for subscription routing. (The class-driven
// observer/report subscription tier was removed in the smol scope-cut: no
// code published agents.report.*, and no path spawned an observer.)
type AgentClass string

const (
	ClassActive AgentClass = "active"

	DefaultRole  string     = "worker"
	DefaultClass AgentClass = ClassActive
)
