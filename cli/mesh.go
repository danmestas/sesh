package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
	Capabilities    string               `json:"capabilities,omitempty"`
	ProtocolVersion string               `json:"protocol_version,omitempty"`
}

// promptSubjectParts holds the tokens parsed out of a prompt subject
// `agents.prompt.<machine>.<project>.<session>`. Any token the subject
// doesn't carry is left empty so callers can fall back to metadata. The
// role is no longer carried in the subject — it lives in metadata.role.
type promptSubjectParts struct {
	Machine string
	Project string
	Session string
}

// parsePromptSubject extracts machine/project/session from a prompt
// subject of the form `agents.prompt.<machine>.<project>.<session>`.
//
// Any non-matching subject returns a zero-value promptSubjectParts so the
// caller fills every field from metadata instead. We require the
// `agents.prompt.` prefix and exactly five dot-separated tokens; anything
// shorter or longer is treated as non-matching (returns ok=false) rather
// than guessing at partial tokens.
func parsePromptSubject(subj string) (promptSubjectParts, bool) {
	const prefix = "agents.prompt."
	if !strings.HasPrefix(subj, prefix) {
		return promptSubjectParts{}, false
	}
	tokens := strings.Split(subj, ".")
	// agents . prompt . machine . project . session == 5 tokens.
	if len(tokens) != 5 {
		return promptSubjectParts{}, false
	}
	return promptSubjectParts{
		Machine: tokens[2],
		Project: tokens[3],
		Session: tokens[4],
	}, true
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
			InstanceID:      info.ID,
			Class:           agentmeta.DefaultedClass(info.Metadata["class"]),
			Capabilities:    info.Metadata["sesh.v04_capabilities"],
			ProtocolVersion: info.Metadata["protocol_version"],
		}
		// Subject: prefer the clean prompt endpoint; fall back to the
		// first advertised endpoint so older adapters still show a subject.
		for _, ep := range info.Endpoints {
			if ep.Name == "prompt" {
				a.Subject = ep.Subject
				break
			}
		}
		if a.Subject == "" && len(info.Endpoints) > 0 {
			a.Subject = info.Endpoints[0].Subject
		}

		// The prompt subject is machine-rooted
		// (agents.prompt.<machine>.<project>.<session>) — agent identity
		// and role live in metadata, not the subject. Parse it so we can
		// fill machine/project/session when metadata is sparse.
		parts, _ := parsePromptSubject(a.Subject)

		// MACHINE: metadata.machine (adapters set it now) → subject token.
		a.Machine = firstNonEmpty(info.Metadata["machine"], parts.Machine)
		// PROJECT: subject token → metadata.project_id.
		a.ProjectID = firstNonEmpty(parts.Project, info.Metadata["project_id"])
		// SESSION: metadata.session → subject token.
		a.Session = firstNonEmpty(info.Metadata["session"], parts.Session)
		// ROLE: from metadata.role only (no longer carried in the subject);
		// the metadata default ("worker") wins when metadata is absent.
		if r := strings.TrimSpace(info.Metadata["role"]); r != "" {
			a.Role = r
		} else {
			a.Role = agentmeta.DefaultedRole("")
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

// firstNonEmpty returns the first of vs that is non-empty, or "" if all
// are empty. Used by QueryMesh to pick between a metadata value and the
// subject-derived fallback.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// capAbbrev maps the long capability names adapters advertise in
// `sesh.v04_capabilities` to the tidy abbreviations the mesh table shows.
// Unknown capabilities pass through verbatim so a new capability still
// renders (just unabbreviated) rather than being dropped.
var capAbbrev = map[string]string{
	"messages":  "msg",
	"artifacts": "art",
	"cards":     "cards",
}

// abbreviateCaps renders a comma-separated capability list (e.g.
// "messages,artifacts,cards") as a tidy abbreviated form ("msg,art,cards").
// Empty input renders as "-". Whitespace around tokens is trimmed; empty
// tokens (from a trailing/double comma) are skipped.
func abbreviateCaps(caps string) string {
	caps = strings.TrimSpace(caps)
	if caps == "" {
		return "-"
	}
	var out []string
	for _, c := range strings.Split(caps, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if ab, ok := capAbbrev[c]; ok {
			out = append(out, ab)
		} else {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

// dashIfEmpty returns s, or "-" when s is empty — so the table never shows
// a blank cell for a missing field.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// hubInfo is the display view of the NATS hub `sesh mesh` is connected to.
// Every field is read from the live connection — the hub is the one mesh
// participant with no AgentCard and no heartbeat, so the CLI speaks for it.
type hubInfo struct {
	URL        string        // ConnectedUrlRedacted (passwords stripped)
	Version    string        // ConnectedServerVersion
	Cluster    string        // ConnectedClusterName, if any
	RTT        time.Duration // round-trip to the hub; 0 if RTT errored
	HaveRTT    bool          // whether RTT succeeded
	JetStream  bool          // AccountInfo succeeded (JetStream reachable)
	AgentCount int           // agents the hub reported (post-filter)
}

// gatherHubInfo pulls the hub's identity + health off the live connection.
// JetStream presence is probed with AccountInfo (a single round-trip); sesh
// requires JetStream, so surfacing its absence here catches a misconfigured
// hub before it bites a session.
func gatherHubInfo(nc *nats.Conn, agentCount int) hubInfo {
	h := hubInfo{
		URL:        nc.ConnectedUrlRedacted(),
		Version:    nc.ConnectedServerVersion(),
		Cluster:    nc.ConnectedClusterName(),
		AgentCount: agentCount,
	}
	if rtt, err := nc.RTT(); err == nil {
		h.RTT, h.HaveRTT = rtt, true
	}
	if js, err := nc.JetStream(); err == nil {
		if _, err := js.AccountInfo(); err == nil {
			h.JetStream = true
		}
	}
	return h
}

// renderHubHeader formats the one-line hub banner shown above the mesh table.
// Pure (no I/O) so it's unit-testable without a server. Shape:
//
//	hub  nats://hub:4222 · nats-server 2.10.22 · cluster c1 · JetStream · rtt 0.6ms · 4 agents
//
// Fields that aren't available are omitted rather than printed empty; a hub
// with no reachable JetStream renders "no-JetStream" since sesh needs it.
func renderHubHeader(h hubInfo) string {
	parts := []string{dashIfEmpty(h.URL)}
	if h.Version != "" {
		parts = append(parts, "nats-server "+h.Version)
	}
	if h.Cluster != "" {
		parts = append(parts, "cluster "+h.Cluster)
	}
	if h.JetStream {
		parts = append(parts, "JetStream")
	} else {
		parts = append(parts, "no-JetStream")
	}
	if h.HaveRTT {
		parts = append(parts, fmt.Sprintf("rtt %s", h.RTT.Round(100*time.Microsecond)))
	}
	plural := "s"
	if h.AgentCount == 1 {
		plural = ""
	}
	parts = append(parts, fmt.Sprintf("%d agent%s", h.AgentCount, plural))
	return "hub  " + strings.Join(parts, " · ") + "\n"
}

// renderTable formats agents as a tab-aligned table keyed on the clean
// v0.4 mesh shape: AGENT MACHINE PROJECT SESSION ROLE CAPS. Owner, class,
// and the full instance id are intentionally NOT in the default table —
// they remain on MeshAgent and in `--format json` output for callers that
// need them.
func renderTable(agents []MeshAgent) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tMACHINE\tPROJECT\tSESSION\tROLE\tCAPS")
	for _, a := range agents {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			dashIfEmpty(a.Agent),
			dashIfEmpty(a.Machine),
			dashIfEmpty(a.ProjectID),
			dashIfEmpty(a.Session),
			dashIfEmpty(a.Role),
			abbreviateCaps(a.Capabilities),
		)
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
// All flags are optional. Defaults: connect via resolveHubURL
// ($SESH_HUB_URL / $NATS_URL), format=table, no filters (show every agent
// the hub knows about).
type MeshCmd struct {
	NATSURL string `name:"nats-url" help:"NATS URL to query (overrides hub discovery)" env:"NATS_URL"`
	Format  string `short:"o" help:"Output format: table | json | tree" default:"table" enum:"table,json,tree"`

	// Filter flags (all AND-combined; empty = no filter)
	Agent   string `help:"Filter by agent kind (e.g. cc, op)"`
	Owner   string `help:"Filter by owner"`
	Session string `help:"Filter by session label"`
	Role    string `help:"Filter by role (e.g. implementer, planner)"`
	Class   string `help:"Filter by class display tag (e.g. active)"`
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
		resolved, err := resolveHubURL()
		if err != nil {
			return fmt.Errorf("mesh: %w (or pass --nats-url)", err)
		}
		url = resolved
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
		// Hub banner first: which mesh am I looking at, and is it healthy.
		// Human-facing only — JSON stays a bare array for scripts.
		fmt.Fprint(cmd.Out, renderHubHeader(gatherHubInfo(nc, len(agents))))
		fmt.Fprint(cmd.Out, renderTable(agents))
	case "json":
		fmt.Fprint(cmd.Out, renderJSON(agents))
	case "tree":
		fmt.Fprint(cmd.Out, renderHubHeader(gatherHubInfo(nc, len(agents))))
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
