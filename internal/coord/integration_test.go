package coord

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startTestBroker spins up an in-process nats-server on a random port
// and returns its client URL. Same pattern as the existing tests in
// internal/refagent/agent_test.go and cli/session_agents_test.go.
func startTestBroker(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Port: -1}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("nats server new: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		srv.Shutdown()
		t.Fatalf("nats server not ready within 2s")
	}
	t.Cleanup(func() { srv.Shutdown() })
	return srv.ClientURL()
}

// TestEndToEnd_PublisherReachesProjectFilter is the load-bearing
// integration test. A publisher emits on the concrete subject from
// ProjectTaskSubject; a subscriber listens on a wildcard ProjectTaskFilter
// covering the same project; we assert the subscriber receives the
// message within 200ms.
//
// This pins that the helper constructors emit subject shapes a real
// NATS server matches per its documented wildcard semantics.
func TestEndToEnd_PublisherReachesProjectFilter(t *testing.T) {
	url := startTestBroker(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	const (
		pid     = "a3f2c1d8"
		target  = "workers"
		role    = "implementer"
		payload = "hello from publisher"
	)

	// Subscribe FIRST so the broker has the interest registered.
	filter := ProjectTaskFilter(MachineLocal, pid, target, WildOne)
	received := make(chan string, 1)
	sub, err := nc.Subscribe(filter.String(), func(msg *nats.Msg) {
		received <- string(msg.Data)
	})
	if err != nil {
		t.Fatalf("subscribe %q: %v", filter.String(), err)
	}
	defer sub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Publish on the concrete subject.
	subj := ProjectTaskSubject(MachineLocal, pid, target, role)
	if err := nc.Publish(subj.String(), []byte(payload)); err != nil {
		t.Fatalf("publish %q: %v", subj.String(), err)
	}

	select {
	case got := <-received:
		if got != payload {
			t.Errorf("payload = %q, want %q", got, payload)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("subscriber did not receive message within 200ms\n  publish: %s\n  filter:  %s", subj.String(), filter.String())
	}
}

// TestEndToEnd_QueueGroupWorkStealing verifies the per-verb queue-group
// policy translates into actual NATS work-stealing semantics. Two
// subscribers both serving role=implementer share a queue group; a
// published message reaches exactly one of them.
func TestEndToEnd_QueueGroupWorkStealing(t *testing.T) {
	url := startTestBroker(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	const (
		pid     = "a3f2c1d8"
		role    = "implementer"
		payload = "work item"
	)

	filter := ProjectTaskFilter(MachineLocal, pid, "workers", role)
	qg := VerbTask.QueueGroup(role)
	if qg == "" {
		t.Fatal("VerbTask.QueueGroup(implementer) returned empty — expected 'implementer'")
	}

	received := make(chan int, 2)
	for i := 0; i < 2; i++ {
		idx := i
		sub, err := nc.QueueSubscribe(filter.String(), qg, func(msg *nats.Msg) {
			received <- idx
		})
		if err != nil {
			t.Fatalf("queue subscribe %q: %v", filter.String(), err)
		}
		defer sub.Unsubscribe()
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	subj := ProjectTaskSubject(MachineLocal, pid, "workers", role)
	if err := nc.Publish(subj.String(), []byte(payload)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Exactly one of the two subscribers should receive within 200ms.
	count := 0
	deadline := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case <-received:
			count++
		case <-deadline:
			break loop
		}
	}
	if count != 1 {
		t.Errorf("queue group delivered to %d subscribers, want 1 (work-stealing semantics)", count)
	}
}

// TestEndToEnd_FanoutBroadcast verifies the converse: broadcast verbs
// have no queue group, so two subscribers each receive a copy.
func TestEndToEnd_FanoutBroadcast(t *testing.T) {
	url := startTestBroker(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	const (
		pid     = "a3f2c1d8"
		payload = "shared finding"
	)

	subj := WorkflowBlackboardSubject(MachineLocal, "a1b2c3d4", "findings", "research")
	if subj.QueueGroup() != "" {
		t.Fatalf("blackboard.QueueGroup() = %q, want '' (fan-out)", subj.QueueGroup())
	}

	received := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		// No queue group — plain Subscribe.
		sub, err := nc.Subscribe(subj.String(), func(*nats.Msg) {
			received <- struct{}{}
		})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		defer sub.Unsubscribe()
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := nc.Publish(subj.String(), []byte(payload)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	count := 0
	deadline := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case <-received:
			count++
			if count == 2 {
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	if count != 2 {
		t.Errorf("fan-out delivered to %d subscribers, want 2", count)
	}
	_ = pid // unused; pid present for symmetry with siblings
}
