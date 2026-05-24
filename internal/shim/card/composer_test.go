package card

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
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
	card, err := c.Compose(context.Background(), AgentKey{Agent: "echo", Owner: "dmestas"})
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
	card, err := c.Compose(context.Background(), AgentKey{Agent: "nobody", Owner: "ghost"})
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
	card, err := c.Compose(context.Background(), AgentKey{Agent: "echo", Owner: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if card.Description != "r2/c2" {
		t.Errorf("expected bob's metadata, got Description=%q", card.Description)
	}
}
