package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestHubURL_RemovedAfterSeshUpShutdown asserts the observable promise of
// the autoShutdownLoop + leaf-teardown chain: when the spawning `sesh up`
// is signalled to exit, the user-wide hub's `~/.sesh/hub.url` is removed
// within a small bounded window. This is the regression test for
// danmestas/sesh#61, where a Docker/Linux bench (orch#131 T1.2) saw
// `hub.url` linger 6+ seconds because the embedded NATS server's
// remote-leaf connection wasn't cleanly disconnected on shutdown, leaving
// the user-wide hub's `NumLeafs()` >= 1 until OS-level TCP keepalive
// eventually tripped (minutes-to-hours).
//
// Expected flow on a clean shutdown:
//
//  1. SIGINT → sesh up's signal handler cancels ctx
//  2. Starter.serve() unblocks, calls s.h.Stop()
//  3. hub.Hub.Stop() shuts down the embedded NATS server, which cleanly
//     closes the outbound leaf solicit to the user-wide hub
//  4. User-wide hub's autoShutdownLoop sees NumLeafs() drop to 0 (within
//     its 500ms poll tick) and cancels its serve ctx
//  5. hub_serve.go's deferred urlLease.Release() removes hub.url
//
// 3s budget covers: 500ms autoShutdownLoop poll + nats-server WaitForShutdown
// + filesystem release, with comfortable slack for slow CI runners.
//
// This test fails against any future regression that breaks the leaf-drain
// chain (e.g. dropping the Stop() call on the signal path, or an EdgeSync
// regression where hub.Stop() stops closing the leaf cleanly).
func TestHubURL_RemovedAfterSeshUpShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: builds binary + spawns subprocess")
	}

	bin := buildSesh(t)

	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	cmd := exec.Command(bin, "up", "--session=hubclean")
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

	// Wait for full bring-up: session JSON has URLs AND hub.url is
	// published. Without the second gate, we'd race against the user-wide
	// hub's own startup and might SIGINT before the leaf has registered
	// with it — making the cleanup look "fast" only because there was
	// nothing to clean up.
	statePath := filepath.Join(project, ".sesh", "sessions", "hubclean.json")
	_ = waitForURLs(t, statePath, 15*time.Second)

	hubURLPath := filepath.Join(home, ".sesh", "hub.url")
	waitForFile(t, hubURLPath, 15*time.Second)

	// Deliberately do NOT pad with sleep here. The bench (orch#131 T1.2)
	// SIGINTs sesh up immediately after the session JSON materializes,
	// and the bug being regressed is precisely that the user-wide hub's
	// autoShutdownLoop's 500ms-tick can miss a sub-500ms leaf
	// connect/disconnect cycle and then wait the full 30s startupGrace
	// instead of shutting down promptly. The fix in hub_serve.go's
	// autoShutdownLoop (faster pre-hadLeaf polling) makes this
	// deterministic — and this test fails the moment that fix is
	// regressed.

	// Signal sesh up to shut down. SIGINT mirrors what `sesh down` sends
	// via Terminate; the leaf-drain path is shared between operator
	// Ctrl-C and `sesh down`.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Logf("sesh up exit: %v", err)
	}

	// Now the 3s budget: hub.url must be gone. If this test fails, look
	// at ~/.sesh/hub.log for the "last leaf disconnected — hub
	// auto-shutting down" line: present = the loop fired but the
	// urlLease.Release deferred path didn't reap; absent = NumLeafs()
	// never went to 0, i.e. the leaf-drain didn't happen on sesh up
	// exit.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(hubURLPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Failure path: capture hub.log tail for diagnostic, matching the
	// shape the orch bench emits when it SKIPs.
	hubLog := filepath.Join(home, ".sesh", "hub.log")
	if data, err := os.ReadFile(hubLog); err == nil {
		t.Logf("hub.log tail:\n%s", tailBytes(data, 2048))
	}
	t.Fatalf("hub.url at %s still present 3s after sesh up SIGINT; "+
		"leaf-disconnect chain did not converge", hubURLPath)
}

// waitForFile blocks until path exists or timeout elapses. Test fails on
// timeout.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", path)
}

// tailBytes returns the last n bytes of b, or all of b if shorter. Used to
// surface hub.log context on failure without dumping arbitrary length.
func tailBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
