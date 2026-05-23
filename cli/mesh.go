package cli

import (
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
