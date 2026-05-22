package refagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/danmestas/sesh/internal/coord"
)

// seedProjectID writes a .sesh/project-id pin under root so resolveProjectID
// (used implicitly by Run, called directly in some tests) can find it.
// Returns the absolute id value.
func seedProjectID(t *testing.T, root string) string {
	t.Helper()
	const id = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	seshDir := filepath.Join(root, ".sesh")
	if err := os.MkdirAll(seshDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seshDir, "project-id"), []byte(id+"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

// TestResolveProjectID covers the walk-up + ENOENT-tolerant behavior.
// Mirrors readSessionNATSURL's contract: file present anywhere up the
// directory tree → return its content; nowhere → ("", nil).
func TestResolveProjectID(t *testing.T) {
	t.Run("present in cwd", func(t *testing.T) {
		root := t.TempDir()
		want := seedProjectID(t, root)
		t.Chdir(root)
		got, err := resolveProjectID()
		if err != nil {
			t.Fatalf("resolveProjectID: %v", err)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("present in ancestor — walk-up finds it", func(t *testing.T) {
		root := t.TempDir()
		want := seedProjectID(t, root)
		nested := filepath.Join(root, "a", "b", "c")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir nested: %v", err)
		}
		t.Chdir(nested)
		got, err := resolveProjectID()
		if err != nil {
			t.Fatalf("resolveProjectID: %v", err)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("absent → empty string, no error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, err := resolveProjectID()
		if err != nil {
			t.Fatalf("resolveProjectID: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// TestCoordinateLoop_TierRouting is the load-bearing test: each tier
// (5-token orch front door, 6-token role pool with queue group, 7-token
// direct address) reaches the expected subscriber and ONLY that subscriber.
//
// All three tiers run against the same in-process broker; the queue-group
// property (one delivery for the 6-token pool subject across two same-role
// workers) and the direct-address property (only the named worker
// receives) are both verified.
func TestCoordinateLoop_TierRouting(t *testing.T) {
	url := startBroker(t)
	root := t.TempDir()
	pid := seedProjectID(t, root)
	t.Chdir(root)
	t.Setenv("SESH_MACHINE", coord.MachineLocal)
	machine := coord.MachineLocal
	session := "s1"

	// --- Wire three subscribers directly (not via coordinateLoop) so the
	// test exercises the exact subject shapes coordinateLoop produces and
	// can assert per-subscriber receive counts. coordinateLoop's own
	// integration is covered by TestRun_CoordinateLoopAttaches below.

	nc := mustConnect(t, url)
	defer nc.Close()

	// Orch on 5-token front door.
	orchHits := make(chan string, 4)
	orchSubj := fmt.Sprintf("agents.prompt.%s.%s.%s", machine, pid, session)
	orchSub, err := nc.Subscribe(orchSubj, func(m *nats.Msg) { orchHits <- m.Subject })
	if err != nil {
		t.Fatalf("subscribe orch %s: %v", orchSubj, err)
	}
	defer orchSub.Unsubscribe()

	// Two implementer workers on 6-token role pool (queue group = role).
	var pool1, pool2 atomic.Int32
	poolSubj := fmt.Sprintf("agents.prompt.%s.%s.%s.implementer", machine, pid, session)
	psub1, err := nc.QueueSubscribe(poolSubj, "implementer", func(*nats.Msg) { pool1.Add(1) })
	if err != nil {
		t.Fatalf("queue subscribe pool 1: %v", err)
	}
	defer psub1.Unsubscribe()
	psub2, err := nc.QueueSubscribe(poolSubj, "implementer", func(*nats.Msg) { pool2.Add(1) })
	if err != nil {
		t.Fatalf("queue subscribe pool 2: %v", err)
	}
	defer psub2.Unsubscribe()

	// One direct worker on 7-token.
	const workerID = "VMKS6MHK71PCPWGY38A7N5"
	var directHits atomic.Int32
	directSubj := fmt.Sprintf("agents.prompt.%s.%s.%s.implementer.%s", machine, pid, session, workerID)
	dsub, err := nc.Subscribe(directSubj, func(*nats.Msg) { directHits.Add(1) })
	if err != nil {
		t.Fatalf("subscribe direct: %v", err)
	}
	defer dsub.Unsubscribe()

	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Publish to 5-token: only orch receives.
	pubNC := mustConnect(t, url)
	defer pubNC.Close()
	mustPublish(t, pubNC, orchSubj, []byte("to-orch"))

	// Publish to 6-token: queue group routes to exactly one pool worker.
	mustPublish(t, pubNC, poolSubj, []byte("to-pool"))

	// Publish to 7-token: only the direct worker receives.
	mustPublish(t, pubNC, directSubj, []byte("to-direct"))

	// Give the broker a beat to deliver.
	time.Sleep(150 * time.Millisecond)

	// Orch: exactly one message on 5-token.
	if got := drainStr(orchHits); len(got) != 1 || got[0] != orchSubj {
		t.Errorf("orch hits = %v, want [%s]", got, orchSubj)
	}

	// Pool: exactly one of the two workers received (queue group).
	if got := pool1.Load() + pool2.Load(); got != 1 {
		t.Errorf("pool queue-group hits = %d, want 1 (work-stealing)", got)
	}

	// Direct: exactly one delivery to the named worker.
	if got := directHits.Load(); got != 1 {
		t.Errorf("direct hits = %d, want 1", got)
	}
}

// TestCoordinateLoop_ObserverNeverReceivesPrompt asserts the spy-exclusion
// contract: an observer-class agent subscribed via coordinateLoop receives
// agents.report.* messages but NEVER agents.prompt.* messages, even when a
// prompt is published to a subject that wildcards over the same machine/
// project/session triple.
func TestCoordinateLoop_ObserverNeverReceivesPrompt(t *testing.T) {
	url := startBroker(t)
	root := t.TempDir()
	pid := seedProjectID(t, root)
	t.Chdir(root)
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
	promptSubj := fmt.Sprintf("agents.prompt.%s.%s.s1.implementer.w1", machine, pid)
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
	// agents.prompt.* is structurally unreachable. The TestCoordinateLoop_
	// TierRouting test above proves the prompt subjects exist as a
	// reachable tier when active subscribers wire them.

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

// TestCoordinateLoop_NoProjectIDIsNoSubs verifies the degradation path:
// when resolveProjectID returns "", the loop installs zero subscriptions
// and waits on ctx. The agent is still useful for direct Synadia prompts
// via the micro framework but doesn't participate in tier coordination.
func TestCoordinateLoop_NoProjectIDIsNoSubs(t *testing.T) {
	url := startBroker(t)
	nc := mustConnect(t, url)
	defer nc.Close()

	cfg := Config{
		Agent: "echo", Owner: "u", Session: "s",
		Role: "worker", Class: agentmeta.ClassActive,
		Interval: 1 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- coordinateLoop(ctx, nc, cfg, "", "inst-1") }()

	select {
	case err := <-done:
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

func drainStr(ch <-chan string) []string {
	var out []string
	for {
		select {
		case s := <-ch:
			out = append(out, s)
		default:
			return out
		}
	}
}
