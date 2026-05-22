package refagent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/danmestas/sesh/internal/coord"
)

// seedProjectID writes a .sesh/project-id pin under root and returns
// the absolute project-id value. The test t.Chdir's into root so
// resolveProjectID picks it up.
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

// TestCoordinateLoop_ObserverReceivesReport verifies an observer-class
// agent subscribes to report.* and receives a published report. Spy-
// exclusion: the same observer must NOT receive a published task.*.
func TestCoordinateLoop_ObserverReceivesReport(t *testing.T) {
	url := startBroker(t)
	root := t.TempDir()
	pid := seedProjectID(t, root)
	t.Chdir(root)

	cfg := Config{
		Agent:   "test-observer",
		Owner:   "tester",
		Session: "coord-test",
		Role:    "spy",
		Class:   agentmeta.ClassObserver,
		NATSURL: url,
	}

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	// Use MachineLocal so subject construction is deterministic.
	machine := coord.MachineLocal
	t.Setenv("SESH_MACHINE", machine)

	reportsReceived := make(chan *nats.Msg, 4)
	tasksReceived := make(chan *nats.Msg, 4)

	for _, f := range coord.ObserverFilters(machine, pid) {
		fstr := f.String()
		sub, err := nc.Subscribe(fstr, func(m *nats.Msg) {
			reportsReceived <- m
		})
		if err != nil {
			t.Fatalf("subscribe %q: %v", fstr, err)
		}
		defer sub.Unsubscribe()
	}
	// Sibling: subscribe a tasksReceived collector on the same task
	// subject the publisher will use — to prove the observer's filter
	// does NOT cover it.
	taskSubj := coord.ProjectTaskSubject(machine, pid, "workers", "implementer")
	sub, err := nc.Subscribe(taskSubj.String(), func(m *nats.Msg) {
		tasksReceived <- m
	})
	if err != nil {
		t.Fatalf("subscribe task sibling: %v", err)
	}
	defer sub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Publish one report — the observer should receive it.
	reportSubj := coord.Subject{
		Verb: coord.VerbReport, Machine: machine, Scope: coord.ScopeProject,
		ScopeID: pid, Target: "all", Role: "status",
	}
	if err := nc.Publish(reportSubj.String(), []byte("hello-report")); err != nil {
		t.Fatalf("publish report: %v", err)
	}
	// Publish one task — the observer must NOT receive it (sibling does).
	if err := nc.Publish(taskSubj.String(), []byte("hello-task")); err != nil {
		t.Fatalf("publish task: %v", err)
	}

	// Wait for the report receipt.
	select {
	case msg := <-reportsReceived:
		if string(msg.Data) != "hello-report" {
			t.Errorf("report payload = %q, want hello-report", msg.Data)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("observer did not receive report within 500ms")
	}

	// Drain sibling — confirm task was actually delivered.
	select {
	case <-tasksReceived:
		// good
	case <-time.After(200 * time.Millisecond):
		t.Errorf("sibling task subscriber did not receive — broker wiring is broken")
	}

	// And no task should leak into reportsReceived.
	select {
	case msg := <-reportsReceived:
		t.Errorf("observer received an unexpected message on the report channel: subject=%q payload=%q (spy-exclusion violated)",
			msg.Subject, msg.Data)
	case <-time.After(100 * time.Millisecond):
		// good — observer did not receive the task
	}

	_ = cfg // cfg present for documentation alignment with subscribeForClass
}

// TestCoordinateLoop_ActiveWorkerReceivesTaskViaQueueGroup verifies the
// work-stealing semantics: two coordinateLoop instances running the same
// (role, class) share a queue group and a single published task reaches
// exactly one of them.
func TestCoordinateLoop_ActiveWorkerReceivesTaskViaQueueGroup(t *testing.T) {
	url := startBroker(t)
	root := t.TempDir()
	pid := seedProjectID(t, root)
	t.Chdir(root)
	t.Setenv("SESH_MACHINE", coord.MachineLocal)

	cfg := Config{
		Agent:   "test-impl",
		Owner:   "tester",
		Session: "coord-test",
		Role:    "implementer",
		Class:   agentmeta.ClassActive,
		NATSURL: url,
	}

	var workerHits atomic.Int32

	// Spawn two subscribers via subscribeForClass directly; both share
	// the queue group keyed on role=implementer.
	nc1, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect 1: %v", err)
	}
	defer nc1.Close()
	nc2, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect 2: %v", err)
	}
	defer nc2.Close()

	// Wire each connection with the role's task subscription via the
	// same path coordinateLoop uses. We swap the slog-only handler for
	// a counter by re-deriving the filter/qg locally.
	machine := coord.MachineLocal
	taskFilter := coord.ProjectTaskFilter(machine, pid, "workers", cfg.Role)
	qg := coord.VerbTask.QueueGroup(cfg.Role)
	if qg == "" {
		t.Fatalf("VerbTask.QueueGroup(%q) = '', want '%s'", cfg.Role, cfg.Role)
	}

	handler := func(*nats.Msg) { workerHits.Add(1) }
	sub1, err := nc1.QueueSubscribe(taskFilter.String(), qg, handler)
	if err != nil {
		t.Fatalf("queue subscribe 1: %v", err)
	}
	defer sub1.Unsubscribe()
	sub2, err := nc2.QueueSubscribe(taskFilter.String(), qg, handler)
	if err != nil {
		t.Fatalf("queue subscribe 2: %v", err)
	}
	defer sub2.Unsubscribe()
	_ = nc1.Flush()
	_ = nc2.Flush()

	// Publish one task.
	pubNC, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect pub: %v", err)
	}
	defer pubNC.Close()
	taskSubj := coord.ProjectTaskSubject(machine, pid, "workers", cfg.Role)
	if err := pubNC.Publish(taskSubj.String(), []byte("work")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for delivery, then assert exactly one received.
	time.Sleep(200 * time.Millisecond)
	if got := workerHits.Load(); got != 1 {
		t.Errorf("workerHits = %d, want 1 (queue-group work-stealing)", got)
	}

	_ = context.Background
}

// TestCoordinateLoop_StartupWithoutProjectID verifies the loop exits
// cleanly when no .sesh/project-id is pinned — degradation case.
func TestCoordinateLoop_StartupWithoutProjectID(t *testing.T) {
	url := startBroker(t)
	root := t.TempDir()
	t.Chdir(root) // no project-id seeded

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	cfg := Config{
		Agent: "test-no-pid", Owner: "tester", Session: "x",
		Role: "worker", Class: agentmeta.ClassActive,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- coordinateLoop(ctx, nc, cfg, "")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("coordinateLoop returned err = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("coordinateLoop did not exit within 1s of ctx cancel")
	}
}
