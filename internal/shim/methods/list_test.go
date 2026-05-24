package methods

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/auth/authtest"
)

// readScopedCtx returns a context carrying a Principal with the
// `agent.read` scope, matching what auth.NoopValidator hands out.
func readScopedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := mustCtx(t)
	t.Cleanup(cancel)
	return authtest.WithPrincipal(ctx, auth.Principal{Sub: "test", Scopes: []string{"agent.read", "agent.write"}})
}

// unscopedCtx returns a context with a Principal that lacks any scopes.
func unscopedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := mustCtx(t)
	t.Cleanup(cancel)
	return authtest.WithPrincipal(ctx, auth.Principal{Sub: "test", Scopes: []string{}})
}

func decodeListResponse(t *testing.T, res any) listTasksResponse {
	t.Helper()
	resp, ok := res.(listTasksResponse)
	if !ok {
		t.Fatalf("result type = %T, want listTasksResponse", res)
	}
	return resp
}

func TestListTasks_EmptyBucket_WithReadScope(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx := readScopedCtx(t)

	res, jerr := disp.listTasks(ctx, nil)
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	if len(resp.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(resp.Tasks))
	}
	if resp.TotalSize != 0 {
		t.Errorf("TotalSize = %d, want 0", resp.TotalSize)
	}
}

func TestListTasks_EmptyBucket_WithoutReadScope(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx := unscopedCtx(t)

	res, jerr := disp.listTasks(ctx, nil)
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	if len(resp.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(resp.Tasks))
	}
}

func TestListTasks_PopulatedBucket_WithReadScope(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctxScope := readScopedCtx(t)
	ctxBare, cancel := mustCtx(t)
	defer cancel()

	// Seed three tasks with deterministic byte-identical payloads.
	payloads := map[string][]byte{
		"a": []byte(`{"id":"a","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`),
		"b": []byte(`{"id":"b","kind":"task","status":{"state":"TASK_STATE_WORKING"}}`),
		"c": []byte(`{"id":"c","kind":"task","status":{"state":"TASK_STATE_COMPLETED"}}`),
	}
	for id, p := range payloads {
		seedTask(t, ctxBare, deps, id, p)
	}

	res, jerr := disp.listTasks(ctxScope, nil)
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	if len(resp.Tasks) != 3 {
		t.Fatalf("Tasks len = %d, want 3", len(resp.Tasks))
	}
	if resp.TotalSize != 3 {
		t.Errorf("TotalSize = %d, want 3", resp.TotalSize)
	}

	// Each returned blob is byte-identical to one seeded payload.
	gotIDs := []string{}
	for _, raw := range resp.Tasks {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("decode task: %v", err)
		}
		gotIDs = append(gotIDs, probe.ID)
		want, ok := payloads[probe.ID]
		if !ok {
			t.Errorf("unexpected id %q", probe.ID)
			continue
		}
		if !bytes.Equal(raw, want) {
			t.Errorf("byte mismatch for %q:\n got=%s\nwant=%s", probe.ID, raw, want)
		}
	}
	sort.Strings(gotIDs)
	want := []string{"a", "b", "c"}
	if !equalStrings(gotIDs, want) {
		t.Errorf("ids = %v, want %v", gotIDs, want)
	}
}

func TestListTasks_PopulatedBucket_WithoutReadScope(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctxBare, cancel := mustCtx(t)
	defer cancel()
	seedTask(t, ctxBare, deps, "a", []byte(`{"id":"a","status":{"state":"TASK_STATE_SUBMITTED"}}`))
	seedTask(t, ctxBare, deps, "b", []byte(`{"id":"b","status":{"state":"TASK_STATE_WORKING"}}`))

	res, jerr := disp.listTasks(unscopedCtx(t), nil)
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	if len(resp.Tasks) != 0 {
		t.Errorf("unscoped Tasks len = %d, want 0", len(resp.Tasks))
	}
}

func TestListTasks_StatusFilter_Match(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctxBare, cancel := mustCtx(t)
	defer cancel()
	seedTask(t, ctxBare, deps, "a", []byte(`{"id":"a","status":{"state":"TASK_STATE_SUBMITTED"}}`))
	seedTask(t, ctxBare, deps, "b", []byte(`{"id":"b","status":{"state":"TASK_STATE_WORKING"}}`))
	seedTask(t, ctxBare, deps, "c", []byte(`{"id":"c","status":{"state":"TASK_STATE_WORKING"}}`))

	res, jerr := disp.listTasks(readScopedCtx(t),
		json.RawMessage(`{"status":"TASK_STATE_WORKING"}`))
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	if len(resp.Tasks) != 2 {
		t.Fatalf("Tasks len = %d, want 2", len(resp.Tasks))
	}
}

func TestListTasks_StatusFilter_NoMatch(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctxBare, cancel := mustCtx(t)
	defer cancel()
	seedTask(t, ctxBare, deps, "a", []byte(`{"id":"a","status":{"state":"TASK_STATE_SUBMITTED"}}`))

	res, jerr := disp.listTasks(readScopedCtx(t),
		json.RawMessage(`{"status":"TASK_STATE_CANCELED"}`))
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	if len(resp.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(resp.Tasks))
	}
}

func TestListTasks_InvalidStatus_Rejects(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)

	_, jerr := disp.listTasks(readScopedCtx(t),
		json.RawMessage(`{"status":"NOT_A_REAL_STATE"}`))
	if jerr == nil || jerr.Code != -32602 {
		t.Fatalf("got %+v, want -32602", jerr)
	}
}

func TestListTasks_NoPrincipal_TreatedAsUnscoped(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctxBare, cancel := mustCtx(t)
	defer cancel()
	seedTask(t, ctxBare, deps, "a", []byte(`{"id":"a","status":{"state":"TASK_STATE_SUBMITTED"}}`))

	// No Principal installed. Must NOT 401 — return empty list with no error.
	res, jerr := disp.listTasks(ctxBare, nil)
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	if len(resp.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(resp.Tasks))
	}
}

func TestListTasks_BucketGetRaceTolerance(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctxBare, cancel := mustCtx(t)
	defer cancel()

	seedTask(t, ctxBare, deps, "stay", []byte(`{"id":"stay","status":{"state":"TASK_STATE_SUBMITTED"}}`))
	kv, _ := seedTask(t, ctxBare, deps, "delete-me", []byte(`{"id":"delete-me","status":{"state":"TASK_STATE_SUBMITTED"}}`))
	// Delete the key after kv.Keys would see it but before the handler
	// Gets it. We simulate by deleting now — the handler's kv.Get will
	// surface ErrKeyNotFound and the loop must skip.
	if err := kv.Delete(ctxBare, "delete-me"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	res, jerr := disp.listTasks(readScopedCtx(t), nil)
	if jerr != nil {
		t.Fatalf("listTasks: %+v", jerr)
	}
	resp := decodeListResponse(t, res)
	// Either 0 (already-deleted excluded from Keys) or 1 (deleted between
	// Keys and Get) is acceptable — we just want no error.
	if len(resp.Tasks) > 1 {
		t.Errorf("Tasks len = %d, want ≤1", len(resp.Tasks))
	}
}

func TestListTasks_InvalidParamsShape(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	_, jerr := disp.listTasks(readScopedCtx(t), json.RawMessage(`{"pageSize":`))
	if jerr == nil || jerr.Code != -32602 {
		t.Fatalf("got %+v, want -32602", jerr)
	}
}

// Confirm scope.Bucket sees the right ScopeKind/ID — guard against
// helpers drifting from production wiring.
func TestListTasks_BucketDerivation(t *testing.T) {
	deps, _, _ := testDeps(t)
	bucket, err := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if bucket == "" {
		t.Errorf("empty bucket")
	}
	// Sanity-check that the bucket would resolve via JS v2 when present.
	ctx, cancel := mustCtx(t)
	defer cancel()
	if _, err := deps.JS.KeyValue(ctx, bucket); err != nil && err != jetstream.ErrBucketNotFound {
		t.Errorf("unexpected error: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
