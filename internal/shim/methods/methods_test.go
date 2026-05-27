package methods

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/subject"
)

// cardKeyAsCoord mirrors the card package's unexported agentKeyAsCoord
// positional punt: it maps a card.AgentKey to a subject.Coord by slot
// (Agent→Machine, Owner→Project, Name→Session) so tests can rebuild the
// exact v0.4 card/cardx subject the Composer fetches under during the
// cutover. Replaced when Slice 3C threads a real Coord through the
// Composer.
func cardKeyAsCoord(key card.AgentKey) subject.Coord {
	return subject.Coord{
		Machine: key.Agent,
		Project: key.Owner,
		Session: key.Name,
	}
}

// startBroker spins up an in-memory nats-server with JetStream enabled
// on a random port. Mirrors the helper in internal/shim/server.
func startBroker(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
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

func testDeps(t *testing.T) (Deps, *nats.Conn, jetstream.JetStream) {
	t.Helper()
	url := startBroker(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js2, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("v2 js: %v", err)
	}
	d := Deps{
		NC:        nc,
		JS:        js2,
		ScopeKind: "project",
		ScopeID:   "abc123",
		AgentKey:  card.AgentKey{Agent: "test-agent", Owner: "test-owner", Name: "test-agent"},
		Machine:   "test-machine",
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return d, nc, js2
}

func mustCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}
