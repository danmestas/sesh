package coord

import (
	"fmt"
	"strings"
)

// Wildcards usable inside a Filter. Both are passed verbatim into the
// produced NATS subject pattern.
const (
	// WildOne matches exactly one token (NATS `*`). Legal in any position.
	WildOne = "*"

	// WildTail matches one or more trailing tokens (NATS `>`). Legal
	// ONLY in the final position of a subject; using it elsewhere is
	// a validation error.
	WildTail = ">"
)

// Filter is a subscribe-side subject pattern under the sesh
// coordination space. Same field order as Subject but each field may
// be a literal token, WildOne, or — only in the final non-empty
// position — WildTail.
//
// Empty trailing fields are elided from the produced subject pattern.
// A Filter with `Verb: "task", Machine: "_local", Scope: "project",
// ScopeID: "a3", Target: ">"` produces `sesh.task._local.project.a3.>`
// — no trailing zero-token segment.
//
// String fields rather than Verb/Scope typed fields because callers
// pass WildOne in many positions and we don't want a Verb("*") that
// the type system thinks is a real verb. Validation still enforces
// that non-wildcard values are valid.
//
// String() is documented as a fast-path (no validation); callers MUST
// call Validate() first when constructing filters from untrusted input.
type Filter struct {
	Verb    string
	Machine string
	Scope   string
	ScopeID string
	Target  string
	Role    string
}

// String returns the wire-form subscription pattern. Trailing empty
// fields are dropped; the first non-empty field after an empty field
// is also illegal (subjects can't have gaps), and Validate catches it.
func (f Filter) String() string {
	segments := []string{
		string(f.Verb),
		f.Machine,
		string(f.Scope),
		f.ScopeID,
		f.Target,
		f.Role,
	}
	// Trim trailing empties.
	for len(segments) > 0 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	var b strings.Builder
	b.Grow(128)
	b.WriteString("sesh")
	for _, seg := range segments {
		b.WriteByte('.')
		b.WriteString(seg)
	}
	return b.String()
}

// Validate enforces the wildcard placement rules and per-field
// well-formedness:
//
//   - Wildcards (`*`, `>`) are recognized in any field.
//   - WildTail (`>`) MUST be the last non-empty segment.
//   - Empty segments are only allowed contiguously at the end (no gaps).
//   - Non-wildcard values for Verb and Scope must be members of the
//     respective enums.
//   - Non-wildcard values for Machine, ScopeID, Target, Role must pass
//     validateTokenOrWildcard.
func (f Filter) Validate() error {
	segments := []struct {
		name  string
		value string
	}{
		{"verb", f.Verb},
		{"machine", f.Machine},
		{"scope", f.Scope},
		{"scope-id", f.ScopeID},
		{"target", f.Target},
		{"role", f.Role},
	}

	// Find the last non-empty segment.
	lastNonEmpty := -1
	for i, seg := range segments {
		if seg.value != "" {
			lastNonEmpty = i
		}
	}
	if lastNonEmpty == -1 {
		return fmt.Errorf("filter is empty (every segment blank)")
	}

	// Reject gaps: every empty segment must come AFTER the last
	// non-empty one.
	for i := 0; i < lastNonEmpty; i++ {
		if segments[i].value == "" {
			return fmt.Errorf("segment %q is empty but later segment is set; subjects cannot have gaps", segments[i].name)
		}
	}

	// Check each present segment.
	for i, seg := range segments {
		if seg.value == "" {
			continue
		}
		// WildTail only legal at lastNonEmpty.
		if seg.value == WildTail && i != lastNonEmpty {
			return fmt.Errorf("recursive wildcard '>' in segment %q is only legal in the final segment", seg.name)
		}
		// WildOne legal anywhere; no further check.
		if seg.value == WildOne {
			continue
		}
		// WildTail at the final position: legal, no further check.
		if seg.value == WildTail {
			continue
		}
		// Concrete value: enum check for verb/scope, token check for others.
		switch seg.name {
		case "verb":
			if !Verb(seg.value).Valid() {
				return fmt.Errorf("verb %q: not one of the six committed verbs", seg.value)
			}
		case "scope":
			if !Scope(seg.value).Valid() {
				return fmt.Errorf("scope %q: not one of the five canonical scopes", seg.value)
			}
		default:
			if err := validateToken(seg.value); err != nil {
				return fmt.Errorf("%s: %w", seg.name, err)
			}
		}
	}
	return nil
}
