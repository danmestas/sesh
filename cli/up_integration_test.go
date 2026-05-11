package cli_test

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Logf("sesh up exit: %v", err)
	}

	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file lingered after shutdown: %v", err)
	}
}

// stateOnDisk mirrors cli.SessionState — keeping the integration test in
// package cli_test (external) means we can't reference the unexported
// claim helpers, but the JSON schema is the public contract.
type stateOnDisk struct {
	PID     int    `json:"pid"`
	NATSURL string `json:"nats_url"`
	LeafURL string `json:"leaf_url"`
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
