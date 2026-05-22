package coord

import (
	"fmt"
	"strings"
)

// Subject describes one publish subject in the sesh coordination space.
// Wire form is:
//
//	sesh.<verb>.<machine>.<scope>.<scope-id>.<target>.<role>
//
// All fields are required for a publishable subject (Validate enforces).
// Filter (filter.go) is the parallel type for subscribe-side wildcards.
//
// The struct is a plain value with no constructor — callers fill fields
// explicitly so zero-value detection in Validate catches forgotten
// fields. Sharing a Subject across goroutines is safe (immutable after
// construction).
//
// Field meanings:
//
//	Verb    — the intent (task, broadcast, control, ...). See verbs.go.
//	Machine — the publishing host's identity. Use coord.Machine() to
//	          resolve the local one; "_local" (MachineLocal) for
//	          single-host setups.
//	Scope   — which tier of the five-scope model. See scopes.go.
//	ScopeID — the identifier for that scope:
//	            ScopeHub      → "" or a singleton
//	            ScopeProject  → project-id from cli.loadOrCreateProjectID
//	            ScopeSession  → the session label
//	            ScopeWorkflow → first 8 hex of the W3C trace-id
//	            ScopeAgent    → the agent instance id
//	Target  — grouping inside the scope: "workers", "spies",
//	          "swarm-<x>", "reviewers", "findings", "all", ...
//	          One tier above Role in the topic tree (proposal
//	          amendment §7).
//	Role    — the function being played: "worker", "implementer",
//	          "verifier", "spy", ... Sourced from agent metadata
//	          (agentmeta.AgentClass / role on registration).
type Subject struct {
	Verb    Verb
	Machine string
	Scope   Scope
	ScopeID string
	Target  string
	Role    string
}

// String returns the wire form. Always seven tokens joined by '.'.
//
// Does NOT validate — callers receiving a Subject from an untrusted
// source MUST call Validate first. String is intentionally fast
// (single allocation, no map lookups) so subscribers reading their
// own subjects in a hot loop don't pay validation cost.
func (s Subject) String() string {
	var b strings.Builder
	// Pre-size: "sesh." + 5 dots + 6 tokens. Most tokens are ≤16 bytes;
	// 128 covers the common case in one allocation.
	b.Grow(128)
	b.WriteString("sesh.")
	b.WriteString(string(s.Verb))
	b.WriteByte('.')
	b.WriteString(s.Machine)
	b.WriteByte('.')
	b.WriteString(string(s.Scope))
	b.WriteByte('.')
	b.WriteString(s.ScopeID)
	b.WriteByte('.')
	b.WriteString(s.Target)
	b.WriteByte('.')
	b.WriteString(s.Role)
	return b.String()
}

// Validate returns nil iff every segment is well-formed:
//   - Verb is one of the six committed verbs
//   - Machine is a valid token (use MachineLocal for single-host)
//   - Scope is one of the five canonical scopes
//   - ScopeID is a valid token (non-empty)
//   - Target is a valid token (non-empty)
//   - Role is a valid token (non-empty)
//
// Returns the FIRST error encountered, prefixed with the segment name
// so the operator knows which field to fix.
func (s Subject) Validate() error {
	if !s.Verb.Valid() {
		return fmt.Errorf("verb %q: not one of the six committed verbs (task, broadcast, control, announce, blackboard, report)", string(s.Verb))
	}
	if err := validateToken(s.Machine); err != nil {
		return fmt.Errorf("machine: %w", err)
	}
	if !s.Scope.Valid() {
		return fmt.Errorf("scope %q: not one of the five canonical scopes (hub, project, session, workflow, agent)", string(s.Scope))
	}
	if err := validateToken(s.ScopeID); err != nil {
		return fmt.Errorf("scope-id: %w", err)
	}
	if err := validateToken(s.Target); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if err := validateToken(s.Role); err != nil {
		return fmt.Errorf("role: %w", err)
	}
	return nil
}

// QueueGroup returns the queue group a subscriber for this subject
// should use, given the role on the Subject. Delegates to Verb.QueueGroup.
//
// Convenience method — equivalent to s.Verb.QueueGroup(s.Role) — but
// the call site is shorter and less error-prone (no risk of passing the
// wrong role).
func (s Subject) QueueGroup() string {
	return s.Verb.QueueGroup(s.Role)
}
