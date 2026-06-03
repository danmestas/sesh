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
//	sesh hub serve                            — run the hub daemon (auto-spawned by sesh up; visible for power users)
type CLI struct {
	Up   seshcli.UpCmd     `cmd:"" help:"Bring a session up — opens a leaf connection to the hub, blocking until SIGINT"`
	Down seshcli.DownCmd   `cmd:"" help:"Bring a session down — SIGINT the sesh up process for this label"`
	Hub  seshcli.HubCmd    `cmd:"" help:"sesh hub serve runs the user-level hub daemon at ~/.sesh/"`
	Mesh seshcli.MeshGroup `cmd:"" help:"Inspect the mesh — list agents (default) or fetch one adapter's AgentCard"`
}

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("sesh"),
		kong.Description("Session manager wrapping EdgeSync. One hub per user (auto-lifecycle), one sesh up per session (foreground)."),
		// Provide context.Context to commands whose Run signature expects it
		// (e.g. MeshCmd uses Run(ctx) for cancellation testability).
		kong.BindTo(context.Background(), (*context.Context)(nil)),
	)
	ctx.FatalIfErrorf(ctx.Run())
}
