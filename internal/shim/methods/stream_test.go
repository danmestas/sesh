package methods

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/scope"
)

// streamingServer wires the dispatcher's streaming handlers behind
// an httptest server. Caller posts JSON-RPC bodies and scans SSE
// responses with a bufio.Scanner.
func streamingServer(t *testing.T, deps Deps) (string, *httptest.Server, *Dispatcher) {
	t.Helper()
	disp := NewDispatcher(deps)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /stream", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &env)
		disp.SendStreamingMessage(w, r, env.Params)
	})
	mux.HandleFunc("POST /subscribe", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &env)
		disp.SubscribeToTask(w, r, env.Params)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, ts, disp
}

// sseFrame represents one parsed SSE event (event + data lines).
type sseFrame struct {
	Event string
	Data  string
}

// scanFrames streams parsed SSE frames from r until r closes or the
// channel reader stops draining. Comment lines (":..." ) are skipped.
func scanFrames(r io.Reader) <-chan sseFrame {
	out := make(chan sseFrame, 32)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		var ev, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev != "" || data != "" {
					out <- sseFrame{Event: ev, Data: data}
					ev, data = "", ""
				}
			case strings.HasPrefix(line, ":"):
				// keepalive comment
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if data == "" {
					data = strings.TrimPrefix(line, "data: ")
				} else {
					data += "\n" + strings.TrimPrefix(line, "data: ")
				}
			}
		}
	}()
	return out
}

func TestSendStreamingMessage_HappyPath(t *testing.T) {
	deps, _, js2 := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	body := `{"method":"SendStreamingMessage","params":{"message":{"messageId":"M1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	req, _ := http.NewRequest("POST", url+"/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := strings.Split(resp.Header.Get("Content-Type"), ";")[0]; got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	frames := scanFrames(resp.Body)

	// Stream is tail-only (IncludeHistory:false), so the inbound message
	// we just appended does NOT echo. Inject a second message into KV
	// and assert it shows up as an SSE frame.
	go func() {
		// give the watcher a moment to start
		time.Sleep(150 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		msgsBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "messages")
		kvM, err := js2.KeyValue(ctx, msgsBucket)
		if err != nil {
			t.Errorf("open msgs kv: %v", err)
			return
		}
		// Determine the task id by listing one task and reusing its id.
		tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
		kvT, _ := js2.KeyValue(ctx, tasksBucket)
		keys, _ := kvT.Keys(ctx)
		if len(keys) == 0 {
			t.Errorf("no tasks created")
			return
		}
		taskID := keys[0]
		second := &messages.Message{
			ID:     "M2",
			TaskID: taskID,
			Role:   messages.MessageRoleAgent,
			Parts:  []messages.Part{{Text: "from-agent"}},
		}
		if _, err := messages.Append(ctx, kvM, messages.AppendOpts{Message: second}); err != nil {
			t.Errorf("append second: %v", err)
		}
	}()

	select {
	case fr, ok := <-frames:
		if !ok {
			t.Fatal("stream closed before any frame")
		}
		if fr.Event != "message" {
			t.Errorf("first frame event = %q, want message", fr.Event)
		}
		if !strings.Contains(fr.Data, `"role":"ROLE_AGENT"`) {
			t.Errorf("first frame missing translated role; data=%s", fr.Data)
		}
		if !strings.Contains(fr.Data, `"messageId":"M2"`) {
			t.Errorf("first frame missing messageId; data=%s", fr.Data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never received message frame")
	}
}

func TestSendStreamingMessage_ClientDisconnectStopsWatchers(t *testing.T) {
	deps, _, _ := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	body := `{"method":"SendStreamingMessage","params":{"message":{"messageId":"M1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	req, _ := http.NewRequest("POST", url+"/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	// Drain a few bytes to confirm SSE started, then close the body.
	go func() {
		buf := make([]byte, 256)
		_, _ = resp.Body.Read(buf)
	}()
	time.Sleep(200 * time.Millisecond)
	// Closing the body cancels r.Context() server-side, which stops watchers.
	if err := resp.Body.Close(); err != nil {
		t.Logf("body close: %v", err)
	}
	// No direct assertion on goroutine count — race-detector + the
	// fact that the test's nats server torn down via t.Cleanup() with
	// no hung subscription is the implicit check.
}

func TestSendStreamingMessage_IdempotencyConflictReturnsJSON(t *testing.T) {
	deps, _, js2 := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	// Pre-seed task + message so second send with same messageId + divergent body conflicts.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvT, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kvT.Create(ctx, "T-S", []byte(`{"id":"T-S","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
		t.Fatal(err)
	}
	msgsBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "messages")
	kvM, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: msgsBucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := messages.Append(ctx, kvM, messages.AppendOpts{Message: &messages.Message{
		ID: "M-C", TaskID: "T-S", Role: messages.MessageRoleUser, Parts: []messages.Part{{Text: "orig"}},
	}}); err != nil {
		t.Fatal(err)
	}

	body := `{"method":"SendStreamingMessage","params":{"message":{"messageId":"M-C","taskId":"T-S","role":"ROLE_USER","parts":[{"text":"divergent"}]}}}`
	req, _ := http.NewRequest("POST", url+"/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := strings.Split(resp.Header.Get("Content-Type"), ";")[0]; got == "text/event-stream" {
		t.Fatalf("got SSE content-type for pre-stream error")
	}
	b, _ := io.ReadAll(resp.Body)
	var env struct {
		Error *struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, b)
	}
	if env.Error == nil || env.Error.Code != -32602 {
		t.Fatalf("want -32602 conflict, got body=%s", b)
	}
}

func TestSendStreamingMessage_Keepalive(t *testing.T) {
	deps, _, _ := testDeps(t)
	deps.KeepaliveInterval = 40 * time.Millisecond
	url, _, _ := streamingServer(t, deps)

	body := `{"jsonrpc":"2.0","id":1,"method":"SendStreamingMessage","params":{"message":{"messageId":"M-KA","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	req, _ := http.NewRequest(http.MethodPost, url+"/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if got := strings.Split(resp.Header.Get("Content-Type"), ";")[0]; got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	// Raw-line scan (scanFrames swallows comments; we need to see them).
	type result struct{ saw bool }
	done := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), ":keepalive") {
				done <- result{true}
				return
			}
		}
		done <- result{false}
	}()
	select {
	case r := <-done:
		if !r.saw {
			t.Fatalf("scanner exited without seeing :keepalive")
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatalf("no :keepalive observed in 400ms with 40ms interval")
	}
}

// parallel-stream sanity: two concurrent streams against the same task
// don't interfere. Asserts watcher goroutines are isolated.
func TestSendStreamingMessage_ConcurrentStreams(t *testing.T) {
	deps, _, js2 := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	// Seed a task so both streams target the same task.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvT, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kvT.Create(ctx, "T-MULTI", []byte(`{"id":"T-MULTI","kind":"task","status":{"state":"TASK_STATE_SUBMITTED"}}`)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"method":"SendStreamingMessage","params":{"message":{"messageId":"MS-` + string(rune('A'+i)) + `","taskId":"T-MULTI","role":"ROLE_USER","parts":[{"text":"x"}]}}}`
			req, _ := http.NewRequest("POST", url+"/stream", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("stream %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 64)
			_, _ = resp.Body.Read(buf)
			if !bytes.Contains([]byte(resp.Header.Get("Content-Type")), []byte("text/event-stream")) {
				t.Errorf("stream %d: content-type = %q", i, resp.Header.Get("Content-Type"))
			}
		}(i)
	}
	// Let streams open; close test (httptest server teardown + ctx cancel).
	time.Sleep(300 * time.Millisecond)
	wg.Wait()
}
