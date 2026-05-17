package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
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
	HTTPPort          int  `help:"Fossil HTTP port (0 = auto)" default:"0"`
	NATSClientPort    int  `help:"NATS client port (0 = auto)" default:"0"`
	NATSLeafPort      int  `help:"NATS leafnode port (0 = auto)" default:"0"`
	NATSWebSocketPort int  `help:"NATS WebSocket port (0 = auto)" default:"0"`
	DisableWebSocket  bool `name:"disable-ws" help:"Disable the embedded NATS WebSocket listener. Default: enabled."`

	// StartupGrace is how long to wait for the first leaf to connect after
	// startup before declaring the hub abandoned and exiting. Only applies
	// when --keepalive is unset. Default generous enough for the slowest
	// sesh-up fork-exec sequence.
	StartupGrace time.Duration `help:"How long to wait for the first leaf before auto-exit (auto-shutdown mode only)" default:"30s"`
}

func (c *HubServeCmd) Run() error {
	seshDir, err := seshHome()
	if err != nil {
		return err
	}
	repoPath := hubRepoPath(seshDir)

	// HubGuard owns the O_EXCL claim on hub.url plus the stale-takeover
	// dance; failure here means another hub is already running or
	// mid-boot.
	urlLease, err := RegisterDaemon(seshDir)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h, err := hub.NewHub(ctx, hub.Config{
		RepoPath:          repoPath,
		ServerName:        "sesh-hub",
		NATSStoreDir:      hubStoreDir(seshDir),
		FossilHTTPPort:    c.HTTPPort,
		NATSClientPort:    c.NATSClientPort,
		NATSLeafPort:      c.NATSLeafPort,
		EnableWebSocket:   !c.DisableWebSocket,
		NATSWebSocketPort: c.NATSWebSocketPort,
		// Inherited from `sesh up` so the hub's Fossil subscribes to the
		// same EdgeSync fossil-sync subject as the project's leaves.
		// Empty (e.g. hand-launched hub) preserves prior auto-generated
		// behavior.
		ProjectCode: os.Getenv("SESH_PROJECT_CODE"),
	})
	if err != nil {
		_ = urlLease.Release()
		return fmt.Errorf("hub: %w", err)
	}

	// Start the HTTP serve loop in a goroutine and gate URL publication on
	// Ready closing. Until Ready closes, the Fossil HTTP listener is bound
	// but not yet calling Accept; advertising hub.fossil.url before then
	// lets racers dial a half-open listener (#15 residual gap closed via
	// EdgeSync#171). Buffered cap-1 so the goroutine never blocks on exit.
	serveErr := make(chan error, 1)
	go func() { serveErr <- h.ServeHTTP(ctx) }()

	select {
	case <-h.Ready():
	case err := <-serveErr:
		_ = urlLease.Release()
		_ = h.Stop()
		if err != nil {
			return fmt.Errorf("hub HTTP serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = urlLease.Release()
		_ = h.Stop()
		return ctx.Err()
	}

	leafURL := h.LeafURL()
	if err := urlLease.Publish(leafURL); err != nil {
		_ = urlLease.Release()
		_ = h.Stop()
		return err
	}
	defer urlLease.Release()

	// Publish hub.nats.url and hub.fossil.url atomically via HubInfo. NATS
	// is the hub's JetStream entry point (sessions run their own domains;
	// the hub's is the shared one). Fossil is the HTTP xfer endpoint
	// peer sessions read at `sesh up` to decide clone-from-hub vs.
	// seed-from-cwd. hub.url is on the parallel ownership channel —
	// urlLease.Publish above — and is not part of WriteHubInfo's surface.
	if err := WriteHubInfo(seshDir, HubInfo{
		NATSURL:   h.NATSURL(),
		FossilURL: "http://" + h.HTTPAddr() + "/",
	}); err != nil {
		_ = h.Stop()
		return fmt.Errorf("publish hub info: %w", err)
	}
	defer func() { _ = ClearHubInfo(seshDir) }()

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
//
// Deliberately has NO idle-after-first-leaf timeout: once a leaf has
// connected, the hub stays alive across arbitrarily long
// connect/disconnect cycles as long as at least one leaf is currently
// present. A leaf that flap-cycles (connects, disconnects, reconnects
// forever) keeps the hub running indefinitely. This is the intended
// behavior because sesh sessions are long-lived: an operator suspending
// their laptop and resuming hours later should find the hub still
// running so the session reattaches cleanly. The cost (a flapping leaf
// pins the hub) is preferred over the alternative (an idle timeout that
// races a slow editor restart and kills the hub mid-flap, forcing every
// session to fork-exec a fresh daemon).
//
// Poll cadence is split between two regimes to close the issue #61
// race-where-a-leaf-connects-and-disconnects-between-ticks bug:
//
//   - **Pre-hadLeaf** (no leaf observed yet): poll every 50ms. Catches
//     rapid `sesh up` / `sesh down` cycles (orch bench T1.2) where the
//     leaf is up for <500ms total. A leaf that connects and disconnects
//     within 50ms of itself is still possible but vanishingly rare in
//     practice; if it happens, the 30s startupGrace still bounds the
//     stuck-hub case.
//   - **Post-hadLeaf** (a leaf has been observed): drop to 500ms. The
//     long-lived-session invariant defended in the prose above only
//     applies once at least one leaf has stably attached, so the fast
//     pre-hadLeaf cadence does not perturb operator-suspends-laptop
//     scenarios.
//
// The split addresses #61 without touching the brief's load-bearing
// long-lived-session behavior: faster polling pre-hadLeaf means the
// observation that a leaf connected at all is more reliable; the
// steady-state 500ms cadence after first-leaf-observed is unchanged.
func autoShutdownLoop(ctx context.Context, cancel context.CancelFunc, h *hub.Hub, startupGrace time.Duration) {
	var hadLeaf atomic.Bool
	startupDeadline := time.Now().Add(startupGrace)

	// preHadLeafTick gives the loop a tight chance to observe `NumLeafs()>0`
	// before the leaf disconnects, even for sub-500ms connect/disconnect
	// cycles. Switches to steadyTick once hadLeaf is true.
	const preHadLeafTick = 50 * time.Millisecond
	const steadyTick = 500 * time.Millisecond

	ticker := time.NewTicker(preHadLeafTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := h.NumLeafs()
			if n > 0 {
				if hadLeaf.CompareAndSwap(false, true) {
					// First-leaf-observed: relax to steady-state cadence.
					ticker.Reset(steadyTick)
				}
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
