package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/subject"
)

// startBroker spins up an in-memory nats-server with JetStream enabled on
// a random port and returns its client URL. Mirrors the helper in
// internal/refagent/agent_test.go.
func startBroker(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("nats server new: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatalf("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

func testConn(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// newTestServer wires the full shim stack against an embedded NATS broker
// and exposes it via httptest.NewTLSServer (which provides its own TLS).
// Caller gets back the URL, the *card.Cache (for asserting on signed
// bytes), the *card.Signer (for verifying signatures), and the
// jetstream.JetStream v2 client (for seeding KV).
func newTestServer(t *testing.T) (string, *card.Cache, *card.Signer, jetstream.JetStream, *nats.Conn) {
	t.Helper()

	url := startBroker(t)
	nc := testConn(t, url)
	js2, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream v2: %v", err)
	}

	signer, err := card.NewDevSigner()
	if err != nil {
		t.Fatalf("dev signer: %v", err)
	}
	composer := card.NewComposer(nc, card.L1Defaults{
		GatewayURL:         "https://shim.test",
		ProtocolVersion:    "1.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}, 200*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache := card.NewCache(composer, signer, time.Minute, 16)

	srv := newServer(Config{
		Listen:    "127.0.0.1:0",
		Dev:       true,
		Auth:      auth.NoopValidator{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Card:      cache,
		Signer:    signer,
		NC:        nc,
		JS:        js2,
		AgentKey:  card.AgentKey{Agent: "test-agent", Owner: "test-owner", Name: "test-agent"},
		ScopeKind: "project",
		ScopeID:   "abc123",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", srv.wrap("/.well-known/agent-card.json", srv.handleAgentCard))
	mux.HandleFunc("GET /.well-known/jwks.json", srv.wrap("/.well-known/jwks.json", srv.handleJWKS))
	mux.HandleFunc("GET /healthz", srv.wrap("/healthz", srv.handleHealthz))
	mux.HandleFunc("GET /readyz", srv.wrap("/readyz", srv.handleReadyz))
	mux.HandleFunc("GET /metrics", srv.wrap("/metrics", srv.handleMetrics))
	mux.Handle("POST /a2a", auth.Middleware(srv.cfg.Auth)(srv.wrapHandler("/a2a", http.HandlerFunc(srv.handleA2A))))

	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, cache, signer, js2, nc
}

func newTLSClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}
}

func TestHealthz(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	resp, err := newTLSClient().Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v", body["status"])
	}
	if _, ok := body["nats"]; !ok {
		t.Errorf("missing nats field")
	}
	if body["signing_key"] != true {
		t.Errorf("signing_key = %v", body["signing_key"])
	}
}

func TestReadyz(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	resp, err := newTLSClient().Get(url + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestAgentCard_Signed(t *testing.T) {
	url, _, signer, _, _ := newTestServer(t)
	resp, err := newTLSClient().Get(url + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	sigsAny, ok := raw["signatures"].([]any)
	if !ok || len(sigsAny) != 1 {
		t.Fatalf("expected 1 signature, got %+v", raw["signatures"])
	}
	sig := sigsAny[0].(map[string]any)
	prot := sig["protected"].(string)
	sigStr := sig["signature"].(string)

	delete(raw, "signatures")
	rebuilt, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jcsForTest(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.VerifyDetached(canonical, prot, sigStr); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestJWKS(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	resp, err := newTLSClient().Get(url + "/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Keys) == 0 {
		t.Fatal("no keys")
	}
}

func TestMetrics_ContainsCounters(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	// Drive at least one HTTP request so the counter map is non-empty.
	_, _ = newTLSClient().Get(url + "/healthz")
	resp, err := newTLSClient().Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		"sesh_shim_http_requests_total",
		"sesh_shim_jsonrpc_errors_total",
		"sesh_shim_card_cache_hits_total",
		"sesh_shim_card_cache_misses_total",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q\n%s", want, text)
		}
	}
}

func postJSONRPC(t *testing.T, url string, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/a2a", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := newTLSClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestA2A_GetTask_Happy(t *testing.T) {
	url, _, _, js, _ := newTestServer(t)
	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_abc123"})
	if err != nil {
		t.Fatal(err)
	}
	taskJSON := []byte(`{"id":"t-1","kind":"task","status":{"state":"submitted"}}`)
	if _, err := kv.Put(ctx, "t-1", taskJSON); err != nil {
		t.Fatal(err)
	}
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":7,"method":"GetTask","params":{"id":"t-1"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int    `json:"code"`
			Name string `json:"name"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Error != nil {
		t.Fatalf("got error: %+v", env.Error)
	}
	if !bytes.Equal(bytes.TrimSpace([]byte(env.Result)), taskJSON) {
		t.Fatalf("result mismatch:\n got=%s\nwant=%s", env.Result, taskJSON)
	}
}

func TestA2A_GetTask_NotFound(t *testing.T) {
	url, _, _, js, _ := newTestServer(t)
	// Create bucket but no key so we hit ErrKeyNotFound.
	if _, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_abc123"}); err != nil {
		t.Fatal(err)
	}
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"missing"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertRPCError(t, body, -32001, "TaskNotFoundError")
}

func TestA2A_GetTask_BucketMissing(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"missing"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertRPCError(t, body, -32001, "TaskNotFoundError")
}

func TestA2A_GetExtendedAgentCard(t *testing.T) {
	// Slice 1 behavior: no adapter responder on agents.card.extended.*,
	// so the handler's FetchExtended times out within the composer's
	// 200ms queryWindow and -32007 surfaces. Slice 5 preserves this
	// path for the "no adapter" steady state.
	url, _, _, _, _ := newTestServer(t)
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"GetExtendedAgentCard"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertRPCError(t, body, -32007, "AuthenticatedExtendedCardNotConfiguredError")
}

// TestA2A_GetExtendedAgentCard_HappyPath wires an adapter stub that
// responds on agents.card.extended.* and asserts the shim returns a
// signed card whose description was overlaid from the L3 partial.
func TestA2A_GetExtendedAgentCard_HappyPath(t *testing.T) {
	url, _, _, _, nc := newTestServer(t)
	// AgentKey matches the one wired in newTestServer.
	subj, err := subject.CardExtended("test-agent", "test-owner", "test-agent")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		_ = m.Respond([]byte(`{
			"description": "extended-only desc",
			"skills": [{"id":"adv","name":"Adv","description":"d","tags":["t"]}]
		}`))
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subj, err)
	}
	defer sub.Unsubscribe()

	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":2,"method":"GetExtendedAgentCard"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int    `json:"code"`
			Name string `json:"name"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, body)
	}
	if env.Error != nil {
		t.Fatalf("expected result, got error: %+v body=%s", env.Error, body)
	}
	// `result` is a json.RawMessage encoded as a JSON-quoted string of
	// the signed card bytes (since the marshaler stringifies
	// RawMessage). The shim returns the RawMessage directly, which
	// the JSON-RPC envelope serializes as the inner JSON object.
	var card map[string]any
	if err := json.Unmarshal(env.Result, &card); err != nil {
		t.Fatalf("decode card: %v result=%s", err, env.Result)
	}
	if card["description"] != "extended-only desc" {
		t.Errorf("description = %v, want extended-only desc", card["description"])
	}
	if _, ok := card["signatures"]; !ok {
		t.Errorf("signed card missing signatures field")
	}
}

// TestA2A_GetExtendedAgentCard_NoAdapter_Returns32007 explicitly
// covers the "principal authorized but adapter silent" branch and
// asserts the response is the same -32007 the unauthorized case
// produces — clients should not be able to probe "this exists but
// I can't see it" vs "this doesn't exist".
func TestA2A_GetExtendedAgentCard_NoAdapter_Returns32007(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":3,"method":"GetExtendedAgentCard"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertRPCError(t, body, -32007, "AuthenticatedExtendedCardNotConfiguredError")
}

// TestA2A_AgentCard_IncludesL3_OnFreshFetch confirms the public
// /.well-known/agent-card.json endpoint pulls L3 contributions from
// the adapter via agents.card.get.*. Uses a unique scope-id to avoid
// cross-test cache contamination through the embedded broker
// (newTestServer creates a fresh broker per test, so a fresh cache).
func TestA2A_AgentCard_IncludesL3_OnFreshFetch(t *testing.T) {
	url, _, _, _, nc := newTestServer(t)
	subj, err := subject.CardGet("test-agent", "test-owner", "test-agent")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		_ = m.Respond([]byte(`{
			"description": "public L3 description",
			"iconUrl": "https://x/icon.png"
		}`))
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subj, err)
	}
	defer sub.Unsubscribe()

	resp, err := newTLSClient().Get(url + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode card: %v body=%s", err, body)
	}
	if raw["description"] != "public L3 description" {
		t.Errorf("description = %v, want L3 value", raw["description"])
	}
	if raw["iconUrl"] != "https://x/icon.png" {
		t.Errorf("iconUrl = %v, want L3 value", raw["iconUrl"])
	}
}

func TestA2A_UnknownMethod(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"FooBar"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertRPCError(t, body, -32601, "Method not found")
}

func TestA2A_MalformedJSON(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	status, body := postJSONRPC(t, url, `{not json`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertRPCError(t, body, -32700, "Parse error")
}

func TestA2A_CancelTask_Happy(t *testing.T) {
	url, _, _, js, _ := newTestServer(t)
	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx, "t-c", []byte(`{"id":"t-c","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
		t.Fatal(err)
	}
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{"id":"t-c"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int    `json:"code"`
			Name string `json:"name"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Error != nil {
		t.Fatalf("got error: %+v", env.Error)
	}
	var probe struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(env.Result, &probe); err != nil {
		t.Fatalf("decode result: %v body=%s", err, env.Result)
	}
	if probe.Status.State != "TASK_STATE_CANCELED" {
		t.Errorf("state = %q, want TASK_STATE_CANCELED", probe.Status.State)
	}
}

func TestA2A_CancelTask_Terminal(t *testing.T) {
	url, _, _, js, _ := newTestServer(t)
	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx, "t-done", []byte(`{"id":"t-done","kind":"task","status":{"state":"TASK_STATE_COMPLETED"}}`)); err != nil {
		t.Fatal(err)
	}
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{"id":"t-done"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	assertRPCError(t, body, -32002, "TaskNotCancelableError")
}

func TestA2A_CancelTask_NotFound(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{"id":"ghost"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertRPCError(t, body, -32001, "TaskNotFoundError")
}

func TestA2A_ListTasks_Empty(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	var env struct {
		Result struct {
			Tasks         []json.RawMessage `json:"tasks"`
			TotalSize     int               `json:"totalSize"`
			NextPageToken string            `json:"nextPageToken"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Error != nil {
		t.Fatalf("got error: %v", env.Error)
	}
	if len(env.Result.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(env.Result.Tasks))
	}
}

func TestA2A_ListTasks_Populated(t *testing.T) {
	url, _, _, js, _ := newTestServer(t)
	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_abc123"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		payload := []byte(`{"id":"` + id + `","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)
		if _, err := kv.Put(ctx, id, payload); err != nil {
			t.Fatal(err)
		}
	}
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	var env struct {
		Result struct {
			Tasks     []json.RawMessage `json:"tasks"`
			TotalSize int               `json:"totalSize"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Error != nil {
		t.Fatalf("got error: %v", env.Error)
	}
	if len(env.Result.Tasks) != 3 {
		t.Errorf("Tasks len = %d, want 3", len(env.Result.Tasks))
	}
	if env.Result.TotalSize != 3 {
		t.Errorf("TotalSize = %d, want 3", env.Result.TotalSize)
	}
}

// TestA2A_ListTasks_NoReadScope exercises the binary scope-gate using
// a Validator that hands out a Principal with NO scopes. The auth
// middleware still attaches it, listTasks then returns an empty list
// (HTTP 200, no JSON-RPC error).
func TestA2A_ListTasks_NoReadScope(t *testing.T) {
	url := newTestServerWithAuth(t, noScopeValidator{})
	ctx := context.Background()
	_ = ctx
	status, body := postJSONRPC(t, url, `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	var env struct {
		Result struct {
			Tasks []json.RawMessage `json:"tasks"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Error != nil {
		t.Fatalf("got error: %v", env.Error)
	}
	if len(env.Result.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0 (no agent.read scope)", len(env.Result.Tasks))
	}
}

// noScopeValidator returns a Principal with no scopes, so the scope-gate
// in listTasks must trip even though the HTTP request is "authenticated".
type noScopeValidator struct{}

func (noScopeValidator) Validate(r *http.Request) (auth.Principal, error) {
	return auth.Principal{Sub: "no-scope"}, nil
}

// newTestServerWithAuth duplicates newTestServer's wiring but lets the
// caller install a non-default Validator (used by the no-scope ListTasks
// case where NoopValidator's hard-coded scopes would defeat the check).
func newTestServerWithAuth(t *testing.T, v auth.Validator) string {
	t.Helper()

	url := startBroker(t)
	nc := testConn(t, url)
	js2, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream v2: %v", err)
	}
	// Seed a task so the scope-gate, not "empty bucket", is what trips.
	bucket := "sesh_tasks_project_abc123"
	kv, err := js2.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: bucket})
	if err != nil {
		t.Fatalf("create kv: %v", err)
	}
	if _, err := kv.Put(context.Background(), "t-1", []byte(`{"id":"t-1","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
		t.Fatalf("put: %v", err)
	}

	signer, err := card.NewDevSigner()
	if err != nil {
		t.Fatalf("dev signer: %v", err)
	}
	composer := card.NewComposer(nc, card.L1Defaults{
		GatewayURL:         "https://shim.test",
		ProtocolVersion:    "1.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}, 200*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache := card.NewCache(composer, signer, time.Minute, 16)

	srv := newServer(Config{
		Listen:    "127.0.0.1:0",
		Dev:       true,
		Auth:      v,
		Card:      cache,
		Signer:    signer,
		NC:        nc,
		JS:        js2,
		AgentKey:  card.AgentKey{Agent: "test-agent", Owner: "test-owner", Name: "test-agent"},
		ScopeKind: "project",
		ScopeID:   "abc123",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	mux := http.NewServeMux()
	mux.Handle("POST /a2a", auth.Middleware(srv.cfg.Auth)(srv.wrapHandler("/a2a", http.HandlerFunc(srv.handleA2A))))
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestA2A_BodyTooLarge_Returns413(t *testing.T) {
	url, _, _, _, _ := newTestServer(t)
	// 1 MiB cap + a bit; the cap is enforced inside handleA2A via
	// http.MaxBytesReader and produces HTTP 413 (not a JSON-RPC envelope).
	body := strings.Repeat("a", (1<<20)+100)
	status, _ := postJSONRPC(t, url, body)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
}

func TestRun_ShutdownOnContext(t *testing.T) {
	cfg := baseRunCfg(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, cfg) }()
	// Give the listener a moment to bind.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func baseRunCfg(t *testing.T) Config {
	t.Helper()
	url := startBroker(t)
	nc := testConn(t, url)
	js2, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := card.NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	composer := card.NewComposer(nc, card.L1Defaults{
		GatewayURL:         "https://shim.test",
		ProtocolVersion:    "1.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}, 200*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache := card.NewCache(composer, signer, time.Minute, 16)
	return Config{
		Listen:        "127.0.0.1:0",
		Dev:           true,
		Auth:          auth.NoopValidator{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Card:          cache,
		Signer:        signer,
		NC:            nc,
		JS:            js2,
		AgentKey:      card.AgentKey{Agent: "test", Owner: "test", Name: "test"},
		ScopeKind:     "project",
		ScopeID:       "abc",
		ShutdownGrace: 500 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func assertRPCError(t *testing.T, body []byte, wantCode int, wantName string) {
	t.Helper()
	var env struct {
		Error *struct {
			Code int    `json:"code"`
			Name string `json:"name"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Error == nil {
		t.Fatalf("expected error object, got body=%s", body)
	}
	if env.Error.Code != wantCode || env.Error.Name != wantName {
		t.Fatalf("got code=%d name=%q, want code=%d name=%q",
			env.Error.Code, env.Error.Name, wantCode, wantName)
	}
}

// jcsForTest is a stand-in canonicalizer for the server_test's signature
// verification. We need an RFC 8785 JCS pass equivalent to the one in
// card/canonicalize.go, but that function is unexported. Rather than
// punching a hole in the card API for tests, do a tight re-implementation
// here that handles the subset of JSON the AgentCard emits (objects with
// string keys, ASCII strings, no NaN/Infinity).
func jcsForTest(b []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jcsWrite(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jcsWrite(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return jcsString(buf, x)
	case json.Number:
		buf.WriteString(x.String())
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := jcsWrite(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		buf.WriteByte('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// JCS sorts by UTF-16 code unit; for ASCII keys (our case) plain
		// lexicographic byte sort is identical.
		sortStrings(keys)
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := jcsString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := jcsWrite(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcsForTest: unsupported %T", v)
	}
	return nil
}

func jcsString(buf *bytes.Buffer, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
