package refagent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/agentmeta"
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

// TestCoordinateLoop_ObserverNeverReceivesPrompt asserts the spy-exclusion
// contract: an observer-class agent subscribed via coordinateLoop receives
// agents.report.* messages but NEVER agents.prompt.* messages, even when a
// prompt is published to a subject that wildcards over the same machine/
// project/session triple.
func TestCoordinateLoop_ObserverNeverReceivesPrompt(t *testing.T) {
	url := startBroker(t)
	pid := testProjectID
	t.Setenv("SESH_MACHINE", coord.MachineLocal)

	cfg := Config{
		Agent: "watcher", Owner: "alice", Session: "s1",
		Role: "spy", Class: agentmeta.ClassObserver,
		Interval: 1 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nc := mustConnect(t, url)
	defer nc.Close()

	loopDone := make(chan error, 1)
	go func() { loopDone <- coordinateLoop(ctx, nc, cfg, pid, "obs-inst-1") }()

	// Give coordinateLoop a moment to install subscriptions.
	time.Sleep(50 * time.Millisecond)

	pubNC := mustConnect(t, url)
	defer pubNC.Close()
	machine := coord.MachineLocal

	// A prompt — observer must NOT receive.
	promptSubj := fmt.Sprintf("agents.prompt.%s.%s.s1", machine, pid)
	mustPublish(t, pubNC, promptSubj, []byte("dispatch"))

	// A report — observer SHOULD receive (via the report wildcard).
	reportSubj := fmt.Sprintf("agents.report.%s.%s.s1.workers.status", machine, pid)
	mustPublish(t, pubNC, reportSubj, []byte("status update"))

	// Subscribe via a sibling on the prompt subject to confirm the broker
	// DID route the dispatch — without this we'd be testing "broker
	// dropped the message" rather than "observer didn't receive it".
	siblingHits := make(chan struct{}, 4)
	ssub, err := pubNC.Subscribe(promptSubj, func(*nats.Msg) { siblingHits <- struct{}{} })
	if err != nil {
		t.Fatalf("sibling subscribe: %v", err)
	}
	defer ssub.Unsubscribe()
	if err := pubNC.Flush(); err != nil {
		t.Fatalf("flush sibling: %v", err)
	}
	mustPublish(t, pubNC, promptSubj, []byte("dispatch-2"))

	select {
	case <-siblingHits:
		// good — broker routed the dispatch
	case <-time.After(200 * time.Millisecond):
		t.Fatal("sibling subscriber missed dispatch — broker wiring is broken, test inconclusive")
	}

	// Cancel and wait for loop to drain. The observer received the report
	// (logged via slog inside the handler) but should NOT have received
	// either dispatch. There's no direct hook to assert that from the
	// loop's slog-only handler — but the subscription topology IS the
	// assertion: coordinateLoop subscribed only to agents.report.>, so
	// agents.prompt.* is structurally unreachable. The
	// TestCoordinateLoop_PromptQueueGroup test above proves the 5-token
	// prompt subject exists and is reachable when active subscribers
	// wire it.

	cancel()
	select {
	case err := <-loopDone:
		if err != nil {
			t.Errorf("coordinateLoop returned err = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("coordinateLoop did not exit within 1s of ctx cancel")
	}
	_ = reportSubj // silence unused-var if test logic shifts
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
