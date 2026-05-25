package methods

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/oklog/ulid/v2"

	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/a2a"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
	"github.com/danmestas/sesh/internal/subject"
)

// sendMessageParams mirrors the A2A SendMessageRequest.params shape.
// The shim ignores the optional `configuration` field for now (see
// plan §3 non-goals).
type sendMessageParams struct {
	Message json.RawMessage `json:"message"`
}

// sendMessage handles the A2A `SendMessage` JSON-RPC method.
// See acceptInboundMessage for the shared accept/append/publish path.
func (d *Dispatcher) sendMessage(ctx context.Context, params json.RawMessage) (any, *jsonrpc.Error) {
	acc, jerr := d.acceptInboundMessage(ctx, params)
	if jerr != nil {
		return nil, jerr
	}
	entry, err := acc.tasksKV.Get(ctx, acc.message.TaskID)
	if err != nil {
		d.deps.Log.Error("sendMessage: re-read task", "task_id", acc.message.TaskID, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	// a2a-go unmarshals JSON-RPC result into StreamResponse, which is a
	// single-field envelope ({"task": ...} | {"message": ...} | ...).
	// Wrap the raw task bytes rather than re-marshalling the typed
	// struct — the bytes already canonicalize what the KV holds.
	return map[string]json.RawMessage{"task": entry.Value()}, nil
}

// acceptedInbound captures the post-accept state for callers that
// need to take further actions (re-read the task, open watchers).
// Returned KV handles are open for the duration of the request.
type acceptedInbound struct {
	message *messages.Message
	tasksKV jetstream.KeyValue
	msgsKV  jetstream.KeyValue
}

// acceptInboundMessage runs the shared first half of SendMessage /
// SendStreamingMessage: decode params, translate role, mint a fresh
// task if needed, append the message, and publish the prompt subject.
// Returns the post-accept state on success or a JSON-RPC error on
// the FIRST pre-stream failure (so streaming callers can emit a plain
// JSON envelope rather than a partial SSE stream).
func (d *Dispatcher) acceptInboundMessage(ctx context.Context, params json.RawMessage) (*acceptedInbound, *jsonrpc.Error) {
	if d.deps.JS == nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "JetStream v2 not configured"})
	}
	var p sendMessageParams
	if len(params) == 0 {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params is required"})
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": err.Error()})
	}
	if len(p.Message) == 0 {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": "params.message is required"})
	}
	m, err := a2a.FromWireMessage(p.Message)
	if err != nil {
		return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"err": err.Error()})
	}

	tasksBucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "tasks")
	if err != nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"})
	}
	msgsBucket, err := scope.Bucket(d.deps.ScopeKind, d.deps.ScopeID, "messages")
	if err != nil {
		return nil, jsonrpc.ErrInternal.WithData(map[string]string{"reason": "invalid scope"})
	}
	tasksKV, jerr := d.openOrCreateKV(ctx, tasksBucket)
	if jerr != nil {
		return nil, jerr
	}
	msgsKV, jerr := d.openOrCreateKV(ctx, msgsBucket)
	if jerr != nil {
		return nil, jerr
	}

	if m.TaskID == "" {
		m.TaskID = newULID()
		if m.ContextID == "" {
			m.ContextID = newULID()
		}
		taskBytes, mErr := json.Marshal(map[string]any{
			"id":        m.TaskID,
			"kind":      "task",
			"contextId": m.ContextID,
			"status":    map[string]any{"state": "TASK_STATE_SUBMITTED"},
		})
		if mErr != nil {
			return nil, jsonrpc.ErrInternal
		}
		if _, err := tasksKV.Create(ctx, m.TaskID, taskBytes); err != nil {
			d.deps.Log.Error("acceptInbound: create task", "task_id", m.TaskID, "err", err)
			return nil, jsonrpc.ErrInternal
		}
	} else {
		if _, err := tasksKV.Get(ctx, m.TaskID); err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil, jsonrpc.ErrTaskNotFound
			}
			d.deps.Log.Error("acceptInbound: read task", "task_id", m.TaskID, "err", err)
			return nil, jsonrpc.ErrInternal
		}
	}

	if m.ID == "" {
		m.ID = newULID()
	}

	if _, err := messages.Append(ctx, msgsKV, messages.AppendOpts{Message: m}); err != nil {
		if errors.Is(err, messages.ErrMessageIDConflict) {
			return nil, jsonrpc.ErrInvalidParams.WithData(map[string]string{"kind": "messageId-conflict"})
		}
		d.deps.Log.Error("acceptInbound: append", "task_id", m.TaskID, "msg_id", m.ID, "err", err)
		return nil, jsonrpc.ErrInternal
	}

	d.publishPromptV2(m)

	return &acceptedInbound{message: m, tasksKV: tasksKV, msgsKV: msgsKV}, nil
}

// openOrCreateKV opens the bucket if it exists, else creates it. The
// defensive Create handles a fresh project/session with no prior task.
func (d *Dispatcher) openOrCreateKV(ctx context.Context, bucket string) (jetstream.KeyValue, *jsonrpc.Error) {
	kv, err := d.deps.JS.KeyValue(ctx, bucket)
	if err == nil {
		return kv, nil
	}
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		d.deps.Log.Error("openOrCreateKV: open", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	kv, err = d.deps.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	if err != nil {
		// Race: another caller created it between our Open and Create.
		if kv2, err2 := d.deps.JS.KeyValue(ctx, bucket); err2 == nil {
			return kv2, nil
		}
		d.deps.Log.Error("openOrCreateKV: create", "bucket", bucket, "err", err)
		return nil, jsonrpc.ErrInternal
	}
	return kv, nil
}

// publishPromptV2 emits the translated Message bytes on the
// agents.prompt.v2.* subject so an adapter subscribed via the queue
// group wakes up. The bytes are re-serialized via a2a.ToMeshMessage so
// the published payload carries the v0.4 mesh-canonical lowercase Role
// ("user"|"agent") expected by the sesh-channels SDK envelope
// validator — NOT the a2a-go SCREAMING_SNAKE wire form
// ("ROLE_USER"|"ROLE_AGENT") used on the SSE / JSON-RPC client wire
// (see a2a.ToWireMessage for that distinct projection). This mirrors
// the storage-path translation already done in acceptInboundMessage
// (sesh#137: asymmetric translation — storage translated but publish
// did not). Errors are logged and swallowed — the KV write already
// happened and is authoritative.
//
// Subject construction matches sesh-channels SDK promptV2():
//
//	agents.prompt.v2.<machine>.<project>.<session>.<role>
//
// Three contract details, all gotchas surfaced by sesh#124:
//
//  1. Project and session are SEPARATE tokens. Session-scoped shims
//     carry a dotted ScopeID ("<project>.<session>") — we split on the
//     first '.' so the subject's project + session tokens match the
//     adapter's SESH_PROJECT / SESH_SESSION env split. ScopeIDs with no
//     dot fall back to project=session=scopeid (back-compat with
//     project-scoped shims that only have a single token).
//
//  2. Role is the abbreviated subject token (e.g. "cc"), discovered
//     from the adapter's $SRV.INFO `metadata.role`. The shim's --agent
//     flag carries the canonical agent ID ("claude-code") which is
//     unsuitable as a subject token; the canonical ID stays in
//     metadata.agent for L1+L2 card composition.
//
//  3. When discovery fails (no adapter responding within the composer
//     query window), we fall back to the --agent flag value so the
//     legacy single-token form still works for adapters that haven't
//     populated metadata.role yet.
func (d *Dispatcher) publishPromptV2(m *messages.Message) {
	if d.deps.NC == nil || d.deps.Machine == "" {
		return
	}

	role := d.discoverRoleToken()
	if role == "" {
		role = d.deps.AgentKey.Agent
	}
	if role == "" {
		return
	}
	if m == nil {
		return
	}
	raw, err := a2a.ToMeshMessage(m)
	if err != nil {
		d.deps.Log.Warn("sendMessage: marshal prompt", "err", err)
		return
	}

	project, session := splitScopeIDForSubject(d.deps.ScopeID)
	subj, err := subject.PromptV2(subject.Coord{
		Machine: d.deps.Machine,
		Project: project,
		Session: session,
		Role:    role,
	})
	if err != nil {
		d.deps.Log.Warn("sendMessage: build prompt subject", "err", err)
		return
	}
	if err := d.deps.NC.Publish(subj, raw); err != nil {
		d.deps.Log.Warn("sendMessage: publish prompt", "subj", subj, "err", err)
	}
}

// discoverRoleToken asks the Composer for the adapter's
// `metadata.role` via $SRV.INFO. Best-effort: returns "" when the
// composer isn't wired (older test deps), when discovery times out, or
// when the matched adapter omits the role field — callers fall back
// to the --agent flag. The window is bounded by the composer's own
// queryWindow so a missing adapter doesn't stall the publish path.
func (d *Dispatcher) discoverRoleToken() string {
	if d.deps.Composer == nil {
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	role, ok := d.deps.Composer.DiscoverRoleToken(ctx, d.deps.AgentKey)
	if !ok {
		return ""
	}
	return role
}

// splitScopeIDForSubject splits a (possibly dotted) scope-id into the
// project + session tokens used by the v2 prompt subject. The
// canonical session-scoped form is "<project>.<session>"; we split on
// the first '.' so a scope-id like "acme.demo" yields
// project="acme", session="demo", matching the adapter's SESH_PROJECT
// / SESH_SESSION env split.
//
// Scope-ids with no dot (project-scoped shims, single-token tests)
// fall back to project=session=scopeid for back-compat: the v0.3 path
// was Project=Session=ScopeID and several integration tests pin that
// shape with a single-token scope-id like "abc123".
//
// Each output token is then sanitized to be subject-safe (any
// remaining '.' or whitespace becomes '_'). The sanitize step is a
// belt-and-braces guard — splitScopeIDForSubject's own logic only
// leaves one '.' per token when the scope-id has more than one dot
// (rare; the v0.4 contract is exactly one).
func splitScopeIDForSubject(scopeID string) (project, session string) {
	if i := strings.Index(scopeID, "."); i >= 0 {
		return sanitizeScopeToken(scopeID[:i]), sanitizeScopeToken(scopeID[i+1:])
	}
	t := sanitizeScopeToken(scopeID)
	return t, t
}

// sanitizeScopeToken makes a scope-id safe to use as a single NATS
// subject token. Mirrors sesh-ops/scope's sanitize rule narrowed to
// the only character that's both valid in a scope-id and invalid in
// a subject token: '.'. Single-segment scope-ids are returned
// unchanged; dotted scope-ids like "acme.demo" become "acme_demo".
func sanitizeScopeToken(s string) string {
	return strings.ReplaceAll(s, ".", "_")
}

// newULID returns a new ULID string using crypto/rand for entropy
// and time.Now().UTC() as the timestamp.
func newULID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now().UTC()), cryptoRand.Reader).String()
}
