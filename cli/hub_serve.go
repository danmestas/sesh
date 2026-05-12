package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/danmestas/EdgeSync/hub"
)

// HubCmd groups hub subcommands. Only one exists today, but kong wants the
// nesting for `sesh hub serve` to render cleanly.
type HubCmd struct {
	Serve HubServeCmd `cmd:"" help:"Run the sesh hub daemon (~/.sesh/). Auto-spawned by sesh up; rarely invoked by hand."`
}

type HubServeCmd struct {
	Keepalive bool `help:"Stay alive past the last session disconnect. Default: exit when last leaf disconnects."`

	// Override knobs — rarely needed. The default ports are auto-picked.
	HTTPPort       int `help:"Fossil HTTP port (0 = auto)" default:"0"`
	NATSClientPort int `help:"NATS client port (0 = auto)" default:"0"`
	NATSLeafPort   int `help:"NATS leafnode port (0 = auto)" default:"0"`

	// StartupGrace is how long to wait for the first leaf to connect after
	// startup before declaring the hub abandoned and exiting. Only applies
	// when --keepalive is unset. Default generous enough for the slowest
	// sesh-up fork-exec sequence.
	StartupGrace time.Duration `help:"How long to wait for the first leaf before auto-exit (auto-shutdown mode only)" default:"30s"`
}

func (c *HubServeCmd) Run() error {
	urlPath, err := hubURLPath()
	if err != nil {
		return err
	}
	repoPath, err := hubRepoPath()
	if err != nil {
		return err
	}
	seshDir, err := seshHome()
	if err != nil {
		return err
	}

	// O_EXCL on hub.url IS the race resolution. `sesh up` callers already
	// serialize via hub.spawn.lock and remove any stale URL before spawn,
	// so reaching this branch means another hub is running OR a manual
	// `sesh hub serve` is racing. Distinguish three cases:
	//   - URL present and reachable: another hub is running, refuse.
	//   - URL present but empty: another hub is mid-boot, refuse.
	//   - URL present and unreachable: truly stale, take over.
	urlFile, err := os.OpenFile(urlPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, _ := os.ReadFile(urlPath)
			trimmed := stringTrim(existing)
			switch {
			case trimmed == "":
				return errors.New("another sesh hub serve is mid-boot (hub.url present but unwritten)")
			case reachable(trimmed):
				return fmt.Errorf("hub already running at %s", trimmed)
			}
			// Truly stale — take over.
			_ = os.Remove(urlPath)
			urlFile, err = os.OpenFile(urlPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		}
		if err != nil {
			return fmt.Errorf("acquire hub.url: %w", err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h, err := hub.NewHub(ctx, hub.Config{
		RepoPath:       repoPath,
		ServerName:     "sesh-hub",
		NATSStoreDir:   filepath.Join(seshDir, "messaging"),
		FossilHTTPPort: c.HTTPPort,
		NATSClientPort: c.NATSClientPort,
		NATSLeafPort:   c.NATSLeafPort,
		// Inherited from `sesh up` so the hub's Fossil subscribes to the
		// same EdgeSync fossil-sync subject as the project's leaves.
		// Empty (e.g. hand-launched hub) preserves prior auto-generated
		// behavior.
		ProjectCode: os.Getenv("SESH_PROJECT_CODE"),
	})
	if err != nil {
		urlFile.Close()
		_ = os.Remove(urlPath)
		return fmt.Errorf("hub: %w", err)
	}

	leafURL := h.LeafURL()
	if _, err := fmt.Fprintln(urlFile, leafURL); err != nil {
		urlFile.Close()
		_ = os.Remove(urlPath)
		_ = h.Stop()
		return fmt.Errorf("write hub.url: %w", err)
	}
	urlFile.Close()
	defer os.Remove(urlPath)

	// Publish the hub's NATS client URL so clients doing hub/project/
	// workflow-scoped KV work can connect to the hub's JetStream domain.
	// Sessions run their own JetStream domains; the hub's is shared.
	natsURLPath, err := hubNATSURLPath()
	if err != nil {
		_ = h.Stop()
		return fmt.Errorf("hub.nats.url path: %w", err)
	}
	if err := writeAtomic(natsURLPath, h.NATSURL()+"\n"); err != nil {
		_ = h.Stop()
		return fmt.Errorf("write hub.nats.url: %w", err)
	}
	defer os.Remove(natsURLPath)

	slog.Info("sesh hub running",
		"keepalive", c.Keepalive,
		"repo", repoPath,
		"nats", h.NATSURL(),
		"leaf_url", leafURL,
		"http", "http://"+h.HTTPAddr(),
	)

	// Auto-shutdown loop: poll NumLeafs. After first connection, exit when
	// it returns to zero. With --keepalive, skip entirely.
	if !c.Keepalive {
		go autoShutdownLoop(ctx, cancel, h, c.StartupGrace)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.ServeHTTP(ctx) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			slog.Error("hub HTTP serve error", "err", err)
		}
	}

	slog.Info("sesh hub shutting down")
	return h.Stop()
}

// autoShutdownLoop polls the hub's leaf connection count. Once a leaf has
// connected at least once, exits the moment the count returns to zero. If
// no leaf connects within startupGrace, exits anyway (the spawning sesh up
// died before connecting, or this hub was started in error).
func autoShutdownLoop(ctx context.Context, cancel context.CancelFunc, h *hub.Hub, startupGrace time.Duration) {
	var hadLeaf atomic.Bool
	startupDeadline := time.Now().Add(startupGrace)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := h.NumLeafs()
			if n > 0 {
				hadLeaf.Store(true)
				continue
			}
			// n == 0
			if hadLeaf.Load() {
				slog.Info("last leaf disconnected — hub auto-shutting down")
				cancel()
				return
			}
			// Never had a leaf yet — only exit if grace period elapsed.
			if time.Now().After(startupDeadline) {
				slog.Warn("no leaf connected within startup grace — hub exiting", "grace", startupGrace)
				cancel()
				return
			}
		}
	}
}

// reachable does a fast TCP dial to test whether a URL's host:port is
// listening. Used to distinguish stale hub.url from a real running hub.
func reachable(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	addr := u.Host
	if addr == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// stringTrim is a tiny helper to trim whitespace from a byte slice as string.
func stringTrim(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
