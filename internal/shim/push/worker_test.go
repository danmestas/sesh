package push

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/notifications"
	"github.com/danmestas/sesh-ops/scope"
)

// compressedRetrySchedule returns a ms-scale schedule for tests so
// the worker's real-time sleeps stay under ~500ms total wall clock.
// Per-Worker config (not package var) — parallel-safe by construction.
func compressedRetrySchedule() []time.Duration {
	return []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
}

// startBroker spins up an in-process nats-server with JetStream.
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
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatalf("nats not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// workerTestStack wires the worker against a fresh broker. Returns
// the bits tests need to drive (nc, js, KV handles, log buffer).
type workerTestStack struct {
	nc            *nats.Conn
	js            jetstream.JetStream
	tasksKV       jetstream.KeyValue
	notifyKV      jetstream.KeyValue
	failKV        jetstream.KeyValue
	scopeKind     string
	scopeID       string
	pushKey       []byte
	log           *slog.Logger
	logBuf        *bytes.Buffer
	retrySchedule []time.Duration // nil ⇒ NewWorker uses DefaultRetrySchedule
}

func newWorkerTestStack(t *testing.T) *workerTestStack {
	t.Helper()
	url := startBroker(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	stack := &workerTestStack{
		nc:        nc,
		js:        js,
		scopeKind: "project",
		scopeID:   "abc123",
	}
	pk, err := NewDevKey()
	if err != nil {
		t.Fatal(err)
	}
	stack.pushKey = pk
	stack.logBuf = new(bytes.Buffer)
	stack.log = slog.New(slog.NewJSONHandler(stack.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, b := range []string{"tasks", "notifycfg", "notifyfail"} {
		name, err := scope.Bucket(stack.scopeKind, stack.scopeID, b)
		if err != nil {
			t.Fatalf("bucket %s: %v", b, err)
		}
		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: name})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		switch b {
		case "tasks":
			stack.tasksKV = kv
		case "notifycfg":
			stack.notifyKV = kv
		case "notifyfail":
			stack.failKV = kv
		}
	}
	return stack
}

// seedTask writes a task with the given state. Always goes through
// Put (not Create) so subsequent state-change updates can be made
// with the same call shape.
func (s *workerTestStack) seedTask(t *testing.T, ctx context.Context, taskID, state string) {
	t.Helper()
	// contextId is required by the a2a-go canonical TaskStatusUpdateEvent
	// wire shape that buildStatusEvent emits. Default to a deterministic
	// per-task value so tests don't need to plumb it explicitly.
	ctxID := "ctx-" + taskID
	raw := []byte(`{"id":"` + taskID + `","kind":"task","contextId":"` + ctxID + `","status":{"state":"` + state + `"}}`)
	if _, err := s.tasksKV.Put(ctx, taskID, raw); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// seedConfig registers a NotifyConfig with the given URL + plaintext
// credentials. Goes through EncryptCredentials so the worker exercises
// the real decryption path.
func (s *workerTestStack) seedConfig(t *testing.T, ctx context.Context, taskID, configID, webhookURL, scheme, plain string) {
	t.Helper()
	enc, err := EncryptCredentials(plain, s.pushKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	cfg := notifications.NotifyConfig{
		TaskID: taskID,
		ID:     configID,
		URL:    webhookURL,
	}
	if scheme != "" {
		cfg.Auth = &notifications.NotifyAuth{Scheme: scheme, Credentials: enc}
	}
	if err := notifications.Set(ctx, s.notifyKV, taskID, configID, cfg); err != nil {
		t.Fatalf("seed cfg: %v", err)
	}
}

// startWorker boots a worker in a goroutine. Returns the cancel fn
// and a Wait callback so callers can cleanly shut down.
func (s *workerTestStack) startWorker(t *testing.T, ctx context.Context, maxRetries int, resolver Resolver, devAllowLocalhost bool) (*Worker, context.CancelFunc) {
	t.Helper()
	wCtx, cancel := context.WithCancel(ctx)
	w := NewWorker(WorkerConfig{
		NC:                s.nc,
		JS:                s.js,
		ScopeKind:         s.scopeKind,
		ScopeID:           s.scopeID,
		PushKey:           s.pushKey,
		Log:               s.log,
		MaxRetries:        maxRetries,
		RetrySchedule:     s.retrySchedule,
		Resolver:          resolver,
		DevAllowLocalhost: devAllowLocalhost,
	})
	go func() { _ = w.Run(wCtx) }()
	// Give the Watcher a moment to subscribe before the test seeds.
	time.Sleep(50 * time.Millisecond)
	return w, cancel
}

// captureWebhook starts an httptest server that records every
// inbound request body + headers. Returns the server (caller closes)
// and a getter for the recorded requests.
type webhookRecord struct {
	body    []byte
	headers http.Header
}

func captureWebhook(t *testing.T, status int) (*httptest.Server, func() []webhookRecord) {
	t.Helper()
	var mu sync.Mutex
	var got []webhookRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, webhookRecord{body: b, headers: r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []webhookRecord {
		mu.Lock()
		defer mu.Unlock()
		out := make([]webhookRecord, len(got))
		copy(out, got)
		return out
	}
}

// localhostResolver maps any host to 127.0.0.1 so the SSRF guard
// accepts the httptest URL (which uses 127.0.0.1) and we don't have
// to special-case the production resolver path.
func localhostResolver(host string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("127.0.0.1")}, nil
}

// extractHostPort returns the (host, port) of an httptest server URL.
func extractHostPort(t *testing.T, srvURL string) (string, string) {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// eventually polls f every 20ms up to timeout. Returns once f returns
// true; t.Fatal-s on timeout.
func eventually(t *testing.T, timeout time.Duration, msg string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("eventually: %s", msg)
}

// --- tests ----------------------------------------------------------

// TestWorker_DeliversOnTaskStatusChange — happy path: seed task + cfg,
// flip task to WORKING, webhook receives one POST with the expected
// payload shape.
func TestWorker_DeliversOnTaskStatusChange(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hook, recorded := captureWebhook(t, http.StatusNoContent)
	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	s.seedConfig(t, ctx, "T1", "cfg-1", hook.URL+"/hook", "Bearer", "")

	_, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)
	defer wCancel()

	// Trigger an update.
	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	eventually(t, 2*time.Second, "webhook delivery", func() bool { return len(recorded()) >= 1 })

	got := recorded()[0]
	// A2A canonical StreamResponse wraps in {"statusUpdate": {...}}.
	if !bytes.Contains(got.body, []byte(`"statusUpdate":`)) {
		t.Errorf("body missing statusUpdate envelope: %s", got.body)
	}
	if !bytes.Contains(got.body, []byte(`"taskId":"T1"`)) {
		t.Errorf("body missing taskId: %s", got.body)
	}
	if !bytes.Contains(got.body, []byte(`"contextId":"ctx-T1"`)) {
		t.Errorf("body missing contextId: %s", got.body)
	}
	if got.headers.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", got.headers.Get("Content-Type"))
	}
}

// TestWorker_RetriesOnNon2xx — webhook returns 500 every time; worker
// records `notifyfail` entries for each attempt. Total attempts =
// 1 + len(s.retrySchedule) under the compressed schedule.
func TestWorker_RetriesOnNon2xx(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	s.seedConfig(t, ctx, "T1", "cfg-1", srv.URL+"/hook", "", "")
	_, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)
	defer wCancel()

	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	want := int32(1 + len(s.retrySchedule)) // 5
	eventually(t, 3*time.Second, "all retries exhausted", func() bool {
		return atomic.LoadInt32(&hits) >= want
	})

	// notifyfail must have 5 entries.
	eventually(t, 1*time.Second, "5 fail entries", func() bool {
		keys, err := s.failKV.Keys(ctx)
		if err != nil {
			return false
		}
		count := 0
		for _, k := range keys {
			if strings.HasPrefix(k, "T1.cfg-1.") {
				count++
			}
		}
		return count == int(want)
	})
}

// TestWorker_StopsRetryingOn2xx — first 2 attempts 500, then 200.
// Total attempts = 3, fail entries = 2.
func TestWorker_StopsRetryingOn2xx(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	s.seedConfig(t, ctx, "T1", "cfg-1", srv.URL+"/hook", "", "")
	_, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)
	defer wCancel()

	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	eventually(t, 2*time.Second, "third attempt succeeds", func() bool {
		return atomic.LoadInt32(&hits) >= 3
	})
	// Brief settle so the count stabilizes.
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hits = %d, want 3", got)
	}

	// Two failure entries (one per non-2xx).
	keys, err := s.failKV.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, k := range keys {
		if strings.HasPrefix(k, "T1.cfg-1.") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("fail entries = %d, want 2", count)
	}
}

// TestWorker_DecryptsAndSendsBearer confirms the Authorization header
// carries `<Scheme> <decrypted-plaintext>`.
func TestWorker_DecryptsAndSendsBearer(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hook, recorded := captureWebhook(t, http.StatusOK)
	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	s.seedConfig(t, ctx, "T1", "cfg-1", hook.URL+"/hook", "Bearer", "decrypted-secret-xyz")
	_, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)
	defer wCancel()

	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	eventually(t, 2*time.Second, "delivery", func() bool { return len(recorded()) >= 1 })
	got := recorded()[0]
	if got.headers.Get("Authorization") != "Bearer decrypted-secret-xyz" {
		t.Errorf("Authorization = %q, want Bearer decrypted-secret-xyz", got.headers.Get("Authorization"))
	}
}

// TestWorker_BlocksDNSRebinding — resolver returns a private IP; SSRF
// guard rejects at delivery time and records a single fail entry.
func TestWorker_BlocksDNSRebinding(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hook URL host that isn't "127.0.0.1" so the SSRF guard goes
	// through the resolver branch.
	hook, _ := captureWebhook(t, http.StatusOK)
	_, port := extractHostPort(t, hook.URL)
	rogueURL := "https://rogue.example.com:" + port + "/hook"

	rebindResolver := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.5")}, nil
	}
	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	s.seedConfig(t, ctx, "T1", "cfg-1", rogueURL, "", "")
	_, wCancel := s.startWorker(t, ctx, 0, rebindResolver, false)
	defer wCancel()

	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	eventually(t, 2*time.Second, "ssrf fail recorded", func() bool {
		keys, err := s.failKV.Keys(ctx)
		if err != nil {
			return false
		}
		for _, k := range keys {
			if strings.HasPrefix(k, "T1.cfg-1.") {
				return true
			}
		}
		return false
	})
}

// TestWorker_HonorsContextCancel — cancel during a retry sleep
// returns from the goroutine promptly (≤ 200ms) rather than running
// the whole schedule out.
func TestWorker_HonorsContextCancel(t *testing.T) {
	s := newWorkerTestStack(t)
	// Use a LONG retry schedule so we can demonstrate the cancel
	// interrupts the sleep.
	s.retrySchedule = []time.Duration{500 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	s.seedConfig(t, ctx, "T1", "cfg-1", srv.URL+"/hook", "", "")
	w, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)

	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	// Let one delivery hit so we're in the retry sleep.
	time.Sleep(100 * time.Millisecond)

	cancelStart := time.Now()
	wCancel()
	w.Wait()
	elapsed := time.Since(cancelStart)
	if elapsed > 300*time.Millisecond {
		t.Errorf("Wait returned in %v; expected ≤ 300ms (cancel during sleep should interrupt)", elapsed)
	}
}

// TestWorker_NoCredentialsInLogs scans the worker's slog buffer over a
// full delivery (incl. one retry) and confirms the plaintext sentinel
// never appears. Sister test to the methods/pushconfig leak scan.
func TestWorker_NoCredentialsInLogs(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sentinel := "PLAINTEXT_SENTINEL_42"
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	s.seedConfig(t, ctx, "T1", "cfg-1", srv.URL+"/hook", "Bearer", sentinel)
	_, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)
	defer wCancel()

	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	eventually(t, 2*time.Second, "second attempt succeeds", func() bool {
		return atomic.LoadInt32(&hits) >= 2
	})
	if strings.Contains(s.logBuf.String(), sentinel) {
		t.Errorf("worker log contains plaintext sentinel:\n%s", s.logBuf.String())
	}
}

// TestWorker_LegacyPlaintextCredentialsStillDelivers — a config Set
// with a raw (no enc: prefix) credential still POSTs the value via
// Authorization, and the worker WARNs once.
func TestWorker_LegacyPlaintextCredentialsStillDelivers(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hook, recorded := captureWebhook(t, http.StatusOK)

	// Bypass seedConfig (which encrypts). Hand-build the legacy record.
	legacy := notifications.NotifyConfig{
		TaskID: "T1",
		ID:     "cfg-old",
		URL:    hook.URL + "/h",
		Auth:   &notifications.NotifyAuth{Scheme: "Bearer", Credentials: "legacy-secret"},
	}
	if err := notifications.Set(ctx, s.notifyKV, "T1", "cfg-old", legacy); err != nil {
		t.Fatal(err)
	}
	s.seedTask(t, ctx, "T1", "TASK_STATE_SUBMITTED")
	_, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)
	defer wCancel()

	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING")
	eventually(t, 2*time.Second, "delivery", func() bool { return len(recorded()) >= 1 })
	got := recorded()[0]
	if got.headers.Get("Authorization") != "Bearer legacy-secret" {
		t.Errorf("Authorization = %q, want Bearer legacy-secret", got.headers.Get("Authorization"))
	}
	if !strings.Contains(s.logBuf.String(), "legacy plaintext") {
		t.Errorf("expected WARN for legacy plaintext, got: %s", s.logBuf.String())
	}
}

// TestWorker_DoesNotPushHistoricalEntries — the UpdatesOnly semantics
// mean a worker started AFTER an initial task Put doesn't re-deliver
// the historical state.
func TestWorker_DoesNotPushHistoricalEntries(t *testing.T) {
	s := newWorkerTestStack(t)
	s.retrySchedule = compressedRetrySchedule()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hook, recorded := captureWebhook(t, http.StatusOK)
	s.seedTask(t, ctx, "T1", "TASK_STATE_WORKING") // historical
	s.seedConfig(t, ctx, "T1", "cfg-1", hook.URL+"/h", "", "")

	_, wCancel := s.startWorker(t, ctx, 0, localhostResolver, true)
	defer wCancel()

	// Wait a moment to confirm no spurious delivery from the historical
	// entry.
	time.Sleep(300 * time.Millisecond)
	if n := len(recorded()); n != 0 {
		t.Errorf("expected 0 historical deliveries, got %d", n)
	}

	// Now make a fresh update; that one MUST deliver.
	s.seedTask(t, ctx, "T1", "TASK_STATE_COMPLETED")
	eventually(t, 2*time.Second, "live delivery", func() bool { return len(recorded()) >= 1 })
}

// TestBuildStatusEvent_ShapeMatchesA2A — canonical a2a-go StreamResponse
// shape: {"statusUpdate": {"contextId", "taskId", "status"}}. No `kind`
// discriminator, no `finalState` sibling — strict a2a-go receivers
// fail-closed on unknown keys.
func TestBuildStatusEvent_ShapeMatchesA2A(t *testing.T) {
	taskRaw := []byte(`{"id":"T1","kind":"task","contextId":"ctx-T1","status":{"state":"TASK_STATE_COMPLETED"}}`)
	out, err := buildStatusEvent("T1", taskRaw, "TASK_STATE_COMPLETED")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := env["kind"]; ok {
		t.Errorf("envelope must NOT carry top-level `kind`: %v", env)
	}
	inner, ok := env["statusUpdate"].(map[string]any)
	if !ok {
		t.Fatalf("missing statusUpdate envelope: %v", env)
	}
	if _, ok := inner["finalState"]; ok {
		t.Errorf("inner must NOT carry `finalState`: %v", inner)
	}
	if inner["taskId"] != "T1" {
		t.Errorf("taskId = %v", inner["taskId"])
	}
	if inner["contextId"] != "ctx-T1" {
		t.Errorf("contextId = %v", inner["contextId"])
	}
	status, ok := inner["status"].(map[string]any)
	if !ok {
		t.Fatalf("status not an object: %v", inner["status"])
	}
	if status["state"] != "TASK_STATE_COMPLETED" {
		t.Errorf("status.state = %v", status["state"])
	}
}

// TestBuildStatusEvent_RejectsMissingContextID — the a2a-go spec marks
// contextId as required; we fail-loud rather than emit an empty string.
func TestBuildStatusEvent_RejectsMissingContextID(t *testing.T) {
	taskRaw := []byte(`{"id":"T1","kind":"task","status":{"state":"TASK_STATE_COMPLETED"}}`)
	if _, err := buildStatusEvent("T1", taskRaw, "TASK_STATE_COMPLETED"); err == nil {
		t.Fatal("expected error for missing contextId")
	}
}
