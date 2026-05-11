// Package cli defines sesh's command-line surface. It owns session and
// agent vocabulary (lockfiles, ULID-shaped session IDs, ~/.sesh disk layout)
// on top of EdgeSync's neutral hub package.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danmestas/EdgeSync/hub"
	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/sesh/coord"
)

// LeafCmd is the top-level command group for session leaves.
type LeafCmd struct {
	Serve LeafServeCmd `cmd:"" help:"Run a session leaf, soliciting upstream to a hub"`
}

type LeafServeCmd struct {
	Upstream string `required:"" help:"Hub leaf URL (e.g. nats-leaf://127.0.0.1:7422) — what this leaf solicits"`
	HubNATS  string `required:"" help:"Hub client NATS URL (e.g. nats://127.0.0.1:4222) — used for session-lease coord on the hub's JetStream"`
	Project  string `required:"" help:"Project name"`
	Session  string `help:"Session label (default: time-prefixed random id; if user-set, a single-machine lockfile guards against collision)"`

	StoreDir       string `help:"JetStream store dir (default sibling of repo)"`
	HTTPPort       int    `help:"Fossil HTTP port (0 = auto)" default:"0"`
	NATSClientPort int    `help:"NATS client port (0 = auto)" default:"0"`
	NATSLeafPort   int    `help:"NATS leafnode port (0 = auto)" default:"0"`

	// RenewInterval controls how often the lease is renewed against the
	// hub. Defaults to ~TTL/3 (10s when TTL is 30s). Override only for
	// tests or unusual deployments.
	RenewInterval time.Duration `help:"How often to renew the session lease" default:"10s"`
}

func (c *LeafServeCmd) Run(g *libfossilcli.Globals) error {
	sessionLabel := c.Session
	sessionUserSet := sessionLabel != ""
	if !sessionUserSet {
		sessionLabel = newSessionID()
	}

	name := fmt.Sprintf("%s-session-%s", c.Project, sessionLabel)

	if sessionUserSet {
		release, err := acquireSessionLock(c.Project, sessionLabel)
		if err != nil {
			return err
		}
		defer release()
	}

	repoPath := g.Repo
	if repoPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		repoPath = filepath.Join(home, ".sesh", "sessions", c.Project, sessionLabel, "session.repo")
		if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(repoPath), err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Claim the session lease on the hub before bringing the local hub up.
	// Fail fast if another machine already holds it.
	owner := ownerID()
	cd, err := coord.Connect(ctx, c.HubNATS)
	if err != nil {
		return err
	}
	defer cd.Close()

	if _, err := cd.Claim(ctx, c.Project, sessionLabel, owner); err != nil {
		return err
	}
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer relCancel()
		if err := cd.Release(relCtx, c.Project, sessionLabel, owner); err != nil {
			slog.Warn("session lease release failed", "err", err)
		}
	}()

	renewerStop := make(chan struct{})
	renewerDone := make(chan struct{})
	go renewerLoop(ctx, cd, c.Project, sessionLabel, owner, c.RenewInterval, renewerStop, renewerDone)
	defer func() {
		close(renewerStop)
		<-renewerDone
	}()

	h, err := hub.NewHub(ctx, hub.Config{
		RepoPath:       repoPath,
		ServerName:     name,
		NATSStoreDir:   c.StoreDir,
		FossilHTTPPort: c.HTTPPort,
		NATSClientPort: c.NATSClientPort,
		NATSLeafPort:   c.NATSLeafPort,
		LeafUpstream:   c.Upstream,
	})
	if err != nil {
		return fmt.Errorf("session leaf: %w", err)
	}

	slog.Info("sesh leaf running",
		"name", h.ServerName(),
		"project", c.Project,
		"session", sessionLabel,
		"session_user_set", sessionUserSet,
		"owner", owner,
		"repo", repoPath,
		"upstream", c.Upstream,
		"hub_nats", c.HubNATS,
		"nats", h.NATSURL(),
		"leaf_listener", h.LeafUpstream(),
		"http", "http://"+h.HTTPAddr(),
	)

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.ServeHTTP(ctx) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			slog.Error("session leaf HTTP server stopped", "err", err)
		}
	}

	slog.Info("shutting down session leaf", "name", name)
	return h.Stop()
}

// newSessionID returns a time-prefixed random id: base36 ms-timestamp + hex
// random. Sortable by start time, unique by construction without a coordinator.
func newSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b)
}

// acquireSessionLock takes an exclusive lock on the (project, label) pair via
// a lockfile under ~/.sesh/state/<project>/sessions/<label>.lock. Stale
// lockfiles from dead processes are reaped automatically.
func acquireSessionLock(project, label string) (release func(), err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(home, ".sesh", "state", project, "sessions")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, label+".lock")

	if data, readErr := os.ReadFile(lockPath); readErr == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil && pid > 0 {
			if alive(pid) {
				return nil, fmt.Errorf("session %q already held by pid %d (%s)", label, pid, lockPath)
			}
			_ = os.Remove(lockPath)
		}
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create lock %s: %w", lockPath, err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()

	return func() { _ = os.Remove(lockPath) }, nil
}

func alive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// ownerID returns the lease owner identity: "<hostname>:<pid>". Sufficient
// for the spike — distinguishes leaves across machines and across processes
// on the same machine. Richer identity (start-time fingerprint, signing key)
// is a later concern.
func ownerID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// renewerLoop renews the lease on a ticker until stop fires. Renew errors are
// logged but don't terminate the loop — a transient NATS hiccup shouldn't tear
// the leaf down. If the lease genuinely expired (e.g. clock skew, prolonged
// hub outage), the next claim attempt by another process will succeed and
// this leaf's renews will keep failing until the operator notices.
func renewerLoop(ctx context.Context, cd *coord.Coord, project, sessionID, owner string, interval time.Duration, stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := cd.Renew(ctx, project, sessionID, owner); err != nil {
				slog.Error("session lease renew failed", "err", err, "project", project, "session", sessionID)
			}
		}
	}
}
