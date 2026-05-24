package methods

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

type getTaskParams struct {
	ID            string `json:"id"`
	HistoryLength int    `json:"historyLength,omitempty"`
}

// getTask reads the raw Task JSON from JetStream KV bucket
// sesh_tasks_<scope-kind>_<scope-id> (derived via scope.Bucket) and
// returns it verbatim. Routing through sesh-ops/task.Get is deferred
// (see Slice 2 plan §D-task) because *task.Task is the sesh-internal
// record shape and lacks A2A wire fields; using it would strip
// contextId, history, artifacts, etc. on the round trip.
func (d *Dispatcher) getTask(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.JetStream == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream not configured"})
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
	kv, err := d.deps.JetStream.KeyValue(bucket)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		d.deps.Log.Error("getTask: open kv", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	entry, err := kv.Get(p.ID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		d.deps.Log.Error("getTask: kv get", "bucket", bucket, "id", p.ID, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	return json.RawMessage(entry.Value()), nil
}
