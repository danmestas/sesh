package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionState_RoundTrip exercises JSON encode/decode with the full
// field set. Codifies the on-disk schema sub-leaves and clients consume.
func TestSessionState_RoundTrip(t *testing.T) {
	in := SessionState{
		PID:     12345,
		NATSURL: "nats://127.0.0.1:54321",
		LeafURL: "nats-leaf://127.0.0.1:54322",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out SessionState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestSessionState_BackCompat verifies a PID-only file written by an older
// sesh still parses cleanly — empty URL fields rather than parse failure.
// This keeps in-flight session files readable across a version bump.
func TestSessionState_BackCompat(t *testing.T) {
	old := []byte(`{"pid":99}`)
	var s SessionState
	if err := json.Unmarshal(old, &s); err != nil {
		t.Fatalf("legacy parse failed: %v", err)
	}
	if s.PID != 99 || s.NATSURL != "" || s.LeafURL != "" {
		t.Fatalf("legacy parse got %+v", s)
	}
}

// TestUpdateSessionState_OverwritesFile claims a session, then updates
// with URLs, and verifies the on-disk file matches.
func TestUpdateSessionState_OverwritesFile(t *testing.T) {
	tmp := t.TempDir()
	chdir(t, tmp)

	release, err := claimSessionState("alpha", os.Getpid())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer release()

	want := SessionState{
		PID:     os.Getpid(),
		NATSURL: "nats://127.0.0.1:4222",
		LeafURL: "nats-leaf://127.0.0.1:7422",
	}
	if err := updateSessionState("alpha", want); err != nil {
		t.Fatalf("update: %v", err)
	}

	path := filepath.Join(tmp, ".sesh", "sessions", "alpha.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got SessionState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("on-disk mismatch: got %+v want %+v", got, want)
	}
}

// TestUpdateSessionState_RequiresClaim refuses to update a session that
// hasn't been claimed — protects against writing a state file for a session
// that no live process owns.
func TestUpdateSessionState_RequiresClaim(t *testing.T) {
	tmp := t.TempDir()
	chdir(t, tmp)

	err := updateSessionState("ghost", SessionState{PID: 1, NATSURL: "x", LeafURL: "y"})
	if err == nil {
		t.Fatalf("expected error updating unclaimed session, got nil")
	}
}

// TestLoadOrCreateProjectCode_PinsOnFirstCall verifies that the project-code
// is generated once and then read back unchanged on subsequent calls — the
// core pinning invariant. A second call MUST NOT re-derive (i.e. mutating
// the on-disk file mid-test should be reflected by the second call, proving
// the second call read from disk rather than re-deriving).
func TestLoadOrCreateProjectCode_PinsOnFirstCall(t *testing.T) {
	tmp := t.TempDir()

	first, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !isValidProjectCode(first) {
		t.Fatalf("first call returned non-hex: %q", first)
	}

	second, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second != first {
		t.Fatalf("second call mismatch: got %q want %q", second, first)
	}

	// Mutate the pinned file to a different valid code and call again —
	// if the third call returns the mutated value (not a fresh derivation
	// from projectName), the function is reading the pin from disk.
	mutated := strings.Repeat("a", 40)
	if err := os.WriteFile(projectCodePath(tmp), []byte(mutated+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite project-code: %v", err)
	}
	third, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if third != mutated {
		t.Fatalf("third call did not read pin from disk: got %q want %q", third, mutated)
	}
}

// TestLoadOrCreateProjectCode_BackwardCompatSeedsFromHostname verifies the
// backward-compat path: on a fresh dir (no .sesh/project-code), the function
// seeds the file from deriveProjectCode(projectName) so existing projects
// already running with that derived code stay subscribed to the same
// EdgeSync sync subject after this change rolls out.
func TestLoadOrCreateProjectCode_BackwardCompatSeedsFromHostname(t *testing.T) {
	tmp := t.TempDir()

	got, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("loadOrCreate: %v", err)
	}
	want := deriveProjectCode("myproj")
	if got != want {
		t.Fatalf("seed mismatch: got %q want %q", got, want)
	}

	// File should now exist with the same content.
	data, err := os.ReadFile(projectCodePath(tmp))
	if err != nil {
		t.Fatalf("read project-code: %v", err)
	}
	if strings.TrimSpace(string(data)) != want {
		t.Fatalf("file content mismatch: got %q want %q", string(data), want)
	}
}

// TestLoadOrCreateProjectCode_RejectsCorruptedFile verifies that a mangled
// project-code file surfaces a clear error rather than silently overwriting.
// Users with a corrupted file have bigger problems and need to see them.
func TestLoadOrCreateProjectCode_RejectsCorruptedFile(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".sesh"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"short", "abc123"},
		{"long", strings.Repeat("a", 41)},
		{"uppercase", strings.Repeat("A", 40)},
		{"non-hex", strings.Repeat("z", 40)},
		{"garbage", "this is not a project code at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(projectCodePath(tmp), []byte(tc.content), 0o644); err != nil {
				t.Fatalf("seed corrupted file: %v", err)
			}
			_, err := loadOrCreateProjectCode(tmp, "myproj")
			if err == nil {
				t.Fatalf("expected error for corrupted content %q, got nil", tc.content)
			}
			if !strings.Contains(err.Error(), "invalid project-code") {
				t.Fatalf("unexpected error message: %v", err)
			}
		})
	}
}

// TestLoadOrCreateProjectCode_StableAcrossHostnameChange simulates the bug
// scenario from issue #16: a project pinned its code on hostname A, then
// the machine is cloned / renamed to hostname B. The pinned code must
// survive — that's the whole point of the pinning.
//
// os.Hostname() reads from the OS rather than an env var, so we simulate
// the prior-host derivation by writing the file directly with the value
// deriveProjectCodeFromHost would have produced under hostname A, then
// verify loadOrCreateProjectCode returns that exact value regardless of
// the current host.
func TestLoadOrCreateProjectCode_StableAcrossHostnameChange(t *testing.T) {
	tmp := t.TempDir()
	pinned := deriveProjectCodeFromHost("old-hostname-A", "myproj")
	if err := os.MkdirAll(filepath.Join(tmp, ".sesh"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(projectCodePath(tmp), []byte(pinned+"\n"), 0o644); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	// Sanity: the pinned value should differ from the current host's
	// derivation (otherwise the test isn't actually exercising the
	// "different hostname" case). Skip the assertion if by some
	// astronomical coincidence the current host hashes to the same value.
	currentDerived := deriveProjectCode("myproj")
	if pinned == currentDerived {
		t.Skipf("current host happens to match 'old-hostname-A' derivation; test inconclusive")
	}

	got, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("loadOrCreate: %v", err)
	}
	if got != pinned {
		t.Fatalf("pin did not survive hostname change: got %q want %q (current-host derive: %q)", got, pinned, currentDerived)
	}
}

// chdir switches the test cwd and restores on cleanup. sesh's state helpers
// derive paths from os.Getwd, so tests must own the cwd.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
