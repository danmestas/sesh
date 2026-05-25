package methods

import (
	"encoding/json"
	"net/http"

	"github.com/danmestas/sesh-ops/artifacts"
	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/jsonrpc"
	"github.com/danmestas/sesh/internal/shim/sse"
)

// SendStreamingMessage handles the A2A SendStreamingMessage method.
// It runs the same accept/append/publish path as SendMessage, then
// upgrades the response to text/event-stream and bridges the message
// + artifact watchers to the client.
//
// Pre-stream errors (decode failure, scope error, idempotency conflict
// before any SSE bytes are written) surface as plain JSON-RPC error
// envelopes. Once SSE headers have been emitted, errors are silently
// terminated (client gets connection close); this matches the
// A2A v1.0 reference adapter behaviour.
//
// Watcher tail-only (WatchOpts.IncludeHistory: false) — we just
// appended the inbound message; including history would echo it back.
func (d *Dispatcher) SendStreamingMessage(w http.ResponseWriter, r *http.Request, params json.RawMessage) {
	acc, jerr := d.acceptInboundMessage(r.Context(), params)
	if jerr != nil {
		jsonrpc.WriteError(w, nil, jerr)
		return
	}

	artsBucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "artifacts")
	if err != nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"}))
		return
	}
	artsKV, kerr := d.openOrCreateKV(r.Context(), artsBucket)
	if kerr != nil {
		jsonrpc.WriteError(w, nil, kerr)
		return
	}

	msgCh, msgStop, err := messages.Watch(r.Context(), acc.msgsKV, acc.message.TaskID, messages.WatchOpts{IncludeHistory: false})
	if err != nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"err": err.Error()}))
		return
	}
	artCh, err := artifacts.Watch(r.Context(), artsKV, acc.message.TaskID, artifacts.WatchOpts{IncludeHistory: false})
	if err != nil {
		msgStop()
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"err": err.Error()}))
		return
	}

	_ = sse.Bridge(r.Context(), w, msgCh, msgStop, artCh, sse.Options{
		KeepaliveInterval: d.deps.KeepaliveInterval,
		Translator:        d.deps.Translator,
	})
}
