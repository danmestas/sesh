package cli_test

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHub_ReadyGatesURLPublication asserts that the daemon does not
// advertise hub.fossil.url until the Fossil HTTP server is accepting
// requests. Pre-fix, hub_serve.go wrote the file as soon as NewHub
// returned (after NATS bind) but before ServeHTTP entered its Accept
// loop, leaving a microsecond window where a racer reading
// hub.fossil.url could fire an HTTP request that hung. Post-fix, the
// publish is gated on EdgeSync's hub.Ready() channel.
//
// Regression strategy: tight-poll for the file, then issue an HTTP
// GET with a short client timeout the instant content appears. With
// the gate in place, any HTTP response — status code irrelevant —
// proves Accept ran before publication. Without the gate, the GET
// would either error or time out as the request sits in the listen
// backlog waiting for srv.Serve to start.
func TestHub_ReadyGatesURLPublication(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: spawns sesh hub serve subprocess")
	}
	t.Parallel()

	bin := buildSesh(t)
	home := t.TempDir()

	cmd := exec.Command(bin, "hub", "serve", "--keepalive")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh hub serve: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil || cmd.ProcessState != nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGINT)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGKILL)
			<-done
		}
	})

	fossilURLPath := filepath.Join(home, ".sesh", "hub.fossil.url")

	deadline := time.Now().Add(15 * time.Second)
	var rawURL string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fossilURLPath)
		if err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				rawURL = s
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if rawURL == "" {
		t.Fatalf("timed out waiting for hub.fossil.url at %s", fossilURLPath)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		t.Fatalf("immediate HTTP GET to %s right after hub.fossil.url appeared: %v "+
			"(Ready gate did not hold publication until Accept loop was live)", u, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}
