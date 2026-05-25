package a2a

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/objects"
)

// TestTranslator_PassThrough_NoParts confirms a Message with no Parts
// serialises identically to the historic ToWireMessage shape — only
// Role swap, no extra fields appearing.
func TestTranslator_PassThrough_NoParts(t *testing.T) {
	tr := NewTranslator("https://example.test", nil)
	m := &messages.Message{
		ID:        "M1",
		TaskID:    "T1",
		ContextID: "C1",
		Role:      messages.MessageRoleAgent,
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["role"] != "ROLE_AGENT" {
		t.Errorf("role = %v", out["role"])
	}
	// Parts is omitted-when-nil-via-json-omitempty? Actually no — the
	// wireMessage struct doesn't tag it with omitempty, so we serialise
	// `"parts":null`. That's fine for the wire (A2A treats null === []).
	if _, present := out["parts"]; !present {
		t.Errorf("parts field missing from output: %s", raw)
	}
}

// TestTranslator_PassThrough_TextOnly — Parts that have only a Text
// field must come back byte-equal (no defensive obj:// machinery
// touching them).
func TestTranslator_PassThrough_TextOnly(t *testing.T) {
	tr := NewTranslator("https://example.test", nil)
	m := &messages.Message{
		ID:    "M1",
		Role:  messages.MessageRoleAgent,
		Parts: []messages.Part{{Text: "hello world"}},
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"text":"hello world"`)) {
		t.Errorf("text part not present: %s", raw)
	}
	if bytes.Contains(raw, []byte("obj://")) || bytes.Contains(raw, []byte("/obj/")) {
		t.Errorf("translator added unexpected obj/translation tokens: %s", raw)
	}
}

// TestTranslator_PassThrough_NonObjURL — a Part with a plain https://
// URL is not an obj reference; it passes through untouched.
func TestTranslator_PassThrough_NonObjURL(t *testing.T) {
	tr := NewTranslator("https://gateway.test", nil)
	m := &messages.Message{
		ID:    "M1",
		Role:  messages.MessageRoleAgent,
		Parts: []messages.Part{{URL: "https://upstream.test/foo.png", MediaType: "image/png"}},
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"url":"https://upstream.test/foo.png"`)) {
		t.Errorf("non-obj URL was modified: %s", raw)
	}
}

// TestTranslator_RewritesObjURL — the contract test. An obj:// URL on a
// Part becomes "<gateway>/obj/<kind>/<id>/<task>/<art>". Asserts the
// rewritten substring directly so the test fails loudly on format drift.
func TestTranslator_RewritesObjURL(t *testing.T) {
	tr := NewTranslator("https://shim.test", nil)
	origURI := objects.URI("project", "abc123", "T1", "A1")
	if origURI == "" {
		t.Fatalf("objects.URI returned empty (scope rejected)")
	}
	m := &messages.Message{
		ID:    "M1",
		Role:  messages.MessageRoleAgent,
		Parts: []messages.Part{{URL: origURI, MediaType: "application/octet-stream"}},
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	want := `"url":"https://shim.test/obj/project/abc123/T1/A1"`
	if !bytes.Contains(raw, []byte(want)) {
		t.Errorf("rewritten URL not found.\n got=%s\nwant substring=%s", raw, want)
	}
	if bytes.Contains(raw, []byte("obj://")) {
		t.Errorf("obj:// scheme leaked through: %s", raw)
	}
}

// TestTranslator_StripsTrailingSlashOnGateway — NewTranslator must
// normalise gateway URLs so a trailing "/" doesn't produce a double
// "//" in the rewritten URL.
func TestTranslator_StripsTrailingSlashOnGateway(t *testing.T) {
	tr := NewTranslator("https://shim.test/", nil)
	origURI := objects.URI("project", "abc123", "T1", "A1")
	m := &messages.Message{
		ID:    "M",
		Role:  messages.MessageRoleAgent,
		Parts: []messages.Part{{URL: origURI}},
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if bytes.Contains(raw, []byte("//obj/")) {
		t.Errorf("doubled slash in path: %s", raw)
	}
	if !bytes.Contains(raw, []byte("https://shim.test/obj/project/abc123/T1/A1")) {
		t.Errorf("expected normalised URL, got %s", raw)
	}
}

// TestTranslator_EmptyGatewayPassesThrough — when GatewayURL is the
// empty string, obj:// URLs survive verbatim (Slice-7 D5: pre-v0.4
// native clients keep working) and we INFO-log once per process.
func TestTranslator_EmptyGatewayPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tr := NewTranslator("", log)
	origURI := objects.URI("project", "abc123", "T1", "A1")
	m := &messages.Message{
		ID:    "M",
		Role:  messages.MessageRoleAgent,
		Parts: []messages.Part{{URL: origURI}, {URL: origURI}},
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if !bytes.Contains(raw, []byte(origURI)) {
		t.Errorf("obj:// URL was modified despite empty gateway: %s", raw)
	}
	// Two Parts in one call must produce exactly one INFO log line.
	if got := strings.Count(buf.String(), "GatewayURL empty"); got != 1 {
		t.Errorf("warnOnce fired %d times in one batch, want 1\nlog=%s", got, buf.String())
	}
	// Second call also must NOT log again.
	buf.Reset()
	if _, err := tr.ToWireMessage(m); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("second batch logged again: %s", buf.String())
	}
}

// TestTranslator_MalformedObjURL — a Part with a syntactically invalid
// obj:// URL falls through to pass-through with a WARN log (Slice-7
// Risk §1: defensive on TS-SDK-form URLs the Go parser rejects).
func TestTranslator_MalformedObjURL(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	tr := NewTranslator("https://shim.test", log)
	// TS-SDK shape: obj://<kind>/<id>/<task>/<art> (4-segment) — Go's
	// ParseURI rejects this because the authority is "project" not
	// "sesh_objects_project_abc123".
	bogus := "obj://project/abc123/T1/A1"
	m := &messages.Message{
		ID:    "M",
		Role:  messages.MessageRoleAgent,
		Parts: []messages.Part{{URL: bogus}},
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if !bytes.Contains(raw, []byte(bogus)) {
		t.Errorf("malformed URL was modified: %s", raw)
	}
	if !strings.Contains(buf.String(), "malformed obj://") {
		t.Errorf("expected WARN, log=%s", buf.String())
	}
}

// TestTranslator_MixedParts — only obj:// Parts get rewritten; siblings
// pass through untouched.
func TestTranslator_MixedParts(t *testing.T) {
	tr := NewTranslator("https://shim.test", nil)
	objURL := objects.URI("project", "abc123", "T1", "A1")
	m := &messages.Message{
		ID:   "M",
		Role: messages.MessageRoleAgent,
		Parts: []messages.Part{
			{Text: "see attachment"},
			{URL: "https://other.test/foo"},
			{URL: objURL, MediaType: "application/pdf"},
		},
	}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, `"text":"see attachment"`) {
		t.Errorf("text part missing: %s", text)
	}
	if !strings.Contains(text, `"url":"https://other.test/foo"`) {
		t.Errorf("plain URL part lost: %s", text)
	}
	if !strings.Contains(text, "https://shim.test/obj/project/abc123/T1/A1") {
		t.Errorf("obj URL not rewritten: %s", text)
	}
	if strings.Contains(text, "obj://") {
		t.Errorf("obj:// leaked: %s", text)
	}
}

// TestTranslator_DefensiveCopy — translatePart must NOT mutate the
// caller's input Parts. Without the defensive copy, every WatchEvent
// pointer in long-lived KV cache state would gradually rot.
func TestTranslator_DefensiveCopy(t *testing.T) {
	tr := NewTranslator("https://shim.test", nil)
	origURI := objects.URI("project", "abc123", "T1", "A1")
	parts := []messages.Part{{URL: origURI, MediaType: "image/png"}}
	m := &messages.Message{ID: "M", Role: messages.MessageRoleAgent, Parts: parts}

	if _, err := tr.ToWireMessage(m); err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if m.Parts[0].URL != origURI {
		t.Errorf("caller's Part.URL mutated:\n got=%s\nwant=%s", m.Parts[0].URL, origURI)
	}
	if parts[0].URL != origURI {
		t.Errorf("caller's parts slice mutated:\n got=%s\nwant=%s", parts[0].URL, origURI)
	}
}

// TestTranslator_NilMessageErrors — defensive.
func TestTranslator_NilMessageErrors(t *testing.T) {
	tr := NewTranslator("https://shim.test", nil)
	if _, err := tr.ToWireMessage(nil); err == nil {
		t.Error("nil message: want error")
	}
}

// TestTranslator_CrossStackGuard documents the TS-SDK divergence
// (plan §Open-Q-1). The Go SDK mints obj:// URLs with the bucket-as-
// authority shape; the TS SDK uses obj://<kind>/<id>/<task>/<art>. This
// test pins the Go-form byte-equality of both objects.URI and the
// translator's rewrite — if either shape silently changes, this test
// fails loudly. When the TS SDK realigns, extend this test with the
// parallel TS-form input and confirm it still passes.
func TestTranslator_CrossStackGuard(t *testing.T) {
	gateway := "https://shim.test"
	tr := NewTranslator(gateway, nil)

	const wantGoURI = "obj://sesh_objects_project_abc123/T1/A1"
	origURI := objects.URI("project", "abc123", "T1", "A1")
	if origURI != wantGoURI {
		t.Fatalf("Go SDK URI shape drifted:\n got=%s\nwant=%s", origURI, wantGoURI)
	}

	want := gateway + "/obj/project/abc123/T1/A1"
	m := &messages.Message{ID: "M", Role: messages.MessageRoleAgent, Parts: []messages.Part{{URL: origURI}}}
	raw, err := tr.ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if !bytes.Contains(raw, []byte(want)) {
		t.Errorf("cross-stack guard mismatch:\nraw=%s\nwant=%s", raw, want)
	}
}
