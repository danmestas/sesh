//go:build !orch_e2e

package cli_test

// End-to-end verification of the collapsed coordination model. Spawns a
// real `sesh up` session, launches four real `sesh-ref-agent` processes
// (orch + two implementer workers + one observer spy), then drives the
// full stack through real NATS publish/subscribe to verify:
//
//  - All four agents register on $SRV.INFO.agents with role + class metadata.
//  - Heartbeats on agents.hb.* carry the role/class sesh-extension fields.
//  - Prompt routing on the single 5-token subject
//    agents.prompt.<machine>.<project>.<session>:
//      every active agent QueueSubscribes it under the fixed prompt queue
//      group, so a publish reaches EXACTLY ONE active agent (work-stealing
//      across orch + the two implementers); the role no longer selects a
//      subject tier.
//  - Spy exclusion: observer-class agent does NOT receive any
//    agents.prompt.* dispatch; DOES receive agents.report.* messages.
//  - Synadia §8.6 shutdown heartbeat: on graceful SIGTERM, the dying
//    agent publishes an empty-payload final heartbeat before exiting.
//
// Build-tag exclusion: this test takes ~15s end-to-end (subprocess
// startup, registration latency, queue-group fairness sampling). Runs in
// the default suite when -short is not set, like the other cli/e2e_*
// tests in this package.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestE2E_CoordTiers_FullStack is the load-bearing tier verification.
// One test rather than five: the four agents are expensive to spawn,
// so we share one fixture and assert each property in subtests.
func TestE2E_CoordTiers_FullStack(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (E2E coord tiers)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	seshBin := buildSesh(t)
	refagentBin := buildRefAgent(t)

	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	const sessionLabel = "coord-e2e"

	// sesh is a NATS client now — it dials an EXTERNAL hub rather than
	// embedding one. Stand up a real nats-server for the session + the
	// ref-agents to share, and point `sesh up` at it via SESH_HUB_URL.
	hubURL := startExternalNATSServer(t)

	// Bring up the sesh session as a client of the external hub. This writes
	// the .sesh/project-id pin we read host-side and inject into each refagent
	// as SESH_PROJECT_ID (the refagent no longer walks the filesystem for it),
	// and publishes the hub URL into the session JSON.
	seshCmd, seshStderr := startSeshArgs(t, seshBin, home, project, sessionLabel, hubURL)
	defer killAndWait(t, seshCmd, seshStderr)
	sess := waitForURLs(t, filepath.Join(project, ".sesh", "sessions", sessionLabel+".json"), 15*time.Second)
	if sess.NATSURL == "" {
		t.Fatalf("sesh session reported no NATS URL after 15s")
	}
	if sess.NATSURL != hubURL {
		t.Fatalf("session NATS URL = %q, want external hub URL %q", sess.NATSURL, hubURL)
	}

	pid := readProjectID(t, project)
	machine := "_local" // refagent's coord.Machine() with no SESH_MACHINE set

	// Spawn four agents. Each gets the same NATS URL + project + session
	// but distinct role/class via env. Role no longer selects a prompt
	// subject tier — all three active agents subscribe to the same 5-token
	// prompt subject under the shared queue group.
	type agentSpec struct {
		agent string
		role  string
		class string
	}
	specs := []agentSpec{
		{agent: "orch", role: "orch", class: "active"},
		{agent: "imp1", role: "implementer", class: "active"},
		{agent: "imp2", role: "implementer", class: "active"},
		{agent: "spy1", role: "spy", class: "observer"},
	}

	// Connect a test-side NATS client to query / publish / capture
	// heartbeats. Subscribe to agents.hb.> BEFORE spawning agents so
	// we catch the immediate-on-startup heartbeat (refagent.Run line ~148)
	// — otherwise the 30s default interval means we'd wait until t+30s
	// for the next periodic, far longer than the test's 4s capture window.
	nc, err := nats.Connect(sess.NATSURL)
	if err != nil {
		t.Fatalf("test NATS connect: %v", err)
	}
	defer nc.Close()

	hbCapture := make(chan *nats.Msg, 128)
	hbSub, err := nc.ChanSubscribe("agents.hb.>", hbCapture)
	if err != nil {
		t.Fatalf("hb capture subscribe: %v", err)
	}
	defer hbSub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush hb subscribe: %v", err)
	}

	procs := make([]*exec.Cmd, len(specs))
	stderrs := make([]*syncBuf, len(specs))
	for i, sp := range specs {
		cmd, sb := startRefAgent(t, refagentBin, sess.NATSURL, project, pid, sessionLabel, sp.agent, sp.role, sp.class)
		procs[i] = cmd
		stderrs[i] = sb
	}
	t.Cleanup(func() {
		for i, c := range procs {
			killAndWait(t, c, stderrs[i])
		}
	})

	// Wait for all four agents to register on $SRV.INFO.agents.
	agents := waitForAgentRegistration(t, nc, sessionLabel, 4, 10*time.Second)
	instanceByRoleAgent := map[string]string{}
	for _, a := range agents {
		instanceByRoleAgent[a.Metadata["agent"]] = a.ID
	}
	for _, sp := range specs {
		if _, ok := instanceByRoleAgent[sp.agent]; !ok {
			t.Fatalf("agent %s never registered\nstderr:\n%s", sp.agent, stderrAll(stderrs))
		}
	}

	// === Subtest 1: heartbeats carry role + class extension fields ===
	t.Run("heartbeats include role and class", func(t *testing.T) {
		hbs := collectHeartbeatsFromChannel(t, hbCapture, sessionLabel, 4, 5*time.Second)
		if len(hbs) < 4 {
			t.Fatalf("only %d heartbeats captured, want ≥4: %+v\nstderr:\n%s", len(hbs), hbs, stderrAll(stderrs))
		}
		seen := map[string]string{} // agent → class
		for _, hb := range hbs {
			if r, ok := hb["role"].(string); ok {
				if c, ok := hb["class"].(string); ok {
					seen[hb["agent"].(string)] = r + "/" + c
				}
			}
		}
		want := map[string]string{
			"orch": "orch/active", "imp1": "implementer/active",
			"imp2": "implementer/active", "spy1": "spy/observer",
		}
		for agent, expected := range want {
			if got := seen[agent]; got != expected {
				t.Errorf("heartbeat for %s: role/class = %q, want %q", agent, got, expected)
			}
		}
	})

	// === Subtest 2: a 5-token publish reaches exactly one active agent ===
	t.Run("5-token prompt reaches one active agent", func(t *testing.T) {
		subj := fmt.Sprintf("agents.prompt.%s.%s.%s", machine, pid, sessionLabel)
		ack := mustRequest(t, nc, subj, []byte("dispatch"), 2*time.Second)
		// Any of the three active agents may answer (queue group); the spy
		// (observer) must never answer.
		role := jsonString(t, ack, "role")
		class := jsonString(t, ack, "class")
		if class != "active" {
			t.Errorf("5-token responder class = %q, want active", class)
		}
		switch role {
		case "orch", "implementer":
			// expected — one active agent answered
		default:
			t.Errorf("5-token responder role = %q, want orch or implementer", role)
		}
	})

	// === Subtest 3: fan-out on the 5-token subject work-steals across the
	// active agents via the shared prompt queue group ===
	t.Run("5-token work-stealing across active agents", func(t *testing.T) {
		subj := fmt.Sprintf("agents.prompt.%s.%s.%s", machine, pid, sessionLabel)
		// Publish 30 messages; each should reach exactly one active agent
		// (queue group). Distribution doesn't have to be even — NATS picks a
		// queue subscriber per-message and fairness over small N is loose.
		// Load-bearing properties:
		//   - every message gets exactly one ack
		//   - more than one distinct active agent participates (work-stealing,
		//     not a single sticky subscriber)
		//   - no observer ever answers
		seen := map[string]int{}
		for i := 0; i < 30; i++ {
			ack := mustRequest(t, nc, subj, []byte(fmt.Sprintf("work-%d", i)), 2*time.Second)
			id := jsonString(t, ack, "instance_id")
			class := jsonString(t, ack, "class")
			if class != "active" {
				t.Errorf("work-stealing responder class = %q, want active", class)
			}
			seen[id]++
		}
		if got := len(seen); got < 2 {
			t.Errorf("queue-group fan-in: %d distinct responders over 30 messages, want ≥2 (work-stealing across active agents)", got)
		}
	})

	// === Subtest 5: spy exclusion — no agents.prompt.* reaches observer ===
	t.Run("spy exclusion: observer never receives agents.prompt", func(t *testing.T) {
		// Publish to the 5-token prompt subject AND to longer spy-keyed
		// subjects, then verify the spy never responds. The observer
		// subscribes only to agents.report.>, so no agents.prompt.* subject
		// should reach it.
		spyInstance := instanceByRoleAgent["spy1"]
		frontSubj := fmt.Sprintf("agents.prompt.%s.%s.%s", machine, pid, sessionLabel)
		spySubj := frontSubj + ".spy." + spyInstance

		// Test passes when each request reaching the spy TIMES OUT (no spy
		// responder). The 5-token subject IS answered by active agents, so
		// we only assert the spy never identifies itself as the responder.
		ack, err := nc.Request(frontSubj, []byte("dispatch"), 2*time.Second)
		if err == nil {
			if role := jsonString(t, ack.Data, "class"); role == "observer" {
				t.Errorf("observer answered the 5-token prompt subject — verb-based spy exclusion violated")
			}
		}

		// Spy-keyed subject has no subscriber at all; expect a timeout/no-
		// responder.
		for _, subj := range []string{spySubj} {
			_, err := nc.Request(subj, []byte("forbidden"), 400*time.Millisecond)
			if err == nil {
				t.Errorf("spy responded to agents.prompt subject %q — verb-based spy exclusion violated", subj)
				continue
			}
			// Acceptable errors: timeout (no subscriber installed at all)
			// or ErrNoResponders (broker saw no interest).
		}
	})

	// === Subtest 6: spy DOES receive agents.report.* messages ===
	t.Run("spy receives agents.report", func(t *testing.T) {
		reportSubj := fmt.Sprintf("agents.report.%s.%s.%s.workers.status", machine, pid, sessionLabel)
		ack := mustRequest(t, nc, reportSubj, []byte("status"), 2*time.Second)
		role := jsonString(t, ack, "role")
		if role != "spy" {
			t.Errorf("agents.report responder role = %q, want spy", role)
		}
		class := jsonString(t, ack, "class")
		if class != "observer" {
			t.Errorf("agents.report responder class = %q, want observer", class)
		}
	})

	// === Subtest 7: §8.6 shutdown heartbeat ===
	t.Run("Synadia §8.6 shutdown emits empty heartbeat", func(t *testing.T) {
		// Subscribe to the heartbeat subject of imp2 BEFORE killing it.
		// Then SIGTERM; wait for an empty-payload message.
		imp2Cmd := procs[2] // specs[2] is imp2
		hbSubj := fmt.Sprintf("agents.hb.imp2.%s.%s", currentUser(t), sessionLabel)
		// (refagent's owner defaults to $USER if SESH_OWNER unset; startRefAgent
		// sets SESH_OWNER explicitly to match.)

		hbCh := make(chan *nats.Msg, 8)
		sub, err := nc.Subscribe(hbSubj, func(m *nats.Msg) { hbCh <- m })
		if err != nil {
			t.Fatalf("hb subscribe: %v", err)
		}
		defer sub.Unsubscribe()
		if err := nc.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}

		// Drain any in-flight periodic heartbeats first so we identify
		// the §8.6 final one unambiguously.
		drainHB(hbCh, 200*time.Millisecond)

		// SIGTERM imp2.
		if err := imp2Cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatalf("signal imp2: %v", err)
		}

		// Wait up to 3s for an empty-payload heartbeat. The cancel-then-
		// nc.Drain in refagent.Run typically takes <500ms but we allow
		// headroom for slow CI.
		deadline := time.After(3 * time.Second)
		var sawEmpty bool
		for !sawEmpty {
			select {
			case msg := <-hbCh:
				if len(msg.Data) == 0 {
					sawEmpty = true
				}
				// Otherwise it's a periodic; keep waiting.
			case <-deadline:
				t.Fatalf("no §8.6 empty-payload heartbeat within 3s of SIGTERM")
			}
		}
	})
}

// ---- E2E helpers ----

func buildRefAgent(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	bin := filepath.Join(t.TempDir(), "sesh-ref-agent")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/sesh-ref-agent")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build sesh-ref-agent: %v\n%s", err, out)
	}
	return bin
}

func startRefAgent(t *testing.T, bin, natsURL, project, projectID, session, agent, role, class string) (*exec.Cmd, *syncBuf) {
	t.Helper()
	cmd := exec.Command(bin, "--agent="+agent)
	cmd.Dir = project // inherits cwd; identity is injected via SESH_PROJECT_ID below
	cmd.Env = append(os.Environ(),
		"NATS_URL="+natsURL,
		"SESH_OWNER="+currentUser(t),
		"SESH_SESSION="+session,
		"SESH_PROJECT_ID="+projectID,
		"SESH_ROLE="+role,
		"SESH_CLASS="+class,
		"SESH_MACHINE=_local",
	)
	sb := &syncBuf{}
	cmd.Stdout = sb
	cmd.Stderr = sb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start refagent %s: %v", agent, err)
	}
	return cmd, sb
}

func readProjectID(t *testing.T, project string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	path := filepath.Join(project, ".sesh", "project-id")
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("project-id pin %s never appeared", path)
	return ""
}

func currentUser(t *testing.T) string {
	t.Helper()
	u := os.Getenv("USER")
	if u == "" {
		t.Fatal("USER env not set — cannot derive expected refagent owner")
	}
	return u
}

// microInfoReply mirrors the subset of $SRV.INFO.agents we parse in tests.
type microInfoReply struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

// waitForAgentRegistration polls $SRV.INFO.agents until at least `want`
// agents for sessionLabel respond, or the deadline elapses.
func waitForAgentRegistration(t *testing.T, nc *nats.Conn, sessionLabel string, want int, timeout time.Duration) []microInfoReply {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []microInfoReply
	for time.Now().Before(deadline) {
		last = queryAgentsForSession(nc, sessionLabel, 500*time.Millisecond)
		if len(last) >= want {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("only %d agents registered after %s, want %d: %+v", len(last), timeout, want, last)
	return last
}

func queryAgentsForSession(nc *nats.Conn, sessionLabel string, window time.Duration) []microInfoReply {
	inbox := nats.NewInbox()
	replies := make(chan *nats.Msg, 32)
	sub, _ := nc.ChanSubscribe(inbox, replies)
	defer sub.Unsubscribe()
	_ = nc.PublishRequest("$SRV.INFO.agents", inbox, nil)
	deadline := time.Now().Add(window)
	var out []microInfoReply
	for time.Now().Before(deadline) {
		select {
		case msg := <-replies:
			var info microInfoReply
			if json.Unmarshal(msg.Data, &info) == nil && info.Metadata["session"] == sessionLabel {
				out = append(out, info)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	return out
}

// collectHeartbeatsFromChannel drains an already-installed agents.hb.>
// channel subscription, waiting up to `window` for at least `want`
// distinct agents (filtered by session) to publish. The channel must be
// subscribed BEFORE the agents start so the immediate-on-startup
// heartbeat (refagent.Run's pre-tick publish) isn't lost.
func collectHeartbeatsFromChannel(t *testing.T, ch <-chan *nats.Msg, sessionLabel string, want int, window time.Duration) []map[string]any {
	t.Helper()
	seen := map[string]map[string]any{} // agent → payload
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	for len(seen) < want {
		select {
		case msg := <-ch:
			if len(msg.Data) == 0 {
				continue // skip §8.6 empty-payload shutdown beats
			}
			var p map[string]any
			if err := json.Unmarshal(msg.Data, &p); err != nil {
				continue
			}
			if s, _ := p["session"].(string); s != sessionLabel {
				continue
			}
			if a, ok := p["agent"].(string); ok {
				seen[a] = p
			}
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	out := make([]map[string]any, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	return out
}

func mustRequest(t *testing.T, nc *nats.Conn, subj string, body []byte, timeout time.Duration) []byte {
	t.Helper()
	msg, err := nc.Request(subj, body, timeout)
	if err != nil {
		t.Fatalf("request %s: %v", subj, err)
	}
	return msg.Data
}

func jsonString(t *testing.T, raw []byte, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse ack %q: %v", raw, err)
	}
	s, _ := m[key].(string)
	return s
}

func drainHB(ch <-chan *nats.Msg, window time.Duration) {
	deadline := time.After(window)
	for {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

func stderrAll(sbs []*syncBuf) string {
	var out strings.Builder
	for i, sb := range sbs {
		fmt.Fprintf(&out, "=== agent %d stderr ===\n", i)
		out.WriteString(sb.String())
	}
	return out.String()
}
