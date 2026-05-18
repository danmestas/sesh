package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeshDown_RejectsLabelTraversal is the tier-1 safety test for the
// down entrypoint. It exercises validateLabel through `sesh down
// --session` with the hostile inputs that would otherwise let the label
// escape its slot under .sesh/. ReadSession / Terminate compose
// <stateDir>/<label>.json; a hostile label like "../sessions" would let
// those calls read or SIGKILL state outside .sesh/sessions/ if the
// validator weren't gating the entrypoint.
//
// We seed a known canary under .sesh/messaging/ plus a sentinel session
// JSON under .sesh/sessions/, fingerprint the .sesh/ tree, run the
// hostile-input matrix, and assert: each invocation exits non-zero, no
// path under .sesh/ mutates, and the canary survives byte-for-byte.
func TestSeshDown_RejectsLabelTraversal(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	cases := []struct {
		name  string
		label string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"slash_prefix", "/etc"},
		{"slash_embedded", "foo/bar"},
		{"backslash_embedded", "foo\\bar"},
		{"dotdot_embedded", "alpha/../beta"},
		{"dotdot_only_embedded", "x..y"},
		{"nul_byte", "alpha\x00beta"},
		{"leading_dot", ".sessions"},
		{"whitespace_only", "   "},
		{"control_char", "alpha\x01"},
		{"newline", "alpha\nbeta"},
		{"parent_sessions", "../sessions"},
		{"deeper_traversal", "../../foo"},
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	seshDir := filepath.Join(project, ".sesh")
	if err := os.MkdirAll(filepath.Join(seshDir, "sessions"), 0o755); err != nil {
		t.Fatalf("seed .sesh/sessions: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(seshDir, "messaging"), 0o755); err != nil {
		t.Fatalf("seed .sesh/messaging: %v", err)
	}
	canary := filepath.Join(seshDir, "messaging", "canary.txt")
	if err := os.WriteFile(canary, []byte("tier-1\n"), 0o644); err != nil {
		t.Fatalf("seed canary: %v", err)
	}
	// Plant a sentinel session JSON. If validateLabel were absent and a
	// hostile label like "../sessions" slipped through, ReadSession +
	// Terminate could find and act on this file via the wrong path. The
	// fingerprint comparison would catch any mutation regardless, but
	// the explicit sentinel makes the threat model legible.
	sentinel := filepath.Join(seshDir, "sessions", "alpha.json")
	const sentinelPayload = `{"pid":1,"nats_url":"sentinel"}`
	if err := os.WriteFile(sentinel, []byte(sentinelPayload), 0o644); err != nil {
		t.Fatalf("seed sentinel session JSON: %v", err)
	}

	before := fingerprintTree(t, seshDir)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, "down", "--session="+tc.label)
			cmd.Dir = project
			cmd.Env = append(os.Environ(), "HOME="+home)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err == nil {
				t.Fatalf("sesh down accepted hostile label %q; stdout=%q stderr=%q",
					tc.label, stdout.String(), stderr.String())
			}
			// Either Kong rejects the flag (empty label), our
			// validator rejects it, or os/exec refuses the arg
			// outright (NUL byte case — argv can't carry NULs on
			// POSIX). All three are acceptable provided the exit
			// is non-zero and tier-1 .sesh/ paths survive
			// (asserted at the end of the parent test).
			combined := strings.ToLower(stderr.String() + stdout.String() + err.Error())
			if !strings.Contains(combined, "label") && !strings.Contains(combined, "session") && !strings.Contains(combined, "invalid argument") {
				t.Errorf("hostile label %q rejected but no 'label'/'session'/'invalid argument' cue; err=%v stderr=%s",
					tc.label, err, stderr.String())
			}
		})
	}

	after := fingerprintTree(t, seshDir)
	if before != after {
		t.Errorf("tier-1 .sesh/ tree fingerprint drifted after hostile-input down runs:\nbefore=%s\nafter=%s",
			before, after)
	}
	if got, err := os.ReadFile(canary); err != nil || string(got) != "tier-1\n" {
		t.Errorf("canary %s mutated by hostile-input down runs; got=%q err=%v",
			canary, string(got), err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != sentinelPayload {
		t.Errorf("sentinel session JSON %s mutated by hostile-input down runs; got=%q err=%v",
			sentinel, string(got), err)
	}
}
