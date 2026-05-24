package methods

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/a2a"
	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

// listTasksParams accepts the full A2A ListTasksRequest shape; Slice 4
// honors only Status (in-memory exact-match filter). All other fields
// are accept-but-ignored per plan §3 non-goals. Pagination is deferred
// until list size matters.
type listTasksParams struct {
	Tenant           string `json:"tenant,omitempty"`
	ContextID        string `json:"contextId,omitempty"`
	Status           string `json:"status,omitempty"`
	PageToken        string `json:"pageToken,omitempty"`
	PageSize         int    `json:"pageSize,omitempty"`
	HistoryLength    *int   `json:"historyLength,omitempty"`
	IncludeArtifacts bool   `json:"includeArtifacts,omitempty"`
}

// listTasksResponse matches the a2a-go ListTasksResponse shape. Tasks
// is always non-nil (initialized via make) so it serializes as `[]`
// rather than `null` for empty results.
type listTasksResponse struct {
	Tasks         []json.RawMessage `json:"tasks"`
	TotalSize     int               `json:"totalSize"`
	PageSize      int               `json:"pageSize"`
	NextPageToken string            `json:"nextPageToken"`
}

const readScope = "agent.read"

// listTasks implements A2A ListTasks. Auth gating is binary in Slice 4:
// principal carrying `agent.read` → return the bucket; absent → return
// an empty list with HTTP 200 (NOT a JSON-RPC error). Per-task ACL
// granularity and pagination are deferred.
func (d *Dispatcher) listTasks(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}

	var p listTasksParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": err.Error()})
		}
	}
	if p.Status != "" && !a2a.IsKnownTaskState(p.Status) {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "unknown status", "status": p.Status})
	}

	// Empty list (HTTP 200) when caller lacks read scope. Absence of a
	// principal counts as "unscoped" — defensive against ctx flowing
	// from non-middleware paths.
	principal, ok := auth.FromContext(ctx)
	if !ok || !hasScope(principal, readScope) {
		return emptyListResponse(), nil
	}

	bucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "tasks")
	if err != nil {
		d.deps.Log.Error("listTasks: bucket derive", "scope_kind", d.deps.ScopeKind, "scope_id", d.deps.ScopeID, "err", err)
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"})
	}
	kv, err := d.deps.JS.KeyValue(ctx, bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return emptyListResponse(), nil
		}
		d.deps.Log.Error("listTasks: open kv", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}

	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return emptyListResponse(), nil
		}
		d.deps.Log.Error("listTasks: kv keys", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}

	out := make([]json.RawMessage, 0, len(keys))
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			// Tolerant of mid-list deletions: a key disappearing between
			// kv.Keys and kv.Get is not fatal.
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			d.deps.Log.Warn("listTasks: kv get", "bucket", bucket, "key", k, "err", err)
			continue
		}
		value := entry.Value()
		if p.Status != "" && parseStatusState(value) != p.Status {
			continue
		}
		// Defensive copy — JetStream v2's caller-ownership semantics for
		// entry.Value() aren't explicitly documented; the few-KB cost is
		// negligible relative to the network hop that already happened.
		cp := make([]byte, len(value))
		copy(cp, value)
		out = append(out, cp)
	}

	return listTasksResponse{
		Tasks:         out,
		TotalSize:     len(out),
		PageSize:      0,
		NextPageToken: "",
	}, nil
}

// hasScope returns true when want appears in p.Scopes. Linear scan is
// fine for the slice sizes we expect (<10 scopes per principal).
func hasScope(p auth.Principal, want string) bool {
	for _, s := range p.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

func emptyListResponse() listTasksResponse {
	return listTasksResponse{
		Tasks:         []json.RawMessage{},
		TotalSize:     0,
		PageSize:      0,
		NextPageToken: "",
	}
}
