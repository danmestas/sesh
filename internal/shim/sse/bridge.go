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

	// Translator is the outbound A2A message serialiser. Nil ⇒ Bridge
	// falls back to a zero-value Translator (no gateway-URL rewrite —
	// matches the Slice-1 free-function shape). Production callers
	// (methods/{stream,subscribe}.go) wire d.deps.Translator so
	// obj:// Part URLs become dereferenceable HTTPS for SSE clients.
	Translator *a2a.Translator

	// ReqID is the JSON-RPC request id threaded in from the inbound
	// JSON-RPC envelope; every SSE chunk echoes it inside a JSON-RPC
	// 2.0 ClientResponse wrapper so a2a-go's parseSSEStream can decode
	// each chunk. Nil/empty ⇒ serialised as JSON `null`.
	ReqID json.RawMessage
}

// Bridge writes SSE events to w as messages and artifacts arrive on the
// supplied watch channels. Returns when ctx is cancelled, both watch
// channels close, or writing to w fails (client disconnect).
//
// Emits ":keepalive\n\n" comment every KeepaliveInterval so reverse
// proxies don't kill idle adapter streams. Each event emits as a single
// `data: <json>\n\n` line whose payload is a JSON-RPC 2.0
// ClientResponse envelope wrapping an a2a.StreamResponse:
//
//	{"jsonrpc":"2.0","id":<ReqID>,"result":{"message":<msg>}}
//	{"jsonrpc":"2.0","id":<ReqID>,"result":{"artifactUpdate":<art>}}
//
// This shape is what a2a-go's parseSSEStream (a2aclient/jsonrpc.go)
// expects — it ignores SSE `event:` decoration and unmarshals each
// `data:` line as jsonrpc.ClientResponse then as a2a.StreamResponse.
// Empty watch events (delete/purge — sesh-ops uses IgnoreDeletes so
// these are rare) are skipped, not emitted.
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
	translator := opts.Translator
	if translator == nil {
		// Fallback preserves the Slice-1 contract (no obj:// rewrite)
		// for callers that haven't been migrated yet — currently the
		// bridge_test fixtures.
		translator = &a2a.Translator{}
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
			data, err := translator.ToWireMessage(ev.Message)
			if err != nil {
				continue
			}
			if err := writeEvent(w, flusher, opts.ReqID, "message", data); err != nil {
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
			if err := writeEvent(w, flusher, opts.ReqID, "artifactUpdate", data); err != nil {
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

// writeEvent serialises a single SSE chunk as a JSON-RPC 2.0
// ClientResponse envelope whose result wraps the event under eventKey
// (per a2a.StreamResponse — see a2a-go/v2/a2a/core.go:event). reqID is
// echoed verbatim; nil/empty becomes JSON `null`.
//
// We marshal the envelope as a single JSON object (not two-step
// concatenation) so id/result ordering and escaping are correct for
// stock JSON-RPC parsers.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, reqID json.RawMessage, eventKey string, eventBytes []byte) error {
	id := reqID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	// Build {"<eventKey>": <eventBytes>} as a RawMessage so we don't
	// re-encode the inner event payload.
	result := append(append(append([]byte(`{`), encodeJSONString(eventKey)...), ':'), eventBytes...)
	result = append(result, '}')

	envelope := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// encodeJSONString escapes s as a JSON string literal (including the
// surrounding quotes). Used to build the single-field result wrapper
// without dragging in a full json.Marshal of a map[string]json.RawMessage.
func encodeJSONString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}
