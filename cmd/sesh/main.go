package main

import (
	"context"

	"github.com/alecthomas/kong"

	seshcli "github.com/danmestas/sesh/cli"
)

// CLI is sesh's command surface.
//
//	sesh up          --session=<label>        — bring a session up (foreground; cwd-derived project)
//	sesh down        --session=<label>        — bring a session down (SIGINT the sesh up process)
type CLI struct {
	Up   seshcli.UpCmd     `cmd:"" help:"Bring a session up — connects to the external hub as a NATS client, blocking until SIGINT"`
	Down seshcli.DownCmd   `cmd:"" help:"Bring a session down — SIGINT the sesh up process for this label"`
	Mesh seshcli.MeshGroup `cmd:"" help:"Inspect the mesh — list agents (default) or fetch one adapter's AgentCard"`
}

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("sesh"),
		kong.Description("Session manager. Connects to an external NATS hub as a client; one sesh up per session (foreground)."),
		// Provide context.Context to commands whose Run signature expects it
		// (e.g. MeshCmd uses Run(ctx) for cancellation testability).
		kong.BindTo(context.Background(), (*context.Context)(nil)),
	)
	ctx.FatalIfErrorf(ctx.Run())
}
