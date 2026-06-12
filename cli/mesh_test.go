package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// registerCleanOpts describes a synthetic v0.4 agent: machine-rooted
// prompt subject (agents.prompt.<machine>.<project>.<session>) plus the
// metadata adapters now advertise (agent, owner, machine, project_id,
// session, role, capabilities). Role lives in metadata, not the subject.
type registerCleanOpts struct {
	agent    string
	owner    string
	machine  string
	project  string
	session  string
	role     string
	caps     string // sesh.v04_capabilities, e.g. "messages,artifacts,cards"
	class    string
	protoVer string
}

// registerTestAgentClean registers a synthetic "agents" micro service that
// advertises the clean v0.4 scheme — a machine-rooted prompt subject and
// the matching service metadata. Mirrors what a post-cutover adapter
// publishes. Returns the live service (stopped on test cleanup).
func registerTestAgentClean(t *testing.T, nc *nats.Conn, o registerCleanOpts) micro.Service {
	t.Helper()
	subj := "agents.prompt." + o.machine + "." + o.project + "." + o.session
	meta := map[string]string{
		"agent":      o.agent,
		"owner":      o.owner,
		"machine":    o.machine,
		"project_id": o.project,
		"session":    o.session,
		"role":       o.role,
	}
	if o.caps != "" {
		meta["sesh.v04_capabilities"] = o.caps
	}
	if o.class != "" {
		meta["class"] = o.class
	}
	if o.protoVer != "" {
		meta["protocol_version"] = o.protoVer
	} else {
		meta["protocol_version"] = "0.4"
	}
	svc, err := micro.AddService(nc, micro.Config{
		Name:        "agents",
		Version:     "0.1.0",
		Description: o.agent + " clean-scheme test agent",
		Metadata:    meta,
	})
	if err != nil {
		t.Fatalf("register clean micro service %q: %v", o.agent, err)
	}
	// Named "prompt" endpoint: QueryMesh keys on the endpoint named
	// "prompt" to capture the clean prompt subject.
	if err := svc.AddEndpoint("prompt",
		micro.HandlerFunc(func(req micro.Request) { _ = req.Respond([]byte("ok")) }),
		micro.WithEndpointSubject(subj)); err != nil {
		t.Fatalf("add prompt endpoint: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after register: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
	return svc
}

func TestQueryMesh_ReturnsAllRegisteredAgents(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	registerTestAgent(t, nc, "cc", "dmestas", "alpha")
	registerTestAgent(t, nc, "oh-my-pi", "dmestas", "alpha")
	registerTestAgent(t, nc, "cc", "dmestas", "beta") // different session — must still show up

	got := QueryMesh(nc, 500*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("QueryMesh returned %d agents, want 3: %+v", len(got), got)
	}

	byKey := map[string]MeshAgent{}
	for _, a := range got {
		key := a.Agent + "/" + a.Session
		byKey[key] = a
	}
	if _, ok := byKey["cc/alpha"]; !ok {
		t.Errorf("missing cc/alpha in result: %+v", got)
	}
	if _, ok := byKey["oh-my-pi/alpha"]; !ok {
		t.Errorf("missing oh-my-pi/alpha in result: %+v", got)
	}
	if _, ok := byKey["cc/beta"]; !ok {
		t.Errorf("missing cc/beta in result: %+v", got)
	}
}

func TestQueryMesh_PopulatesProtocolFields(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	// Clean v0.4 subject: agents.prompt.<machine>.<project>.<session>.
	registerTestAgentClean(t, nc, registerCleanOpts{
		agent: "claude-code", owner: "dmestas",
		machine: "m4-host", project: "sesh", session: "alpha", role: "worker",
	})

	got := QueryMesh(nc, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("want 1 agent, got %d", len(got))
	}
	a := got[0]
	if a.Agent != "claude-code" {
		t.Errorf("Agent=%q, want claude-code", a.Agent)
	}
	if a.Owner != "dmestas" {
		t.Errorf("Owner=%q, want dmestas", a.Owner)
	}
	if a.Session != "alpha" {
		t.Errorf("Session=%q, want alpha", a.Session)
	}
	if a.Machine != "m4-host" {
		t.Errorf("Machine=%q, want m4-host", a.Machine)
	}
	if a.ProjectID != "sesh" {
		t.Errorf("ProjectID=%q, want sesh", a.ProjectID)
	}
	if a.Role != "worker" {
		t.Errorf("Role=%q, want worker", a.Role)
	}
	if a.ProtocolVersion != "0.4" {
		t.Errorf("ProtocolVersion=%q, want 0.4", a.ProtocolVersion)
	}
	if a.InstanceID == "" {
		t.Errorf("InstanceID empty; want non-empty")
	}
	if a.Subject != "agents.prompt.m4-host.sesh.alpha" {
		t.Errorf("Subject=%q, want agents.prompt.m4-host.sesh.alpha", a.Subject)
	}
}

func TestQueryMesh_EmptyHubReturnsNil(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	// No agents registered.
	got := QueryMesh(nc, 200*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("want empty, got %d: %+v", len(got), got)
	}
}

func TestQueryMesh_MalformedReplyIsSkipped(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()

	// A responder that replies to $SRV.INFO.agents with garbage bytes.
	// QueryMesh should silently skip the malformed JSON and return any
	// other valid responders.
	_, err := nc.Subscribe("$SRV.INFO.agents", func(m *nats.Msg) {
		_ = m.Respond([]byte("this is not json"))
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	registerTestAgent(t, nc, "cc", "dmestas", "alpha") // valid responder

	got := QueryMesh(nc, 500*time.Millisecond)
	if len(got) != 1 || got[0].Agent != "cc" {
		t.Errorf("want exactly 1 cc agent (garbage skipped), got %+v", got)
	}
}

func TestApplyFilter_BySession(t *testing.T) {
	all := []MeshAgent{
		{Agent: "cc", Session: "alpha", InstanceID: "1"},
		{Agent: "oh-my-pi", Session: "alpha", InstanceID: "2"},
		{Agent: "cc", Session: "beta", InstanceID: "3"},
	}
	got := ApplyFilter(all, MeshFilter{Session: "alpha"})
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2: %+v", len(got), got)
	}
}

func TestApplyFilter_ByRoleAndClass(t *testing.T) {
	all := []MeshAgent{
		{Agent: "cc", Role: "implementer", Class: "active", InstanceID: "1"},
		{Agent: "cc", Role: "implementer", Class: "observer", InstanceID: "2"},
		{Agent: "cc", Role: "planner", Class: "active", InstanceID: "3"},
	}
	got := ApplyFilter(all, MeshFilter{Role: "implementer", Class: "active"})
	if len(got) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(got), got)
	}
	if got[0].InstanceID != "1" {
		t.Errorf("wrong agent: %+v", got[0])
	}
}

func TestApplyFilter_AllFiltersConjunct(t *testing.T) {
	all := []MeshAgent{
		{Agent: "cc", Owner: "dmestas", Session: "alpha", Role: "implementer", Class: "active", Machine: "abc12345", InstanceID: "1"},
		{Agent: "cc", Owner: "dmestas", Session: "alpha", Role: "implementer", Class: "active", Machine: "def67890", InstanceID: "2"},
	}
	got := ApplyFilter(all, MeshFilter{Agent: "cc", Owner: "dmestas", Session: "alpha", Role: "implementer", Class: "active", Machine: "abc12345"})
	if len(got) != 1 || got[0].InstanceID != "1" {
		t.Fatalf("conjunct filter wrong: %+v", got)
	}
}

func TestApplyFilter_EmptyFilterReturnsAll(t *testing.T) {
	all := []MeshAgent{{InstanceID: "1"}, {InstanceID: "2"}}
	got := ApplyFilter(all, MeshFilter{})
	if len(got) != 2 {
		t.Errorf("empty filter dropped agents: got %d want 2", len(got))
	}
}

func TestRenderTable_ContainsHeadersAndAgentRows(t *testing.T) {
	agents := []MeshAgent{
		{Agent: "claude-code", Owner: "dmestas", Session: "smoke-test", Role: "implementer", Class: "active",
			InstanceID: "ABC123456789", Machine: "f9a1b2c3", ProjectID: "sesh", Capabilities: "messages,artifacts,cards"},
		{Agent: "oh-my-pi", Owner: "dmestas", Session: "smoke-test", Role: "planner", Class: "active",
			InstanceID: "XYZ987654321", Machine: "f9a1b2c3", ProjectID: "sesh", Capabilities: "messages"},
	}
	out := renderTable(agents)

	for _, want := range []string{
		// New v0.4 columns: AGENT MACHINE PROJECT SESSION ROLE CAPS.
		"AGENT", "MACHINE", "PROJECT", "SESSION", "ROLE", "CAPS",
		"claude-code", "oh-my-pi", "implementer", "planner", "smoke-test",
		"f9a1b2c3", "sesh",
		"msg,art,cards", // abbreviated capability list
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderTable output missing %q\nfull:\n%s", want, out)
		}
	}

	// Owner, class, and the full instance id were moved OUT of the default
	// table (they remain in the struct + JSON output).
	for _, absent := range []string{"OWNER", "CLASS", "INSTANCE", "active", "ABC123456789"} {
		if strings.Contains(out, absent) {
			t.Errorf("renderTable output should not contain %q (moved to JSON)\nfull:\n%s", absent, out)
		}
	}
}

func TestRenderTable_EmptyCapsRendersDash(t *testing.T) {
	agents := []MeshAgent{
		{Agent: "claude-code", Machine: "m1", ProjectID: "p1", Session: "s1", Role: "worker"},
	}
	out := renderTable(agents)
	if !strings.Contains(out, "-") {
		t.Errorf("empty caps should render as '-'\nfull:\n%s", out)
	}
}

func TestRenderHubHeader_FullInfo(t *testing.T) {
	out := renderHubHeader(hubInfo{
		URL:        "nats://hub:4222",
		Version:    "2.10.22",
		Cluster:    "c1",
		RTT:        612 * time.Microsecond,
		HaveRTT:    true,
		JetStream:  true,
		AgentCount: 4,
	})
	for _, want := range []string{
		"hub", "nats://hub:4222", "nats-server 2.10.22",
		"cluster c1", "JetStream", "rtt", "4 agents",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hub header missing %q\nfull: %s", want, out)
		}
	}
	if strings.Contains(out, "no-JetStream") {
		t.Errorf("JetStream-enabled hub must not render no-JetStream\nfull: %s", out)
	}
}

func TestRenderHubHeader_NoJetStreamIsFlagged(t *testing.T) {
	out := renderHubHeader(hubInfo{URL: "nats://localhost:4222", JetStream: false, AgentCount: 0})
	if !strings.Contains(out, "no-JetStream") {
		t.Errorf("a hub without reachable JetStream must be flagged\nfull: %s", out)
	}
	// 0 agents pluralizes to "agents"; 1 should be singular.
	if !strings.Contains(out, "0 agents") {
		t.Errorf("expected '0 agents'\nfull: %s", out)
	}
}

func TestRenderHubHeader_OmitsAbsentFieldsAndSingularizes(t *testing.T) {
	out := renderHubHeader(hubInfo{URL: "nats://h:4222", JetStream: true, AgentCount: 1})
	if !strings.Contains(out, "1 agent") || strings.Contains(out, "1 agents") {
		t.Errorf("single agent should be singular\nfull: %s", out)
	}
	// No version, cluster, or RTT supplied → those tokens must be absent.
	for _, absent := range []string{"nats-server", "cluster", "rtt"} {
		if strings.Contains(out, absent) {
			t.Errorf("absent field %q should be omitted\nfull: %s", absent, out)
		}
	}
}

func TestRenderTable_EmptyInputReturnsHeadersOnly(t *testing.T) {
	out := renderTable(nil)
	if !strings.Contains(out, "AGENT") {
		t.Errorf("empty render should still show headers: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Errorf("empty render should be 1 line (headers), got %d: %q", len(lines), out)
	}
}

func TestRenderJSON_ParsesBackToAgents(t *testing.T) {
	in := []MeshAgent{
		{Agent: "cc", Owner: "dmestas", Session: "s", InstanceID: "1", Role: "worker", Class: "active"},
	}
	out := renderJSON(in)

	var parsed []MeshAgent
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\nout: %s", err, out)
	}
	if len(parsed) != 1 || parsed[0].Agent != "cc" {
		t.Errorf("round-trip mismatch: %+v", parsed)
	}
}

func TestRenderJSON_EmptyInputIsEmptyArray(t *testing.T) {
	out := strings.TrimSpace(renderJSON(nil))
	if out != "[]" {
		t.Errorf("empty render want \"[]\", got %q", out)
	}
}

func TestRenderTree_GroupsByHierarchy(t *testing.T) {
	agents := []MeshAgent{
		{Agent: "cc", Owner: "dmestas", Session: "smoke-test", Role: "implementer", Class: "active",
			Machine: "f9a1b2c3", ProjectID: "abcdef0123", InstanceID: "ID1"},
		{Agent: "oh-my-pi", Owner: "dmestas", Session: "smoke-test", Role: "planner", Class: "active",
			Machine: "f9a1b2c3", ProjectID: "abcdef0123", InstanceID: "ID2"},
		{Agent: "cc", Owner: "dmestas", Session: "other", Role: "worker", Class: "active",
			Machine: "f9a1b2c3", ProjectID: "abcdef0123", InstanceID: "ID3"},
	}
	out := renderTree(agents)

	// Must contain each grouping key.
	for _, want := range []string{
		"machine f9a1b2c3",
		"project abcdef0123",
		"session smoke-test",
		"session other",
		"role implementer",
		"role planner",
		"role worker",
		"cc/dmestas",
		"oh-my-pi/dmestas",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tree output missing %q\nfull:\n%s", want, out)
		}
	}

	// Sessions of the same machine must appear nested under the same machine
	// header (i.e. machine appears exactly once even though it's shared).
	if strings.Count(out, "machine f9a1b2c3") != 1 {
		t.Errorf("expected machine f9a1b2c3 to appear once (grouped); got %d:\n%s",
			strings.Count(out, "machine f9a1b2c3"), out)
	}
}

func TestRenderTree_EmptyInputIsEmptyString(t *testing.T) {
	out := renderTree(nil)
	if out != "" {
		t.Errorf("empty tree should be empty string, got %q", out)
	}
}

func TestMeshCmd_Run_TableOutputByDefault(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	registerTestAgent(t, nc, "cc", "dmestas", "alpha")

	var out bytes.Buffer
	cmd := &MeshCmd{
		NATSURL: url,
		Out:     &out, // injectable writer for tests
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "AGENT") || !strings.Contains(out.String(), "cc") {
		t.Errorf("default output missing table headers or agent row:\n%s", out.String())
	}
}

func TestMeshCmd_Run_JSONFormat(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	registerTestAgent(t, nc, "cc", "dmestas", "alpha")

	var out bytes.Buffer
	cmd := &MeshCmd{NATSURL: url, Format: "json", Out: &out}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var parsed []MeshAgent
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(parsed) != 1 || parsed[0].Agent != "cc" {
		t.Errorf("unexpected parsed output: %+v", parsed)
	}
}

func TestMeshCmd_Run_FilterBySession(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	registerTestAgent(t, nc, "cc", "dmestas", "alpha")
	registerTestAgent(t, nc, "cc", "dmestas", "beta")

	var out bytes.Buffer
	cmd := &MeshCmd{NATSURL: url, Session: "alpha", Format: "json", Out: &out}
	_ = cmd.Run(context.Background())

	var parsed []MeshAgent
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if len(parsed) != 1 || parsed[0].Session != "alpha" {
		t.Errorf("filter wrong: %+v", parsed)
	}
}

func TestMeshCmd_Run_RejectsUnknownFormat(t *testing.T) {
	_, url := startTestNATSServer(t)
	// Use a live URL so the connect succeeds and we exercise the format
	// dispatch path specifically (not the connect-error path).
	cmd := &MeshCmd{NATSURL: url, Format: "bogus", Out: &bytes.Buffer{}}
	err := cmd.Run(context.Background())
	if err == nil {
		t.Errorf("expected error for unknown format")
	}
}
