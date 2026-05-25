package methods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/notifications"
	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
	"github.com/danmestas/sesh/internal/shim/push"
)

// Scope names for push notification CRUD. Tighter least-privilege
// than `agent.read` / `agent.write` per Decision D1: a caller with
// read-only task access shouldn't be able to register webhooks that
// exfiltrate task state.
const (
	notifyReadScope  = "agent.notify.read"
	notifyWriteScope = "agent.notify.write"
)

// pushConfigParams is the A2A "params" envelope shape for the four
// push methods. All four route through the same wire shape;
// optionality of pushNotificationConfig (Create/list don't both need
// it) is enforced per-handler.
type pushConfigParams struct {
	// TaskID is the task this config belongs to. Required by every
	// handler; missing ⇒ ErrInvalidParams.
	TaskID string `json:"taskId,omitempty"`

	// PushNotificationConfigID is the per-task config id. Required
	// by Get/Delete; Create accepts blank and mints a ULID; List
	// ignores it entirely.
	PushNotificationConfigID string `json:"pushNotificationConfigId,omitempty"`

	// PushNotificationConfig is the full config object. Required by
	// Create; ignored by Get/List/Delete.
	PushNotificationConfig *notifications.NotifyConfig `json:"pushNotificationConfig,omitempty"`
}

// createTaskPushNotificationConfig stores an encrypted NotifyConfig
// against (taskId, configId). Returns the original cfg (plaintext
// Credentials echoed) for the response so the caller can confirm what
// they sent. Encryption happens at the storage boundary inside this
// function — neither the wire shape nor the response carries the
// "enc:" envelope.
func (d *Dispatcher) createTaskPushNotificationConfig(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.PushKey == nil {
		return nil, jsonrpc.ErrPushNotConfigured
	}
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}

	principal, ok := auth.FromContext(ctx)
	if !ok || !hasScope(principal, notifyWriteScope) {
		return nil, jsonrpc.ErrInvalidReq.WithData(map[string]string{"reason": "missing scope " + notifyWriteScope})
	}

	p, jerr := decodePushParams(params)
	if jerr != nil {
		return nil, jerr
	}
	if p.TaskID == "" {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params.taskId is required"})
	}
	if p.PushNotificationConfig == nil {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params.pushNotificationConfig is required"})
	}
	cfg := *p.PushNotificationConfig
	if cfg.URL == "" {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "pushNotificationConfig.url is required"})
	}
	if err := push.ValidateURL(cfg.URL, d.deps.PushDevAllowLocalhost, nil); err != nil {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "url rejected", "detail": err.Error()})
	}

	// Verify task exists before persisting the config (downgrade noise
	// for callers that hit a typo'd id; matches SendMessage shape).
	tasksKV, jerr := d.openOrCreateKV(ctx, mustBucket(d.deps.ScopeKind, d.deps.ScopeID, "tasks"))
	if jerr != nil {
		return nil, jerr
	}
	if _, err := tasksKV.Get(ctx, p.TaskID); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		d.deps.Log.Error("createPushCfg: read task", "task_id", p.TaskID, "err", err)
		return nil, jsonrpc.ErrInternal
	}

	configID := p.PushNotificationConfigID
	if configID == "" && cfg.ID != "" {
		configID = cfg.ID
	}
	if configID == "" {
		configID = newULID()
	}

	// Encrypt at the boundary. Encrypted copy goes into KV; the
	// response returns the original plaintext (caller already has it).
	stored := cfg
	stored.TaskID = p.TaskID
	stored.ID = configID
	if cfg.Auth != nil && cfg.Auth.Credentials != "" {
		enc, err := push.EncryptCredentials(cfg.Auth.Credentials, d.deps.PushKey)
		if err != nil {
			d.deps.Log.Error("createPushCfg: encrypt", "task_id", p.TaskID, "config_id", configID, "err", err)
			return nil, jsonrpc.ErrInternal
		}
		// Copy Auth so we don't mutate the caller's struct.
		authCopy := *cfg.Auth
		authCopy.Credentials = enc
		stored.Auth = &authCopy
	}

	notifyKV, jerr := d.openOrCreateKV(ctx, mustBucket(d.deps.ScopeKind, d.deps.ScopeID, "notifycfg"))
	if jerr != nil {
		return nil, jerr
	}
	if err := notifications.Set(ctx, notifyKV, p.TaskID, configID, stored); err != nil {
		d.deps.Log.Error("createPushCfg: notifications.Set", "task_id", p.TaskID, "config_id", configID, "err", err)
		return nil, jsonrpc.ErrInternal
	}

	// Response: plaintext echoed. Caller already has it; this is
	// confirmation, not disclosure.
	resp := cfg
	resp.TaskID = p.TaskID
	resp.ID = configID
	return resp, nil
}

// getTaskPushNotificationConfig fetches a single config, decrypts
// Credentials, and returns the plaintext-bearing struct. Legacy
// plaintext (no "enc:" prefix) is tolerated with a WARN — the
// pre-Slice-6 record gets returned as-is for backward compatibility.
func (d *Dispatcher) getTaskPushNotificationConfig(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.PushKey == nil {
		return nil, jsonrpc.ErrPushNotConfigured
	}
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}

	principal, ok := auth.FromContext(ctx)
	if !ok || !hasScope(principal, notifyReadScope) {
		return nil, jsonrpc.ErrInvalidReq.WithData(map[string]string{"reason": "missing scope " + notifyReadScope})
	}

	p, jerr := decodePushParams(params)
	if jerr != nil {
		return nil, jerr
	}
	if p.TaskID == "" || p.PushNotificationConfigID == "" {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params.taskId and params.pushNotificationConfigId are required"})
	}

	notifyKV, jerr := d.openOrCreateKV(ctx, mustBucket(d.deps.ScopeKind, d.deps.ScopeID, "notifycfg"))
	if jerr != nil {
		return nil, jerr
	}
	cfg, err := notifications.Get(ctx, notifyKV, p.TaskID, p.PushNotificationConfigID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, jsonrpc.ErrTaskNotFound
		}
		d.deps.Log.Error("getPushCfg: notifications.Get", "task_id", p.TaskID, "config_id", p.PushNotificationConfigID, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	if err := decryptIntoCfg(&cfg, d.deps.PushKey, d.deps.Log); err != nil {
		return nil, jsonrpc.ErrInternal
	}
	return cfg, nil
}

// listTaskPushNotificationConfigs decrypts every config for the task.
// Missing read scope yields `{configs: []}` HTTP 200 (mirrors
// ListTasks); empty bucket yields the same.
func (d *Dispatcher) listTaskPushNotificationConfigs(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.PushKey == nil {
		return nil, jsonrpc.ErrPushNotConfigured
	}
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}

	p, jerr := decodePushParams(params)
	if jerr != nil {
		return nil, jerr
	}
	if p.TaskID == "" {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params.taskId is required"})
	}

	principal, ok := auth.FromContext(ctx)
	if !ok || !hasScope(principal, notifyReadScope) {
		return emptyPushList(), nil
	}

	bucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "notifycfg")
	if err != nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"})
	}
	notifyKV, err := d.deps.JS.KeyValue(ctx, bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return emptyPushList(), nil
		}
		d.deps.Log.Error("listPushCfg: open kv", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	configs, err := notifications.List(ctx, notifyKV, p.TaskID)
	if err != nil {
		d.deps.Log.Error("listPushCfg: notifications.List", "task_id", p.TaskID, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	out := make([]notifications.NotifyConfig, 0, len(configs))
	for i := range configs {
		c := configs[i]
		if err := decryptIntoCfg(&c, d.deps.PushKey, d.deps.Log); err != nil {
			return nil, jsonrpc.ErrInternal
		}
		out = append(out, c)
	}
	return pushListResponse{Configs: out}, nil
}

// deleteTaskPushNotificationConfig removes a single config. Idempotent
// on missing key (notifications.Delete swallows ErrKeyNotFound).
func (d *Dispatcher) deleteTaskPushNotificationConfig(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.PushKey == nil {
		return nil, jsonrpc.ErrPushNotConfigured
	}
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}

	principal, ok := auth.FromContext(ctx)
	if !ok || !hasScope(principal, notifyWriteScope) {
		return nil, jsonrpc.ErrInvalidReq.WithData(map[string]string{"reason": "missing scope " + notifyWriteScope})
	}

	p, jerr := decodePushParams(params)
	if jerr != nil {
		return nil, jerr
	}
	if p.TaskID == "" || p.PushNotificationConfigID == "" {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params.taskId and params.pushNotificationConfigId are required"})
	}

	notifyKV, jerr := d.openOrCreateKV(ctx, mustBucket(d.deps.ScopeKind, d.deps.ScopeID, "notifycfg"))
	if jerr != nil {
		return nil, jerr
	}
	if err := notifications.Delete(ctx, notifyKV, p.TaskID, p.PushNotificationConfigID); err != nil {
		d.deps.Log.Error("deletePushCfg: notifications.Delete", "task_id", p.TaskID, "config_id", p.PushNotificationConfigID, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	return struct{}{}, nil
}

// pushListResponse mirrors the a2a-go ListPushNotificationConfigsResponse
// shape: `{configs:[...]}`. Configs is always non-nil so it serializes
// as `[]` rather than `null` when empty.
type pushListResponse struct {
	Configs []notifications.NotifyConfig `json:"configs"`
}

func emptyPushList() pushListResponse {
	return pushListResponse{Configs: []notifications.NotifyConfig{}}
}

// decodePushParams unmarshals the JSON-RPC params envelope into a
// pushConfigParams. Centralized so the four handlers share the same
// "params is required + must be JSON" failure shape.
func decodePushParams(params json.RawMessage) (pushConfigParams, *jsonrpc.Error) {
	var p pushConfigParams
	if len(params) == 0 {
		return p, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params is required"})
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return p, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": err.Error()})
	}
	return p, nil
}

// decryptIntoCfg unpacks cfg.Auth.Credentials in place. Legacy
// plaintext (no "enc:" prefix) is tolerated: the value is returned
// as-is and we WARN-log once per record. Any other decrypt error
// (tampered, wrong key) is fatal and propagates.
func decryptIntoCfg(cfg *notifications.NotifyConfig, key []byte, log slogger) error {
	if cfg.Auth == nil || cfg.Auth.Credentials == "" {
		return nil
	}
	plain, err := push.DecryptCredentials(cfg.Auth.Credentials, key)
	if err != nil {
		if errors.Is(err, push.ErrLegacyPlaintext) {
			if log != nil {
				// Avoid logging the credential itself — kid-style id is
				// fine but the value is not.
				log.Warn("push: legacy plaintext credentials", "task_id", cfg.TaskID, "config_id", cfg.ID)
			}
			cfg.Auth.Credentials = plain
			return nil
		}
		if log != nil {
			log.Error("push: decrypt credentials", "task_id", cfg.TaskID, "config_id", cfg.ID, "err", err)
		}
		return err
	}
	cfg.Auth.Credentials = plain
	return nil
}

// slogger is the minimal logger surface this file uses. Lets test
// callers pass a buffer-backed *slog.Logger without us coupling
// to the full *slog.Logger type at every callsite.
type slogger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// mustBucket panics if scope.Bucket fails (only happens with invalid
// scope kind/id, which is validated at server boot). Used to keep the
// handler bodies readable; the panic is unreachable in production.
func mustBucket(kind, id, suffix string) string {
	b, err := scope.Bucket(kind, id, suffix)
	if err != nil {
		panic(fmt.Sprintf("push: scope.Bucket(%q,%q,%q): %v", kind, id, suffix, err))
	}
	return b
}
