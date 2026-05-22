package coord

import (
	"strings"
	"testing"
)

func TestValidateToken(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		wantOK bool
		errSub string
	}{
		{"alpha", "implementer", true, ""},
		{"alpha-hyphen", "swarm-alpha", true, ""},
		{"alpha-underscore", "swarm_alpha", true, ""},
		{"hex", "a3f2c1d8", true, ""},
		{"mixed", "Swarm-Alpha-7", true, ""},
		{"sentinel", "_local", true, ""},
		{"empty", "", false, "empty"},
		{"dot", "foo.bar", false, "dot"},
		{"space", "foo bar", false, "whitespace"},
		{"tab", "foo\tbar", false, "whitespace"},
		{"dollar-prefix", "$SRV", false, "leading"},
		{"angle", "foo>bar", false, "illegal char"},
		{"star", "foo*bar", false, "illegal char"},
		{"slash", "foo/bar", false, "illegal char"},
		{"too long", strings.Repeat("a", 64), false, "max 63"},
		{"63 ok", strings.Repeat("a", 63), true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateToken(tc.token)
			if tc.wantOK && err != nil {
				t.Fatalf("validateToken(%q) = %v, want nil", tc.token, err)
			}
			if !tc.wantOK {
				if err == nil {
					t.Fatalf("validateToken(%q) = nil, want error containing %q", tc.token, tc.errSub)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("validateToken(%q) err = %v, want substring %q", tc.token, err, tc.errSub)
				}
			}
		})
	}
}
