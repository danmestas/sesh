package methods

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/notifications"
	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/auth/authtest"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
	"github.com/danmestas/sesh/internal/shim/push"
)

// pushSentinel is the plaintext we feed through Create so the "never
// in KV, never in logs" assertion has a stable, grep-able witness.
const pushSentinel = "PLAINTEXT_SENTINEL_42"

func pushTestKey(t *testing.T) []byte {
	t.Helper()
	k, err := push.NewDevKey()
	if err != nil {
		t.Fatalf("dev key: %v", err)
	}
	return k
}

// pushDeps wraps testDeps with PushKey set + dev-localhost permission
// + a buffer-backed slog logger so credential-leak scans have something
// to inspect.
func pushDeps(t *testing.T) (Deps, *bytes.Buffer) {
	t.Helper()
	d, _, _ := testDeps(t)
	buf := new(bytes.Buffer)
	d.Log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d.PushKey = pushTestKey(t)
	d.PushDevAllowLocalhost = true
	return d, buf
}

func notifyWriteCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := mustCtx(t)
	t.Cleanup(cancel)
	return authtest.WithPrincipal(ctx, auth.Principal{Sub: "test", Scopes: []string{notifyWriteScope, notifyReadScope}})
}

func notifyReadCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := mustCtx(t)
	t.Cleanup(cancel)
	return authtest.WithPrincipal(ctx, auth.Principal{Sub: "test", Scopes: []string{notifyReadScope}})
}

func notifyNoScopeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := mustCtx(t)
	t.Cleanup(cancel)
	return authtest.WithPrincipal(ctx, auth.Principal{Sub: "test", Scopes: []string{}})
}

// seedTaskForPush creates the tasks bucket + a stub task so the Create
// handler's existence check succeeds. Returns the task id.
func seedTaskForPush(t *testing.T, ctx context.Context, d Deps, taskID string) {
	t.Helper()
	payload := []byte(`{"id":"` + taskID + `","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)
	seedTask(t, ctx, d, taskID, payload)
}

func validCreateParams(t *testing.T, taskID, configID, credentials string) json.RawMessage {
	t.Helper()
	body := map[string]any{
		"taskId": taskID,
		"pushNotificationConfig": map[string]any{
			"url": "http://127.0.0.1:9999/hook",
			"authentication": map[string]any{
				"scheme":      "Bearer",
				"credentials": credentials,
			},
		},
	}
	if configID != "" {
		body["pushNotificationConfigId"] = configID
		body["pushNotificationConfig"].(map[string]any)["id"] = configID
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawNotifyValue reads the on-disk bytes for (taskID, configID).
// Used to assert ciphertext at rest.
func rawNotifyValue(t *testing.T, ctx context.Context, d Deps, taskID, configID string) []byte {
	t.Helper()
	bucket, err := scope.Bucket(d.ScopeKind, d.ScopeID, "notifycfg")
	if err != nil {
		t.Fatal(err)
	}
	kv, err := d.JS.KeyValue(ctx, bucket)
	if err != nil {
		t.Fatalf("open notifycfg kv: %v", err)
	}
	entry, err := kv.Get(ctx, taskID+"."+configID)
	if err != nil {
		t.Fatalf("get notifycfg entry: %v", err)
	}
	return entry.Value()
}

// --- Create -----------------------------------------------------------

// TestCreatePushCfg_HappyPath_EncryptsAtRest exercises the load-bearing
// invariant: plaintext goes in via the wire, ciphertext lands in KV,
// and the response echoes plaintext for caller confirmation.
func TestCreatePushCfg_HappyPath_EncryptsAtRest(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	seedTaskForPush(t, ctx, d, "T1")

	res, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "T1", "cfg-1", pushSentinel))
	if jerr != nil {
		t.Fatalf("create: %+v", jerr)
	}
	cfg, ok := res.(notifications.NotifyConfig)
	if !ok {
		t.Fatalf("result type = %T, want NotifyConfig", res)
	}
	if cfg.Auth == nil || cfg.Auth.Credentials != pushSentinel {
		t.Errorf("response credentials = %+v, want plaintext", cfg.Auth)
	}

	raw := rawNotifyValue(t, ctx, d, "T1", "cfg-1")
	if bytes.Contains(raw, []byte(pushSentinel)) {
		t.Errorf("KV bytes contain plaintext sentinel:\n  %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"credentials":"enc:`)) {
		t.Errorf("KV bytes missing enc: prefix:\n  %s", raw)
	}
}

// TestCreatePushCfg_AssignsBlankID — a Create with no configId mints
// a ULID and returns it. Caller relies on the response shape to know
// what to subsequently Get/Delete.
func TestCreatePushCfg_AssignsBlankID(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	seedTaskForPush(t, ctx, d, "T1")

	params := json.RawMessage(`{"taskId":"T1","pushNotificationConfig":{"url":"http://127.0.0.1:9999/h"}}`)
	res, jerr := disp.createTaskPushNotificationConfig(ctx, params)
	if jerr != nil {
		t.Fatalf("create: %+v", jerr)
	}
	cfg := res.(notifications.NotifyConfig)
	if cfg.ID == "" {
		t.Error("expected minted ID, got empty")
	}
}

// TestCreatePushCfg_TaskNotFound — the handler must verify the task
// exists before writing config, so callers can't accumulate orphan
// records.
func TestCreatePushCfg_TaskNotFound(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	// Don't seed the task.
	_, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "ghost", "cfg", pushSentinel))
	if jerr == nil || jerr.Name != "TaskNotFoundError" {
		t.Fatalf("got %+v, want TaskNotFoundError", jerr)
	}
}

// TestCreatePushCfg_RejectsSSRF runs every plan-mandated reject case
// (http in prod, RFC1918, link-local, metadata) and confirms Create
// surfaces ErrInvalidParams without writing to KV.
func TestCreatePushCfg_RejectsSSRF(t *testing.T) {
	d, _ := pushDeps(t)
	d.PushDevAllowLocalhost = false // tighten to prod for these cases
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	seedTaskForPush(t, ctx, d, "T1")

	bad := []string{
		"http://example.com/hook",      // http in prod
		"https://10.0.0.5/hook",        // RFC1918
		"https://192.168.0.1/hook",     // RFC1918
		"https://169.254.169.254/hook", // metadata
		"https://[::1]/hook",           // loopback v6
		"https://127.0.0.1/hook",       // loopback v4
		"ftp://example.com/hook",       // bad scheme
	}
	for _, u := range bad {
		body := map[string]any{
			"taskId":                   "T1",
			"pushNotificationConfigId": "cfg-x",
			"pushNotificationConfig":   map[string]any{"url": u},
		}
		raw, _ := json.Marshal(body)
		_, jerr := disp.createTaskPushNotificationConfig(ctx, raw)
		if jerr == nil || jerr.Code != -32602 {
			t.Errorf("url=%q got %+v, want -32602", u, jerr)
		}
	}
}

// TestCreatePushCfg_MissingScope — caller carrying no
// agent.notify.write scope is rejected (NOT silently accepted nor
// "empty-list-on-200" like List; Create is a privilege operation).
func TestCreatePushCfg_MissingScope(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyNoScopeCtx(t)
	_, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "T1", "cfg-1", pushSentinel))
	if jerr == nil || jerr.Code != jsonrpc.ErrInvalidReq.Code {
		t.Fatalf("got %+v, want ErrInvalidReq", jerr)
	}
}

// TestCreatePushCfg_NoPushKey — push not configured ⇒ -32008
// regardless of scope. Lets operators run the shim with push disabled
// without surfacing a misleading auth failure to clients.
func TestCreatePushCfg_NoPushKey(t *testing.T) {
	d, _ := pushDeps(t)
	d.PushKey = nil
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	_, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "T1", "cfg-1", pushSentinel))
	if jerr == nil || jerr.Code != -32008 {
		t.Fatalf("got %+v, want -32008", jerr)
	}
}

// --- Get --------------------------------------------------------------

// TestGetPushCfg_HappyPath_Decrypts confirms a previously-Set record
// round-trips through Get with plaintext Credentials.
func TestGetPushCfg_HappyPath_Decrypts(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	seedTaskForPush(t, ctx, d, "T1")
	_, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "T1", "cfg-1", pushSentinel))
	if jerr != nil {
		t.Fatalf("seed create: %+v", jerr)
	}

	params := json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"cfg-1"}`)
	res, jerr := disp.getTaskPushNotificationConfig(notifyReadCtx(t), params)
	if jerr != nil {
		t.Fatalf("get: %+v", jerr)
	}
	cfg := res.(notifications.NotifyConfig)
	if cfg.Auth == nil || cfg.Auth.Credentials != pushSentinel {
		t.Errorf("get credentials = %+v, want plaintext", cfg.Auth)
	}
}

// TestGetPushCfg_NotFound — missing record surfaces TaskNotFoundError.
// (The A2A spec doesn't define a "PushConfigNotFound"; reuse
// TaskNotFound to stay inside the spec's vocabulary.)
func TestGetPushCfg_NotFound(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyReadCtx(t)
	// Seed the bucket but not the key.
	bucket, err := scope.Bucket(d.ScopeKind, d.ScopeID, "notifycfg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}
	_, jerr := disp.getTaskPushNotificationConfig(ctx, json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"missing"}`))
	if jerr == nil || jerr.Name != "TaskNotFoundError" {
		t.Fatalf("got %+v, want TaskNotFoundError", jerr)
	}
}

// TestGetPushCfg_MissingScope — read scope is required; absence is a
// hard error (not the empty-list pattern, because Get returns a
// single record, not a collection).
func TestGetPushCfg_MissingScope(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	_, jerr := disp.getTaskPushNotificationConfig(notifyNoScopeCtx(t), json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"x"}`))
	if jerr == nil || jerr.Code != jsonrpc.ErrInvalidReq.Code {
		t.Fatalf("got %+v, want ErrInvalidReq", jerr)
	}
}

// TestGetPushCfg_LegacyPlaintextWarns — a record predating Slice 6
// (raw plaintext credential, no "enc:" prefix) decrypts to itself
// + WARN log. The Get response carries the value as-is so existing
// deployments don't break on the cutover.
func TestGetPushCfg_LegacyPlaintextWarns(t *testing.T) {
	d, buf := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyReadCtx(t)
	bucket, _ := scope.Bucket(d.ScopeKind, d.ScopeID, "notifycfg")
	kv, err := d.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	// Hand-craft a legacy record with raw credentials.
	legacy := notifications.NotifyConfig{
		TaskID: "T1",
		ID:     "cfg-old",
		URL:    "https://example.com/h",
		Auth:   &notifications.NotifyAuth{Scheme: "Bearer", Credentials: "legacy-plain-secret"},
	}
	if err := notifications.Set(ctx, kv, "T1", "cfg-old", legacy); err != nil {
		t.Fatal(err)
	}

	res, jerr := disp.getTaskPushNotificationConfig(ctx, json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"cfg-old"}`))
	if jerr != nil {
		t.Fatalf("get: %+v", jerr)
	}
	cfg := res.(notifications.NotifyConfig)
	if cfg.Auth.Credentials != "legacy-plain-secret" {
		t.Errorf("legacy passthrough mismatch: got %q", cfg.Auth.Credentials)
	}
	if !strings.Contains(buf.String(), "legacy plaintext") {
		t.Errorf("expected WARN log for legacy plaintext, got: %s", buf.String())
	}
}

// --- List -------------------------------------------------------------

// TestListPushCfg_DecryptsAll seeds 3 records via Create then asserts
// List returns all three with plaintext Credentials.
func TestListPushCfg_DecryptsAll(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	seedTaskForPush(t, ctx, d, "T1")
	for _, id := range []string{"a", "b", "c"} {
		_, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "T1", id, pushSentinel+"-"+id))
		if jerr != nil {
			t.Fatalf("seed %s: %+v", id, jerr)
		}
	}

	res, jerr := disp.listTaskPushNotificationConfigs(notifyReadCtx(t), json.RawMessage(`{"taskId":"T1"}`))
	if jerr != nil {
		t.Fatalf("list: %+v", jerr)
	}
	resp := res.(pushListResponse)
	if len(resp.Configs) != 3 {
		t.Fatalf("Configs len = %d, want 3", len(resp.Configs))
	}
	for _, c := range resp.Configs {
		if c.Auth == nil || !strings.HasPrefix(c.Auth.Credentials, pushSentinel) {
			t.Errorf("config %s: credentials = %+v, want plaintext", c.ID, c.Auth)
		}
	}
}

// TestListPushCfg_MissingScope_ReturnsEmpty mirrors ListTasks: absence
// of the read scope yields an empty list at HTTP 200, NOT a JSON-RPC
// error.
func TestListPushCfg_MissingScope_ReturnsEmpty(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	res, jerr := disp.listTaskPushNotificationConfigs(notifyNoScopeCtx(t), json.RawMessage(`{"taskId":"T1"}`))
	if jerr != nil {
		t.Fatalf("list: %+v", jerr)
	}
	resp := res.(pushListResponse)
	if len(resp.Configs) != 0 {
		t.Errorf("Configs len = %d, want 0", len(resp.Configs))
	}
}

// TestListPushCfg_EmptyBucket — no records yet ⇒ empty list, no
// error. Important: this is the steady state for a fresh task with
// no push-configs registered.
func TestListPushCfg_EmptyBucket(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	res, jerr := disp.listTaskPushNotificationConfigs(notifyReadCtx(t), json.RawMessage(`{"taskId":"T1"}`))
	if jerr != nil {
		t.Fatalf("list: %+v", jerr)
	}
	resp := res.(pushListResponse)
	if len(resp.Configs) != 0 {
		t.Errorf("Configs len = %d, want 0", len(resp.Configs))
	}
}

// --- Delete -----------------------------------------------------------

// TestDeletePushCfg_HappyPath removes a previously-Set record. A
// subsequent Get must return TaskNotFoundError.
func TestDeletePushCfg_HappyPath(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	seedTaskForPush(t, ctx, d, "T1")
	if _, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "T1", "cfg-1", pushSentinel)); jerr != nil {
		t.Fatalf("seed create: %+v", jerr)
	}

	_, jerr := disp.deleteTaskPushNotificationConfig(ctx, json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"cfg-1"}`))
	if jerr != nil {
		t.Fatalf("delete: %+v", jerr)
	}
	_, jerr = disp.getTaskPushNotificationConfig(notifyReadCtx(t), json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"cfg-1"}`))
	if jerr == nil || jerr.Name != "TaskNotFoundError" {
		t.Errorf("post-delete get: got %+v, want TaskNotFoundError", jerr)
	}
}

// TestDeletePushCfg_Idempotent — deleting a missing key is not an
// error. Lets clients re-issue Delete during retries without
// surfacing spurious failures.
func TestDeletePushCfg_Idempotent(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	// No prior Set.
	_, jerr := disp.deleteTaskPushNotificationConfig(ctx, json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"missing"}`))
	if jerr != nil {
		t.Fatalf("idempotent delete: %+v", jerr)
	}
}

// TestDeletePushCfg_MissingScope — write scope required.
func TestDeletePushCfg_MissingScope(t *testing.T) {
	d, _ := pushDeps(t)
	disp := NewDispatcher(d)
	_, jerr := disp.deleteTaskPushNotificationConfig(notifyNoScopeCtx(t), json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"x"}`))
	if jerr == nil || jerr.Code != jsonrpc.ErrInvalidReq.Code {
		t.Fatalf("got %+v, want ErrInvalidReq", jerr)
	}
}

// --- Cross-cutting ----------------------------------------------------

// TestPush_CredentialsNeverLeak_InLogs runs Create→Get→List→Delete
// while scanning a buffer-backed slog handler. The sentinel must
// never appear in any log line.
func TestPush_CredentialsNeverLeak_InLogs(t *testing.T) {
	d, buf := pushDeps(t)
	disp := NewDispatcher(d)
	ctx := notifyWriteCtx(t)
	seedTaskForPush(t, ctx, d, "T1")

	if _, jerr := disp.createTaskPushNotificationConfig(ctx, validCreateParams(t, "T1", "cfg-1", pushSentinel)); jerr != nil {
		t.Fatalf("create: %+v", jerr)
	}
	if _, jerr := disp.getTaskPushNotificationConfig(notifyReadCtx(t), json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"cfg-1"}`)); jerr != nil {
		t.Fatalf("get: %+v", jerr)
	}
	if _, jerr := disp.listTaskPushNotificationConfigs(notifyReadCtx(t), json.RawMessage(`{"taskId":"T1"}`)); jerr != nil {
		t.Fatalf("list: %+v", jerr)
	}
	if _, jerr := disp.deleteTaskPushNotificationConfig(ctx, json.RawMessage(`{"taskId":"T1","pushNotificationConfigId":"cfg-1"}`)); jerr != nil {
		t.Fatalf("delete: %+v", jerr)
	}

	if strings.Contains(buf.String(), pushSentinel) {
		t.Errorf("log contains plaintext sentinel:\n%s", buf.String())
	}
}
