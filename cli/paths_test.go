package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectID_DeriveIsHostnameFree verifies that deriveProjectID produces a
// stable value that is independent of the host — unlike deriveProjectCodeFromHost
// which salts the hash with the hostname. Two calls with the same project name
// must agree and must differ from a hostname-salted project-code.
func TestProjectID_DeriveIsHostnameFree(t *testing.T) {
	id1 := deriveProjectID("myproject")
	id2 := deriveProjectID("myproject")
	if id1 != id2 {
		t.Errorf("deriveProjectID not deterministic: %q != %q", id1, id2)
	}
	if len(id1) != 40 {
		t.Errorf("deriveProjectID returned %d chars; want 40", len(id1))
	}
	// Must differ from hostname-salted project-code.
	code := deriveProjectCodeFromHost("some-host", "myproject")
	if id1 == code {
		t.Error("project-id must not equal hostname-salted project-code")
	}
	// Different projects must produce different ids.
	if other := deriveProjectID("otherproject"); other == id1 {
		t.Error("deriveProjectID: different project names produced same id")
	}
}

// TestProjectID_LoadOrCreate_PinsAndReloads verifies the pin-on-first-run
// semantic: the first call creates .sesh/project-id, the second call reads
// the pinned value back unchanged.
func TestProjectID_LoadOrCreate_PinsAndReloads(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".sesh"), 0o755); err != nil {
		t.Fatal(err)
	}

	id1, err := loadOrCreateProjectID(cwd, "myproject")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(id1) != 40 {
		t.Errorf("id1 len = %d; want 40", len(id1))
	}

	id2, err := loadOrCreateProjectID(cwd, "myproject")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if id1 != id2 {
		t.Errorf("id reloaded as %q; want %q", id2, id1)
	}

	// The pinned file must contain the id.
	data, err := os.ReadFile(projectIDPath(cwd))
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != id1 {
		t.Errorf("pinned project-id = %q; want %q", got, id1)
	}
}

// TestProjectID_RejectsMangled verifies that a corrupted .sesh/project-id
// file returns an error rather than silently overwriting.
func TestProjectID_RejectsMangled(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".sesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectIDPath(cwd), []byte("not-hex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateProjectID(cwd, "myproject"); err == nil {
		t.Error("expected error on mangled project-id, got nil")
	}
}

// TestSanitizeProjectToken pins the cross-language sanitization contract:
// sanitizeProjectToken MUST mirror claude-nats-channel server.ts
// sanitizeSessionName byte-for-byte — (1) every char NOT in [A-Za-z0-9_-]
// becomes a single '-' (no '+' run-collapsing), (2) lowercase, (3) trim
// leading/trailing '-'. These cases are copied from the TS adapter test and
// are the wire contract; both languages must agree.
func TestSanitizeProjectToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Sesh", "sesh"},
		{"my repo!", "my-repo"},    // space->-, !->-, trailing - trimmed
		{"-x-", "x"},               // leading/trailing dashes trimmed
		{"a  b", "a--b"},           // two spaces -> two dashes, NOT collapsed
		{"a..b", "a--b"},           // two dots -> two dashes, NOT collapsed (single-char class, no '+')
		{"sesh-talk", "sesh-talk"}, // already clean, unchanged
		{"", ""},                   // empty stays empty
	}
	for _, tc := range cases {
		got := sanitizeProjectToken(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeProjectToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Idempotency: re-sanitizing a sanitized value is a no-op.
		if again := sanitizeProjectToken(got); again != got {
			t.Errorf("sanitizeProjectToken not idempotent: f(f(%q)) = %q, want %q", tc.in, again, got)
		}
	}
}
