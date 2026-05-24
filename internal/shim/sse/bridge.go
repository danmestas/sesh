// Package sse bridges sesh-ops KV watch channels (messages + artifacts)
// to an HTTP Server-Sent Events response stream. The bridge owns the
// SSE wire format, the proxy-keepalive heartbeat, channel-close
// handling, and ctx-cancellation cleanup. Callers supply already-open
// watch channels and a stop func; the bridge takes ownership for the
// duration of the call.
package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/danmestas/sesh-ops/artifacts"
	"github.com/danmestas/sesh-ops/messages"

	"github.com/danmestas/sesh/internal/shim/a2a"
)

// DefaultKeepaliveInterval beats common reverse-proxy idle-timeouts
// (nginx 60s, cloudflare 100s) with margin. Override via
// Options.KeepaliveInterval for tests.
const DefaultKeepaliveInterval = 25 * time.Second

// Options narrows Bridge's behaviour. Zero values mean defaults.
type Options struct {
	KeepaliveInterval time.Duration
}

// Bridge writes SSE events to w as messages and artifacts arrive on the
// supplied watch channels. Returns when ctx is cancelled, both watch
// channels close, or writing to w fails (client disconnect).
//
// Emits ":keepalive\n\n" comment every KeepaliveInterval so reverse
// proxies don't kill idle adapter streams. Messages emit as
// `event: message\ndata: <wire-json>\n\n`, artifacts as
// `event: artifact-update\ndata: <json>\n\n`. Empty watch events
// (delete/purge — sesh-ops uses IgnoreDeletes so these are rare) are
// skipped, not emitted.
//
// Always sets `Content-Type: text/event-stream` and flushes after each
// write. The caller MUST have already verified the response can be
// streamed (i.e., w must implement http.Flusher).
func Bridge(
	ctx context.Context,
	w http.ResponseWriter,
	msgCh <-chan messages.WatchEvent,
	msgStop func(),
	artCh <-chan artifacts.Update,
	opts Options,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("sse.Bridge: ResponseWriter does not implement http.Flusher")
	}
	if msgStop != nil {
		defer msgStop()
	}

	keepalive := opts.KeepaliveInterval
	if keepalive <= 0 {
		keepalive = DefaultKeepaliveInterval
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-msgCh:
			if !ok {
				// Channel closed; stop selecting on it without busy-spin.
				msgCh = nil
				if msgCh == nil && artCh == nil {
					return nil
				}
				continue
			}
			if ev.Message == nil {
				continue
			}
			data, err := a2a.ToWireMessage(ev.Message)
			if err != nil {
				continue
			}
			if err := writeEvent(w, flusher, "message", data); err != nil {
				return err
			}
		case upd, ok := <-artCh:
			if !ok {
				artCh = nil
				if msgCh == nil && artCh == nil {
					return nil
				}
				continue
			}
			if upd.Artifact == nil {
				continue
			}
			data, err := json.Marshal(upd.Artifact)
			if err != nil {
				continue
			}
			if err := writeEvent(w, flusher, "artifact-update", data); err != nil {
				return err
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, flusher http.Flusher, name string, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
