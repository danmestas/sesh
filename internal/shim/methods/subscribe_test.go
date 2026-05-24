package methods

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/scope"
)

func TestSubscribeToTask_Backfill(t *testing.T) {
	deps, _, js2 := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed task + 3 messages.
	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvT, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kvT.Create(ctx, "T-SUB", []byte(`{"id":"T-SUB","kind":"task","status":{"state":"TASK_STATE_WORKING"}}`)); err != nil {
		t.Fatal(err)
	}
	msgsBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "messages")
	kvM, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: msgsBucket})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"M1", "M2", "M3"} {
		role := messages.MessageRoleUser
		if i%2 == 1 {
			role = messages.MessageRoleAgent
		}
		if _, err := messages.Append(ctx, kvM, messages.AppendOpts{Message: &messages.Message{
			ID: id, TaskID: "T-SUB", Role: role, Parts: []messages.Part{{Text: id}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	body := `{"method":"SubscribeToTask","params":{"id":"T-SUB"}}`
	req, _ := http.NewRequest("POST", url+"/subscribe", strings.NewReader(body))
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

	// Collect 3 backfill frames.
	got := make(map[string]bool)
	deadline := time.After(3 * time.Second)
	for len(got) < 3 {
		select {
		case fr, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed early; got=%v", got)
			}
			if fr.Event != "message" {
				continue
			}
			for _, id := range []string{"M1", "M2", "M3"} {
				if strings.Contains(fr.Data, `"messageId":"`+id+`"`) {
					got[id] = true
				}
			}
		case <-deadline:
			t.Fatalf("only got %d/3 backfill frames: %v", len(got), got)
		}
	}

	// Now write a 4th message; assert it tails through.
	go func() {
		time.Sleep(80 * time.Millisecond)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel2()
		_, _ = messages.Append(ctx2, kvM, messages.AppendOpts{Message: &messages.Message{
			ID: "M4", TaskID: "T-SUB", Role: messages.MessageRoleAgent, Parts: []messages.Part{{Text: "tail"}},
		}})
	}()
	deadline = time.After(3 * time.Second)
	for {
		select {
		case fr, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before M4")
			}
			if fr.Event == "message" && strings.Contains(fr.Data, `"messageId":"M4"`) {
				return
			}
		case <-deadline:
			t.Fatal("did not observe M4 tail")
		}
	}
}

func TestSubscribeToTask_UnknownTaskNoSSE(t *testing.T) {
	deps, _, _ := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	body := `{"method":"SubscribeToTask","params":{"id":"ghost"}}`
	req, _ := http.NewRequest("POST", url+"/subscribe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := strings.Split(resp.Header.Get("Content-Type"), ";")[0]; got == "text/event-stream" {
		t.Fatalf("expected JSON envelope for unknown task, got SSE")
	}
	b, _ := io.ReadAll(resp.Body)
	var env struct {
		Error *struct {
			Code int    `json:"code"`
			Name string `json:"name"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, b)
	}
	if env.Error == nil || env.Error.Code != -32001 {
		t.Fatalf("want -32001 TaskNotFound; got body=%s", b)
	}
}

func TestSubscribeToTask_InvalidParams(t *testing.T) {
	deps, _, _ := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	for _, body := range []string{
		`{"method":"SubscribeToTask"}`,
		`{"method":"SubscribeToTask","params":{}}`,
		`{"method":"SubscribeToTask","params":"not-an-object"}`,
	} {
		req, _ := http.NewRequest("POST", url+"/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Split(resp.Header.Get("Content-Type"), ";")[0]; got == "text/event-stream" {
			t.Errorf("body=%q: expected JSON envelope, got SSE", body)
		}
		resp.Body.Close()
	}
}

func TestSubscribeToTask_ClientDisconnectStops(t *testing.T) {
	deps, _, js2 := testDeps(t)
	url, _, _ := streamingServer(t, deps)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tasksBucket, _ := scope.Bucket(deps.ScopeKind, deps.ScopeID, "tasks")
	kvT, err := js2.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: tasksBucket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kvT.Create(ctx, "T-DC", []byte(`{"id":"T-DC","kind":"task","status":{"state":"TASK_STATE_WORKING"}}`)); err != nil {
		t.Fatal(err)
	}

	body := `{"method":"SubscribeToTask","params":{"id":"T-DC"}}`
	req, _ := http.NewRequest("POST", url+"/subscribe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	_, _ = resp.Body.Read(buf)
	if err := resp.Body.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	// Race-detector + test teardown is the implicit check that watchers
	// returned cleanly.
}
