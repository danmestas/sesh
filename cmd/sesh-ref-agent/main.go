// Command sesh-ref-agent is sesh's executable Synadia Agent Protocol
// v0.3 reference. Run it inside a `sesh up` session — with no
// arguments — and it registers as an `agents` micro service, echoes
// prompts, heartbeats, and serves the status endpoint. See
// docs/sesh-ref-agent.md for the §12 conformance map and
// docs/synadia-agents-on-sesh.md for the contract this implements.
//
// CLI surface is intentionally minimal: one optional flag for the
// agent identifier. Everything else (owner, session, NATS URL,
// heartbeat interval) is env-derived so the binary works as a drop-in
// inside `sesh up` without per-instance configuration.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/danmestas/sesh/internal/refagent"
)

func main() {
	agentName := flag.String("agent", "echo",
		"§3.2 `agent` metadata token (5th identifier in subject tokens)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			`sesh-ref-agent — Synadia Agent Protocol v0.3 reference implementation.

Usage:
  sesh-ref-agent [--agent=<name>]

All identity beyond --agent comes from the environment:
  SESH_OWNER    — owner metadata token (else $USER, else os user)
  SESH_SESSION  — session metadata token (omitted if empty)
  NATS_URL      — bus URL (else .sesh/sessions/<label>.json, else ~/.sesh/hub.url)

See docs/sesh-ref-agent.md.
`)
	}
	flag.Parse()

	// SIGINT/SIGTERM cancels the context. refagent.Run returns once it
	// has drained the service and the NATS connection — no
	// fire-and-forget exit.
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := refagent.Config{Agent: *agentName}
	if err := refagent.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "sesh-ref-agent: %v\n", err)
		os.Exit(1)
	}
}
