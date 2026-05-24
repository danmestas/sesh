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
	subj, err := subject.CardGet("echo", "dmestas", "echo")
	if err != nil {
		t.Fatalf("CardGet: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"public echo card","skills":[]}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "echo",
		Owner:   "dmestas",
		Name:    "echo",
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
	if !strings.Contains(out.String(), "agents.card.get.echo.dmestas.echo") {
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
	subj, err := subject.CardExtended("echo", "dmestas", "echo")
	if err != nil {
		t.Fatalf("CardExtended: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"extended view","skills":[]}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:    "echo",
		Owner:    "dmestas",
		Name:     "echo",
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
	if !strings.Contains(out.String(), "agents.card.extended.echo.dmestas.echo") {
		t.Errorf("output missing extended subject header:\n%s", out.String())
	}
}

func TestMeshCardCmd_Timeout_ReturnsError(t *testing.T) {
	_, url := startTestNATSServer(t)
	// No stub subscribed — request must time out within window.
	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "ghost",
		Owner:   "nobody",
		Name:    "ghost",
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

func TestMeshCardCmd_OwnerDefaultsToUser(t *testing.T) {
	t.Setenv("USER", "rosalind")
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.CardGet("echo", "rosalind", "echo")
	if err != nil {
		t.Fatalf("CardGet: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"x"}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "echo",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "json",
		Out:     &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "rosalind") {
		t.Errorf("output should reference owner=rosalind:\n%s", out.String())
	}
}

func TestMeshCardCmd_FormatTree(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.CardGet("echo", "dmestas", "echo")
	if err != nil {
		t.Fatalf("CardGet: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{
		"description": "echo card",
		"iconUrl": "https://x/icon.png",
		"skills": [{"id":"echo.say","name":"Say","description":"d","tags":["t"]}]
	}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "echo",
		Owner:   "dmestas",
		Name:    "echo",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "tree",
		Out:     &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{
		"subject", "agents.card.get.echo.dmestas.echo",
		"description echo card",
		"icon", "https://x/icon.png",
		"skills", "echo.say",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tree output missing %q:\n%s", want, out.String())
		}
	}
}

func TestMeshCardCmd_RejectsBadAgentToken(t *testing.T) {
	_, url := startTestNATSServer(t)
	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "bad.token", // dot is reserved per subject.validateToken
		Owner:   "dmestas",
		Name:    "echo",
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Out:     &out,
	}
	err := cmd.Run(context.Background())
	if err == nil {
		t.Fatal("expected validation error for bad agent token")
	}
	if !strings.Contains(err.Error(), "build subject") {
		t.Errorf("error = %q, want 'build subject' wrapping", err.Error())
	}
}

func TestMeshCardCmd_DefaultNameMirrorsAgent(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	// Both Name=="" and Agent=="echo" → subject token should be "echo".
	subj, err := subject.CardGet("echo", "dmestas", "echo")
	if err != nil {
		t.Fatalf("CardGet: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"x"}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "echo",
		Owner:   "dmestas",
		Name:    "", // explicit zero — must default to agent
		NATSURL: url,
		Window:  500 * time.Millisecond,
		Format:  "json",
		Out:     &out,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "agents.card.get.echo.dmestas.echo") {
		t.Errorf("Name=\"\" did not default to Agent; output:\n%s", out.String())
	}
}

func TestMeshCardCmd_NonJSONReplyFallback(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	subj, err := subject.CardGet("echo", "dmestas", "echo")
	if err != nil {
		t.Fatalf("CardGet: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`not json at all`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "echo",
		Owner:   "dmestas",
		Name:    "echo",
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
	subj, err := subject.CardGet("echo", "dmestas", "echo")
	if err != nil {
		t.Fatalf("CardGet: %v", err)
	}
	stubCardSubject(t, nc, subj, []byte(`{"description":"hello"}`))

	var out bytes.Buffer
	cmd := &MeshCardCmd{
		Agent:   "echo",
		Owner:   "dmestas",
		Name:    "echo",
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
