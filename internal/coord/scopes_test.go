package coord

import "testing"

func TestScope_String(t *testing.T) {
	cases := []struct {
		s    Scope
		want string
	}{
		{ScopeHub, "hub"},
		{ScopeProject, "project"},
		{ScopeSession, "session"},
		{ScopeWorkflow, "workflow"},
		{ScopeAgent, "agent"},
	}
	for _, tc := range cases {
		if got := string(tc.s); got != tc.want {
			t.Errorf("Scope(%v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestScope_Valid(t *testing.T) {
	for _, s := range KnownScopes() {
		if !s.Valid() {
			t.Errorf("Scope(%q).Valid() = false, want true", s)
		}
	}
	if Scope("proj").Valid() {
		t.Errorf("Scope('proj').Valid() = true, want false")
	}
	if Scope("").Valid() {
		t.Errorf("Scope('').Valid() = true, want false")
	}
}
