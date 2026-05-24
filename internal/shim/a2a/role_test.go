package a2a

import (
	"testing"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/danmestas/sesh-ops/messages"
)

func TestToSeshRole(t *testing.T) {
	tests := []struct {
		name string
		in   a2a.MessageRole
		want messages.MessageRole
	}{
		{"user", a2a.MessageRoleUser, messages.MessageRoleUser},
		{"agent", a2a.MessageRoleAgent, messages.MessageRoleAgent},
		{"unspecified", a2a.MessageRoleUnspecified, messages.MessageRole("")},
		{"unknown", a2a.MessageRole("ROLE_FOO"), messages.MessageRole("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToSeshRole(tt.in); got != tt.want {
				t.Errorf("ToSeshRole(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestToA2ARole(t *testing.T) {
	tests := []struct {
		name string
		in   messages.MessageRole
		want a2a.MessageRole
	}{
		{"user", messages.MessageRoleUser, a2a.MessageRoleUser},
		{"agent", messages.MessageRoleAgent, a2a.MessageRoleAgent},
		{"empty", messages.MessageRole(""), a2a.MessageRoleUnspecified},
		{"unknown", messages.MessageRole("system"), a2a.MessageRoleUnspecified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToA2ARole(tt.in); got != tt.want {
				t.Errorf("ToA2ARole(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRole_RoundTrip(t *testing.T) {
	for _, r := range []a2a.MessageRole{
		a2a.MessageRoleUser,
		a2a.MessageRoleAgent,
		a2a.MessageRoleUnspecified,
	} {
		if got := ToA2ARole(ToSeshRole(r)); got != r {
			t.Errorf("round-trip %q -> %q -> %q", r, ToSeshRole(r), got)
		}
	}
	for _, r := range []messages.MessageRole{
		messages.MessageRoleUser,
		messages.MessageRoleAgent,
		messages.MessageRole(""),
	} {
		if got := ToSeshRole(ToA2ARole(r)); got != r {
			t.Errorf("round-trip %q -> %q -> %q", r, ToA2ARole(r), got)
		}
	}
}
