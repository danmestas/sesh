package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/nats-io/nats.go"
)

// MeshAgent is the transient debug-display view of one agent on the mesh.
//
// Intentionally distinct from AgentRef (cli/session.go). AgentRef is the
// on-disk session-manifest shape — small, stable, versioned. MeshAgent
// adds machine/project/protocol fields and types Class for the richer
// display the mesh command produces. Don't merge them — the two have
// different stability contracts.
type MeshAgent struct {
	Agent           string               `json:"agent"`
	Owner           string               `json:"owner"`
	Session         string               `json:"session"`
	InstanceID      string               `json:"instance_id"`
	Subject         string               `json:"subject"`
	Role            string               `json:"role"`
	Class           agentmeta.AgentClass `json:"class"`
	Machine         string               `json:"machine,omitempty"`
	ProjectID       string               `json:"project_id,omitempty"`
	ProtocolVersion string               `json:"protocol_version,omitempty"`
}

// QueryMesh issues one `$SRV.INFO.agents` discovery request and returns
// every distinct responder within window. Returns all agents reachable on
// the connected hub (no session filter); callers apply MeshFilter to slice.
//
// Underlying NATS round-trip lives in queryServiceInfo (cli/agent_watcher.go);
// QueryMesh only does the microInfo → MeshAgent mapping.
func QueryMesh(nc *nats.Conn, window time.Duration) []MeshAgent {
	infos := queryServiceInfo(nc, window)
	var agents []MeshAgent
	for _, info := range infos {
		a := MeshAgent{
			Agent:           info.Metadata["agent"],
			Owner:           info.Metadata["owner"],
			Session:         info.Metadata["session"],
			InstanceID:      info.ID,
			Role:            agentmeta.DefaultedRole(info.Metadata["role"]),
			Class:           agentmeta.DefaultedClass(info.Metadata["class"]),
			Machine:         info.Metadata["machine"],
			ProjectID:       info.Metadata["project_id"],
			ProtocolVersion: info.Metadata["protocol_version"],
		}
		for _, ep := range info.Endpoints {
			if ep.Name == "prompt" {
				a.Subject = ep.Subject
				break
			}
		}
		if a.Subject == "" && len(info.Endpoints) > 0 {
			a.Subject = info.Endpoints[0].Subject
		}
		agents = append(agents, a)
	}
	return agents
}

// MeshFilter selects a subset of agents. Empty fields are wildcards.
// All set fields combine as AND.
type MeshFilter struct {
	Agent   string
	Owner   string
	Session string
	Role    string
	Class   string
	Machine string
}

// ApplyFilter returns a new slice containing only agents matching every
// non-empty field in f. An empty MeshFilter returns the input unchanged
// (modulo slice copy).
func ApplyFilter(agents []MeshAgent, f MeshFilter) []MeshAgent {
	if (f == MeshFilter{}) {
		out := make([]MeshAgent, len(agents))
		copy(out, agents)
		return out
	}
	var out []MeshAgent
	for _, a := range agents {
		if f.Agent != "" && a.Agent != f.Agent {
			continue
		}
		if f.Owner != "" && a.Owner != f.Owner {
			continue
		}
		if f.Session != "" && a.Session != f.Session {
			continue
		}
		if f.Role != "" && a.Role != f.Role {
			continue
		}
		if f.Class != "" && string(a.Class) != f.Class {
			continue
		}
		if f.Machine != "" && a.Machine != f.Machine {
			continue
		}
		out = append(out, a)
	}
	return out
}

// renderTable formats agents as a tab-aligned table. Instance IDs are
// truncated to 8 chars to keep rows readable; full ID is available via
// `sesh mesh --id <id>` or the JSON output.
func renderTable(agents []MeshAgent) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tOWNER\tSESSION\tROLE\tCLASS\tMACHINE\tINSTANCE")
	for _, a := range agents {
		id := a.InstanceID
		if len(id) > 8 {
			id = id[:8]
		}
		machine := a.Machine
		if machine == "" {
			machine = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Agent, a.Owner, a.Session, a.Role, a.Class, machine, id)
	}
	_ = w.Flush()
	return buf.String()
}

// renderJSON marshals agents as a pretty-printed JSON array. Nil input
// produces "[]" (not "null") so consumers can always parse the result
// as a list.
func renderJSON(agents []MeshAgent) string {
	if agents == nil {
		agents = []MeshAgent{}
	}
	b, _ := json.MarshalIndent(agents, "", "  ")
	return string(b) + "\n"
}

// renderTree groups agents by machine → project → session → role and
// prints an indented tree. Missing machine/project IDs coalesce under
// "(local)" / "(no-project)".
//
// Implementation is the classic sort-then-emit group-by: sort the flat
// list by (machine, project, session, role, agent), then walk it emitting
// a header whenever the cluster key changes. ~25 lines, single pass, no
// nested maps. Empty input produces an empty string (caller prints a
// "no agents" hint).
func renderTree(agents []MeshAgent) string {
	if len(agents) == 0 {
		return ""
	}
	mOf := func(s string) string {
		if s == "" {
			return "(local)"
		}
		return s
	}
	pOf := func(s string) string {
		if s == "" {
			return "(no-project)"
		}
		return s
	}

	sorted := make([]MeshAgent, len(agents))
	copy(sorted, agents)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.Agent < b.Agent
	})

	var buf bytes.Buffer
	var lastM, lastP, lastS, lastR string
	for i, a := range sorted {
		m, p := mOf(a.Machine), pOf(a.ProjectID)
		if i == 0 || m != lastM {
			fmt.Fprintf(&buf, "machine %s\n", m)
			lastM, lastP, lastS, lastR = m, "", "", ""
		}
		if p != lastP {
			fmt.Fprintf(&buf, "  project %s\n", p)
			lastP, lastS, lastR = p, "", ""
		}
		if a.Session != lastS {
			fmt.Fprintf(&buf, "    session %s\n", a.Session)
			lastS, lastR = a.Session, ""
		}
		if a.Role != lastR {
			fmt.Fprintf(&buf, "      role %s\n", a.Role)
			lastR = a.Role
		}
		id := a.InstanceID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Fprintf(&buf, "        %s/%s [%s] %s\n", a.Agent, a.Owner, a.Class, id)
	}
	return buf.String()
}

// MeshCmd is the kong-driven `sesh mesh` subcommand.
//
// All flags are optional. Defaults: connect via ReadHubInfo(stateDir),
// format=table, no filters (show every agent the local hub knows about).
type MeshCmd struct {
	NATSURL string `help:"NATS URL to query (overrides hub discovery)" env:"NATS_URL"`
	Format  string `short:"o" help:"Output format: table | json | tree" default:"table" enum:"table,json,tree"`

	// Filter flags (all AND-combined; empty = no filter)
	Agent   string `help:"Filter by agent kind (e.g. cc, op)"`
	Owner   string `help:"Filter by owner"`
	Session string `help:"Filter by session label"`
	Role    string `help:"Filter by role (e.g. implementer, planner)"`
	Class   string `help:"Filter by class (active | observer)"`
	Machine string `help:"Filter by machine ID (first 8 hex of /etc/machine-id)"`

	// Window controls how long QueryMesh waits for INFO replies.
	Window time.Duration `help:"Reply collection window" default:"500ms"`

	// Out is the writer for command output. Tests inject a bytes.Buffer;
	// kong leaves it nil and we fall back to os.Stdout.
	Out io.Writer `kong:"-"`
}

// Run is kong's entry point for `sesh mesh ...`.
func (cmd *MeshCmd) Run(ctx context.Context) error {
	if cmd.Out == nil {
		cmd.Out = os.Stdout
	}

	url := cmd.NATSURL
	if url == "" {
		stateDir, err := seshHome()
		if err != nil {
			return fmt.Errorf("mesh: state dir: %w", err)
		}
		info, err := ReadHubInfo(stateDir)
		if err != nil {
			return fmt.Errorf("mesh: hub URL not found (run `sesh up` first or pass --nats-url): %w", err)
		}
		url = info.NATSURL
	}

	nc, err := nats.Connect(url,
		nats.Name("sesh-mesh"),
		nats.Timeout(2*time.Second),
		nats.MaxReconnects(0),
	)
	if err != nil {
		return fmt.Errorf("mesh: connect %s: %w", url, err)
	}
	defer nc.Close()

	// When MeshCmd is constructed in code (e.g. tests) the kong-applied
	// "default:" tags don't fire, so a zero Window would short-circuit
	// QueryMesh to zero replies. Mirror the kong default here.
	window := cmd.Window
	if window <= 0 {
		window = 500 * time.Millisecond
	}
	agents := QueryMesh(nc, window)
	agents = ApplyFilter(agents, MeshFilter{
		Agent: cmd.Agent, Owner: cmd.Owner, Session: cmd.Session,
		Role: cmd.Role, Class: cmd.Class, Machine: cmd.Machine,
	})

	switch cmd.Format {
	case "", "table":
		fmt.Fprint(cmd.Out, renderTable(agents))
	case "json":
		fmt.Fprint(cmd.Out, renderJSON(agents))
	case "tree":
		out := renderTree(agents)
		if out == "" {
			fmt.Fprintln(cmd.Out, "(no agents on the mesh)")
		} else {
			fmt.Fprint(cmd.Out, out)
		}
	default:
		return fmt.Errorf("mesh: unknown format %q (want table|json|tree)", cmd.Format)
	}
	return nil
}
