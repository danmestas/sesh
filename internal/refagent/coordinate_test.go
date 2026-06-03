package refagent

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/coord"
	"github.com/danmestas/sesh/internal/subject"
)

// testProjectID is the injected pinned 40-hex routing key used across the
// coordination tests. Identity is injected, not derived: coordinateLoop takes
// the projectID as a parameter (cfg.ProjectID at boot), so tests pass this
// value directly rather than seeding a .sesh/project-id pin on disk.
const testProjectID = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// TestCoordinateLoop_PromptQueueGroup is the load-bearing test for the
// collapsed scheme: active agents subscribe to the SINGLE 5-token prompt
// subject under the fixed PromptQueueGroup. A prompt published to that
// subject is delivered to exactly one of two same-session subscribers
// (work-stealing) — the queue group is what preserves single-delivery,
// now that the role-pool and direct-instance tiers are gone.
func TestCoordinateLoop_PromptQueueGroup(t *testing.T) {
	url := startBroker(t)
	pid := testProjectID
	t.Setenv("SESH_MACHINE", coord.MachineLocal)
	machine := coord.MachineLocal
	session := "s1"

	// --- Wire two subscribers directly (not via coordinateLoop) so the
	// test exercises the exact subject + queue group coordinateLoop
	// produces and can assert delivery counts. coordinateLoop's own
	// integration is covered by TestRun_CoordinateLoopAttaches below.

	nc := mustConnect(t, url)
	defer nc.Close()

	promptSubj := fmt.Sprintf("agents.prompt.%s.%s.%s", machine, pid, session)

	// Two active agents on the same 5-token subject, same queue group.
	var hits1, hits2 atomic.Int32
	qsub1, err := nc.QueueSubscribe(promptSubj, subject.PromptQueueGroup, func(*nats.Msg) { hits1.Add(1) })
	if err != nil {
		t.Fatalf("queue subscribe 1: %v", err)
	}
	defer qsub1.Unsubscribe()
	qsub2, err := nc.QueueSubscribe(promptSubj, subject.PromptQueueGroup, func(*nats.Msg) { hits2.Add(1) })
	if err != nil {
		t.Fatalf("queue subscribe 2: %v", err)
	}
	defer qsub2.Unsubscribe()

	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Publish one prompt to the 5-token subject.
	pubNC := mustConnect(t, url)
	defer pubNC.Close()
	mustPublish(t, pubNC, promptSubj, []byte("to-session"))

	// Give the broker a beat to deliver.
	time.Sleep(150 * time.Millisecond)

	// Queue group: exactly one of the two subscribers received.
	if got := hits1.Load() + hits2.Load(); got != 1 {
		t.Errorf("queue-group hits = %d, want 1 (work-stealing)", got)
	}
}

// TestCoordinateLoop_SubscribesPromptRegardlessOfClass asserts the
// post-scope-cut contract: class is no longer a routing input. An agent
// with an arbitrary class value still QueueSubscribes the 5-token prompt
// subject via coordinateLoop and receives a dispatch published to it —
// there is no class-driven observer/report branch.
func TestCoordinateLoop_SubscribesPromptRegardlessOfClass(t *testing.T) {
	url := startBroker(t)
	pid := testProjectID
	t.Setenv("SESH_MACHINE", coord.MachineLocal)

	cfg := Config{
		Agent: "watcher", Owner: "alice", Session: "s1",
		Role: "spy", Class: "observer", // arbitrary display tag; must NOT gate routing
		Interval: 1 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nc := mustConnect(t, url)
	defer nc.Close()

	loopDone := make(chan error, 1)
	go func() { loopDone <- coordinateLoop(ctx, nc, cfg, pid, "obs-inst-1") }()

	// Give coordinateLoop a moment to install the subscription.
	time.Sleep(50 * time.Millisecond)

	pubNC := mustConnect(t, url)
	defer pubNC.Close()
	machine := coord.MachineLocal

	// Publish a prompt to the 5-token subject as a request: coordinateLoop's
	// handler responds with an ack when a reply inbox is set. Receiving the
	// ack proves the loop subscribed to prompt despite class="observer".
	promptSubj := fmt.Sprintf("agents.prompt.%s.%s.s1", machine, pid)
	resp, err := pubNC.Request(promptSubj, []byte("dispatch"), 500*time.Millisecond)
	if err != nil {
		t.Fatalf("prompt request got no ack — class gated routing: %v", err)
	}
	if !bytes.Contains(resp.Data, []byte(`"verb":"prompt"`)) {
		t.Errorf("ack = %q, want a prompt-verb ack", resp.Data)
	}

	cancel()
	select {
	case err := <-loopDone:
		if err != nil {
			t.Errorf("coordinateLoop returned err = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("coordinateLoop did not exit within 1s of ctx cancel")
	}
}

// ---- helpers ----

func mustConnect(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	return nc
}

func mustPublish(t *testing.T, nc *nats.Conn, subj string, data []byte) {
	t.Helper()
	if err := nc.Publish(subj, data); err != nil {
		t.Fatalf("publish %s: %v", subj, err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after publish: %v", err)
	}
}
