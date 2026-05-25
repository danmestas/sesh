package methods

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/scope"
)

func TestSendMessage_NewTask(t *testing.T) {
	deps, nc, js2 := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	// Subscribe to the v2 prompt subject so we can confirm publish.
	got := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("agents.prompt.v2.>", func(m *nats.Msg) {
		select {
		case got <- m:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	params := json.RawMessage(`{"message":{"messageId":"M1","role":"ROLE_USER","parts":[{"text":"hello"}]}}`)
	res, jerr := disp.sendMessage(ctx, params)
	if jerr != nil {
		t.Fatalf("sendMessage: %+v", jerr)
	}
	raw := mustUnwrapTask(t, res)
	var task struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		ContextID string `json:"contextId"`
		Status    struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("decode task: %v body=%s", err, raw)
	}
	if task.Kind != "task" {
		t.Errorf("task.kind = %q", task.Kind)
	}
	if task.ID == "" || task.ContextID == "" {
		t.Errorf("task ids empty: %+v", task)
	}
	if task.Status.State != "TASK_STATE_SUBMITTED" {
		t.Errorf("status.state = %q", task.Status.State)
	}

	// Confirm KV writes: tasks bucket has task; messages bucket has msg.
	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvTasks, err := js2.KeyValue(ctx, tasksBucket)
	if err != nil {
		t.Fatalf("open tasks kv: %v", err)
	}
	if _, err := kvTasks.Get(ctx, task.ID); err != nil {
		t.Fatalf("task missing in kv: %v", err)
	}
	msgsBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "messages")
	kvMsgs, err := js2.KeyValue(ctx, msgsBucket)
	if err != nil {
		t.Fatalf("open messages kv: %v", err)
	}
	entry, err := kvMsgs.Get(ctx, messages.Key(task.ID, "M1"))
	if err != nil {
		t.Fatalf("message missing in kv: %v", err)
	}
	stored, err := messages.Unmarshal(entry.Value())
	if err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if stored.Role != messages.MessageRoleUser {
		t.Errorf("stored role = %q, want %q", stored.Role, messages.MessageRoleUser)
	}

	// Confirm publish.
	select {
	case m := <-got:
		if !strings.HasPrefix(m.Subject, "agents.prompt.v2.test-machine.abc123.abc123.test-agent") {
			t.Errorf("publish subject = %q", m.Subject)
		}
		if !bytes.Contains(m.Data, []byte(`"messageId":"M1"`)) {
			t.Errorf("publish payload missing messageId: %s", m.Data)
		}
	case <-time.After(time.Second):
		t.Error("never received prompt publish")
	}
}

func TestSendMessage_ExistingTask(t *testing.T) {
	deps, _, js2 := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	// Pre-create the tasks bucket and seed a task.
	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvTasks, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket})
	if err != nil {
		t.Fatal(err)
	}
	seedTask := []byte(`{"id":"PRE-T","kind":"task","status":{"state":"TASK_STATE_WORKING"}}`)
	if _, err := kvTasks.Create(ctx, "PRE-T", seedTask); err != nil {
		t.Fatal(err)
	}

	params := json.RawMessage(`{"message":{"messageId":"M2","taskId":"PRE-T","role":"ROLE_USER","parts":[{"text":"hi"}]}}`)
	res, jerr := disp.sendMessage(ctx, params)
	if jerr != nil {
		t.Fatalf("sendMessage: %+v", jerr)
	}
	raw := mustUnwrapTask(t, res)
	if !bytes.Contains(raw, []byte(`"id":"PRE-T"`)) {
		t.Errorf("returned task not PRE-T: %s", raw)
	}
}

func TestSendMessage_UnknownTask(t *testing.T) {
	deps, _, js2 := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	if _, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket}); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"message":{"messageId":"M","taskId":"ghost","role":"ROLE_USER","parts":[{"text":"x"}]}}`)
	_, jerr := disp.sendMessage(ctx, params)
	if jerr == nil {
		t.Fatal("want TaskNotFound error")
	}
	if jerr.Code != -32001 {
		t.Errorf("code = %d, want -32001", jerr.Code)
	}
}

func TestSendMessage_IdempotentRetry_Identical(t *testing.T) {
	deps, _, js2 := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	// Pre-create tasks bucket with a fixed task so both calls go to existing.
	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvT, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kvT.Create(ctx, "T-IDEMP", []byte(`{"id":"T-IDEMP","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
		t.Fatal(err)
	}

	params := json.RawMessage(`{"message":{"messageId":"M-DUP","taskId":"T-IDEMP","role":"ROLE_USER","parts":[{"text":"same"}]}}`)
	if _, jerr := disp.sendMessage(ctx, params); jerr != nil {
		t.Fatalf("first call: %+v", jerr)
	}
	if _, jerr := disp.sendMessage(ctx, params); jerr != nil {
		t.Fatalf("retry (identical) should succeed: %+v", jerr)
	}
	msgsBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "messages")
	kvM, err := js2.KeyValue(ctx, msgsBucket)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := kvM.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, k := range keys {
		if strings.HasPrefix(k, "T-IDEMP.") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 message for task, got %d (keys=%v)", count, keys)
	}
}

func TestSendMessage_IdempotentRetry_Divergent(t *testing.T) {
	deps, _, js2 := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvT, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kvT.Create(ctx, "T-DIV", []byte(`{"id":"T-DIV","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
		t.Fatal(err)
	}

	first := json.RawMessage(`{"message":{"messageId":"M-X","taskId":"T-DIV","role":"ROLE_USER","parts":[{"text":"original"}]}}`)
	if _, jerr := disp.sendMessage(ctx, first); jerr != nil {
		t.Fatalf("first call: %+v", jerr)
	}
	second := json.RawMessage(`{"message":{"messageId":"M-X","taskId":"T-DIV","role":"ROLE_USER","parts":[{"text":"different"}]}}`)
	_, jerr := disp.sendMessage(ctx, second)
	if jerr == nil {
		t.Fatal("divergent retry: want -32602")
	}
	if jerr.Code != -32602 {
		t.Errorf("code = %d, want -32602", jerr.Code)
	}
	var data map[string]string
	if err := json.Unmarshal(jerr.Data, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data["kind"] != "messageId-conflict" {
		t.Errorf("data.kind = %q, want messageId-conflict", data["kind"])
	}
}

func TestSendMessage_RoleTranslation_AgentRoleStored(t *testing.T) {
	deps, _, js2 := testDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	params := json.RawMessage(`{"message":{"messageId":"M-AGT","role":"ROLE_AGENT","parts":[{"text":"x"}]}}`)
	res, jerr := disp.sendMessage(ctx, params)
	if jerr != nil {
		t.Fatalf("sendMessage: %+v", jerr)
	}
	raw := mustUnwrapTask(t, res)
	var task struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &task)

	msgsBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "messages")
	kvM, err := js2.KeyValue(ctx, msgsBucket)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := kvM.Get(ctx, messages.Key(task.ID, "M-AGT"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := messages.Unmarshal(entry.Value())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Role != messages.MessageRoleAgent {
		t.Errorf("stored role = %q, want %q", stored.Role, messages.MessageRoleAgent)
	}
}

func TestSendMessage_InvalidParams(t *testing.T) {
	deps, _, _ := testDeps(t)
	disp := NewDispatcher(deps)

	cases := []struct {
		name   string
		params json.RawMessage
	}{
		{"empty", json.RawMessage{}},
		{"no message", json.RawMessage(`{}`)},
		{"bad json", json.RawMessage(`{garbage`)},
		{"bad inner message", json.RawMessage(`{"message":"not-an-object"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := mustCtx(t)
			defer cancel()
			_, jerr := disp.sendMessage(ctx, tc.params)
			if jerr == nil {
				t.Fatalf("want error")
			}
			if jerr.Code != -32602 {
				t.Errorf("code = %d, want -32602", jerr.Code)
			}
		})
	}
}

// TestSendMessage_DottedScopeID_PublishesPromptV2 covers sesh#121:
// a session-scoped shim has ScopeID = "<project>.<session>" which
// contains '.', and subject.PromptV2 rejects tokens with '.'.
// publishPromptV2 must sanitize ('.' -> '_') so the publish succeeds
// and an adapter subscribed to the canonical sanitized subject wakes
// up. Regression guard: prior to the fix this published nothing and
// the adapter starved.
func TestSendMessage_DottedScopeID_PublishesPromptV2(t *testing.T) {
	deps, nc, _ := testDeps(t)
	deps.ScopeKind = "session"
	deps.ScopeID = "acme.demo"
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()

	// Subscribe to the exact sanitized subject we expect.
	wantSubj := "agents.prompt.v2.test-machine.acme_demo.acme_demo.test-agent"
	got := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe(wantSubj, func(m *nats.Msg) {
		select {
		case got <- m:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	params := json.RawMessage(`{"message":{"messageId":"M-DOT","role":"ROLE_USER","parts":[{"text":"x"}]}}`)
	if _, jerr := disp.sendMessage(ctx, params); jerr != nil {
		t.Fatalf("sendMessage with dotted scope-id: %+v", jerr)
	}

	select {
	case m := <-got:
		if m.Subject != wantSubj {
			t.Errorf("publish subject = %q, want %q", m.Subject, wantSubj)
		}
		if !bytes.Contains(m.Data, []byte(`"messageId":"M-DOT"`)) {
			t.Errorf("publish payload missing messageId: %s", m.Data)
		}
	case <-time.After(time.Second):
		t.Errorf("no publish on sanitized subject %q (sesh#121 regression — dotted scope-id should be sanitized)", wantSubj)
	}
}

// TestSendMessage_PublishNoOp_WhenAgentEmpty verifies that when no
// agent token is configured (so publishPromptV2 early-returns), the
// SendMessage path still completes successfully — i.e., the publish
// is fire-and-forget and never gates the JSON-RPC response.
func TestSendMessage_PublishNoOp_WhenAgentEmpty(t *testing.T) {
	deps, _, _ := testDeps(t)
	deps.AgentKey.Agent = "" // force publishPromptV2 to return early
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()
	params := json.RawMessage(`{"message":{"messageId":"M-NOPUB","role":"ROLE_USER","parts":[{"text":"x"}]}}`)
	if _, jerr := disp.sendMessage(ctx, params); jerr != nil {
		t.Fatalf("sendMessage should succeed even with publish no-op: %+v", jerr)
	}
}

// mustUnwrapTask extracts the raw task JSON from sendMessage's
// StreamResponse envelope (`{"task": <raw>}` per the a2a-go spec).
func mustUnwrapTask(t *testing.T, res any) json.RawMessage {
	t.Helper()
	env, ok := res.(map[string]json.RawMessage)
	if !ok {
		t.Fatalf("result type = %T, want map[string]json.RawMessage", res)
	}
	raw, ok := env["task"]
	if !ok {
		t.Fatalf("result envelope missing 'task' key: %v", env)
	}
	return raw
}
