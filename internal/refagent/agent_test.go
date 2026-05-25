package refagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/sesh/internal/agentmeta"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// ---------- unit tests: wire-format vs Synadia Appendix B ----------

// TestAckChunk pins the §6.4 / B.6 byte sequence. If this byte string
// ever drifts the SDK's wire-compat tests will fail too — keep them
// in lockstep.
func TestAckChunk(t *testing.T) {
	got := ackChunk()
	want := `{"type":"status","data":"ack"}`
	if string(got) != want {
		t.Fatalf("ack chunk mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestResponseChunkString covers B.4 — a response chunk whose `data`
// is a UTF-8 string.
func TestResponseChunkString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"appendix B.4", "Hello, world.", `{"type":"response","data":"Hello, world."}`},
		{"empty", "", `{"type":"response","data":""}`},
		{"unicode", "héllo 🌍", `{"type":"response","data":"héllo 🌍"}`},
		{"with quotes", `say "hi"`, `{"type":"response","data":"say \"hi\""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := responseChunk(tc.in)
			if string(got) != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestMalformedBody pins the §9.1 error JSON body shape. The contract
// doc spells it as {"error":"malformed_request","message":<detail>}.
func TestMalformedBody(t *testing.T) {
	got := malformedBody("missing 'prompt' field")
	want := `{"error":"malformed_request","message":"missing 'prompt' field"}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// TestParsePrompt covers the §5.3 discrimination rule + §9.2 400
// triggers. Table-driven; each row is one §5 wire example.
func TestParsePrompt(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
		errSub  string
	}{
		{"plain-text shorthand (B.1)", "summarize the attached report", "summarize the attached report", false, ""},
		{"JSON text only (B.2)", `{"prompt":"summarize the attached report"}`, "summarize the attached report", false, ""},
		{"plain-text with leading whitespace", "  hello\n", "hello", false, ""},
		{"JSON with unknown fields tolerated (§5.6)", `{"prompt":"hi","metadata":{"x":1},"extra":true}`, "hi", false, ""},
		{"empty body rejected", "", "", true, "empty payload"},
		{"whitespace-only body rejected", "   \n\t", "", true, "empty payload"},
		{"malformed JSON rejected", `{"prompt":`, "", true, "invalid JSON"},
		{"empty prompt field rejected", `{"prompt":""}`, "", true, "empty 'prompt'"},
		{"missing prompt field rejected", `{"metadata":{}}`, "", true, "missing 'prompt'"},
		{"attachments rejected (attachments_ok=false)", `{"prompt":"hi","attachments":[{"filename":"x","content":"AA=="}]}`, "", true, "attachments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePrompt([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (parsed %q)", tc.errSub, got)
				}
				if tc.errSub != "" && !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("want error containing %q, got %q", tc.errSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSplitUTF8 verifies that chunking never splits a multi-byte rune
// and that chunk sizes respect the budget.
func TestSplitUTF8(t *testing.T) {
	// 4-byte rune 🌍 repeated; budget of 5 should yield one rune per chunk.
	in := "🌍🌍🌍"
	got := splitUTF8(in, 5)
	if len(got) != 3 {
		t.Fatalf("want 3 chunks, got %d: %q", len(got), got)
	}
	if strings.Join(got, "") != in {
		t.Fatalf("rejoined %q != input %q", strings.Join(got, ""), in)
	}
	for _, c := range got {
		if len(c) > 5 {
			t.Fatalf("chunk %q exceeds budget 5", c)
		}
	}

	// Short string fits in one chunk.
	if g := splitUTF8("hi", 100); len(g) != 1 || g[0] != "hi" {
		t.Fatalf("got %v", g)
	}

	// Empty yields one empty chunk so the stream still emits one
	// response message (parsePrompt rejects empty, but the encoder is
	// defensive).
	if g := splitUTF8("", 10); len(g) != 1 || g[0] != "" {
		t.Fatalf("got %v", g)
	}

	// Pure ASCII chunked by a budget smaller than the input.
	long := strings.Repeat("a", 25)
	got = splitUTF8(long, 10)
	if strings.Join(got, "") != long {
		t.Fatalf("rejoin mismatch")
	}
	for _, c := range got {
		if len(c) > 10 {
			t.Fatalf("chunk size violation: %d > 10", len(c))
		}
	}
}

// TestHeartbeatPayloadShape covers B.11 — agent, owner, session,
// instance_id, ts (RFC 3339), interval_s. Session is omitted when empty.
func TestHeartbeatPayloadShape(t *testing.T) {
	now := time.Date(2026, 4, 28, 14, 23, 1, 0, time.UTC)

	t.Run("with session (B.11)", func(t *testing.T) {
		a := &agent{cfg: Config{
			Agent: "claude-code", Owner: "aconnolly",
			Session: "synadia-com-2", Interval: 30 * time.Second,
		}}
		p := a.heartbeatPayload(now)
		// instance_id is empty when svc==nil; the wire test exercises the populated path.
		if p["agent"] != "claude-code" || p["owner"] != "aconnolly" || p["session"] != "synadia-com-2" {
			t.Fatalf("identity fields wrong: %+v", p)
		}
		if p["ts"] != "2026-04-28T14:23:01Z" {
			t.Fatalf("ts %v, want RFC 3339 UTC", p["ts"])
		}
		if p["interval_s"] != 30 {
			t.Fatalf("interval_s %v, want 30", p["interval_s"])
		}
	})

	t.Run("session omitted when empty (§3.2)", func(t *testing.T) {
		a := &agent{cfg: Config{Agent: "echo", Owner: "u", Interval: 30 * time.Second}}
		p := a.heartbeatPayload(now)
		if _, ok := p["session"]; ok {
			t.Fatalf("session should be absent: %+v", p)
		}
	})

	t.Run("sesh extension: role and class included when set", func(t *testing.T) {
		a := &agent{cfg: Config{
			Agent: "claude-code", Owner: "alice", Session: "s1",
			Role: "implementer", Class: agentmeta.ClassActive,
			Interval: 30 * time.Second,
		}}
		p := a.heartbeatPayload(now)
		if p["role"] != "implementer" {
			t.Errorf("role = %v, want implementer", p["role"])
		}
		if p["class"] != "active" {
			t.Errorf("class = %v, want active", p["class"])
		}
	})

	t.Run("sesh extension: role and class omitted when empty", func(t *testing.T) {
		a := &agent{cfg: Config{
			Agent: "echo", Owner: "u", Session: "s",
			Interval: 30 * time.Second,
			// Role and Class deliberately zero
		}}
		p := a.heartbeatPayload(now)
		if _, ok := p["role"]; ok {
			t.Errorf("role should be absent when empty: %+v", p)
		}
		if _, ok := p["class"]; ok {
			t.Errorf("class should be absent when empty: %+v", p)
		}
	})
}

// TestShutdownPublishesFinalHeartbeat covers Synadia §8.6 — agents
// SHOULD publish one final heartbeat with an empty payload to the same
// heartbeat subject before graceful shutdown, signalling immediate
// offline so observers don't have to wait for 3× interval missed-beats.
func TestShutdownPublishesFinalHeartbeat(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)

	// Subscribe to the hb subject BEFORE the agent starts so the empty
	// final heartbeat isn't missed.
	probe := testConn(t, url)
	hbSub := subSync(t, probe, "agents.hb.echo.alice.s1")

	cancel, done := runAgent(t, Config{
		Agent: "echo", Owner: "alice", Session: "s1",
		Interval: 30 * time.Second, // long enough that periodic hb doesn't fire during the test
	})
	waitForService(t, probe)

	// Drain the immediate-on-startup heartbeat (Run line ~148) so the
	// shutdown heartbeat we're looking for isn't masked.
	if _, err := hbSub.NextMsg(500 * time.Millisecond); err != nil {
		t.Fatalf("startup heartbeat not received: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}

	// The shutdown defer publishes an empty-payload heartbeat per §8.6.
	final, err := hbSub.NextMsg(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("no final heartbeat after shutdown: %v", err)
	}
	if len(final.Data) != 0 {
		t.Errorf("final heartbeat payload len = %d, want 0 (§8.6 immediate-offline)", len(final.Data))
	}
}

func TestFormatMaxPayload(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{1024 * 1024, "1MB"},
		{8 * 1024 * 1024, "8MB"},
		{1024, "1KB"},
		{1024 * 1024 * 1024, "1GB"},
		{500, "500B"},
		{0, "1MB"}, // fallback
		{-1, "1MB"},
	}
	for _, tc := range cases {
		if got := formatMaxPayload(tc.bytes); got != tc.want {
			t.Errorf("formatMaxPayload(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestValidateTokens(t *testing.T) {
	ok := []Config{
		{Agent: "echo", Owner: "alice"},
		{Agent: "claude-code", Owner: "aconnolly"},
	}
	for _, c := range ok {
		if err := validateTokens(c); err != nil {
			t.Errorf("validateTokens(%+v) unexpected err: %v", c, err)
		}
	}
	bad := []Config{
		{Agent: "", Owner: "u"},
		{Agent: "$bad", Owner: "u"},
		{Agent: "ok", Owner: "with.dot"},
		{Agent: strings.Repeat("a", 64), Owner: "u"},
	}
	for _, c := range bad {
		if err := validateTokens(c); err == nil {
			t.Errorf("validateTokens(%+v) expected error", c)
		}
	}
}

// ---------- URL resolution -----------------------------------------

func TestResolveNATSURL_Override(t *testing.T) {
	t.Setenv("NATS_URL", "nats://from-env:4222")
	got, err := resolveNATSURL("nats://explicit:4222", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "nats://explicit:4222" {
		t.Fatalf("override should win, got %q", got)
	}
}

func TestResolveNATSURL_Env(t *testing.T) {
	t.Setenv("NATS_URL", "nats://from-env:4222")
	got, err := resolveNATSURL("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "nats://from-env:4222" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveNATSURL_SessionFile(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, ".sesh", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":1,"nats_url":"nats://from-session:4222","leaf_url":"nats://leaf:5223"}`
	if err := os.WriteFile(filepath.Join(sessDir, "demo.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NATS_URL", "")
	chdir(t, dir)
	got, err := resolveNATSURL("", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got != "nats://from-session:4222" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveNATSURL_HubURLFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir respects $HOME on unix
	if err := os.MkdirAll(filepath.Join(home, ".sesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".sesh", "hub.url"),
		[]byte("nats://from-hub:4222\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NATS_URL", "")
	// CWD has no .sesh/sessions — force resolution to fall through.
	chdir(t, t.TempDir())
	got, err := resolveNATSURL("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "nats://from-hub:4222" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveNATSURL_AllExhausted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NATS_URL", "")
	chdir(t, t.TempDir())
	_, err := resolveNATSURL("", "")
	if err == nil {
		t.Fatal("want error when no URL source available")
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// ---------- behavior tests against embedded NATS -------------------

// startBroker spins up an in-memory nats-server on a random port and
// returns its client URL. Caller is responsible for nothing — t.Cleanup
// shuts the server down.
func startBroker(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host: "127.0.0.1",
		Port: -1, // random
		// MaxPayload defaults to 1MB which matches Synadia Appendix B.
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("nats server new: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatalf("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// runAgent starts Run in a goroutine and returns a cancel + done.
func runAgent(t *testing.T, cfg Config) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, cfg) }()
	t.Cleanup(cancelFn)
	return cancelFn, errCh
}

// testConn connects to url and registers cleanup so callers don't
// scatter `defer nc.Close()` through every test. Failures fail the test.
func testConn(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// subSync subscribes to subject and registers cleanup. Mirrors testConn.
func subSync(t *testing.T, nc *nats.Conn, subject string) *nats.Subscription {
	t.Helper()
	sub, err := nc.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("SubscribeSync %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return sub
}

// waitForService spins until $SRV.INFO.agents returns a response
// with at least one endpoint registered. The agent registers
// asynchronously and AddService + AddEndpoint are non-atomic, so
// a probe that lands between them sees an empty endpoints array.
// Returning at the first response triggers tests that assert on
// the endpoint subject; we keep polling until endpoints is populated.
func waitForService(t *testing.T, nc *nats.Conn) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		msg, err := nc.Request("$SRV.INFO.agents", nil, 200*time.Millisecond)
		if err == nil {
			last = msg.Data
			if !bytes.Contains(last, []byte(`"endpoints":[]`)) {
				return last
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(last) > 0 {
		t.Fatalf("agent registered but endpoints stayed empty after 3s:\n%s", last)
	}
	t.Fatal("agent never registered (no $SRV.INFO response)")
	return nil
}

// TestServiceRegistration is the end-to-end §12 conformance probe:
// drive the agent via Run, then verify $SRV.INFO.agents matches the
// shape in the contract doc's worked example (§9).
func TestServiceRegistration(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)
	t.Setenv("SESH_SESSION", "")

	cancel, done := runAgent(t, Config{
		Agent: "echo", Owner: "alice", Session: "demo",
		Interval: 200 * time.Millisecond,
	})

	nc := testConn(t, url)

	info := waitForService(t, nc)

	var got struct {
		Name        string            `json:"name"`
		ID          string            `json:"id"`
		Version     string            `json:"version"`
		Description string            `json:"description"`
		Metadata    map[string]string `json:"metadata"`
		Endpoints   []struct {
			Name       string            `json:"name"`
			Subject    string            `json:"subject"`
			QueueGroup string            `json:"queue_group"`
			Metadata   map[string]string `json:"metadata"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(info, &got); err != nil {
		t.Fatalf("info parse: %v\n%s", err, info)
	}

	if got.Name != "agents" {
		t.Errorf("name=%q want agents", got.Name)
	}
	if got.ID == "" {
		t.Error("id (instance_id) must be set by framework")
	}
	if got.Metadata["agent"] != "echo" || got.Metadata["owner"] != "alice" ||
		got.Metadata["session"] != "demo" || got.Metadata["protocol_version"] != "0.3" {
		t.Errorf("metadata wrong: %+v", got.Metadata)
	}
	// v0.4 metadata-key convention (design doc §8): namespaced
	// sesh.protocol_version must mirror the legacy unnamespaced
	// protocol_version, and sesh.v04_capabilities must be ABSENT to
	// signal "no v0.4 capabilities" — ref-agent serves v0.3 wire format.
	if got.Metadata["sesh.protocol_version"] != "0.3" {
		t.Errorf("sesh.protocol_version=%q want 0.3", got.Metadata["sesh.protocol_version"])
	}
	if _, ok := got.Metadata["sesh.v04_capabilities"]; ok {
		t.Errorf("sesh.v04_capabilities must be absent for v0.3 ref-agent; got %q",
			got.Metadata["sesh.v04_capabilities"])
	}

	wantEndpoints := map[string]string{
		"prompt": "agents.prompt.echo.alice.demo",
		"status": "agents.status.echo.alice.demo",
	}
	got_subj := map[string]string{}
	got_qg := map[string]string{}
	for _, e := range got.Endpoints {
		got_subj[e.Name] = e.Subject
		got_qg[e.Name] = e.QueueGroup
	}
	for name, subj := range wantEndpoints {
		if got_subj[name] != subj {
			t.Errorf("endpoint %s subject %q want %q", name, got_subj[name], subj)
		}
		if got_qg[name] != "agents" {
			t.Errorf("endpoint %s queue_group %q want agents", name, got_qg[name])
		}
	}

	// prompt metadata
	for _, e := range got.Endpoints {
		if e.Name == "prompt" {
			if e.Metadata["max_payload"] == "" {
				t.Errorf("prompt max_payload missing")
			}
			if e.Metadata["attachments_ok"] != "false" {
				t.Errorf("prompt attachments_ok = %q want false", e.Metadata["attachments_ok"])
			}
		}
	}

	// Ensure Run returns cleanly on cancel.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return within 2s of cancel")
	}
}

// TestSessionOmitted exercises the session-less subject layout from §2.3.
func TestSessionOmitted(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)
	t.Setenv("SESH_SESSION", "")
	runAgent(t, Config{Agent: "echo", Owner: "alice", Interval: 1 * time.Second})

	nc := testConn(t, url)

	info := waitForService(t, nc)
	if !strings.Contains(string(info), `"agents.prompt.echo.alice"`) {
		t.Errorf("expected session-less subject:\n%s", info)
	}
	if strings.Contains(string(info), `"session":`) {
		t.Errorf("expected metadata.session to be absent:\n%s", info)
	}
}

// TestPromptStreamEcho drives the prompt endpoint and asserts the §6
// stream shape: ack first, response chunks in order, terminator last.
func TestPromptStreamEcho(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)
	runAgent(t, Config{Agent: "echo", Owner: "alice", Session: "s1", Interval: 1 * time.Second})

	nc := testConn(t, url)
	waitForService(t, nc)

	inbox := nats.NewInbox()
	sub := subSync(t, nc, inbox)

	if err := nc.PublishRequest("agents.prompt.echo.alice.s1", inbox,
		[]byte(`{"prompt":"hello, world"}`)); err != nil {
		t.Fatal(err)
	}

	msgs := drain(t, sub, 3, 2*time.Second)

	// Chunk 1: ack
	if string(msgs[0].Data) != `{"type":"status","data":"ack"}` {
		t.Errorf("chunk[0] = %q, want ack", msgs[0].Data)
	}
	// Chunk 2: response
	if !strings.Contains(string(msgs[1].Data), `"type":"response"`) ||
		!strings.Contains(string(msgs[1].Data), `"hello, world"`) {
		t.Errorf("chunk[1] = %q, want response with hello, world", msgs[1].Data)
	}
	// Chunk 3: terminator — zero body, no headers (§6.5).
	if len(msgs[2].Data) != 0 {
		t.Errorf("terminator must be empty body: %q", msgs[2].Data)
	}
	if len(msgs[2].Header) != 0 {
		t.Errorf("terminator must have no headers: %+v", msgs[2].Header)
	}
}

// TestPromptMalformed asserts the §9.2 400 + body shape on bad input.
func TestPromptMalformed(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)
	runAgent(t, Config{Agent: "echo", Owner: "alice", Session: "s1", Interval: 1 * time.Second})

	nc := testConn(t, url)
	waitForService(t, nc)

	inbox := nats.NewInbox()
	sub := subSync(t, nc, inbox)

	nc.PublishRequest("agents.prompt.echo.alice.s1", inbox, []byte(`{"prompt":`))

	msgs := drain(t, sub, 3, 2*time.Second)
	// ack, error-body, terminator.
	if string(msgs[0].Data) != `{"type":"status","data":"ack"}` {
		t.Errorf("ack mismatch: %q", msgs[0].Data)
	}
	// Error message: 400 headers + JSON body.
	if msgs[1].Header.Get("Nats-Service-Error-Code") != "400" {
		t.Errorf("want 400 header, got %q", msgs[1].Header.Get("Nats-Service-Error-Code"))
	}
	if !strings.Contains(string(msgs[1].Data), `"error":"malformed_request"`) {
		t.Errorf("error body = %q, want malformed_request", msgs[1].Data)
	}
}

// TestStatusEndpoint covers §8.7 / B.11a — status returns a §8.3
// heartbeat-shaped payload built fresh per request.
func TestStatusEndpoint(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)
	runAgent(t, Config{Agent: "echo", Owner: "alice", Session: "s1", Interval: 5 * time.Second})

	nc := testConn(t, url)
	waitForService(t, nc)

	msg, err := nc.Request("agents.status.echo.alice.s1", nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("status payload parse: %v\n%s", err, msg.Data)
	}
	for _, k := range []string{"agent", "owner", "session", "instance_id", "ts", "interval_s"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status payload missing %q: %+v", k, got)
		}
	}
	if got["agent"] != "echo" || got["interval_s"].(float64) != 5 {
		t.Errorf("status payload identity wrong: %+v", got)
	}
}

// TestHeartbeatCadence subscribes to the heartbeat subject and asserts
// at least two payloads arrive within 3× interval. §8.2 cadence.
func TestHeartbeatCadence(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)
	runAgent(t, Config{Agent: "echo", Owner: "alice", Session: "s1", Interval: 100 * time.Millisecond})

	nc := testConn(t, url)
	hbSub := subSync(t, nc, "agents.hb.echo.alice.s1")
	waitForService(t, nc)

	// Within 3× interval we want at least 2 heartbeats (the immediate
	// one plus one timer fire).
	msgs := drain(t, hbSub, 2, 1*time.Second)

	var hb map[string]any
	if err := json.Unmarshal(msgs[0].Data, &hb); err != nil {
		t.Fatalf("hb parse: %v", err)
	}
	for _, k := range []string{"agent", "owner", "session", "instance_id", "ts", "interval_s"} {
		if _, ok := hb[k]; !ok {
			t.Errorf("hb missing %q: %+v", k, hb)
		}
	}
}

// TestShutdownDrains verifies that cancelling ctx unwinds the agent
// cleanly — Run returns within a short window and the service is
// no longer in $SRV.INFO.agents.
func TestShutdownDrains(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)
	cancel, done := runAgent(t, Config{Agent: "echo", Owner: "alice", Session: "s1", Interval: 1 * time.Second})

	nc := testConn(t, url)
	waitForService(t, nc)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}

	// After cancel + drain, $SRV.INFO.agents should yield nothing.
	// Run() returning is necessary but not sufficient: micro service
	// deregistration is async and on CI runners can lag the Run() return
	// by hundreds of ms. Poll until the request times out (no responder)
	// or our 2s budget elapses. Locally completes on the first poll.
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lastErr = nc.Request("$SRV.INFO.agents", nil, 100*time.Millisecond)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Error("$SRV.INFO.agents still responds 2s after shutdown")
	} else if !errors.Is(lastErr, nats.ErrTimeout) && !errors.Is(lastErr, nats.ErrNoResponders) {
		t.Errorf("unexpected error type %v", lastErr)
	}
}

// ---------- Role / Class config + metadata -------------------------

// TestRegister_EmitsRoleAndClassMetadata asserts the on-the-wire INFO
// response carries role / class keys, the source of truth for downstream
// parsers (agent_watcher, helper SDKs, coordination-subject dispatchers).
func TestRegister_EmitsRoleAndClassMetadata(t *testing.T) {
	url := startBroker(t)
	t.Setenv("NATS_URL", url)

	runAgent(t, Config{
		Agent: "echo", Owner: "alice", Session: "rc-test",
		Role: "implementer", Class: agentmeta.ClassActive,
		Interval: 1 * time.Second,
	})

	nc := testConn(t, url)
	info := waitForService(t, nc)
	body := string(info)
	if !strings.Contains(body, `"role":"implementer"`) {
		t.Errorf("INFO body missing role=implementer:\n%s", body)
	}
	if !strings.Contains(body, `"class":"active"`) {
		t.Errorf("INFO body missing class=active:\n%s", body)
	}
}

// TestApplyDefaults_RoleAndClassFromEnv exercises the env-read path:
// SESH_ROLE / SESH_CLASS populate the typed Config fields.
func TestApplyDefaults_RoleAndClassFromEnv(t *testing.T) {
	t.Setenv("SESH_ROLE", "implementer")
	t.Setenv("SESH_CLASS", "active")

	var c Config
	c.applyDefaults()

	if c.Role != "implementer" {
		t.Errorf("Role = %q, want implementer", c.Role)
	}
	if c.Class != agentmeta.ClassActive {
		t.Errorf("Class = %v, want active", c.Class)
	}
}

// TestApplyDefaults_RoleAndClassDefaults asserts the back-compat defaults
// when neither env nor explicit Config field is set.
func TestApplyDefaults_RoleAndClassDefaults(t *testing.T) {
	t.Setenv("SESH_ROLE", "")
	t.Setenv("SESH_CLASS", "")

	var c Config
	c.applyDefaults()

	if c.Role != agentmeta.DefaultRole {
		t.Errorf("Role default = %q, want %q", c.Role, agentmeta.DefaultRole)
	}
	if c.Class != agentmeta.DefaultClass {
		t.Errorf("Class default = %v, want %v", c.Class, agentmeta.DefaultClass)
	}
}

// TestRun_RejectsBadClassAtBoot asserts the agent refuses to start with an
// unknown SESH_CLASS value rather than silently coercing.
func TestRun_RejectsBadClassAtBoot(t *testing.T) {
	t.Setenv("SESH_CLASS", "passive")
	t.Setenv("SESH_ROLE", "worker")
	t.Setenv("NATS_URL", "nats://127.0.0.1:1") // unreachable; should never be dialed

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Run(ctx, Config{Agent: "echo", Owner: "alice"})
	if err == nil || !strings.Contains(err.Error(), "class") {
		t.Fatalf("Run with SESH_CLASS=passive: err = %v, want class error", err)
	}
}

// TestRun_RejectsBadRoleAtBoot asserts the agent refuses to start with a
// role that doesn't match the canonical regex.
func TestRun_RejectsBadRoleAtBoot(t *testing.T) {
	t.Setenv("SESH_ROLE", "Bad Role")
	t.Setenv("SESH_CLASS", "active")
	t.Setenv("NATS_URL", "nats://127.0.0.1:1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Run(ctx, Config{Agent: "echo", Owner: "alice"})
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("Run with SESH_ROLE=\"Bad Role\": err = %v, want role error", err)
	}
}

// drain reads exactly n messages from sub with a per-batch timeout.
// Fails the test if fewer than n messages arrive.
func drain(t *testing.T, sub *nats.Subscription, n int, timeout time.Duration) []*nats.Msg {
	t.Helper()
	deadline := time.Now().Add(timeout)
	msgs := make([]*nats.Msg, 0, n)
	for len(msgs) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("drain timed out: got %d/%d messages", len(msgs), n)
		}
		m, err := sub.NextMsg(remaining)
		if err != nil {
			t.Fatalf("drain NextMsg after %d: %v", len(msgs), err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}
