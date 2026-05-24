package methods

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/scope"
	seshtask "github.com/danmestas/sesh-ops/task"

	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

type getTaskParams struct {
	ID            string `json:"id"`
	HistoryLength int    `json:"historyLength,omitempty"`
}

// getTask reads the raw Task JSON from JetStream KV bucket
// sesh_tasks_<scope-kind>_<scope-id> (derived via scope.Bucket) and
// returns it verbatim. Routes through `task.GetRaw` after sesh-ops#25 —
// byte-passthrough so A2A wire fields (kind, contextId, status.state,
// history, artifacts) survive unmolested. The typed `task.Get` is still
// deferred (Slice 2 D-task) because *task.Task is the sesh-internal
// record shape and lacks A2A fields; using it would strip them on the
// round trip.
func (d *Dispatcher) getTask(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}
	var p getTaskParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": err.Error()})
		}
	}
	if p.ID == "" {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "id is required"})
	}
	bucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "tasks")
	if err != nil {
		d.deps.Log.Error("getTask: bucket derive", "scope_kind", d.deps.ScopeKind, "scope_id", d.deps.ScopeID, "err", err)
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"})
	}
	kv, err := d.deps.JS.KeyValue(ctx, bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		d.deps.Log.Error("getTask: open kv", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	value, _, err := seshtask.GetRaw(ctx, kv, p.ID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		d.deps.Log.Error("getTask: kv get", "bucket", bucket, "id", p.ID, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	return json.RawMessage(value), nil
}
