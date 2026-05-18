package main

import (
	"github.com/alecthomas/kong"

	seshcli "github.com/danmestas/sesh/cli"
)

// CLI is sesh's command surface.
//
//	sesh up          --session=<label>        — bring a session up (foreground; cwd-derived project)
//	sesh down        --session=<label>        — bring a session down (SIGINT the sesh up process)
//	sesh worktree    <label> [--scope=...]    — provision a fossil checkout at .sesh/checkouts/<label>/
//	sesh worker-cwd  <label> [--scope=...]    — print the absolute checkout path; read-only (no provisioning)
//	sesh materialize <label> [--scope=...]    — overlay fossil trunk HEAD into a git worktree (or --output=<dir>)
//	sesh hub serve                            — run the hub daemon (auto-spawned by sesh up; visible for power users)
type CLI struct {
	Up          seshcli.UpCmd          `cmd:"" help:"Bring a session up — opens a leaf connection to the hub, blocking until SIGINT"`
	Down        seshcli.DownCmd        `cmd:"" help:"Bring a session down — SIGINT the sesh up process for this label"`
	Worktree    seshcli.WorktreeCmd    `cmd:"" help:"Provision a fossil checkout at .sesh/checkouts/<label>/ for a worker to cd into. Prints the absolute checkout path on success."`
	WorkerCwd   seshcli.WorkerCwdCmd   `cmd:"" name:"worker-cwd" help:"Print the absolute fossil checkout path for <label>. Read-only — does not provision; pair with 'sesh worktree' once up front."`
	Materialize seshcli.MaterializeCmd `cmd:"" help:"Overlay the fossil trunk HEAD for <label> into a git worktree (default: cwd). The fossil→git bridge for mission-complete materialization."`
	Hub         seshcli.HubCmd         `cmd:"" help:"sesh hub serve runs the user-level hub daemon at ~/.sesh/"`
}

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("sesh"),
		kong.Description("Session manager wrapping EdgeSync. One hub per user (auto-lifecycle), one sesh up per session (foreground)."),
	)
	ctx.FatalIfErrorf(ctx.Run())
}
