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
)

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
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("legacy js: %v", err)
	}
	js2, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("v2 js: %v", err)
	}
	d := Deps{
		NC:        nc,
		JetStream: js,
		JS:        js2,
		ScopeKind: "project",
		ScopeID:   "abc123",
		AgentKey:  card.AgentKey{Agent: "test-agent", Owner: "test-owner"},
		Machine:   "test-machine",
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return d, nc, js2
}

func mustCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}
