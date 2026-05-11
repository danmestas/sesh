package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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
