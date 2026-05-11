package main

import (
	"github.com/alecthomas/kong"

	_ "github.com/danmestas/libfossil/db/driver/modernc"

	seshcli "github.com/danmestas/sesh/cli"
)

type CLI struct {
	Leaf seshcli.LeafCmd `cmd:"" help:"Session leaf — solicits a leaf connection to a hub"`
}

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("sesh"),
		kong.Description("Session-aware leaf wrapper for EdgeSync. Owns the session/agent vocabulary and disk layout on top of EdgeSync's NATS+fossil hub substrate."),
	)
	ctx.FatalIfErrorf(ctx.Run())
}
