package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// syncBuf is a goroutine-safe wrapper around bytes.Buffer. Necessary
// because os/exec writes a subprocess's stderr to the buffer from a
// goroutine while the test reads from it in a polling loop.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// startExternalNATSServer starts an in-process nats-server on a random port
// and returns its client URL. sesh is a NATS client now — it dials an
// external hub rather than embedding one, so the integration tests stand up
// a real server here and point `sesh up` at it via SESH_HUB_URL. The server
// shuts down when the test ends.
func startExternalNATSServer(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Port: -1} // random port
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("external nats server new: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatalf("external nats server not ready within 5s")
	}
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

// startSeshArgs launches `sesh up --session=<label>` pointed at the external
// hub (SESH_HUB_URL=hubURL), in project with HOME set, returning the cmd and
// a goroutine-safe stderr buffer. extra args (e.g. --exec=...) are appended
// after --session.
func startSeshArgs(t *testing.T, bin, home, project, session, hubURL string, extra ...string) (*exec.Cmd, *syncBuf) {
	t.Helper()
	args := append([]string{"up", "--session=" + session}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home, "SESH_HUB_URL="+hubURL)
	cmd.Stdout = os.Stderr
	stderr := &syncBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh up %s: %v", strings.Join(args, " "), err)
	}
	return cmd, stderr
}

// killAndWait SIGINTs the sesh up process and waits up to 5s for it to exit,
// escalating to SIGKILL on timeout. Safe to call on an already-exited cmd.
func killAndWait(t *testing.T, cmd *exec.Cmd, _ *syncBuf) {
	t.Helper()
	if cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-done
	}
}

// setupGitWorktree initializes a minimal git repo in dir so refagent's
// resolveProjectID can pin a project-id and `sesh up` has a worktree to run
// inside. Uses isolated git identity so no global config is required.
func setupGitWorktree(t *testing.T, dir string) {
	t.Helper()
	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	mustWrite := func(rel, content string, mode os.FileMode) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	run("git", "init", "-q", "-b", "main")
	mustWrite(".gitignore", ".sesh/\n", 0o644)
	mustWrite("hello.txt", "hello\n", 0o644)
	run("git", "add", ".gitignore", "hello.txt")
	run("git", "commit", "-q", "-m", "init")
}
