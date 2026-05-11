package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/danmestas/EdgeSync/hub"
)

// UpCmd brings a session up. Cwd-derived project; --session required.
type UpCmd struct {
	Session string `required:"" help:"Session label (free-form)"`

	HTTPPort       int `help:"Fossil HTTP port (0 = auto)" default:"0"`
	NATSClientPort int `help:"NATS client port (0 = auto)" default:"0"`
	NATSLeafPort   int `help:"NATS leafnode port (0 = auto)" default:"0"`
}

func (c *UpCmd) Run() error {
	project, err := defaultProject()
	if err != nil {
		return err
	}

	// Atomic same-machine same-name guard.
	release, err := claimSessionState(c.Session, os.Getpid())
	if err != nil {
		return err
	}
	defer release()

	leafURL, err := ensureHubRunning()
	if err != nil {
		return fmt.Errorf("hub bring-up: %w", err)
	}

	name := fmt.Sprintf("%s-session-%s", project, c.Session)

	cwd, _ := os.Getwd()
	repoPath := filepath.Join(cwd, ".sesh", "sessions", c.Session+".repo")
	storeDir := filepath.Join(cwd, ".sesh", "sessions", c.Session+".messaging")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h, err := hub.NewHub(ctx, hub.Config{
		RepoPath:       repoPath,
		ServerName:     name,
		NATSStoreDir:   storeDir,
		FossilHTTPPort: c.HTTPPort,
		NATSClientPort: c.NATSClientPort,
		NATSLeafPort:   c.NATSLeafPort,
		LeafUpstream:   leafURL,
	})
	if err != nil {
		return fmt.Errorf("sesh up: %w", err)
	}

	if err := updateSessionState(c.Session, SessionState{
		PID:     os.Getpid(),
		NATSURL: h.NATSURL(),
		LeafURL: h.LeafURL(),
	}); err != nil {
		_ = h.Stop()
		return fmt.Errorf("publish session URLs: %w", err)
	}

	slog.Info("sesh up running",
		"name", h.ServerName(),
		"project", project,
		"session", c.Session,
		"pid", os.Getpid(),
		"repo", repoPath,
		"hub_url", leafURL,
		"nats", h.NATSURL(),
		"http", "http://"+h.HTTPAddr(),
	)

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.ServeHTTP(ctx) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			slog.Error("HTTP serve error", "err", err)
		}
	}

	slog.Info("sesh up shutting down", "name", name)
	return h.Stop()
}

// ensureHubRunning returns the hub's leaf URL, spawning the hub via
// fork-exec of `sesh hub serve` if no hub is currently running.
//
// Concurrent `sesh up` invocations serialize on hub.spawn.lock (flock) so
// only one ever fork-execs a hub. The losers block on the lock, wake up,
// see the URL is reachable, and return without spawning. Without this,
// each racer would fork-exec its own hub, contend on the shared fossil +
// JetStream storage at ~/.sesh, and no hub would stabilize.
func ensureHubRunning() (string, error) {
	urlPath, err := hubURLPath()
	if err != nil {
		return "", err
	}

	// Fast path: existing URL points at a running hub. No lock needed.
	if url, err := readHubURL(urlPath); err == nil && reachable(url) {
		return url, nil
	}

	// Slow path: serialize the spawn.
	lockPath, err := hubSpawnLockPath()
	if err != nil {
		return "", err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "", fmt.Errorf("open hub spawn lock: %w", err)
	}
	defer lockFile.Close() // releases the flock
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("flock hub spawn lock: %w", err)
	}

	// Re-check under the lock: another caller may have spawned while we
	// waited.
	if url, err := readHubURL(urlPath); err == nil && reachable(url) {
		return url, nil
	}
	_ = os.Remove(urlPath)

	if err := spawnHub(); err != nil {
		return "", err
	}

	// Poll for the spawned hub to publish its URL. 15s covers slow
	// JetStream replay on a warm store.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if url, err := readHubURL(urlPath); err == nil && reachable(url) {
			return url, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", errors.New("hub didn't come up within 15s")
}

// readHubURL reads and trims hub.url.
func readHubURL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return stringTrim(data), nil
}

// spawnHub fork-execs `sesh hub serve` as a detached daemon. Stdout/stderr
// go to ~/.sesh/hub.log; stdin is /dev/null. setsid detaches from the
// controlling terminal so the daemon survives parent shutdown.
func spawnHub() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("self executable: %w", err)
	}
	logPath, err := hubLogPath()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open hub log: %w", err)
	}

	cmd := exec.Command(exe, "hub", "serve")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("spawn hub: %w", err)
	}
	// Don't wait — the daemon owns the log file from here.
	_ = cmd.Process.Release()
	return nil
}
