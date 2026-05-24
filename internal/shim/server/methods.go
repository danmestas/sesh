package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

// dispatch resolves a JSON-RPC method name to its handler. New methods
// land here in Slice 2+; Slice 3 promotes this map to its own package.
func (s *server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *jsonrpc.Error) {
	switch method {
	case "GetTask":
		return s.getTask(ctx, params)
	case "GetExtendedAgentCard":
		return s.getExtendedAgentCard(ctx, params)
	default:
		return nil, jsonrpc.ErrMethodNotFound
	}
}

type getTaskParams struct {
	ID            string `json:"id"`
	HistoryLength int    `json:"historyLength,omitempty"`
}

// getTask reads the raw Task JSON from JetStream KV bucket
// sesh_tasks_<scope-kind>_<scope-id> and returns it verbatim. Slice 2
// will move this through sesh-ops/task.Get(...).
func (s *server) getTask(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if s.cfg.JetStream == nil {
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
	bucket := fmt.Sprintf("sesh_tasks_%s_%s", s.cfg.ScopeKind, s.cfg.ScopeID)
	kv, err := s.cfg.JetStream.KeyValue(bucket)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		s.log.Error("getTask: open kv", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	entry, err := kv.Get(p.ID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		s.log.Error("getTask: kv get", "bucket", bucket, "id", p.ID, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	return json.RawMessage(entry.Value()), nil
}

// getExtendedAgentCard always returns the AuthenticatedExtendedCardNotConfigured
// error in Slice 1. Slice 5 implements the real fetch path.
func (s *server) getExtendedAgentCard(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	return nil, jsonrpc.ErrExtendedCardNotConfigured
}
