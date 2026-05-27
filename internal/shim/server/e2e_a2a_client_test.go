// Slice 9 — end-to-end test exercising the shim against an in-process
// NATS hub and a mock v0.4 adapter, driven by the stock a2a-go client
// SDK. Mirrors a real client → shim → mesh path: a2a-go marshals
// JSON-RPC envelopes onto the wire, the shim publishes prompts onto
// NATS, our mock adapter replies via micro/$SRV.INFO, agents.card.*,
// and agents.prompt.* — same surface a real sesh-channels adapter
// would expose.
//
// This is the gate test per the v0.4 master plan: green here means the
// shim binary speaks A2A v1.0 well enough for a stock SDK to drive it.
// Pre-stream gaps (SendMessage StreamResponse wrap, SSE chunk shape,
// PushConfig param shape) surface as subtest failures rather than
// being papered over inside the test — fixing them belongs in
// shim-side methods/, not here.

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nats.go/micro"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/subject"
)

// e2eFixture bundles the wired shim + mock adapter so each subtest can
// inspect the mock's recorded prompts and drive the a2a-go client
// against a known scope.
type e2eFixture struct {
	url    string
	js     jetstream.JetStream
	nc     *nats.Conn
	mock   *mockAdapter
	client *http.Client
}

// newE2EFixture brings up the shim with a non-empty Machine so the
// prompt-publish path is live, then registers the mock adapter on the
// same broker. Each subtest gets a fresh fixture (broker is per-test).
func newE2EFixture(t *testing.T) *e2eFixture {
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

	// The card's GatewayURL must match the actual TLS server URL so the
	// a2a-go client (which dials whatever's in AgentInterface.URL on the
	// fetched card) lands back on the shim. Bring up the HTTP server
	// first, then wire the composer with its URL, then attach the
	// dispatcher via a delayed Config (newServer takes Config by value
	// so the composer must already point at the right URL before we
	// call it).
	tsHandler := http.NewServeMux()
	ts := httptest.NewTLSServer(tsHandler)
	t.Cleanup(ts.Close)

	// L3-bind the composer to the same (machine, project, session) the
	// mock adapter stubs its card/cardx responders on (Slice 3C). The
	// mock derives cardCoord from (agentToken, owner, name) =
	// (testagent, testowner, testagent); keep this literal in lockstep.
	composer := card.NewComposer(nc, subject.Coord{Machine: "testagent", Project: "testowner", Session: "testagent"}, card.L1Defaults{
		GatewayURL:      ts.URL + "/a2a",
		ProtocolVersion: "1.0",
		Capabilities: a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: true,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}, 500*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cacheC := card.NewCache(composer, signer, time.Minute, 16)

	pushKey := make([]byte, 32)
	for i := range pushKey {
		pushKey[i] = byte(i*7 + 1)
	}

	srv := newServer(Config{
		Listen:                "127.0.0.1:0",
		Dev:                   true,
		Auth:                  auth.NoopValidator{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Card:                  cacheC,
		Signer:                signer,
		NC:                    nc,
		JS:                    js2,
		AgentKey:              card.AgentKey{Agent: "testagent", Owner: "testowner", Name: "testagent"},
		ScopeKind:             "project",
		ScopeID:               "e2escope",
		Machine:               "testmachine",
		GatewayURL:            ts.URL,
		Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		PushKey:               pushKey,
		PushDevAllowLocalhost: true,
		PushWorkerDisabled:    true,
	})

	tsHandler.HandleFunc("GET /.well-known/agent-card.json", srv.wrap("/.well-known/agent-card.json", srv.handleAgentCard))
	tsHandler.HandleFunc("GET /.well-known/jwks.json", srv.wrap("/.well-known/jwks.json", srv.handleJWKS))
	tsHandler.HandleFunc("GET /healthz", srv.wrap("/healthz", srv.handleHealthz))
	tsHandler.Handle("POST /a2a", auth.Middleware(srv.cfg.Auth)(srv.wrapHandler("/a2a", http.HandlerFunc(srv.handleA2A))))

	mock := startMockAdapter(t, nc, "testagent", "testowner", "testagent", "project", "e2escope")

	return &e2eFixture{
		url:    ts.URL,
		js:     js2,
		nc:     nc,
		mock:   mock,
		client: newTLSClient(),
	}
}

// mockAdapter is a v0.4-shaped responder bound to one (agent, owner,
// name) tuple. Speaks four subjects:
//   - $SRV.INFO.agents — micro service registration so the shim's
//     composer.discover() finds an adapter for this AgentKey
//   - agents.card.<machine>.<project>.<session> — L3 AgentCard partial
//   - agents.cardx.<machine>.<project>.<session> — L3 extended overlay
//   - agents.prompt.<machine>.<project>.<session>.<role> — captures the
//     inbound prompt so subtests can assert publish happened
//
// The card/cardx subjects use the v0.4 positional mapping
// (Agent→Machine, Owner→Project, Name→Session) so they line up with the
// subject.Coord the shim composer L3-binds to at construction (Slice
// 3C). Captured prompts go into prompts; subtests inspect under mu.
type mockAdapter struct {
	t       *testing.T
	svc     micro.Service
	subs    []*nats.Subscription
	scope   string
	mu      sync.Mutex
	prompts []*nats.Msg
}

func startMockAdapter(t *testing.T, nc *nats.Conn, agentToken, owner, name, scopeKind, scopeID string) *mockAdapter {
	t.Helper()

	svc, err := micro.AddService(nc, micro.Config{
		Name:        "agents",
		Version:     "0.4.0",
		Description: "e2e mock v0.4 adapter",
		Metadata: map[string]string{
			"agent": agentToken,
			"owner": owner,
			"name":  name,
		},
	})
	if err != nil {
		t.Fatalf("micro AddService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	m := &mockAdapter{t: t, svc: svc, scope: scopeID}

	// v0.4 positional mapping: card subjects key on (machine, project,
	// session). The shim composer L3-binds to this same Coord at
	// construction (Slice 3C), derived from (agentToken, owner, name).
	cardCoord := subject.Coord{Machine: agentToken, Project: owner, Session: name}

	cardSubj, err := subject.Card(cardCoord)
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	cardSub, err := nc.Subscribe(cardSubj, func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{
			"description": "e2e mock adapter — slice 9",
			"iconUrl": "https://shim.test/icon.png",
			"skills": [{"id":"echo","name":"Echo","description":"echo skill","tags":["test"]}]
		}`))
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", cardSubj, err)
	}
	t.Cleanup(func() { _ = cardSub.Unsubscribe() })
	m.subs = append(m.subs, cardSub)

	extSubj, err := subject.Cardx(cardCoord)
	if err != nil {
		t.Fatalf("Cardx: %v", err)
	}
	extSub, err := nc.Subscribe(extSubj, func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{
			"description": "e2e mock extended description"
		}`))
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", extSubj, err)
	}
	t.Cleanup(func() { _ = extSub.Unsubscribe() })
	m.subs = append(m.subs, extSub)

	// Prompt subject mirrors what the shim publishes: the role token is
	// AgentKey.Agent (== agentToken in this fixture) and the
	// machine/scope come from server Config; we match the wildcard slot
	// for the role and last segment to keep the subscriber loose.
	promptSubj := "agents.prompt.>"
	promptSub, err := nc.Subscribe(promptSubj, func(msg *nats.Msg) {
		m.mu.Lock()
		m.prompts = append(m.prompts, msg)
		m.mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", promptSubj, err)
	}
	t.Cleanup(func() { _ = promptSub.Unsubscribe() })
	m.subs = append(m.subs, promptSub)

	return m
}

// waitForPrompt polls the captured prompts for up to d, returning the
// first one or failing the test if none arrive. Helper because the
// publish is fire-and-forget — the shim's RPC reply returns before
// NATS hands the message to the subscriber.
func (m *mockAdapter) waitForPrompt(d time.Duration) *nats.Msg {
	m.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		n := len(m.prompts)
		var msg *nats.Msg
		if n > 0 {
			msg = m.prompts[0]
		}
		m.mu.Unlock()
		if msg != nil {
			return msg
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.t.Fatalf("no prompt published within %s", d)
	return nil
}

// newE2EClient constructs an a2a-go client pointing at the shim,
// fetching the public AgentCard first. Uses an InsecureSkipVerify
// transport so httptest.NewTLSServer's self-signed cert works.
func newE2EClient(ctx context.Context, t *testing.T, fix *e2eFixture) *a2aclient.Client {
	t.Helper()

	resolver := agentcard.NewResolver(fix.client)
	agentCard, err := resolver.Resolve(ctx, fix.url)
	if err != nil {
		t.Fatalf("resolve card: %v", err)
	}
	cl, err := a2aclient.NewFromCard(ctx, agentCard, a2aclient.WithJSONRPCTransport(fix.client))
	if err != nil {
		t.Fatalf("new a2a client: %v", err)
	}
	t.Cleanup(func() { _ = cl.Destroy() })
	return cl
}

// TestE2E_A2AClient — Slice 9 gate. Each subtest exercises one A2A RPC
// against the wired shim+mock-adapter pair through the stock a2a-go
// client. Subtests are independent (each is a fresh fixture) — costs
// the broker startup per subtest but isolates failure modes.
//
// Long-tail: don't run under -short since the broker bringup + TLS
// handshake adds ~50ms per subtest. Five subtests × ~50ms = ~250ms,
// well under the 180s CI timeout but explicit so devs running -short
// during fast inner-loop iteration don't pay the cost.
func TestE2E_A2AClient(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test; rerun without -short to exercise stock a2a-go client path")
	}

	t.Run("GetAgentCard", func(t *testing.T) {
		fix := newE2EFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resolver := agentcard.NewResolver(fix.client)
		card, err := resolver.Resolve(ctx, fix.url)
		if err != nil {
			t.Fatalf("resolve card: %v", err)
		}
		if card.Name != "testagent" {
			t.Errorf("card.Name = %q, want testagent", card.Name)
		}
		if len(card.SupportedInterfaces) == 0 {
			t.Errorf("card.SupportedInterfaces empty")
		}
		if !card.Capabilities.Streaming {
			t.Errorf("card.Capabilities.Streaming = false, want true")
		}
	})

	t.Run("SendMessage", func(t *testing.T) {
		fix := newE2EFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cl := newE2EClient(ctx, t, fix)
		req := &a2a.SendMessageRequest{
			Message: &a2a.Message{
				ID:    "msg-send-1",
				Role:  a2a.MessageRoleUser,
				Parts: a2a.ContentParts{a2a.NewTextPart("hello mock")},
			},
			Config: &a2a.SendMessageConfig{ReturnImmediately: true},
		}
		result, err := cl.SendMessage(ctx, req)
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		if result == nil {
			t.Fatal("SendMessage returned nil result")
		}
		task, ok := result.(*a2a.Task)
		if !ok {
			t.Fatalf("SendMessage result type = %T, want *a2a.Task", result)
		}
		if task.ID == "" {
			t.Errorf("task.ID empty")
		}
		// Confirm the shim published the prompt onto NATS for the mock.
		msg := fix.mock.waitForPrompt(time.Second)
		if msg.Subject == "" {
			t.Errorf("prompt subject empty")
		}
	})

	t.Run("SendStreamingMessage", func(t *testing.T) {
		fix := newE2EFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cl := newE2EClient(ctx, t, fix)

		// Mock plays the adapter: when the prompt arrives, look up the
		// freshly minted task (the shim mints taskId on first message;
		// the published prompt is the inbound wire-form, which in this
		// test doesn't carry a taskId — so we discover it by listing
		// the tasks KV). Then drop an agent reply into the messages KV
		// so the SSE bridge picks it up.
		go func() {
			_ = fix.mock.waitForPrompt(2 * time.Second)
			tasksBucket := "sesh_tasks_project_e2escope"
			msgsBucket := "sesh_messages_project_e2escope"
			tasksKV, err := fix.js.KeyValue(ctx, tasksBucket)
			if err != nil {
				return
			}
			// Tasks KV may take a tick to settle after the shim's Create.
			var taskID string
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) && taskID == "" {
				keys, _ := tasksKV.Keys(ctx)
				if len(keys) > 0 {
					taskID = keys[0]
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if taskID == "" {
				return
			}
			msgsKV, err := fix.js.KeyValue(ctx, msgsBucket)
			if err != nil {
				return
			}
			reply := []byte(`{"messageId":"reply-1","taskId":"` + taskID + `","role":"agent","parts":[{"text":"ack","mediaType":"text/plain"}]}`)
			// Key shape: <taskId>.<messageId> per sesh-ops messages.Key.
			_, _ = msgsKV.Put(ctx, taskID+".reply-1", reply)
		}()

		req := &a2a.SendMessageRequest{
			Message: &a2a.Message{
				ID:    "msg-stream-1",
				Role:  a2a.MessageRoleUser,
				Parts: a2a.ContentParts{a2a.NewTextPart("stream me")},
			},
		}
		var got int
		streamCtx, streamCancel := context.WithTimeout(ctx, 2*time.Second)
		defer streamCancel()
		for ev, err := range cl.SendStreamingMessage(streamCtx, req) {
			if err != nil {
				t.Fatalf("SendStreamingMessage: %v (events=%d)", err, got)
			}
			if ev == nil {
				continue
			}
			got++
			if got >= 1 {
				streamCancel()
				break
			}
		}
		if got == 0 {
			t.Errorf("no SSE events received from SendStreamingMessage")
		}
	})

	t.Run("CancelTask", func(t *testing.T) {
		fix := newE2EFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Seed a task directly so we don't depend on SendMessage's
		// success path (which is itself under test).
		kv, err := fix.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_e2escope"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Put(ctx, "task-cancel-1", []byte(`{"id":"task-cancel-1","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
			t.Fatal(err)
		}

		cl := newE2EClient(ctx, t, fix)
		task, err := cl.CancelTask(ctx, &a2a.CancelTaskRequest{ID: "task-cancel-1"})
		if err != nil {
			t.Fatalf("CancelTask: %v", err)
		}
		if task == nil {
			t.Fatal("CancelTask returned nil task")
		}
		if task.Status.State != a2a.TaskStateCanceled {
			t.Errorf("task.Status.State = %q, want %q", task.Status.State, a2a.TaskStateCanceled)
		}
	})

	t.Run("ListTasks", func(t *testing.T) {
		fix := newE2EFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		kv, err := fix.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_e2escope"})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"list-a", "list-b", "list-c"} {
			payload := []byte(`{"id":"` + id + `","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)
			if _, err := kv.Put(ctx, id, payload); err != nil {
				t.Fatal(err)
			}
		}

		cl := newE2EClient(ctx, t, fix)
		resp, err := cl.ListTasks(ctx, &a2a.ListTasksRequest{})
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if resp == nil {
			t.Fatal("ListTasks returned nil response")
		}
		if len(resp.Tasks) < 3 {
			t.Errorf("ListTasks returned %d tasks, want >= 3", len(resp.Tasks))
		}
	})

	t.Run("CreateTaskPushConfig", func(t *testing.T) {
		fix := newE2EFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Seed a task so the push handler's existence check passes.
		kv, err := fix.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "sesh_tasks_project_e2escope"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Put(ctx, "task-push-1", []byte(`{"id":"task-push-1","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
			t.Fatal(err)
		}

		// Stand up a webhook receiver — push deliveries hit it only
		// when the worker is enabled; we only assert SetPushConfig
		// returns success here (CRUD round-trip, not delivery).
		var hits atomic.Int32
		webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(webhook.Close)

		cl := newE2EClient(ctx, t, fix)
		cfg, err := cl.CreateTaskPushConfig(ctx, &a2a.PushConfig{
			TaskID: "task-push-1",
			ID:     "cfg-1",
			URL:    webhook.URL,
			Auth:   &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "sentinel-token"},
		})
		if err != nil {
			t.Fatalf("CreateTaskPushConfig: %v", err)
		}
		if cfg == nil {
			t.Fatal("CreateTaskPushConfig returned nil config")
		}
		if cfg.URL != webhook.URL {
			t.Errorf("cfg.URL = %q, want %q", cfg.URL, webhook.URL)
		}
	})
}
