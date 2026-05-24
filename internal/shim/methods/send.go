package methods

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/json"
	"errors"
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
	return json.RawMessage(entry.Value()), nil
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

	d.publishPromptV2(p.Message)

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

// publishPromptV2 emits the inbound Message bytes on the
// agents.prompt.v2.* subject so an adapter subscribed via the queue
// group wakes up. Errors are logged and swallowed — the KV write
// already happened and is authoritative.
func (d *Dispatcher) publishPromptV2(raw json.RawMessage) {
	if d.deps.NC == nil || d.deps.Machine == "" {
		return
	}
	role := d.deps.AgentKey.Agent
	if role == "" {
		return
	}
	// TODO Slice 4+: distinguish project from session once scope plumbing
	// has both. v0.4 ships session-scoped only, so Project=Session=ScopeID
	// is intentional — the subject reads as `agents.prompt.v2.<m>.<s>.<s>.<r>`
	// and adapters subscribe with the matching coord. Don't copy-paste-flag.
	subj, err := subject.PromptV2(subject.Coord{
		Machine: d.deps.Machine,
		Project: d.deps.ScopeID,
		Session: d.deps.ScopeID,
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

// newULID returns a new ULID string using crypto/rand for entropy
// and time.Now().UTC() as the timestamp.
func newULID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now().UTC()), cryptoRand.Reader).String()
}
