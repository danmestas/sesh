package main

import (
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// TestRemovedCommandsRejected asserts the kong CLI parser no longer accepts
// the worktree-subsystem commands removed in slice A1. Each must fail to
// parse as an unknown command — kong reports those as
// "unexpected argument <name>". A still-registered command would instead
// error on its missing "<label>" positional, so we explicitly reject that
// shape to keep the test honest (RED while the commands exist).
func TestRemovedCommandsRejected(t *testing.T) {
	removed := []string{"worktree", "materialize", "worker-cwd"}
	for _, name := range removed {
		t.Run(name, func(t *testing.T) {
			var c CLI
			parser, err := kong.New(&c, kong.Name("sesh"))
			if err != nil {
				t.Fatalf("kong.New: %v", err)
			}
			_, err = parser.Parse([]string{name})
			if err == nil {
				t.Fatalf("expected parse error for removed command %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "unexpected argument "+name) {
				t.Fatalf("command %q still registered: parser error = %q (want unknown-command rejection)", name, err)
			}
		})
	}
}
