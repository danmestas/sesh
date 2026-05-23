package cli

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestQueryMesh_ReturnsAllRegisteredAgents(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	registerTestAgent(t, nc, "cc", "dmestas", "alpha")
	registerTestAgent(t, nc, "op", "dmestas", "alpha")
	registerTestAgent(t, nc, "cc", "dmestas", "beta") // different session — must still show up

	got := QueryMesh(nc, 500*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("QueryMesh returned %d agents, want 3: %+v", len(got), got)
	}

	byKey := map[string]MeshAgent{}
	for _, a := range got {
		key := a.Agent + "/" + a.Session
		byKey[key] = a
	}
	if _, ok := byKey["cc/alpha"]; !ok {
		t.Errorf("missing cc/alpha in result: %+v", got)
	}
	if _, ok := byKey["op/alpha"]; !ok {
		t.Errorf("missing op/alpha in result: %+v", got)
	}
	if _, ok := byKey["cc/beta"]; !ok {
		t.Errorf("missing cc/beta in result: %+v", got)
	}
}

func TestQueryMesh_PopulatesProtocolFields(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	registerTestAgent(t, nc, "cc", "dmestas", "alpha")

	got := QueryMesh(nc, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("want 1 agent, got %d", len(got))
	}
	a := got[0]
	if a.Agent != "cc" {
		t.Errorf("Agent=%q, want cc", a.Agent)
	}
	if a.Owner != "dmestas" {
		t.Errorf("Owner=%q, want dmestas", a.Owner)
	}
	if a.Session != "alpha" {
		t.Errorf("Session=%q, want alpha", a.Session)
	}
	if a.ProtocolVersion != "0.3" {
		t.Errorf("ProtocolVersion=%q, want 0.3", a.ProtocolVersion)
	}
	if a.InstanceID == "" {
		t.Errorf("InstanceID empty; want non-empty")
	}
	if a.Subject != "agents.prompt.cc.dmestas.alpha" {
		t.Errorf("Subject=%q, want agents.prompt.cc.dmestas.alpha", a.Subject)
	}
}

func TestQueryMesh_EmptyHubReturnsNil(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	// No agents registered.
	got := QueryMesh(nc, 200*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("want empty, got %d: %+v", len(got), got)
	}
}

func TestQueryMesh_MalformedReplyIsSkipped(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()

	// A responder that replies to $SRV.INFO.agents with garbage bytes.
	// QueryMesh should silently skip the malformed JSON and return any
	// other valid responders.
	_, err := nc.Subscribe("$SRV.INFO.agents", func(m *nats.Msg) {
		_ = m.Respond([]byte("this is not json"))
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	registerTestAgent(t, nc, "cc", "dmestas", "alpha") // valid responder

	got := QueryMesh(nc, 500*time.Millisecond)
	if len(got) != 1 || got[0].Agent != "cc" {
		t.Errorf("want exactly 1 cc agent (garbage skipped), got %+v", got)
	}
}
