package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"reflect"
	"time"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/nats-io/nats.go"
)

// runAgentWatcher connects to the session's NATS server and periodically
// polls $SRV.INFO.agents, updating the session JSON's agents[] field.
//
// Poll strategy: every pollInterval, publish to $SRV.INFO.agents and collect
// replies for replyWindow. The set of live agents is whatever responded;
// agents whose metadata.session doesn't match sessionLabel are excluded.
// Writes are change-gated against an in-memory lastAgents cache — when the
// agent set is stable (the common case) the watcher does no disk I/O.
//
// Connect strategy: the embedded NATS server may not be ready the instant
// runAgentWatcher fires (small race against bindHub). The initial dial is
// retried with exponential backoff (100ms, 200ms, 400ms, 800ms, 1.6s, 3.2s,
// then 5s cap) until success or ctx cancellation. Once connected, nats.go's
// own reconnection options handle steady-state drops.
//
// The watcher is best-effort: connection and write errors are logged and
// retried next tick. The watcher exits when ctx is done or the session
// file disappears (fs.ErrNotExist from UpdateAgents).
func runAgentWatcher(ctx context.Context, natsURL string, sess *Session, sessionLabel string) {
	const (
		pollInterval = 1 * time.Second
		replyWindow  = 200 * time.Millisecond
	)

	nc, ok := connectWithBackoff(ctx, natsURL)
	if !ok {
		return // ctx cancelled during backoff
	}
	defer nc.Close()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastAgents []AgentRef // change-detection cache; nil means "never written"
	written := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			agents := queryAgents(nc, sessionLabel, replyWindow)
			// Change detection: skip the disk write when the agent set is
			// identical to the previously written set. Saves a full
			// read-marshal-rename cycle every tick in the steady state.
			if written && reflect.DeepEqual(agents, lastAgents) {
				continue
			}
			if err := sess.UpdateAgents(agents); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// Session file gone — session is ending; stop watcher.
					return
				}
				slog.Warn("agent watcher: update agents failed", "err", err)
				continue
			}
			lastAgents = agents
			written = true
		}
	}
}

// connectWithBackoff dials natsURL with exponential backoff capped at 5s,
// honoring ctx cancellation. Returns (nc, true) on success, (nil, false) if
// ctx fires before a connection succeeds. The returned conn is configured
// with infinite reconnect so a steady-state hub bounce reconnects on its own.
func connectWithBackoff(ctx context.Context, natsURL string) (*nats.Conn, bool) {
	const (
		initialBackoff = 100 * time.Millisecond
		maxBackoff     = 5 * time.Second
	)
	backoff := initialBackoff
	for {
		nc, err := nats.Connect(natsURL,
			nats.Name("sesh-agent-watcher"),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(500*time.Millisecond),
		)
		if err == nil {
			return nc, true
		}
		slog.Debug("agent watcher: NATS connect retry", "err", err, "next_wait", backoff)
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// microInfo is the subset of the NATS micro INFO response we decode.
type microInfo struct {
	Name      string            `json:"name"`
	ID        string            `json:"id"`
	Metadata  map[string]string `json:"metadata"`
	Endpoints []struct {
		Name    string `json:"name"`
		Subject string `json:"subject"`
	} `json:"endpoints"`
}

// queryServiceInfo issues one $SRV.INFO.agents discovery request and
// returns every distinct microInfo response within window. Deduplicates
// by service ID (info.ID). Returns nil (not empty slice) when no
// responder replies — preserves the watcher's reflect.DeepEqual cache
// semantics. Filtering + struct mapping is the caller's job.
func queryServiceInfo(nc *nats.Conn, window time.Duration) []microInfo {
	inbox := nc.NewInbox()
	replies := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe(inbox, replies)
	if err != nil {
		slog.Warn("queryServiceInfo: subscribe inbox failed", "err", err)
		return nil
	}
	defer func() {
		_ = sub.Unsubscribe()
		for {
			select {
			case <-replies:
			default:
				return
			}
		}
	}()

	if err := nc.PublishRequest("$SRV.INFO.agents", inbox, nil); err != nil {
		slog.Warn("queryServiceInfo: publish INFO request failed", "err", err)
		return nil
	}

	deadline := time.Now().Add(window)
	var out []microInfo
	seen := make(map[string]struct{})

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case msg, ok := <-replies:
			timer.Stop()
			if !ok {
				return out
			}
			var info microInfo
			if err := json.Unmarshal(msg.Data, &info); err != nil {
				continue
			}
			if _, dup := seen[info.ID]; dup {
				continue
			}
			seen[info.ID] = struct{}{}
			out = append(out, info)
		case <-timer.C:
			return out
		}
	}
	return out
}

// queryAgents sends $SRV.INFO.agents and collects responses for window,
// returning AgentRefs for instances whose metadata.session matches label.
// Returns a nil slice (not a zero-length non-nil slice) when no agents
// respond — this lets reflect.DeepEqual cache hits work cleanly across ticks.
func queryAgents(nc *nats.Conn, label string, window time.Duration) []AgentRef {
	infos := queryServiceInfo(nc, window)
	var refs []AgentRef
	for _, info := range infos {
		if info.Metadata["session"] != label {
			continue
		}
		ref := AgentRef{
			Agent:      info.Metadata["agent"],
			Owner:      info.Metadata["owner"],
			InstanceID: info.ID,
			Role:       agentmeta.DefaultedRole(info.Metadata["role"]),
			Class:      string(agentmeta.DefaultedClass(info.Metadata["class"])),
		}
		for _, ep := range info.Endpoints {
			if ep.Name == "prompt" {
				ref.Subject = ep.Subject
				break
			}
		}
		if ref.Subject == "" && len(info.Endpoints) > 0 {
			ref.Subject = info.Endpoints[0].Subject
		}
		refs = append(refs, ref)
	}
	return refs
}
