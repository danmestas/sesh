// Package methods implements the JSON-RPC handlers wired behind the
// shim's POST /a2a endpoint. Each method lives in its own file
// (gettask.go, extendedcard.go, send.go, stream.go, subscribe.go);
// Dispatch routes by method name. Streaming methods route through
// dedicated handler methods invoked by server when IsStreaming
// returns true.
package methods

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

// Deps wires the dispatcher's collaborators. All fields are required
// unless otherwise noted. JS is the jetstream.JetStream v2 client used
// by sesh-ops packages (task, messages, artifacts); the legacy
// nats.JetStreamContext was dropped in Slice 4 after gettask migrated
// to task.GetRaw.
type Deps struct {
	NC        *nats.Conn
	JS        jetstream.JetStream
	ScopeKind string
	ScopeID   string
	AgentKey  card.AgentKey
	Machine   string
	Log       *slog.Logger

	// KeepaliveInterval is the SSE keepalive comment cadence for
	// SendStreamingMessage + SubscribeToTask. Zero means default (25s).
	// Tests inject a short interval to make keepalive observable
	// without 25s of real-time wait.
	KeepaliveInterval time.Duration
}

// JSON-RPC method names. Centralizing them here keeps the routing in
// methods.go, the streaming-branch in server.go, and the test fixtures
// from drifting apart.
const (
	MethodGetTask              = "GetTask"
	MethodGetExtendedAgentCard = "GetExtendedAgentCard"
	MethodSendMessage          = "SendMessage"
	MethodSendStreamingMessage = "SendStreamingMessage"
	MethodSubscribeToTask      = "SubscribeToTask"
	MethodCancelTask           = "CancelTask"
	MethodListTasks            = "ListTasks"
)

// Dispatcher is the JSON-RPC method router. Construct via NewDispatcher.
type Dispatcher struct {
	deps Deps
}

// NewDispatcher returns a Dispatcher wired to deps. Caller owns the
// jetstream contexts and nats connection — the dispatcher does not
// close them.
func NewDispatcher(d Deps) *Dispatcher {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Dispatcher{deps: d}
}

// Dispatch routes a non-streaming JSON-RPC method to its handler.
// Streaming methods are routed via IsStreaming at the server layer.
func (d *Dispatcher) Dispatch(ctx context.Context, method string, params json.RawMessage) (any, *jsonrpc.Error) {
	switch method {
	case MethodGetTask:
		return d.getTask(ctx, params)
	case MethodGetExtendedAgentCard:
		return d.getExtendedAgentCard(ctx, params)
	case MethodSendMessage:
		return d.sendMessage(ctx, params)
	case MethodCancelTask:
		return d.cancelTask(ctx, params)
	case MethodListTasks:
		return d.listTasks(ctx, params)
	default:
		return nil, jsonrpc.ErrMethodNotFound
	}
}

// IsStreaming returns true for methods whose response is an SSE stream
// rather than a single JSON-RPC envelope. The server layer routes these
// to dedicated handlers that own the http.ResponseWriter directly.
func (d *Dispatcher) IsStreaming(method string) bool {
	switch method {
	case MethodSendStreamingMessage, MethodSubscribeToTask:
		return true
	}
	return false
}
