package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/coord"
	"github.com/danmestas/sesh/internal/subject"
)

// MeshGroup is the parent kong command for `sesh mesh ...`. It exists
// so we can dispatch between `mesh list` (the legacy snapshot) and the
// Slice 5 `mesh card` debug subcommand under one CLI noun.
//
// The bare `sesh mesh` invocation is preserved via
// `default:"withargs"` on List — kong runs the List subcommand when
// no sub-token is provided. This keeps the operator habit intact
// while letting the parent group host new subcommands.
type MeshGroup struct {
	List MeshCmd     `cmd:"" default:"withargs" help:"Snapshot agents on the mesh (default)"`
	Card MeshCardCmd `cmd:""                    help:"Fetch the L3 AgentCard from one adapter via NATS"`
}

// MeshCardCmd is `sesh mesh card <session> [--project=… --machine=…
// --extended --window=500ms -o json|tree]`.
//
// Operator-facing debug surface for the L3 card path on the clean v0.4
// subject scheme: issues a window-bounded nats.Request to
// agents.card.<machine>.<project>.<session> (or agents.cardx.<…> with
// --extended) and prints whatever the session's adapter replied with.
// Exits 1 on timeout-or-no-reply, 0 on success.
//
// Coordinate derivation matches the adapter side of the cutover:
// --machine defaults to the local machine id (coord.Machine(), which
// honors $SESH_MACHINE), --project defaults to $SESH_PROJECT, and the
// positional <session> arg is the session token. The old
// agent/owner/name addressing is gone — cards are session-scoped now.
//
// Reuses the same NATS-URL resolution path as MeshCmd (--nats-url
// flag → resolveHubURL fallback) so operator habits transfer between
// `sesh mesh` and `sesh mesh card`.
type MeshCardCmd struct {
	Session  string        `arg:""            help:"Session token (the <session> subject segment)"`
	Project  string        `name:"project"    env:"SESH_PROJECT" help:"Project token; defaults to $SESH_PROJECT"`
	Machine  string        `name:"machine"    env:"SESH_MACHINE" help:"Machine token; defaults to the local machine id"`
	Extended bool          `name:"extended"   help:"Fetch the auth-gated extended card via agents.cardx.* instead of the public agents.card.*"`
	NATSURL  string        `name:"nats-url"   env:"NATS_URL" help:"NATS URL to query (overrides hub discovery)"`
	Format   string        `short:"o"         default:"json"  enum:"json,tree" help:"Output format: json | tree"`
	Window   time.Duration `name:"window"     default:"500ms" help:"Reply timeout window"`

	// Out is the writer for command output. Tests inject a
	// bytes.Buffer; kong leaves it nil and we fall back to os.Stdout.
	Out io.Writer `kong:"-"`
}

// Run is kong's entry point for `sesh mesh card ...`. Returns nil on
// success and a wrapped error on connect/timeout/decode failure. The
// kong driver maps a non-nil error to exit code 1.
func (cmd *MeshCardCmd) Run(ctx context.Context) error {
	if cmd.Out == nil {
		cmd.Out = os.Stdout
	}
	if cmd.Window <= 0 {
		cmd.Window = 500 * time.Millisecond
	}
	if cmd.Format == "" {
		cmd.Format = "json"
	}

	machine := cmd.Machine
	if machine == "" {
		machine = coord.Machine()
	}
	project := cmd.Project
	if project == "" {
		return fmt.Errorf("mesh card: --project is required (set --project or $SESH_PROJECT)")
	}

	c := subject.Coord{Machine: machine, Project: project, Session: cmd.Session}

	var (
		subj string
		err  error
	)
	if cmd.Extended {
		subj, err = subject.Cardx(c)
	} else {
		subj, err = subject.Card(c)
	}
	if err != nil {
		return fmt.Errorf("mesh card: build subject: %w", err)
	}

	url := cmd.NATSURL
	if url == "" {
		resolved, err := resolveHubURL()
		if err != nil {
			return fmt.Errorf("mesh card: %w (or pass --nats-url)", err)
		}
		url = resolved
	}

	nc, err := nats.Connect(url,
		nats.Name("sesh-mesh-card"),
		nats.Timeout(2*time.Second),
		nats.MaxReconnects(0),
	)
	if err != nil {
		return fmt.Errorf("mesh card: connect %s: %w", url, err)
	}
	defer nc.Close()

	msg, err := nc.Request(subj, nil, cmd.Window)
	if err != nil {
		return fmt.Errorf("mesh card: no reply on %s within %s: %w", subj, cmd.Window, err)
	}

	return renderCardReply(cmd.Out, subj, msg.Data, cmd.Format)
}

// cardReplyTreeView is the minimal projection of the adapter's
// agents.card.* reply used by the `tree` format. Mirrors the byte
// shape of the Go internal `cardPartial` (and the TS
// `AgentCardPartial`) — keep in sync if either changes.
type cardReplyTreeView struct {
	Description      string `json:"description,omitempty"`
	IconURL          string `json:"iconUrl,omitempty"`
	DocumentationURL string `json:"documentationUrl,omitempty"`
	Skills           []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"skills,omitempty"`
	Capabilities *struct {
		Extensions []struct {
			URI string `json:"uri"`
		} `json:"extensions,omitempty"`
	} `json:"capabilities,omitempty"`
}

// renderCardReply formats the adapter's reply per the requested
// format. "json" is `json.Indent` of the raw bytes (preserves any
// vendor extensions the SDK adds). "tree" is a 5–10 line operator
// summary keyed off the well-known fields.
func renderCardReply(w io.Writer, subj string, body []byte, format string) error {
	switch format {
	case "", "json":
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err != nil {
			// Non-JSON reply — surface the raw bytes verbatim AND return
			// an error so the operator's exit code reflects the malformed
			// adapter response. Without this, scripts piping `sesh mesh
			// card ... -o json | jq` would silently succeed on garbage.
			fmt.Fprintf(w, "# raw reply on %s (not valid JSON):\n%s\n", subj, body)
			return fmt.Errorf("mesh card: adapter reply on %s is not valid JSON: %w", subj, err)
		}
		fmt.Fprintf(w, "# reply on %s\n", subj)
		_, _ = pretty.WriteTo(w)
		fmt.Fprintln(w)
		return nil
	case "tree":
		var v cardReplyTreeView
		if err := json.Unmarshal(body, &v); err != nil {
			return fmt.Errorf("mesh card: decode reply for tree view: %w", err)
		}
		fmt.Fprintf(w, "subject     %s\n", subj)
		if v.Description != "" {
			fmt.Fprintf(w, "description %s\n", v.Description)
		}
		if v.IconURL != "" {
			fmt.Fprintf(w, "icon        %s\n", v.IconURL)
		}
		if v.DocumentationURL != "" {
			fmt.Fprintf(w, "docs        %s\n", v.DocumentationURL)
		}
		if len(v.Skills) > 0 {
			fmt.Fprintln(w, "skills")
			for _, s := range v.Skills {
				fmt.Fprintf(w, "  %s — %s\n", s.ID, s.Name)
			}
		}
		if v.Capabilities != nil && len(v.Capabilities.Extensions) > 0 {
			fmt.Fprintln(w, "extensions")
			for _, e := range v.Capabilities.Extensions {
				fmt.Fprintf(w, "  %s\n", e.URI)
			}
		}
		return nil
	default:
		return fmt.Errorf("mesh card: unknown format %q (want json|tree)", format)
	}
}
