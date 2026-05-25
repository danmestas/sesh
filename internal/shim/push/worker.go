package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/notifications"
	"github.com/danmestas/sesh-ops/scope"
)

// DefaultRetrySchedule is the per-attempt sleep BEFORE the next attempt
// (so the first attempt happens immediately and a non-2xx waits
// DefaultRetrySchedule[0] before retry 1). Default = 4 retries at
// 1s / 4s / 16s / 64s = total 85s window. Per-worker override via
// WorkerConfig.RetrySchedule.
var DefaultRetrySchedule = []time.Duration{
	1 * time.Second,
	4 * time.Second,
	16 * time.Second,
	64 * time.Second,
}

// WorkerConfig wires the delivery worker to its NATS/KV stack and
// its HTTP target surface. All fields except MaxRetries are required;
// MaxRetries ≤ 0 means "use len(RetrySchedule)".
type WorkerConfig struct {
	NC         *nats.Conn
	JS         jetstream.JetStream
	ScopeKind  string
	ScopeID    string
	PushKey    []byte
	HTTPClient *http.Client
	Log        *slog.Logger
	// MaxRetries caps the per-attempt retry loop. Total attempts =
	// 1 + min(MaxRetries, len(RetrySchedule)). Defaults to
	// len(RetrySchedule) when ≤ 0.
	MaxRetries int

	// RetrySchedule is the per-attempt sleep schedule. Nil ⇒
	// DefaultRetrySchedule. Tests inject a compressed schedule (ms
	// scale) per Worker rather than via a package-level swap so
	// parallel test packages don't race.
	RetrySchedule []time.Duration

	// Resolver is the SSRF DNS resolver. Production wiring leaves
	// this nil ⇒ DefaultResolver; tests inject deterministic stubs
	// to demonstrate DNS-rebinding rejection.
	Resolver Resolver

	// DevAllowLocalhost mirrors --dev for the per-delivery SSRF
	// re-check. Must match the shim's CRUD setting; mismatch would
	// produce records that pass Create-time validation and fail
	// every delivery attempt.
	DevAllowLocalhost bool
}

// Worker owns one JetStream tasks-bucket WatchAll subscription and
// fans out webhook deliveries. One Worker per shim scope.
type Worker struct {
	cfg WorkerConfig
	wg  sync.WaitGroup
}

// NewWorker constructs a Worker. Lifecycle is Run(ctx) → Wait().
func NewWorker(cfg WorkerConfig) *Worker {
	if cfg.RetrySchedule == nil {
		cfg.RetrySchedule = DefaultRetrySchedule
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultHTTPClient(cfg.DevAllowLocalhost, cfg.Resolver)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = len(cfg.RetrySchedule)
	}
	return &Worker{cfg: cfg}
}

// defaultHTTPClient is the production HTTP client. 10s timeout,
// MaxIdleConnsPerHost: 10, and a CheckRedirect that re-validates the
// SSRF gate on every hop — without this, a malicious webhook can
// `Location:` to a private IP or cloud-metadata endpoint and bypass
// the registration-time URL check.
func defaultHTTPClient(devAllowLocalhost bool, resolver Resolver) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 10
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: t,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("push: stopped after 10 redirects")
			}
			if err := ValidateURL(req.URL.String(), devAllowLocalhost, resolver); err != nil {
				return fmt.Errorf("push: redirect target rejected: %w", err)
			}
			return nil
		},
	}
}

// Run blocks until ctx is cancelled or WatchAll terminates. Opens
// the tasks + notifycfg + notifyfail buckets defensively (matches
// the shim's "create on first use" pattern). Errors during bucket
// open propagate; transient WatchAll updates are logged and tolerated.
func (w *Worker) Run(ctx context.Context) error {
	tasksBucket, err := scope.Bucket(w.cfg.ScopeKind, w.cfg.ScopeID, "tasks")
	if err != nil {
		return fmt.Errorf("worker: tasks bucket: %w", err)
	}
	notifyBucket, err := scope.Bucket(w.cfg.ScopeKind, w.cfg.ScopeID, "notifycfg")
	if err != nil {
		return fmt.Errorf("worker: notifycfg bucket: %w", err)
	}
	failBucket, err := scope.Bucket(w.cfg.ScopeKind, w.cfg.ScopeID, "notifyfail")
	if err != nil {
		return fmt.Errorf("worker: notifyfail bucket: %w", err)
	}

	tasksKV, err := w.cfg.JS.KeyValue(ctx, tasksBucket)
	if err != nil {
		return fmt.Errorf("worker: open tasks kv: %w", err)
	}
	notifyKV, err := openOrCreateKV(ctx, w.cfg.JS, notifyBucket)
	if err != nil {
		return fmt.Errorf("worker: open notifycfg kv: %w", err)
	}
	failKV, err := openOrCreateKV(ctx, w.cfg.JS, failBucket)
	if err != nil {
		return fmt.Errorf("worker: open notifyfail kv: %w", err)
	}

	// IgnoreDeletes + UpdatesOnly skips historical state-change
	// records on (re)start — a freshly-started worker doesn't re-
	// deliver every task that ever existed.
	watcher, err := tasksKV.WatchAll(ctx,
		jetstream.IgnoreDeletes(),
		jetstream.UpdatesOnly(),
	)
	if err != nil {
		return fmt.Errorf("worker: WatchAll: %w", err)
	}
	defer func() { _ = watcher.Stop() }()

	w.cfg.Log.Info("push worker: started",
		"scope_kind", w.cfg.ScopeKind,
		"scope_id", w.cfg.ScopeID,
		"max_retries", w.cfg.MaxRetries,
	)

	for {
		select {
		case <-ctx.Done():
			return nil
		case entry, ok := <-watcher.Updates():
			if !ok {
				return nil
			}
			if entry == nil {
				// Initial-batch sentinel (we use UpdatesOnly so
				// this fires immediately).
				continue
			}
			state := parseTaskState(entry.Value())
			if state == "" {
				continue
			}
			taskRaw := entry.Value()
			taskID := entry.Key()
			w.wg.Add(1)
			go func(tID string, raw []byte, st string) {
				defer w.wg.Done()
				w.deliverAll(ctx, notifyKV, failKV, tID, raw, st)
			}(taskID, taskRaw, state)
		}
	}
}

// Wait blocks until all in-flight delivery goroutines complete. Run
// callers invoke this after their own ctx cancellation so the
// shim's ShutdownGrace bounds tail deliveries.
func (w *Worker) Wait() {
	w.wg.Wait()
}

// deliverAll fans out one task update to every registered config.
func (w *Worker) deliverAll(ctx context.Context, notifyKV, failKV jetstream.KeyValue, taskID string, taskRaw []byte, state string) {
	configs, err := notifications.List(ctx, notifyKV, taskID)
	if err != nil {
		w.cfg.Log.Warn("push worker: list configs", "task_id", taskID, "err", err)
		return
	}
	for i := range configs {
		cfg := configs[i]
		w.wg.Add(1)
		go func(c notifications.NotifyConfig) {
			defer w.wg.Done()
			w.deliverOne(ctx, failKV, c, taskRaw, state)
		}(cfg)
	}
}

// finalStates encodes the A2A terminal task states. The flag flows
// into the TaskStatusUpdateEvent payload so receivers know whether
// to expect further updates.
var finalStates = map[string]bool{
	"TASK_STATE_COMPLETED":      true,
	"TASK_STATE_CANCELED":       true,
	"TASK_STATE_FAILED":         true,
	"TASK_STATE_REJECTED":       true,
	"TASK_STATE_AUTH_REQUIRED":  false, // intermediate, listed for documentation
	"TASK_STATE_INPUT_REQUIRED": false,
	"TASK_STATE_WORKING":        false,
	"TASK_STATE_SUBMITTED":      false,
}

// deliverOne tries to POST taskRaw to a single webhook. Attempts =
// 1 + MaxRetries. Each failure records a `notifyfail` entry; 2xx
// breaks the loop.
func (w *Worker) deliverOne(ctx context.Context, failKV jetstream.KeyValue, cfg notifications.NotifyConfig, taskRaw []byte, state string) {
	// SSRF re-check per delivery — defends against DNS-rebinding
	// between Create and delivery.
	if err := ValidateURL(cfg.URL, w.cfg.DevAllowLocalhost, w.cfg.Resolver); err != nil {
		w.cfg.Log.Warn("push worker: ssrf reject at delivery", "task_id", cfg.TaskID, "config_id", cfg.ID, "err", err)
		_ = notifications.AppendFail(ctx, failKV, cfg.TaskID, cfg.ID, 1, "ssrf: "+err.Error())
		return
	}

	// Decrypt credentials once for the retry loop.
	plain := ""
	if cfg.Auth != nil && cfg.Auth.Credentials != "" {
		var err error
		plain, err = DecryptCredentials(cfg.Auth.Credentials, w.cfg.PushKey)
		if err != nil && !errors.Is(err, ErrLegacyPlaintext) {
			w.cfg.Log.Error("push worker: decrypt creds", "task_id", cfg.TaskID, "config_id", cfg.ID, "err", err)
			_ = notifications.AppendFail(ctx, failKV, cfg.TaskID, cfg.ID, 1, "decrypt failed")
			return
		}
		if errors.Is(err, ErrLegacyPlaintext) {
			w.cfg.Log.Warn("push worker: legacy plaintext credentials", "task_id", cfg.TaskID, "config_id", cfg.ID)
		}
	}

	payload, err := buildStatusEvent(cfg.TaskID, taskRaw, state)
	if err != nil {
		w.cfg.Log.Error("push worker: build event", "task_id", cfg.TaskID, "config_id", cfg.ID, "err", err)
		return
	}

	totalAttempts := 1 + w.cfg.MaxRetries
	if totalAttempts > 1+len(w.cfg.RetrySchedule) {
		totalAttempts = 1 + len(w.cfg.RetrySchedule)
	}
	for attempt := 1; attempt <= totalAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		status, postErr := w.post(ctx, cfg, plain, payload)
		if postErr == nil && status >= 200 && status < 300 {
			return
		}
		// Failure path: record + sleep before the next attempt
		// (no sleep after the LAST attempt — sleeping a retry
		// schedule we won't use just delays shutdown).
		failMsg := fmt.Sprintf("attempt=%d status=%d err=%v", attempt, status, postErr)
		// Best-effort: a fail-log write failure is logged but not
		// propagated; the next attempt's record still fires.
		if err := notifications.AppendFail(ctx, failKV, cfg.TaskID, cfg.ID, attempt, failMsg); err != nil {
			w.cfg.Log.Warn("push worker: append fail", "task_id", cfg.TaskID, "config_id", cfg.ID, "attempt", attempt, "err", err)
		}
		if attempt >= totalAttempts {
			return
		}
		// attempt is 1-indexed; RetrySchedule[0] is the sleep
		// BEFORE retry 1 (i.e. after attempt 1 fails).
		var sleep time.Duration
		if attempt-1 < len(w.cfg.RetrySchedule) {
			sleep = w.cfg.RetrySchedule[attempt-1]
		}
		if sleep > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(sleep):
			}
		}
	}
}

// post sends one HTTP request. Returns status (0 on transport error)
// and any error.
func (w *Worker) post(ctx context.Context, cfg notifications.NotifyConfig, plainCreds string, payload []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("X-A2A-Notification-Token", cfg.Token)
	}
	if cfg.Auth != nil && cfg.Auth.Scheme != "" && plainCreds != "" {
		req.Header.Set("Authorization", cfg.Auth.Scheme+" "+plainCreds)
	}
	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain to allow connection reuse — capped at 64 KiB to defend
	// against malicious webhooks that stream multi-GB responses to
	// stall the worker goroutine and exhaust the HTTP idle-pool slot.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, nil
}

// buildStatusEvent assembles the A2A v1.0 StreamResponse-wrapped
// TaskStatusUpdateEvent payload. Per a2a-go/v2 a2a/core.go (StreamResponse
// "oneof" envelope + TaskStatusUpdateEvent struct), the canonical wire
// shape is:
//
//	{"statusUpdate": {"contextId": "<ctx>", "taskId": "<id>", "status": <obj>}}
//
// contextId is REQUIRED by the spec and MUST be extracted from the task
// JSON. The finalState bit is implicit in status.state (caller derives
// via a2a.TaskState.Terminal); no extra `finalState` sibling field.
//
// Strict a2a-go receivers fail-closed on unknown sibling keys, so we
// emit only the spec-defined fields. Missing contextId in the task JSON
// is a hard error rather than silent empty-string emission.
func buildStatusEvent(taskID string, taskRaw []byte, _ string) ([]byte, error) {
	var probe struct {
		ContextID string          `json:"contextId"`
		Status    json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(taskRaw, &probe); err != nil {
		return nil, fmt.Errorf("task json: %w", err)
	}
	if len(probe.Status) == 0 {
		return nil, errors.New("task json missing status")
	}
	if probe.ContextID == "" {
		return nil, errors.New("task json missing contextId")
	}
	inner := map[string]any{
		"contextId": probe.ContextID,
		"taskId":    taskID,
		"status":    probe.Status,
	}
	return json.Marshal(map[string]any{"statusUpdate": inner})
}

// parseTaskState pulls the .status.state string out of a task JSON
// blob. Returns "" when the path is absent or unparseable; callers
// treat "" as "skip this update".
func parseTaskState(taskRaw []byte) string {
	var probe struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(taskRaw, &probe); err != nil {
		return ""
	}
	return probe.Status.State
}

// openOrCreateKV mirrors the Dispatcher helper but lives in package
// push (we can't import internal/shim/methods from here without
// introducing a cycle).
func openOrCreateKV(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if err == nil {
		return kv, nil
	}
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, err
	}
	kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	if err == nil {
		return kv, nil
	}
	// Race: another caller created it between Open and Create.
	if kv2, err2 := js.KeyValue(ctx, bucket); err2 == nil {
		return kv2, nil
	}
	return nil, err
}
