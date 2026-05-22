package agentmeta

import (
	"strings"
	"testing"
)

func TestValidateRole(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		wantOK bool
		errSub string
	}{
		{"worker", "worker", true, ""},
		{"implementer", "implementer", true, ""},
		{"hyphen-and-underscore_ok", "abc-def_ghi", true, ""},
		{"digits", "v2", true, ""},
		{"empty", "", false, "empty"},
		{"uppercase", "Worker", false, "must match"},
		{"space", "im plementer", false, "must match"},
		{"slash", "im/plementer", false, "must match"},
		{"too long", strings.Repeat("a", 64), false, "max 63"},
		{"63 ok", strings.Repeat("a", 63), true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRole(tc.role)
			if tc.wantOK && err != nil {
				t.Fatalf("ValidateRole(%q) = %v, want nil", tc.role, err)
			}
			if !tc.wantOK {
				if err == nil {
					t.Fatalf("ValidateRole(%q) = nil, want error containing %q", tc.role, tc.errSub)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("ValidateRole(%q) err = %v, want substring %q", tc.role, err, tc.errSub)
				}
			}
		})
	}
}

func TestValidateClass(t *testing.T) {
	if err := ValidateClass(ClassActive); err != nil {
		t.Errorf("ValidateClass(active) = %v, want nil", err)
	}
	if err := ValidateClass(ClassObserver); err != nil {
		t.Errorf("ValidateClass(observer) = %v, want nil", err)
	}
	if err := ValidateClass(AgentClass("passive")); err == nil {
		t.Errorf("ValidateClass(passive) = nil, want error")
	}
	if err := ValidateClass(AgentClass("")); err == nil {
		t.Errorf("ValidateClass(empty) = nil, want error")
	}
}

func TestDefaultedRole(t *testing.T) {
	if got := DefaultedRole(""); got != DefaultRole {
		t.Errorf("DefaultedRole(empty) = %q, want %q", got, DefaultRole)
	}
	if got := DefaultedRole("implementer"); got != "implementer" {
		t.Errorf("DefaultedRole(implementer) = %q, want implementer", got)
	}
}

func TestDefaultedClass(t *testing.T) {
	if got := DefaultedClass(""); got != DefaultClass {
		t.Errorf("DefaultedClass(empty) = %v, want %v", got, DefaultClass)
	}
	if got := DefaultedClass("observer"); got != ClassObserver {
		t.Errorf("DefaultedClass(observer) = %v, want observer", got)
	}
	if got := DefaultedClass("active"); got != ClassActive {
		t.Errorf("DefaultedClass(active) = %v, want active", got)
	}
	// Unknown values pass through unchanged — validation is the caller's job.
	if got := DefaultedClass("passive"); got != AgentClass("passive") {
		t.Errorf("DefaultedClass(passive) = %v, want passive (unchanged)", got)
	}
}
