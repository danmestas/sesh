package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// startTestNATSServer starts an in-process NATS server on a random port
// and returns the server and its client URL. The server is shut down when
// the test ends.
func startTestNATSServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	opts := &server.Options{Port: -1} // random port
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("nats server new: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatalf("nats server not ready within 5s")
	}
	t.Cleanup(s.Shutdown)
	return s, s.ClientURL()
}

// registerTestAgent registers a synthetic NATS micro service named "agents"
// with the given metadata on nc, and returns a stop function. This mimics
// what a real sesh agent (e.g. claude-code) would do per the Synadia §3
// registration contract.
func registerTestAgent(t *testing.T, nc *nats.Conn, agentName, owner, sessionLabel string) micro.Service {
	t.Helper()
	subject := "agents.prompt." + agentName + "." + owner + "." + sessionLabel
	svc, err := micro.AddService(nc, micro.Config{
		Name:        "agents",
		Version:     "0.1.0",
		Description: agentName + " test agent",
		Metadata: map[string]string{
			"agent":            agentName,
			"owner":            owner,
			"session":          sessionLabel,
			"protocol_version": "0.3",
		},
		Endpoint: &micro.EndpointConfig{
			Subject: subject,
			Handler: micro.HandlerFunc(func(req micro.Request) {
				_ = req.Respond([]byte("ok"))
			}),
		},
	})
	if err != nil {
		t.Fatalf("register micro service %q: %v", agentName, err)
	}
	// Flush so the SRV.INFO.agents subscription is registered with the server
	// before the caller publishes a discovery request — otherwise the request
	// races subscription installation and the test sees zero replies.
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after register: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
	return svc
}

// TestAgentWatcher_RegistrationUpdatesFile verifies that after an agent
// registers as an "agents" micro service, the session JSON agents[] array
// is updated within a few seconds. After the agent is stopped, agents[]
// is cleared on the next poll.
//
// This is the "registration → file-update integration test" from the issue.
func TestAgentWatcher_RegistrationUpdatesFile(t *testing.T) {
	_, natsURL := startTestNATSServer(t)

	// Claim a session and set up the state file with the NATS URL.
	dir := t.TempDir()
	sess, err := ClaimSession(dir, "mytest")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release() })

	if err := sess.Publish(SessionState{
		PID:     os.Getpid(),
		NATSURL: natsURL,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Connect two clients: one for the watcher, one for the test agent.
	watcherNC, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("watcher connect: %v", err)
	}
	defer watcherNC.Close()

	agentNC, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("agent connect: %v", err)
	}
	defer agentNC.Close()

	// Register a test agent before the watcher runs.
	svc := registerTestAgent(t, agentNC, "claude-code", "testuser", "mytest")

	// Poll up to 2s for the agent to appear. Under parallel load the first
	// discovery request can arrive at the server before the agent's INFO
	// handler subscription is installed; a short retry absorbs that race.
	var agents []AgentRef
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		agents = queryAgents(watcherNC, "mytest", 500*time.Millisecond)
		if len(agents) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(agents) == 0 {
		t.Fatal("queryAgents returned no agents after registration")
	}
	if err := sess.UpdateAgents(agents); err != nil {
		t.Fatalf("UpdateAgents: %v", err)
	}

	// Verify the JSON on disk contains the agent.
	path := filepath.Join(dir, "mytest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(state.Agents) == 0 {
		t.Fatalf("agents[] empty after registration; state = %+v", state)
	}
	found := false
	for _, a := range state.Agents {
		if a.Agent == "claude-code" && a.Owner == "testuser" && a.InstanceID == svc.Info().ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("claude-code not found in agents[]; got %+v", state.Agents)
	}

	// Stop the agent. Next poll should clear agents[].
	if err := svc.Stop(); err != nil {
		t.Fatalf("stop svc: %v", err)
	}
	// Flush to ensure unsubscribes have been delivered to the server.
	if err := agentNC.Flush(); err != nil {
		t.Fatalf("flush after stop: %v", err)
	}

	agents2 := queryAgents(watcherNC, "mytest", 500*time.Millisecond)
	if err := sess.UpdateAgents(agents2); err != nil {
		t.Fatalf("UpdateAgents after stop: %v", err)
	}

	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session file after stop: %v", err)
	}
	var state2 SessionState
	if err := json.Unmarshal(data2, &state2); err != nil {
		t.Fatalf("unmarshal after stop: %v", err)
	}
	if len(state2.Agents) != 0 {
		t.Fatalf("agents[] not empty after deregistration; got %+v", state2.Agents)
	}
}

// TestAgentWatcher_SessionFilter verifies that agents belonging to a
// different session are excluded from agents[].
func TestAgentWatcher_SessionFilter(t *testing.T) {
	_, natsURL := startTestNATSServer(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	// Register two agents: one for "mytest" session, one for "other" session.
	registerTestAgent(t, nc, "agent-a", "user1", "mytest")
	registerTestAgent(t, nc, "agent-b", "user1", "other")

	// Poll until agent-a appears, then assert agent-b is filtered out.
	var agents []AgentRef
	agentAFound := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		agents = queryAgents(nc, "mytest", 500*time.Millisecond)
		for _, a := range agents {
			if a.Agent == "agent-a" {
				agentAFound = true
			}
		}
		if agentAFound {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !agentAFound {
		t.Fatalf("agent-a (correct session) not found; got %+v", agents)
	}
	for _, a := range agents {
		if a.Agent == "agent-b" {
			t.Fatalf("agent from different session leaked into agents[]: %+v", a)
		}
	}
}

// TestUpdateAgents_AtomicWrite verifies that UpdateAgents never leaves a
// partial file visible to a concurrent reader. The test runs a tight reader
// loop while the writer performs many UpdateAgents calls and checks that
// every read produces valid JSON.
func TestUpdateAgents_AtomicWrite(t *testing.T) {
	dir := t.TempDir()

	sess, err := ClaimSession(dir, "atomic")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release() })

	if err := sess.Publish(SessionState{PID: os.Getpid(), NATSURL: "nats://127.0.0.1:9999"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	path := filepath.Join(dir, "atomic.json")
	const iters = 500

	// Writer goroutine alternates between an empty and a non-empty agents[].
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < iters; i++ {
			var agents []AgentRef
			if i%2 == 0 {
				agents = []AgentRef{{Agent: "test", Owner: "user", InstanceID: "ABC123", Subject: "agents.prompt.test.user.atomic"}}
			}
			if err := sess.UpdateAgents(agents); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return
				}
				// Log but don't fail — writer errors are expected on shutdown.
				t.Logf("UpdateAgents: %v", err)
				return
			}
		}
	}()

	// Reader loop: every file read must be valid JSON.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-writerDone:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				// File may briefly not exist at transitions — acceptable.
				continue
			}
			if len(data) == 0 {
				t.Errorf("reader saw empty file (partial write?)")
				return
			}
			var s SessionState
			if err := json.Unmarshal(data, &s); err != nil {
				t.Errorf("reader saw invalid JSON (partial write?): %v | data=%q", err, data)
				return
			}
		}
	}()

	<-writerDone
	<-readerDone
}

// TestUpdateAgents_SessionGone verifies that UpdateAgents returns
// fs.ErrNotExist when the session file has been removed, so the watcher
// goroutine can exit cleanly.
func TestUpdateAgents_SessionGone(t *testing.T) {
	dir := t.TempDir()

	sess, err := ClaimSession(dir, "ghost")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Remove the session file without calling Release (simulates hub crash).
	if err := os.Remove(filepath.Join(dir, "ghost.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	err = sess.UpdateAgents([]AgentRef{{Agent: "x", Owner: "y", InstanceID: "z"}})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected fs.ErrNotExist, got %v", err)
	}
}

// TestSessionState_BackCompatAgents verifies that a session JSON without
// agents[] parses without error and Agents is nil/empty.
func TestSessionState_BackCompatAgents(t *testing.T) {
	old := []byte(`{"pid":99,"nats_url":"nats://127.0.0.1:4222"}`)
	var s SessionState
	if err := json.Unmarshal(old, &s); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if s.PID != 99 {
		t.Errorf("pid = %d, want 99", s.PID)
	}
	if len(s.Agents) != 0 {
		t.Errorf("agents[] = %v, want empty", s.Agents)
	}
}

// TestConnectWithBackoff_RetriesUntilServerUp verifies that the initial
// NATS connect retry loop survives an unreachable URL and succeeds once
// the server comes up — closing the race between bindHub and runAgentWatcher.
func TestConnectWithBackoff_RetriesUntilServerUp(t *testing.T) {
	// Capture a free port without holding it: bind, read the port, close.
	// A racer could grab the port before the server below claims it, but on
	// a quiet test host this is overwhelmingly unlikely.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	url := "nats://127.0.0.1:" + strconv.Itoa(port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// After a delay, bring a NATS server up on the captured port.
	// connectWithBackoff's early dials must fail; later dials must succeed.
	serverUp := make(chan *server.Server, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		opts := &server.Options{Port: port}
		s2, err := server.NewServer(opts)
		if err != nil {
			t.Errorf("start server: %v", err)
			return
		}
		go s2.Start()
		if !s2.ReadyForConnections(5 * time.Second) {
			t.Errorf("server not ready")
			return
		}
		serverUp <- s2
	}()

	nc, ok := connectWithBackoff(ctx, url)
	if !ok {
		t.Fatalf("connectWithBackoff returned !ok; expected eventual success")
	}
	defer nc.Close()
	s2 := <-serverUp
	t.Cleanup(s2.Shutdown)
}

// TestConnectWithBackoff_HonorsCtxCancel verifies the retry loop exits
// promptly when ctx is cancelled, even if the server is unreachable.
func TestConnectWithBackoff_HonorsCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 200ms — well within the first backoff window.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	nc, ok := connectWithBackoff(ctx, "nats://127.0.0.1:1") // unreachable
	if ok || nc != nil {
		t.Fatalf("expected connectWithBackoff to fail on ctx cancel; got ok=%v nc=%v", ok, nc)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("connectWithBackoff didn't exit promptly on ctx cancel: %v", elapsed)
	}
}

// ---------- AgentRef role / class -------------------------------------

// TestAgentRef_JSONShapeIncludesRoleAndClass pins the wire shape — the JSON
// keys the session manifest exposes to dashboards / sesh-ops / outside tools.
func TestAgentRef_JSONShapeIncludesRoleAndClass(t *testing.T) {
	ref := AgentRef{
		Agent:      "claude-code",
		Owner:      "dmestas",
		InstanceID: "ABC123",
		Subject:    "agents.prompt.m4-host.sesh.foo.implementer",
		Role:       "implementer",
		Class:      "active",
	}
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []string{
		`"agent":"claude-code"`,
		`"owner":"dmestas"`,
		`"instance_id":"ABC123"`,
		`"subject":"agents.prompt.m4-host.sesh.foo.implementer"`,
		`"role":"implementer"`,
		`"class":"active"`,
	}
	got := string(b)
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("marshaled AgentRef missing %s\nfull: %s", w, got)
		}
	}
}

// TestSessionJSON_RoleAndClassRoundTrip writes an agents[] slice with role
// + class through UpdateAgents and reads back the on-disk JSON. This pins
// the JSON tag wiring — a stray `json:"-"` on Role would pass Task 4 but
// fail here.
func TestSessionJSON_RoleAndClassRoundTrip(t *testing.T) {
	dir := t.TempDir()

	sess, err := ClaimSession(dir, "rc")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release() })

	if err := sess.Publish(SessionState{PID: os.Getpid(), NATSURL: "nats://127.0.0.1:9999"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	agents := []AgentRef{
		{
			Agent:      "claude-code",
			Owner:      "dmestas",
			InstanceID: "ABC",
			Subject:    "agents.prompt.m4-host.sesh.rc.implementer",
			Role:       "implementer",
			Class:      "active",
		},
		{
			Agent:      "pi",
			Owner:      "dmestas",
			InstanceID: "XYZ",
			Subject:    "agents.prompt.m4-host.sesh.rc.spy",
			Role:       "spy",
			Class:      "observer",
		},
	}
	if err := sess.UpdateAgents(agents); err != nil {
		t.Fatalf("UpdateAgents: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "rc.json"))
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`"role":"implementer"`,
		`"class":"active"`,
		`"role":"spy"`,
		`"class":"observer"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session JSON missing %s\nfull:\n%s", want, body)
		}
	}
}

// TestAgentWatcher_PopulatesRoleAndClassFromMetadata verifies the watcher
// reads metadata.role / metadata.class from $SRV.INFO.agents and stores
// them on AgentRef. Covers both classes (active and observer).
func TestAgentWatcher_PopulatesRoleAndClassFromMetadata(t *testing.T) {
	_, url := startTestNATSServer(t)

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	subject1 := "agents.prompt.claude-code.dmestas.testlabel"
	svc1, err := micro.AddService(nc, micro.Config{
		Name:    "agents",
		Version: "0.1.0",
		Metadata: map[string]string{
			"agent":   "claude-code",
			"owner":   "dmestas",
			"session": "testlabel",
			"role":    "implementer",
			"class":   "active",
		},
	})
	if err != nil {
		t.Fatalf("svc1: %v", err)
	}
	defer svc1.Stop()
	_ = svc1.AddEndpoint("prompt", micro.HandlerFunc(func(m micro.Request) { _ = m.Respond(nil) }),
		micro.WithEndpointSubject(subject1))

	subject2 := "agents.prompt.pi.dmestas.testlabel"
	svc2, err := micro.AddService(nc, micro.Config{
		Name:    "agents",
		Version: "0.1.0",
		Metadata: map[string]string{
			"agent":   "pi",
			"owner":   "dmestas",
			"session": "testlabel",
			"role":    "spy",
			"class":   "observer",
		},
	})
	if err != nil {
		t.Fatalf("svc2: %v", err)
	}
	defer svc2.Stop()
	_ = svc2.AddEndpoint("prompt", micro.HandlerFunc(func(m micro.Request) { _ = m.Respond(nil) }),
		micro.WithEndpointSubject(subject2))

	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Poll up to 2s for both agents to appear.
	var agents []AgentRef
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		agents = queryAgents(nc, "testlabel", 500*time.Millisecond)
		if len(agents) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(agents) != 2 {
		t.Fatalf("queryAgents returned %d agents, want 2", len(agents))
	}

	byAgent := map[string]AgentRef{}
	for _, a := range agents {
		byAgent[a.Agent] = a
	}
	if byAgent["claude-code"].Role != "implementer" {
		t.Errorf("claude-code Role = %q, want implementer", byAgent["claude-code"].Role)
	}
	if byAgent["claude-code"].Class != "active" {
		t.Errorf("claude-code Class = %q, want active", byAgent["claude-code"].Class)
	}
	if byAgent["pi"].Role != "spy" {
		t.Errorf("pi Role = %q, want spy", byAgent["pi"].Role)
	}
	if byAgent["pi"].Class != "observer" {
		t.Errorf("pi Class = %q, want observer", byAgent["pi"].Class)
	}
}

// TestAgentWatcher_DefaultsForMissingRoleClassMetadata covers back-compat:
// an old adapter that registered before the role/class fields existed must
// still appear in agents[] with the canonical defaults.
func TestAgentWatcher_DefaultsForMissingRoleClassMetadata(t *testing.T) {
	_, url := startTestNATSServer(t)

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	subject := "agents.prompt.legacy.dmestas.bclabel"
	svc, err := micro.AddService(nc, micro.Config{
		Name:    "agents",
		Version: "0.1.0",
		Metadata: map[string]string{
			"agent":   "legacy",
			"owner":   "dmestas",
			"session": "bclabel",
			// No role / class — back-compat.
		},
	})
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	defer svc.Stop()
	_ = svc.AddEndpoint("prompt", micro.HandlerFunc(func(m micro.Request) { _ = m.Respond(nil) }),
		micro.WithEndpointSubject(subject))

	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var agents []AgentRef
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		agents = queryAgents(nc, "bclabel", 500*time.Millisecond)
		if len(agents) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(agents) != 1 {
		t.Fatalf("queryAgents returned %d, want 1", len(agents))
	}
	if agents[0].Role != "worker" {
		t.Errorf("default Role = %q, want worker", agents[0].Role)
	}
	if agents[0].Class != "active" {
		t.Errorf("default Class = %q, want active", agents[0].Class)
	}
}

// TestAgentRef_JSONOmitsEmptyRoleAndClass asserts the omitempty tags work —
// an old AgentRef constructed before the watcher knew about role/class
// must not pollute the session JSON with empty strings.
func TestAgentRef_JSONOmitsEmptyRoleAndClass(t *testing.T) {
	ref := AgentRef{
		Agent:      "claude-code",
		Owner:      "dmestas",
		InstanceID: "ABC123",
		Subject:    "agents.prompt.m4-host.sesh.foo.worker",
	}
	b, _ := json.Marshal(ref)
	got := string(b)
	if strings.Contains(got, `"role"`) {
		t.Errorf("expected role to be omitted when empty: %s", got)
	}
	if strings.Contains(got, `"class"`) {
		t.Errorf("expected class to be omitted when empty: %s", got)
	}
}
