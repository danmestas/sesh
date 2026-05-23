package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestQueryMesh_ReturnsAllRegisteredAgents(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	registerTestAgent(t, nc, "cc", "dmestas", "alpha")
	registerTestAgent(t, nc, "op", "dmestas", "alpha")
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
	if _, ok := byKey["op/alpha"]; !ok {
		t.Errorf("missing op/alpha in result: %+v", got)
	}
	if _, ok := byKey["cc/beta"]; !ok {
		t.Errorf("missing cc/beta in result: %+v", got)
	}
}

func TestQueryMesh_PopulatesProtocolFields(t *testing.T) {
	_, url := startTestNATSServer(t)
	nc, _ := nats.Connect(url)
	defer nc.Close()
	registerTestAgent(t, nc, "cc", "dmestas", "alpha")

	got := QueryMesh(nc, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("want 1 agent, got %d", len(got))
	}
	a := got[0]
	if a.Agent != "cc" {
		t.Errorf("Agent=%q, want cc", a.Agent)
	}
	if a.Owner != "dmestas" {
		t.Errorf("Owner=%q, want dmestas", a.Owner)
	}
	if a.Session != "alpha" {
		t.Errorf("Session=%q, want alpha", a.Session)
	}
	if a.ProtocolVersion != "0.3" {
		t.Errorf("ProtocolVersion=%q, want 0.3", a.ProtocolVersion)
	}
	if a.InstanceID == "" {
		t.Errorf("InstanceID empty; want non-empty")
	}
	if a.Subject != "agents.prompt.cc.dmestas.alpha" {
		t.Errorf("Subject=%q, want agents.prompt.cc.dmestas.alpha", a.Subject)
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
		{Agent: "op", Session: "alpha", InstanceID: "2"},
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
		{Agent: "cc", Owner: "dmestas", Session: "smoke-test", Role: "implementer", Class: "active", InstanceID: "ABC123456789", Machine: "f9a1b2c3"},
		{Agent: "op", Owner: "dmestas", Session: "smoke-test", Role: "planner", Class: "active", InstanceID: "XYZ987654321", Machine: "f9a1b2c3"},
	}
	out := renderTable(agents)

	for _, want := range []string{
		"AGENT", "OWNER", "SESSION", "ROLE", "CLASS", "MACHINE", "INSTANCE",
		"cc", "op", "implementer", "planner", "active", "smoke-test",
		"ABC12345", // ID is truncated to first 8 chars for readability
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderTable output missing %q\nfull:\n%s", want, out)
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
		{Agent: "op", Owner: "dmestas", Session: "smoke-test", Role: "planner", Class: "active",
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
		"op/dmestas",
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
