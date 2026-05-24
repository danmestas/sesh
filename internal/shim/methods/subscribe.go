package methods

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/artifacts"
	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/jsonrpc"
	"github.com/danmestas/sesh/internal/shim/sse"
)

type subscribeParams struct {
	ID string `json:"id"`
}

// SubscribeToTask reattaches an SSE stream to an existing task without
// appending or publishing. Unlike SendStreamingMessage the watcher
// opens with WatchOpts.IncludeHistory: true so a client reconnecting
// after disconnect receives backfill.
//
// Errors that occur before the SSE upgrade (bad params, unknown task)
// surface as a plain JSON-RPC error envelope; after the upgrade the
// stream just closes on error (no retroactive error envelope possible).
func (d *Dispatcher) SubscribeToTask(w http.ResponseWriter, r *http.Request, params json.RawMessage) {
	if d.deps.JS == nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"}))
		return
	}
	var p subscribeParams
	if len(params) == 0 {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params is required"}))
		return
	}
	if err := json.Unmarshal(params, &p); err != nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": err.Error()}))
		return
	}
	if p.ID == "" {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "id is required"}))
		return
	}

	tasksBucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "tasks")
	if err != nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"}))
		return
	}
	msgsBucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "messages")
	if err != nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"}))
		return
	}
	artsBucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "artifacts")
	if err != nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"}))
		return
	}

	tasksKV, jerr := d.openOrCreateKV(r.Context(), tasksBucket)
	if jerr != nil {
		jsonrpc.WriteError(w, nil, jerr)
		return
	}
	if _, err := tasksKV.Get(r.Context(), p.ID); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			jsonrpc.WriteError(w, nil, jsonrpc.ErrTaskNotFound)
			return
		}
		d.deps.Log.Error("SubscribeToTask: read task", "task_id", p.ID, "err", err)
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal)
		return
	}

	msgsKV, jerr := d.openOrCreateKV(r.Context(), msgsBucket)
	if jerr != nil {
		jsonrpc.WriteError(w, nil, jerr)
		return
	}
	artsKV, jerr := d.openOrCreateKV(r.Context(), artsBucket)
	if jerr != nil {
		jsonrpc.WriteError(w, nil, jerr)
		return
	}

	msgCh, msgStop, err := messages.Watch(r.Context(), msgsKV, p.ID, messages.WatchOpts{IncludeHistory: true})
	if err != nil {
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"err": err.Error()}))
		return
	}
	artCh, err := artifacts.Watch(r.Context(), artsKV, p.ID, artifacts.WatchOpts{IncludeHistory: true})
	if err != nil {
		msgStop()
		jsonrpc.WriteError(w, nil, jsonrpc.ErrInternal.WithData(map[string]string{"err": err.Error()}))
		return
	}

	_ = sse.Bridge(r.Context(), w, msgCh, msgStop, artCh, sse.Options{KeepaliveInterval: d.deps.KeepaliveInterval})
}
