package methods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/scope"
	seshtask "github.com/danmestas/sesh-ops/task"

	"github.com/danmestas/sesh/internal/shim/a2a"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

// cancelTaskParams mirrors the A2A CancelTaskRequest.params shape.
// metadata is accepted-but-ignored in Slice 4 (see plan §3 non-goals).
type cancelTaskParams struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

const cancelStatus = a2a.TaskStateCanceled

// casRetryBackoffs is the 3-retry backoff schedule: 10ms, 20ms, 40ms.
// Bounded at 70ms cumulative — well under the 1s test budget for the
// CAS_Exhausted case (plan §4.2). Exported as a var so tests can shrink
// it further if needed; production callers should leave it alone.
var casRetryBackoffs = []time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
}

// testCASInjector is a package-private hook invoked between GetRaw and
// kv.Update inside the CAS retry loop. Only set by cancel_test.go; nil
// in production. Lets tests deterministically interleave a racer write
// without leaking test machinery into Deps.
var testCASInjector func()

// cancelTask implements A2A CancelTask. Decodes the stored Task bytes
// (preserving unknown fields via map[string]any round-trip), checks for
// terminal status, rewrites status.state = TASK_STATE_CANCELED, and
// CAS-updates the entry. Retries up to 3× on revision conflicts before
// surfacing -32603 task-leased. Cancel propagation to a running adapter
// is out of Slice 4 scope (sesh-channels concern).
func (d *Dispatcher) cancelTask(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}

	var p cancelTaskParams
	if len(params) == 0 {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params is required"})
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": err.Error()})
	}
	if p.ID == "" {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "id is required"})
	}

	bucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "tasks")
	if err != nil {
		d.deps.Log.Error("cancelTask: bucket derive", "scope_kind", d.deps.ScopeKind, "scope_id", d.deps.ScopeID, "err", err)
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"})
	}
	kv, err := d.deps.JS.KeyValue(ctx, bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		d.deps.Log.Error("cancelTask: open kv", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}

	for attempt := 0; attempt < len(casRetryBackoffs); attempt++ {
		value, rev, err := seshtask.GetRaw(ctx, kv, p.ID)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil, jsonrpc.ErrTaskNotFound
			}
			d.deps.Log.Error("cancelTask: kv get", "bucket", bucket, "id", p.ID, "err", err)
			return nil, jsonrpc.ErrInternal
		}

		state := parseStatusState(value)
		if a2a.IsTerminalTaskState(state) {
			return nil, jsonrpc.ErrTaskNotCancelable
		}

		mutated, err := setStatusState(value, cancelStatus)
		if err != nil {
			d.deps.Log.Error("cancelTask: re-encode", "id", p.ID, "err", err)
			return nil, jsonrpc.ErrInternal
		}

		if testCASInjector != nil {
			testCASInjector()
		}

		if _, err := kv.Update(ctx, p.ID, mutated, rev); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				if attempt+1 < len(casRetryBackoffs) {
					select {
					case <-ctx.Done():
						return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": ctx.Err().Error()})
					case <-time.After(casRetryBackoffs[attempt]):
					}
					continue
				}
				return nil, jsonrpc.ErrInternal.WithData(map[string]string{"kind": "task-leased"})
			}
			d.deps.Log.Error("cancelTask: kv update", "id", p.ID, "err", err)
			return nil, jsonrpc.ErrInternal
		}

		return json.RawMessage(mutated), nil
	}

	return nil, jsonrpc.ErrInternal.WithData(map[string]string{"kind": "task-leased"})
}

// parseStatusState partial-decodes the stored Task bytes to extract
// status.state without touching any sibling fields. Returns "" if the
// path is missing — callers treat that as non-terminal.
func parseStatusState(b []byte) string {
	var probe struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return ""
	}
	return probe.Status.State
}

// setStatusState decodes b to a generic map, mutates status.state in
// place, and re-encodes. Preserves all unknown sibling fields verbatim
// (contextId, history, artifacts, etc.). If status is missing it is
// created; if status is present but not an object, returns an error.
func setStatusState(b []byte, state string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("decode task: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	rawStatus, ok := doc["status"]
	if !ok {
		doc["status"] = map[string]any{"state": state}
	} else {
		statusMap, ok := rawStatus.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("status is %T, want object", rawStatus)
		}
		statusMap["state"] = state
		doc["status"] = statusMap
	}
	return json.Marshal(doc)
}
