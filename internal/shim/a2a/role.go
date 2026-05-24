// Package a2a translates between the A2A v1.0 wire shapes (per
// github.com/a2aproject/a2a-go/v2) and the sesh-ops storage shapes.
// Slice 3 covers MessageRole and Message JSON; later slices extend with
// TaskState and artifact-event translation.
//
// Why this lives in its own package: the cross-stack divergence —
// a2a-go uses "ROLE_USER"/"ROLE_AGENT", sesh-ops uses "user"/"agent" —
// is the single most fragile contract in the shim. A named package
// gives a grep-able home and pins the mapping behind a test file.
package a2a

import (
	a2a "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/danmestas/sesh-ops/messages"
)

// ToSeshRole maps an A2A wire-format MessageRole to the sesh-ops
// short-form. ROLE_UNSPECIFIED / "" round-trips to "".
func ToSeshRole(r a2a.MessageRole) messages.MessageRole {
	switch r {
	case a2a.MessageRoleUser:
		return messages.MessageRoleUser
	case a2a.MessageRoleAgent:
		return messages.MessageRoleAgent
	default:
		return messages.MessageRole("")
	}
}

// ToA2ARole inverts ToSeshRole. Unknown values round-trip as
// MessageRoleUnspecified rather than fabricating a wire constant.
func ToA2ARole(r messages.MessageRole) a2a.MessageRole {
	switch r {
	case messages.MessageRoleUser:
		return a2a.MessageRoleUser
	case messages.MessageRoleAgent:
		return a2a.MessageRoleAgent
	default:
		return a2a.MessageRoleUnspecified
	}
}
