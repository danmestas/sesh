package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// TestHubSubcommandRemoved asserts that the kong CLI no longer registers a
// `hub` subcommand after the embedded-hub removal (slice B1). sesh is a NATS
// client only — there is no `sesh hub serve` to run. Kong must reject `hub`
// as an unexpected argument (unknown command), the same way it rejects the
// A1-removed worktree subcommands.
//
// This mirrors cmd/sesh/main_cmds_test.go but lives in the cli package so it
// can construct the CLI surface directly; we re-declare the root shape used
// by cmd/sesh/main.go (Up/Down/Mesh) MINUS the Hub field.
func TestHubSubcommandRemoved(t *testing.T) {
	var root struct {
		Up   UpCmd     `cmd:"" help:"up"`
		Down DownCmd   `cmd:"" help:"down"`
		Mesh MeshGroup `cmd:"" help:"mesh"`
	}
	parser, err := kong.New(&root, kong.Name("sesh"))
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	_, err = parser.Parse([]string{"hub", "serve"})
	if err == nil {
		t.Fatal("expected parse error for removed `hub` command, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected argument hub") {
		t.Fatalf("`hub` still registered: parser error = %q (want unknown-command rejection)", err)
	}
}

// TestResolveHubURL_HardFailsWithoutConfig asserts the Q2 hard-fail mode:
// when neither SESH_HUB_URL nor NATS_URL is set, hub-URL resolution returns
// an actionable error that names the remediation env var (SESH_HUB_URL) and
// does NOT silently fall back to localhost or attempt to spawn a hub.
func TestResolveHubURL_HardFailsWithoutConfig(t *testing.T) {
	t.Setenv("SESH_HUB_URL", "")
	t.Setenv("NATS_URL", "")
	// Ensure neither var leaks from the ambient environment.
	os.Unsetenv("SESH_HUB_URL")
	os.Unsetenv("NATS_URL")

	url, err := resolveHubURL()
	if err == nil {
		t.Fatalf("resolveHubURL returned %q with no hub configured; want hard error", url)
	}
	if !strings.Contains(err.Error(), "SESH_HUB_URL") {
		t.Fatalf("error %q does not name the remediation env var SESH_HUB_URL", err)
	}
}

// TestResolveHubURL_PrefersSeshHubURL asserts the resolution precedence:
// SESH_HUB_URL wins over NATS_URL, and NATS_URL is the fallback. This pins
// the explicit-env contract (no implicit localhost, no auto-spawn).
func TestResolveHubURL_PrefersSeshHubURL(t *testing.T) {
	t.Setenv("NATS_URL", "nats://from-nats-url:4222")
	t.Setenv("SESH_HUB_URL", "nats://from-sesh-hub-url:4222")
	got, err := resolveHubURL()
	if err != nil {
		t.Fatalf("resolveHubURL: %v", err)
	}
	if got != "nats://from-sesh-hub-url:4222" {
		t.Errorf("resolveHubURL = %q, want SESH_HUB_URL value", got)
	}

	t.Setenv("SESH_HUB_URL", "")
	os.Unsetenv("SESH_HUB_URL")
	got, err = resolveHubURL()
	if err != nil {
		t.Fatalf("resolveHubURL (NATS_URL fallback): %v", err)
	}
	if got != "nats://from-nats-url:4222" {
		t.Errorf("resolveHubURL = %q, want NATS_URL fallback value", got)
	}
}

// TestUpCmd_Run_HardFailsWithoutHub asserts that `sesh up` with no resolvable
// hub URL returns a clear error naming SESH_HUB_URL and does NOT attempt to
// spawn a hub. The session label is derived from cwd; HOME is isolated so no
// ambient hub state interferes. The error must surface the remediation env
// var so the operator knows what to set.
func TestUpCmd_Run_HardFailsWithoutHub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	t.Setenv("SESH_HUB_URL", "")
	t.Setenv("NATS_URL", "")
	os.Unsetenv("SESH_HUB_URL")
	os.Unsetenv("NATS_URL")

	c := &UpCmd{Session: "alpha", Scope: "session"}
	err := c.Run()
	if err == nil {
		t.Fatal("sesh up succeeded with no hub configured; want hard error")
	}
	if !strings.Contains(err.Error(), "SESH_HUB_URL") {
		t.Fatalf("sesh up error %q does not name SESH_HUB_URL remediation", err)
	}
}
