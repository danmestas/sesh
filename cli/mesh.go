package cli

import (
	"bytes"
	"fmt"
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
