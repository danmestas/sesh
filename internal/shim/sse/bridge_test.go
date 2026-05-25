package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/sesh-ops/artifacts"
	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/objects"

	"github.com/danmestas/sesh/internal/shim/a2a"
)

// flushRecorder wraps httptest.ResponseRecorder with an http.Flusher
// no-op so Bridge can drive it (the recorder's Write surfaces in
// Body, which we scan from a parallel goroutine).
type flushRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func (f *flushRecorder) Flush() {
	// no-op; httptest.ResponseRecorder.Body is already updated by Write
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *flushRecorder) snapshot() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Body.String()
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func TestBridge_EmitsMessageEvent(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent, 1)
	artCh := make(chan artifacts.Update)

	stopped := &atomic.Bool{}
	stop := func() { stopped.Store(true) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, stop, artCh, Options{KeepaliveInterval: time.Second})
		close(done)
	}()

	msgCh <- messages.WatchEvent{Op: "put", Key: "T.M", Message: &messages.Message{
		ID:     "M",
		TaskID: "T",
		Role:   messages.MessageRoleUser,
		Parts:  []messages.Part{{Text: "hi"}},
	}}

	// Allow Bridge to write.
	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(w.snapshot(), "event: message") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("never observed message event; got=%q", w.snapshot())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	<-done
	if !stopped.Load() {
		t.Errorf("msgStop was not called")
	}
	body := w.snapshot()
	if !strings.Contains(body, `"role":"ROLE_USER"`) {
		t.Errorf("role not translated to wire form: %s", body)
	}
	if !strings.Contains(body, `data: {`) {
		t.Errorf("missing data: line: %s", body)
	}
}

func TestBridge_EmitsArtifactEvent(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent)
	artCh := make(chan artifacts.Update, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, nil, artCh, Options{KeepaliveInterval: time.Second})
		close(done)
	}()

	artCh <- artifacts.Update{Op: "put", Artifact: &artifacts.Artifact{ID: "A1"}}

	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(w.snapshot(), "event: artifact-update") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("never observed artifact event; got=%q", w.snapshot())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	<-done
}

func TestBridge_KeepaliveAtInterval(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent)
	artCh := make(chan artifacts.Update)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, nil, artCh, Options{KeepaliveInterval: 50 * time.Millisecond})
		close(done)
	}()

	time.Sleep(250 * time.Millisecond)
	cancel()
	<-done

	body := w.snapshot()
	count := strings.Count(body, ":keepalive\n\n")
	if count < 3 {
		t.Errorf("expected >=3 keepalive lines in 250ms with 50ms interval, got %d. body=%q", count, body)
	}
}

func TestBridge_CtxCancelReturnsPromptlyAndStops(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent)
	artCh := make(chan artifacts.Update)

	stopped := &atomic.Bool{}
	stop := func() { stopped.Store(true) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, stop, artCh, Options{KeepaliveInterval: time.Second})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Bridge did not return within 500ms of cancel")
	}
	if !stopped.Load() {
		t.Errorf("msgStop was not called via defer")
	}
}

func TestBridge_BothChannelsClosedReturns(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent)
	artCh := make(chan artifacts.Update)

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, nil, artCh, Options{KeepaliveInterval: time.Second})
		close(done)
	}()

	close(msgCh)
	close(artCh)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("Bridge did not return after both channels closed")
	}
}

func TestBridge_NilMessageSkipped(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent, 1)
	artCh := make(chan artifacts.Update)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, nil, artCh, Options{KeepaliveInterval: time.Second})
		close(done)
	}()
	msgCh <- messages.WatchEvent{Op: "delete", Key: "T.M", Message: nil}
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	body := w.snapshot()
	if strings.Contains(body, "event: message") {
		t.Errorf("nil message should not produce event; body=%q", body)
	}
}

func TestBridge_HeadersSetAndContentTypeStream(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent)
	artCh := make(chan artifacts.Update)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, nil, artCh, Options{KeepaliveInterval: time.Second})
		close(done)
	}()
	// give Bridge a moment to write headers
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if got := w.Result().Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
}

// TestBridge_OverHTTPTestServer drives Bridge through a real httptest
// server to confirm Flush actually moves bytes to the wire and the
// SSE framing parses with a real bufio.Scanner.
func TestBridge_OverHTTPTestServer(t *testing.T) {
	msgCh := make(chan messages.WatchEvent, 4)
	artCh := make(chan artifacts.Update)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = Bridge(r.Context(), w, msgCh, nil, artCh, Options{KeepaliveInterval: time.Second})
	}))
	defer srv.Close()

	go func() {
		msgCh <- messages.WatchEvent{Op: "put", Key: "T.M1", Message: &messages.Message{
			ID: "M1", TaskID: "T", Role: messages.MessageRoleAgent,
			Parts: []messages.Part{{Text: "first"}},
		}}
	}()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	sc := bufio.NewScanner(resp.Body)
	gotEvent := false
	var gotData string
	deadline := time.After(2 * time.Second)
	type line struct{ s string }
	out := make(chan line, 32)
	go func() {
		for sc.Scan() {
			out <- line{sc.Text()}
		}
		close(out)
	}()
	for !gotEvent {
		select {
		case <-deadline:
			t.Fatalf("did not observe event in time")
		case l, ok := <-out:
			if !ok {
				t.Fatalf("scanner closed without event")
			}
			if l.s == "event: message" {
				gotEvent = true
			}
			if strings.HasPrefix(l.s, "data: ") {
				gotData = strings.TrimPrefix(l.s, "data: ")
			}
		}
	}
	// drain one more data line if not captured yet
	if gotData == "" {
		select {
		case l := <-out:
			if strings.HasPrefix(l.s, "data: ") {
				gotData = strings.TrimPrefix(l.s, "data: ")
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	if gotData != "" {
		var probe map[string]any
		if err := json.Unmarshal([]byte(gotData), &probe); err != nil {
			t.Errorf("data payload not valid JSON: %v %q", err, gotData)
		}
		if probe["role"] != "ROLE_AGENT" {
			t.Errorf("role payload = %v, want ROLE_AGENT", probe["role"])
		}
	}
}

// TestBridge_TranslatesObjURL — when Options.Translator is supplied,
// obj:// Part URLs in WatchEvents are rewritten to gateway-rooted
// HTTPS in the SSE `data:` line. This is the Slice-7 end-to-end
// guarantee at the SSE boundary.
func TestBridge_TranslatesObjURL(t *testing.T) {
	w := newFlushRecorder()
	msgCh := make(chan messages.WatchEvent, 1)
	artCh := make(chan artifacts.Update)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Bridge(ctx, w, msgCh, nil, artCh, Options{
			KeepaliveInterval: time.Second,
			Translator:        a2a.NewTranslator("https://shim.test", nil),
		})
		close(done)
	}()

	origURI := objects.URI("project", "abc123", "T1", "A1")
	msgCh <- messages.WatchEvent{Op: "put", Key: "T1.M", Message: &messages.Message{
		ID:     "M",
		TaskID: "T1",
		Role:   messages.MessageRoleAgent,
		Parts:  []messages.Part{{URL: origURI, MediaType: "application/pdf"}},
	}}

	deadline := time.After(2 * time.Second)
	for {
		body := w.snapshot()
		if strings.Contains(body, "https://shim.test/obj/project/abc123/T1/A1") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("obj:// not rewritten in SSE data\ngot=%q", w.snapshot())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	<-done

	body := w.snapshot()
	if strings.Contains(body, "obj://") {
		t.Errorf("obj:// leaked through to SSE wire:\n%s", body)
	}
}
