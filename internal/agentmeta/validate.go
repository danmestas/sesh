package agentmeta

import (
	"fmt"
	"regexp"
)

var roleTokenRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidateRole returns nil if role matches ^[a-z0-9_-]+$ and is 1-63 bytes.
func ValidateRole(role string) error {
	if role == "" {
		return fmt.Errorf("role is empty")
	}
	if len(role) > 63 {
		return fmt.Errorf("role %q is %d bytes; max 63", role, len(role))
	}
	if !roleTokenRE.MatchString(role) {
		return fmt.Errorf("role %q must match ^[a-z0-9_-]+$", role)
	}
	return nil
}

// ValidateClass returns nil iff c is one of the canonical class values.
func ValidateClass(c AgentClass) error {
	if c != ClassActive && c != ClassObserver {
		return fmt.Errorf("class %q must be %q or %q", string(c), ClassActive, ClassObserver)
	}
	return nil
}

// DefaultedRole returns s, or DefaultRole if s is empty.
// Does NOT validate — caller decides whether to validate after defaulting.
func DefaultedRole(s string) string {
	if s == "" {
		return DefaultRole
	}
	return s
}

// DefaultedClass returns AgentClass(s), or DefaultClass if s is empty.
// Does NOT validate — used on the read path where unknown values should
// surface as-is so the caller can decide between defaulting and erroring.
func DefaultedClass(s string) AgentClass {
	if s == "" {
		return DefaultClass
	}
	return AgentClass(s)
}
