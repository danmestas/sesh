package main

import (
	"github.com/alecthomas/kong"

	_ "github.com/danmestas/libfossil/db/driver/modernc"

	edgecli "github.com/danmestas/EdgeSync/cli"
	repocli "github.com/danmestas/EdgeSync/cli/repo"

	seshcli "github.com/danmestas/sesh/cli"
)

// CLI is sesh's single-binary surface: the hub substrate from EdgeSync,
// plus sesh's own session-aware leaf command. The Globals embed lets
// EdgeSync's HubCmd receive the standard libfossil `-R` flag.
type CLI struct {
	repocli.Globals

	Hub  edgecli.HubCmd  `cmd:"" help:"Embedded NATS + fossil hub (root or soliciting via --leaf-upstream)"`
	Leaf seshcli.LeafCmd `cmd:"" help:"Session leaf — solicits a leaf connection to a hub"`
}

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("sesh"),
		kong.Description("Session-aware leaf manager. Wraps EdgeSync's NATS+fossil hub substrate with session/agent vocabulary and (soon) coordination state via JetStream KV on the hub."),
	)
	ctx.FatalIfErrorf(ctx.Run(&c.Globals))
}
