package coord

// Scope is the third segment in a coordination subject (after verb and
// machine). It identifies which scope of the five-tier model
// (docs/scoped-memory.md) the message belongs to.
//
// The scope dictates how the <scope-id> segment is derived:
//   - ScopeHub      → "" or a singleton identifier; only one hub per host
//   - ScopeProject  → project-id (hostname-free; see cli/paths.go.deriveProjectID)
//   - ScopeSession  → the session label
//   - ScopeWorkflow → the W3C trace-id, first 8 hex
//   - ScopeAgent    → the agent instance id
type Scope string

const (
	ScopeHub      Scope = "hub"
	ScopeProject  Scope = "project"
	ScopeSession  Scope = "session"
	ScopeWorkflow Scope = "workflow"
	ScopeAgent    Scope = "agent"
)

// KnownScopes returns the canonical list of scopes in tier order
// (broadest to narrowest). Useful for documentation generators and
// validation loops.
func KnownScopes() []Scope {
	return []Scope{ScopeHub, ScopeProject, ScopeSession, ScopeWorkflow, ScopeAgent}
}

// Valid reports whether s is one of the five canonical scopes.
func (s Scope) Valid() bool {
	switch s {
	case ScopeHub, ScopeProject, ScopeSession, ScopeWorkflow, ScopeAgent:
		return true
	default:
		return false
	}
}
