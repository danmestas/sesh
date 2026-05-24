package card

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"

	"github.com/danmestas/sesh/internal/subject"
)

func startTestNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

func registerStubAgent(t *testing.T, nc *nats.Conn, agent, owner, role, class string) {
	t.Helper()
	svc, err := micro.AddService(nc, micro.Config{
		Name:        "agents",
		Version:     "0.0.0",
		Description: "stub for composer test",
		Metadata: map[string]string{
			"agent":       agent,
			"owner":       owner,
			"role":        role,
			"class":       class,
			"harness_ver": "9.9.9",
		},
	})
	if err != nil {
		t.Fatalf("add service: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
}

func defaultL1() L1Defaults {
	return L1Defaults{
		GatewayURL:         "https://shim.example.com/a2a",
		ProtocolVersion:    "1.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}
}

func TestComposer_AppliesL2(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	registerStubAgent(t, nc, "echo", "dmestas", "implementer", "cc")

	c := NewComposer(nc, defaultL1(), 750*time.Millisecond, nil)
	card, err := c.Compose(context.Background(), AgentKey{Agent: "echo", Owner: "dmestas", Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "echo" {
		t.Errorf("Name = %q, want echo", card.Name)
	}
	if card.Version != "9.9.9" {
		t.Errorf("Version = %q, want 9.9.9", card.Version)
	}
	if card.Description != "implementer/cc" {
		t.Errorf("Description = %q, want implementer/cc", card.Description)
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != "https://shim.example.com/a2a" {
		t.Errorf("SupportedInterfaces wrong: %+v", card.SupportedInterfaces)
	}
	if card.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC {
		t.Errorf("ProtocolBinding = %q, want %q", card.SupportedInterfaces[0].ProtocolBinding, a2a.TransportProtocolJSONRPC)
	}
}

func TestComposer_NoMatchReturnsL1Skeleton(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	c := NewComposer(nc, defaultL1(), 250*time.Millisecond, nil)
	card, err := c.Compose(context.Background(), AgentKey{Agent: "nobody", Owner: "ghost", Name: "nobody"})
	if err != nil {
		t.Fatalf("Compose should not error on no match: %v", err)
	}
	if card.Name != "nobody" {
		t.Errorf("Name = %q, want nobody (fallback to key)", card.Name)
	}
	if len(card.SupportedInterfaces) != 1 {
		t.Errorf("L1 SupportedInterfaces missing")
	}
}

func TestComposer_OwnerFiltersOutOtherAgents(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	registerStubAgent(t, nc, "echo", "alice", "r1", "c1")
	registerStubAgent(t, nc, "echo", "bob", "r2", "c2")

	c := NewComposer(nc, defaultL1(), 750*time.Millisecond, nil)
	card, err := c.Compose(context.Background(), AgentKey{Agent: "echo", Owner: "bob", Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if card.Description != "r2/c2" {
		t.Errorf("expected bob's metadata, got Description=%q", card.Description)
	}
}

// ---------------- Slice 5: L3 contribution ----------------

// stubL3Reply subscribes to subj and replies with body to every request
// it sees, until the test ends. Returns a counter for the test to
// assert how many fetches were issued.
func stubL3Reply(t *testing.T, nc *nats.Conn, subj string, body []byte) *int32 {
	t.Helper()
	var n int32
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		atomic.AddInt32(&n, 1)
		_ = m.Respond(body)
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subj, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &n
}

// stubL3Sleep subscribes but sleeps `delay` before responding, so the
// composer's queryWindow expires first.
func stubL3Sleep(t *testing.T, nc *nats.Conn, subj string, body []byte, delay time.Duration) {
	t.Helper()
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		time.Sleep(delay)
		_ = m.Respond(body)
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subj, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func mustCardGet(t *testing.T, key AgentKey) string {
	t.Helper()
	subj, err := subject.CardGet(key.Agent, key.Owner, key.Name)
	if err != nil {
		t.Fatalf("subject.CardGet: %v", err)
	}
	return subj
}

func mustCardExtended(t *testing.T, key AgentKey) string {
	t.Helper()
	subj, err := subject.CardExtended(key.Agent, key.Owner, key.Name)
	if err != nil {
		t.Fatalf("subject.CardExtended: %v", err)
	}
	return subj
}

// silentLogger is used in tests that want to assert about side effects
// without polluting the test output with WARN/INFO lines.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func TestComposer_ApplyPartial_PreservesL1Fields(t *testing.T) {
	c := NewComposer(nil, defaultL1(), 0, silentLogger())
	card := c.l1Card()
	card.Name = "preserved-name"
	card.Version = "preserved-version"

	c.ApplyPartial(card, cardPartial{
		Description:      "new-desc",
		IconURL:          "https://icons/x.png",
		DocumentationURL: "https://docs/x",
		Skills: []a2a.AgentSkill{
			{ID: "s1", Name: "Skill One", Description: "First", Tags: []string{"t"}},
		},
		Capabilities: &partialCaps{
			Extensions: []a2a.AgentExtension{{URI: "https://ext/x"}},
		},
	})

	if card.Name != "preserved-name" {
		t.Errorf("L1 Name overwritten: %q", card.Name)
	}
	if card.Version != "preserved-version" {
		t.Errorf("L1 Version overwritten: %q", card.Version)
	}
	if card.Description != "new-desc" {
		t.Errorf("Description not applied: %q", card.Description)
	}
	if card.IconURL != "https://icons/x.png" {
		t.Errorf("IconURL not applied: %q", card.IconURL)
	}
	if card.DocumentationURL != "https://docs/x" {
		t.Errorf("DocumentationURL not applied: %q", card.DocumentationURL)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "s1" {
		t.Errorf("Skills not applied: %+v", card.Skills)
	}
	if len(card.Capabilities.Extensions) != 1 || card.Capabilities.Extensions[0].URI != "https://ext/x" {
		t.Errorf("Extensions not applied: %+v", card.Capabilities.Extensions)
	}
}

func TestComposer_ApplyPartial_EmptyFieldsPreserveBase(t *testing.T) {
	c := NewComposer(nil, defaultL1(), 0, silentLogger())
	card := c.l1Card()
	card.Description = "base-desc"
	card.Skills = []a2a.AgentSkill{{ID: "base"}}

	// Empty partial — no field should change.
	c.ApplyPartial(card, cardPartial{})
	if card.Description != "base-desc" {
		t.Errorf("Description changed unexpectedly: %q", card.Description)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "base" {
		t.Errorf("Skills changed unexpectedly: %+v", card.Skills)
	}
}

// TestComposer_DecodesSDKWireShape catches drift between the Go
// cardPartial JSON tags and the TypeScript SDK's AgentCardPartial. The
// fixture below is a hand-authored byte-shape match for what
// `registerAgentCard` produces in sesh-channels/sdk/src/card.ts —
// adjust both sides together if the TS shape changes.
func TestComposer_DecodesSDKWireShape(t *testing.T) {
	fixture := []byte(`{
		"description": "echo agent — repeats messages",
		"skills": [
			{
				"id": "echo.say",
				"name": "Echo Say",
				"description": "Repeat the input",
				"tags": ["echo", "demo"]
			}
		],
		"iconUrl": "https://cdn.example.com/echo.png",
		"documentationUrl": "https://docs.example.com/echo",
		"capabilities": {
			"extensions": [
				{"uri": "https://ext.example.com/extra", "required": false}
			]
		}
	}`)

	var p cardPartial
	if err := json.Unmarshal(fixture, &p); err != nil {
		t.Fatalf("decode SDK wire shape: %v", err)
	}
	if p.Description != "echo agent — repeats messages" {
		t.Errorf("Description = %q", p.Description)
	}
	if len(p.Skills) != 1 || p.Skills[0].ID != "echo.say" {
		t.Errorf("Skills = %+v", p.Skills)
	}
	if p.Skills[0].Name != "Echo Say" {
		t.Errorf("Skills[0].Name = %q", p.Skills[0].Name)
	}
	if len(p.Skills[0].Tags) != 2 {
		t.Errorf("Skills[0].Tags = %v", p.Skills[0].Tags)
	}
	if p.IconURL != "https://cdn.example.com/echo.png" {
		t.Errorf("IconURL = %q", p.IconURL)
	}
	if p.DocumentationURL != "https://docs.example.com/echo" {
		t.Errorf("DocumentationURL = %q", p.DocumentationURL)
	}
	if p.Capabilities == nil || len(p.Capabilities.Extensions) != 1 {
		t.Fatalf("Capabilities = %+v", p.Capabilities)
	}
	if p.Capabilities.Extensions[0].URI != "https://ext.example.com/extra" {
		t.Errorf("Extensions[0].URI = %q", p.Capabilities.Extensions[0].URI)
	}
}

func TestComposer_ComposeBase_OmitsL3(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	registerStubAgent(t, nc, "echo", "alice", "r", "c")
	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}

	// If ComposeBase ever fetched L3, this counter would fire.
	calls := stubL3Reply(t, nc, mustCardGet(t, key), []byte(`{"description":"L3"}`))

	c := NewComposer(nc, defaultL1(), 250*time.Millisecond, silentLogger())
	card, err := c.ComposeBase(context.Background(), key)
	if err != nil {
		t.Fatalf("ComposeBase: %v", err)
	}
	if card.Description == "L3" {
		t.Errorf("ComposeBase leaked L3 description")
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Errorf("ComposeBase issued %d L3 fetches, want 0", atomic.LoadInt32(calls))
	}
}

func TestComposer_L3_AppliesL3_OverlaysOverL2(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	registerStubAgent(t, nc, "echo", "alice", "implementer", "cc")
	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	stubL3Reply(t, nc, mustCardGet(t, key), []byte(`{
		"description": "L3 wins description",
		"skills": [{"id":"echo.say","name":"Echo Say","description":"d","tags":["t"]}],
		"iconUrl": "https://x/icon.png"
	}`))

	c := NewComposer(nc, defaultL1(), 750*time.Millisecond, silentLogger())
	card, err := c.Compose(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	// L1+L2 should preserve Name + Version.
	if card.Name != "echo" {
		t.Errorf("Name = %q (L2)", card.Name)
	}
	if card.Version != "9.9.9" {
		t.Errorf("Version = %q (L2)", card.Version)
	}
	// L3 wins description, icon, skills.
	if card.Description != "L3 wins description" {
		t.Errorf("Description = %q, want L3 value", card.Description)
	}
	if card.IconURL != "https://x/icon.png" {
		t.Errorf("IconURL = %q", card.IconURL)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "echo.say" {
		t.Errorf("Skills not L3: %+v", card.Skills)
	}
}

func TestComposer_L3_MissingFallsThrough(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	registerStubAgent(t, nc, "echo", "alice", "r", "c")

	c := NewComposer(nc, defaultL1(), 150*time.Millisecond, silentLogger())
	start := time.Now()
	card, err := c.Compose(context.Background(), AgentKey{Agent: "echo", Owner: "alice", Name: "echo"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	// Returned card is L1+L2; no L3 reply means card.Description came from L2 only.
	if card.Description != "r/c" {
		t.Errorf("Description = %q, want L2 r/c", card.Description)
	}
	// Wall time bounded by queryWindow plus L2 round trip; should be well under 1s.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Compose took %s, want <1.5s (queryWindow=150ms)", elapsed)
	}
}

func TestComposer_L3_DecodeErrorFallsThrough(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	registerStubAgent(t, nc, "echo", "alice", "r", "c")

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	stubL3Reply(t, nc, mustCardGet(t, key), []byte(`this is not JSON {`))

	c := NewComposer(nc, defaultL1(), 250*time.Millisecond, silentLogger())
	card, err := c.Compose(context.Background(), key)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if card.Description != "r/c" {
		t.Errorf("Description = %q, want L2 r/c (L3 garbage must be ignored)", card.Description)
	}
}

func TestComposer_L3_Timeout(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	registerStubAgent(t, nc, "echo", "alice", "r", "c")

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	stubL3Sleep(t, nc, mustCardGet(t, key), []byte(`{"description":"too late"}`), 500*time.Millisecond)

	c := NewComposer(nc, defaultL1(), 100*time.Millisecond, silentLogger())
	start := time.Now()
	card, err := c.Compose(context.Background(), key)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if card.Description == "too late" {
		t.Errorf("L3 leaked past timeout window")
	}
	if elapsed > 1*time.Second {
		t.Errorf("Compose blocked %s past timeout window", elapsed)
	}
}

func TestComposer_FetchExtended_Happy(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	stubL3Reply(t, nc, mustCardExtended(t, key), []byte(`{
		"description": "extended view",
		"skills": [{"id":"echo.privileged","name":"Privileged","description":"d","tags":["t"]}]
	}`))

	c := NewComposer(nc, defaultL1(), 250*time.Millisecond, silentLogger())
	p, ok := c.FetchExtended(context.Background(), key)
	if !ok {
		t.Fatal("FetchExtended returned !ok on happy path")
	}
	if p.Description != "extended view" {
		t.Errorf("Description = %q", p.Description)
	}
	if len(p.Skills) != 1 || p.Skills[0].ID != "echo.privileged" {
		t.Errorf("Skills = %+v", p.Skills)
	}
}

func TestComposer_FetchExtended_Timeout(t *testing.T) {
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	c := NewComposer(nc, defaultL1(), 100*time.Millisecond, silentLogger())
	start := time.Now()
	_, ok := c.FetchExtended(context.Background(), AgentKey{Agent: "echo", Owner: "ghost", Name: "echo"})
	elapsed := time.Since(start)
	if ok {
		t.Error("FetchExtended returned ok when no responder")
	}
	if elapsed > 800*time.Millisecond {
		t.Errorf("FetchExtended blocked %s past timeout", elapsed)
	}
}

func TestComposer_FetchExtended_PubliccardGetUnused(t *testing.T) {
	// Confirm FetchExtended hits agents.card.extended.* not agents.card.get.*.
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	publicCalls := stubL3Reply(t, nc, mustCardGet(t, key), []byte(`{"description":"public"}`))
	stubL3Reply(t, nc, mustCardExtended(t, key), []byte(`{"description":"extended"}`))

	c := NewComposer(nc, defaultL1(), 250*time.Millisecond, silentLogger())
	p, ok := c.FetchExtended(context.Background(), key)
	if !ok {
		t.Fatal("FetchExtended returned !ok")
	}
	if p.Description != "extended" {
		t.Errorf("Description = %q, want extended", p.Description)
	}
	if atomic.LoadInt32(publicCalls) != 0 {
		t.Errorf("FetchExtended issued %d requests to agents.card.get.*, want 0", atomic.LoadInt32(publicCalls))
	}
}
