package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/subject"
)

// stubCardSubject subscribes to subj and responds with body to every
// request. Returns the subscription's Unsubscribe via t.Cleanup.
func stubCardSubject(t *testing.T, nc *nats.Conn, subj string, body []byte) {
	t.Helper()
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		_ = m.Respond(body)
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subj, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestMeshCardCmd_Public_Happy(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.Card(subject.Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"public echo card","skills":[]}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "s1",
		Project: "p1",
		Machine: "m1",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "json",
		Out:     &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "public echo card") {
		t.Errorf("output missing description:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "agents.card.m1.p1.s1") {
		t.Errorf("output missing subject header:\n%s", out.String())
	}
}

func TestMeshCardCmd_Extended_Happy(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.Cardx(subject.Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Cardx: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"extended view","skills":[]}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session:  "s1",
		Project:  "p1",
		Machine:  "m1",
		Extended: true,
		NATSURL:  url,
		Window:   500 * time.Millisecond,
		Format:   "json",
		Out:      &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "extended view") {
		t.Errorf("output missing extended description:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "agents.cardx.m1.p1.s1") {
		t.Errorf("output missing extended subject header:\n%s", out.String())
	}
}

func TestMeshCardCmd_Timeout_ReturnsError(t *testing.T) {
	_, url := startTestNATSServer(t)
	// No stub subscribed — request must time out within window.
	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "ghost",
		Project: "p1",
		Machine: "m1",
		NATSURL: url,
		Window:  100 * time.Millisecond,
		Format:  "json",
		Out:     &out,
	}
	start := time.Now()
	err := cmd.Run(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "no reply") {
		t.Errorf("error = %q, want 'no reply' wrapping", err.Error())
	}
	if elapsed > 1*time.Second {
		t.Errorf("waited %s past window", elapsed)
	}
}

// TestMeshCardCmd_MachineDefaultsToCoord verifies that omitting
// --machine falls back to coord.Machine() (which honors $SESH_MACHINE).
func TestMeshCardCmd_MachineDefaultsToCoord(t *testing.T) {
	t.Setenv("SESH_MACHINE", "envmachine")
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.Card(subject.Coord{Machine: "envmachine", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"x"}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "s1",
		Project: "p1",
		// Machine intentionally empty — must default to $SESH_MACHINE.
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "json",
		Out:     &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "agents.card.envmachine.p1.s1") {
		t.Errorf("Machine did not default to $SESH_MACHINE; output:\n%s", out.String())
	}
}

// TestMeshCardCmd_ProjectRequired confirms an empty --project (and no
// $SESH_PROJECT) is a hard error before any NATS round trip.
func TestMeshCardCmd_ProjectRequired(t *testing.T) {
	_, url := startTestNATSServer(t)
	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "s1",
		Machine: "m1",
		// Project intentionally empty.
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Out:     &out,
	}
	err := cmd.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for missing --project")
	}
	if !strings.Contains(err.Error(), "--project is required") {
		t.Errorf("error = %q, want '--project is required'", err.Error())
	}
}

func TestMeshCardCmd_FormatTree(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.Card(subject.Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{
		"description": "echo card",
		"iconUrl": "https://x/icon.png",
		"skills": [{"id":"echo.say","name":"Say","description":"d","tags":["t"]}]
	}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "s1",
		Project: "p1",
		Machine: "m1",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "tree",
		Out:     &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{
		"subject", "agents.card.m1.p1.s1",
		"description echo card",
		"icon", "https://x/icon.png",
		"skills", "echo.say",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tree output missing %q:\n%s", want, out.String())
		}
	}
}

func TestMeshCardCmd_RejectsBadSessionToken(t *testing.T) {
	_, url := startTestNATSServer(t)
	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "bad.token", // dot is reserved per subject.validateToken
		Project: "p1",
		Machine: "m1",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Out:     &out,
	}
	err := cmd.Run(context.Background())
	if err == nil {
		t.Fatal("expected validation error for bad session token")
	}
	if !strings.Contains(err.Error(), "build subject") {
		t.Errorf("error = %q, want 'build subject' wrapping", err.Error())
	}
}

func TestMeshCardCmd_NonJSONReplyFallback(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.Card(subject.Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`not json at all`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "s1",
		Project: "p1",
		Machine: "m1",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "json", // raw passthrough for non-JSON
		Out:     &out,
	}
	// Non-JSON reply now returns an error so scripted `... | jq` callers
	// see a non-zero exit. The body is still surfaced for diagnosis.
	if err := cmd.Run(context.Background()); err == nil {
		t.Fatalf("Run: expected non-JSON error, got nil")
	}
	if !strings.Contains(out.String(), "not valid JSON") {
		t.Errorf("expected raw-passthrough banner:\n%s", out.String())
	}
}

// TestMeshCardCmd_OutputIsValidJSON guards against the wrapper banner
// breaking a downstream `jq` pipe. The pretty-printed reply body must
// be parseable JSON when we strip the leading "# reply on ..." line.
func TestMeshCardCmd_OutputIsValidJSON(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.Card(subject.Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"hello"}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Session: "s1",
		Project: "p1",
		Machine: "m1",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "json",
		Out:     &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Strip the leading comment line; the remainder should be JSON.
	lines := strings.SplitN(out.String(), "\n", 2)
	if len(lines) != 2 {
		t.Fatalf("expected at least 2 output lines:\n%s", out.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[1])), &parsed); err != nil {
		t.Fatalf("output JSON did not parse: %v\n%s", err, lines[1])
	}
	if parsed["description"] != "hello" {
		t.Errorf("parsed description = %v", parsed["description"])
	}
}
