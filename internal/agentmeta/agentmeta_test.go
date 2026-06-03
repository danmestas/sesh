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
	if got := DefaultedClass("active"); got != ClassActive {
		t.Errorf("DefaultedClass(active) = %v, want active", got)
	}
	// Class is free display metadata now — any non-empty value passes
	// through unchanged (no closed enum, no validation).
	if got := DefaultedClass("observer"); got != AgentClass("observer") {
		t.Errorf("DefaultedClass(observer) = %v, want observer (unchanged)", got)
	}
	if got := DefaultedClass("passive"); got != AgentClass("passive") {
		t.Errorf("DefaultedClass(passive) = %v, want passive (unchanged)", got)
	}
}
