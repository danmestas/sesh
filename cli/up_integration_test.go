package cli_test

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestUp_PopulatesSessionURLs is the topology pre-req: a running `sesh up`
// must surface its NATS client URL and leafnode URL in the project state
// file so sub-leaves and clients can attach without grepping logs.
//
// Builds the sesh binary into a tmpdir, isolates HOME and the project cwd,
// spawns `sesh up`, polls for the JSON to gain URLs, TCP-dials both, then
// SIGINTs and verifies the state file is reaped.
func TestUp_PopulatesSessionURLs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: builds binary + spawns subprocess")
	}

	bin := buildSesh(t)

	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	cmd := exec.Command(bin, "up", "--session=alpha")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh up: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})

	statePath := filepath.Join(project, ".sesh", "sessions", "alpha.json")
	state := waitForURLs(t, statePath, 15*time.Second)

	if state.PID != cmd.Process.Pid {
		t.Errorf("state PID = %d, want %d", state.PID, cmd.Process.Pid)
	}
	dial(t, state.NATSURL, "NATSURL")
	dial(t, state.LeafURL, "LeafURL")

	// nats_ws_url is the WebSocket endpoint for browser / CF Workers
	// clients. WS is enabled by default; the field must be present and
	// the underlying TCP socket dialable. (NATS-level WS pub/sub
	// round-trip is covered by EdgeSync's hub package tests; sesh's
	// job here is just to confirm the URL surfaces in the session JSON
	// and the socket is bound end-to-end.)
	if state.NATSWSURL == "" {
		t.Fatal("nats_ws_url missing from session JSON (WS default-enabled)")
	}
	if !strings.HasPrefix(state.NATSWSURL, "ws://") {
		t.Errorf("nats_ws_url = %q, want ws:// scheme", state.NATSWSURL)
	}
	dial(t, state.NATSWSURL, "NATSWSURL")

	// hub.nats.url is the hub's client NATS URL — clients doing
	// hub/project/workflow-scoped KV work connect here so their KV
	// buckets live in the shared (hub) JetStream domain, not in a
	// session's domain.
	hubNATSURLPath := filepath.Join(home, ".sesh", "hub.nats.url")
	hubNATSURL := readTrimmed(t, hubNATSURLPath)
	dial(t, hubNATSURL, "hub.nats.url")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Logf("sesh up exit: %v", err)
	}

	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file lingered after shutdown: %v", err)
	}
	// hub.nats.url is cleaned up by the hub process on its auto-shutdown
	// (~500ms after sesh up exits), not by sesh up itself — same as
	// hub.url. Not asserted here; race-prone.
}

// readTrimmed reads a file and trims trailing whitespace. The hub
// writes its NATS URL with a trailing newline; clients trim before use.
func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(data)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\r' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// stateOnDisk mirrors cli.SessionState — keeping the integration test in
// package cli_test (external) means we can't reference the unexported
// claim helpers, but the JSON schema is the public contract.
type stateOnDisk struct {
	PID       int    `json:"pid"`
	NATSURL   string `json:"nats_url"`
	NATSWSURL string `json:"nats_ws_url"`
	LeafURL   string `json:"leaf_url"`
	FossilURL string `json:"fossil_url"`
}

func waitForURLs(t *testing.T, path string, timeout time.Duration) stateOnDisk {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last stateOnDisk
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(data, &last); err == nil {
				if last.NATSURL != "" && last.LeafURL != "" {
					return last
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for URLs in %s; last=%+v", path, last)
	return last
}

func dial(t *testing.T, u, label string) {
	t.Helper()
	if u == "" {
		t.Fatalf("%s empty", label)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse %s %q: %v", label, u, err)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s %s: %v", label, parsed.Host, err)
	}
	_ = conn.Close()
}

func buildSesh(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	bin := filepath.Join(t.TempDir(), "sesh")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/sesh")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}
