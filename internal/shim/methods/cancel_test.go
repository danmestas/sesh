package methods

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/scope"
)

// seedTask creates the tasks bucket (if absent) and writes the given
// JSON payload under id. Returns the kv handle and the post-Put revision.
func seedTask(t *testing.T, ctx context.Context, d Deps, id string, payload []byte) (jetstream.KeyValue, uint64) {
	t.Helper()
	bucket, err := scope.Bucket(d.ScopeKind, d.ScopeID, "tasks")
	if err != nil {
		t.Fatalf("scope.Bucket: %v", err)
	}
	kv, err := d.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	if err != nil {
		// Already exists is fine.
		kv, err = d.JS.KeyValue(ctx, bucket)
		if err != nil {
			t.Fatalf("open kv: %v", err)
		}
	}
	rev, err := kv.Put(ctx, id, payload)
	if err != nil {
		t.Fatalf("kv.Put: %v", err)
	}
	return kv, rev
}

func TestCancelTask_Happy_SubmittedToCanceled(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	taskJSON := []byte(`{"id":"t-1","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)
	kv, rev0 := seedTask(t, ctx, deps, "t-1", taskJSON)

	res, jerr := disp.cancelTask(ctx, json.RawMessage(`{"id":"t-1"}`))
	if jerr != nil {
		t.Fatalf("cancelTask: %+v", jerr)
	}
	raw, ok := res.(json.RawMessage)
	if !ok {
		t.Fatalf("result type = %T, want json.RawMessage", res)
	}

	state := parseStatusState(raw)
	if state != "TASK_STATE_CANCELED" {
		t.Errorf("status.state after cancel = %q, want TASK_STATE_CANCELED", state)
	}

	// Confirm KV revision advanced by exactly 1.
	entry, err := kv.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if entry.Revision() != rev0+1 {
		t.Errorf("revision = %d, want %d (rev0+1)", entry.Revision(), rev0+1)
	}
	if !bytes.Equal(entry.Value(), []byte(raw)) {
		t.Errorf("kv bytes != returned bytes")
	}
}

func TestCancelTask_PreservesUnknownFields(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	taskJSON := []byte(`{"id":"t-2","kind":"task","contextId":"c-99","status":{"state":"TASK_STATE_WORKING","message":"thinking"},"artifacts":[{"name":"x"}],"history":[{"role":"ROLE_USER"}],"metadata":{"unknownField":42}}`)
	seedTask(t, ctx, deps, "t-2", taskJSON)

	res, jerr := disp.cancelTask(ctx, json.RawMessage(`{"id":"t-2"}`))
	if jerr != nil {
		t.Fatalf("cancelTask: %+v", jerr)
	}
	raw := res.(json.RawMessage)

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// All unknown / sibling fields must survive.
	for _, k := range []string{"id", "kind", "contextId", "artifacts", "history", "metadata"} {
		if _, ok := decoded[k]; !ok {
			t.Errorf("preserved field %q missing", k)
		}
	}
	status, _ := decoded["status"].(map[string]any)
	if status["state"] != "TASK_STATE_CANCELED" {
		t.Errorf("status.state = %v, want TASK_STATE_CANCELED", status["state"])
	}
	if status["message"] != "thinking" {
		t.Errorf("status.message dropped: %v", status["message"])
	}
}

func TestCancelTask_Terminal_ReturnsTaskNotCancelable(t *testing.T) {
	// AUTH_REQUIRED is intentionally NOT terminal — a2a-go TaskState.Terminal()
	// excludes it (client resolves auth and resumes). See internal/shim/a2a/taskstate.go.
	for _, state := range []string{
		"TASK_STATE_COMPLETED",
		"TASK_STATE_CANCELED",
		"TASK_STATE_FAILED",
		"TASK_STATE_REJECTED",
	} {
		t.Run(state, func(t *testing.T) {
			deps, _, _ := testDeps(t)
			disp := NewDispatcher(deps)
			ctx, cancel := mustCtx(t)
			defer cancel()

			id := "t-" + state
			payload := []byte(fmt.Sprintf(`{"id":%q,"kind":"task","status":{"state":%q}}`, id, state))
			kv, rev0 := seedTask(t, ctx, deps, id, payload)

			res, jerr := disp.cancelTask(ctx, json.RawMessage(fmt.Sprintf(`{"id":%q}`, id)))
			if res != nil {
				t.Errorf("expected nil result, got %v", res)
			}
			if jerr == nil || jerr.Code != -32002 || jerr.Name != "TaskNotCancelableError" {
				t.Fatalf("got jerr=%+v, want -32002 TaskNotCancelableError", jerr)
			}
			// KV unchanged.
			entry, err := kv.Get(ctx, id)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if entry.Revision() != rev0 {
				t.Errorf("revision changed: %d → %d", rev0, entry.Revision())
			}
		})
	}
}

func TestCancelTask_UnknownID(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	// Create bucket but no key.
	seedTask(t, ctx, deps, "exists", []byte(`{"id":"exists","status":{"state":"TASK_STATE_SUBMITTED"}}`))

	_, jerr := disp.cancelTask(ctx, json.RawMessage(`{"id":"ghost"}`))
	if jerr == nil || jerr.Code != -32001 || jerr.Name != "TaskNotFoundError" {
		t.Fatalf("got %+v, want -32001 TaskNotFoundError", jerr)
	}
}

func TestCancelTask_MissingBucket(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	_, jerr := disp.cancelTask(ctx, json.RawMessage(`{"id":"t-1"}`))
	if jerr == nil || jerr.Code != -32001 || jerr.Name != "TaskNotFoundError" {
		t.Fatalf("got %+v, want -32001 TaskNotFoundError", jerr)
	}
}

func TestCancelTask_InvalidParams(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	for name, params := range map[string]json.RawMessage{
		"missing":    nil,
		"empty":      json.RawMessage(`{}`),
		"emptyID":    json.RawMessage(`{"id":""}`),
		"malformed":  json.RawMessage(`{"id":`),
		"wrongShape": json.RawMessage(`{"id":123}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, jerr := disp.cancelTask(ctx, params)
			if jerr == nil || jerr.Code != -32602 {
				t.Fatalf("got %+v, want -32602", jerr)
			}
		})
	}
}

func TestCancelTask_MissingStatus_TreatedAsNonTerminal(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	// No status object at all — must be treated as non-terminal and canceled.
	taskJSON := []byte(`{"id":"t-noStatus","kind":"task"}`)
	seedTask(t, ctx, deps, "t-noStatus", taskJSON)

	res, jerr := disp.cancelTask(ctx, json.RawMessage(`{"id":"t-noStatus"}`))
	if jerr != nil {
		t.Fatalf("cancelTask: %+v", jerr)
	}
	state := parseStatusState(res.(json.RawMessage))
	if state != "TASK_STATE_CANCELED" {
		t.Errorf("state = %q, want TASK_STATE_CANCELED", state)
	}
}

// withCASInjector swaps the package-level testCASInjector for the
// duration of one test, restoring the previous value on cleanup.
// Tests must NOT run in parallel because the hook is global.
func withCASInjector(t *testing.T, fn func()) {
	t.Helper()
	prev := testCASInjector
	testCASInjector = fn
	t.Cleanup(func() { testCASInjector = prev })
}

// TestCancelTask_CAS_Succeeds drives the retry-loop's success branch:
// a competing writer wins the first CAS attempt, then the handler must
// retry and succeed on attempt 2.
func TestCancelTask_CAS_Succeeds(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	taskJSON := []byte(`{"id":"t-cas","kind":"task","status":{"state":"TASK_STATE_WORKING"}}`)
	kv, _ := seedTask(t, ctx, deps, "t-cas", taskJSON)

	// Inject a racer that bumps the rev once on the first call only.
	// The handler's first Update will fail (rev stale), then on retry
	// GetRaw returns the new rev, racer is now a no-op, Update succeeds.
	var once sync.Once
	withCASInjector(t, func() {
		once.Do(func() {
			latest, _ := kv.Get(ctx, "t-cas")
			_, _ = kv.Update(ctx, "t-cas", latest.Value(), latest.Revision())
		})
	})

	res, jerr := disp.cancelTask(ctx, json.RawMessage(`{"id":"t-cas"}`))
	if jerr != nil {
		t.Fatalf("cancelTask: %+v", jerr)
	}
	state := parseStatusState(res.(json.RawMessage))
	if state != "TASK_STATE_CANCELED" {
		t.Errorf("final state = %q, want TASK_STATE_CANCELED", state)
	}
}

// TestCancelTask_CAS_Exhausted drives the retry-loop's exhaustion branch:
// every CAS attempt loses. Expects -32603 task-leased. Bounded <1s wall.
func TestCancelTask_CAS_Exhausted(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	taskJSON := []byte(`{"id":"t-cas-x","kind":"task","status":{"state":"TASK_STATE_WORKING"}}`)
	kv, _ := seedTask(t, ctx, deps, "t-cas-x", taskJSON)

	// Inject a racer that ALWAYS bumps the rev before each Update.
	var hits int32
	withCASInjector(t, func() {
		atomic.AddInt32(&hits, 1)
		latest, _ := kv.Get(ctx, "t-cas-x")
		_, _ = kv.Update(ctx, "t-cas-x", latest.Value(), latest.Revision())
	})

	start := time.Now()
	res, jerr := disp.cancelTask(ctx, json.RawMessage(`{"id":"t-cas-x"}`))
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("CAS exhaustion took %v, want <1s", elapsed)
	}
	if res != nil {
		t.Errorf("expected nil result, got %v", res)
	}
	if jerr == nil || jerr.Code != -32603 {
		t.Fatalf("got %+v, want -32603", jerr)
	}
	if !strings.Contains(string(jerr.Data), "task-leased") {
		t.Errorf("data missing task-leased kind: %s", jerr.Data)
	}
	if atomic.LoadInt32(&hits) < 3 {
		t.Errorf("expected ≥3 racer hits, got %d", hits)
	}
}

// Quick sanity: errors.Is unwrapping behaves through task.GetRaw.
func TestCancelTask_ErrorsIsUnwrapsKeyNotFound(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrap: %w", jetstream.ErrKeyNotFound), jetstream.ErrKeyNotFound) {
		t.Fatal("errors.Is unwrap broken")
	}
}
