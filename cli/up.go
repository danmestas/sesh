package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
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

	// Seed controls what gets committed to this session's Fossil repo
	// at sesh up. Only applies on fresh repos — a session that's been
	// up before retains whatever commits accumulated. "all" (default)
	// captures tracked + untracked-but-not-gitignored files; "tracked"
	// captures only what's in the git index; "none" skips seeding.
	Seed string `help:"Seed mode for the session's Fossil repo: all|tracked|none" enum:"all,tracked,none" default:"all"`

	// Scope selects the Fossil repo path. "session" (default) gives
	// each session its own repo at .sesh/sessions/<label>.repo;
	// commits propagate cross-session via NATS autosync. "project"
	// shares one repo at .sesh/project.repo across all sessions in the
	// project; cross-session writes are synchronous via SQLite, with
	// the trade-off that concurrent writers contend on the WAL lock
	// (queued by busy_timeout, set automatically by EdgeSync). Modes
	// can mix in the same project — see cli/scope.go for the full
	// trade-off rationale.
	Scope string `help:"Fossil repo scope: session (per-session repo) or project (shared file)" enum:"session,project" default:"session"`
}

func (c *UpCmd) Run() error {
	project, err := defaultProject()
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	// Atomic same-machine same-name guard.
	release, err := claimSessionState(c.Session, os.Getpid())
	if err != nil {
		return err
	}
	defer release()

	// Fossil project-code: pinned at <cwd>/.sesh/project-code on first
	// `sesh up`, read back on subsequent runs. All sesh leaves in this
	// project arrive at the same code and therefore subscribe to the same
	// EdgeSync fossil-sync subject — that's what lets the hub's repo and
	// the session's repo see each other's commits. Pinning (rather than
	// re-deriving from hostname every run) means cross-leaf sync survives
	// VM clones, container migrations, dotfiles sync to a new laptop, and
	// manual hostname renames. Passed to hub.NewHub here and via env var
	// to the spawned `sesh hub serve` (see spawnHub).
	projectCode, err := loadOrCreateProjectCode(cwd, project)
	if err != nil {
		return fmt.Errorf("project-code: %w", err)
	}

	leafURL, err := ensureHubRunning(projectCode)
	if err != nil {
		return fmt.Errorf("hub bring-up: %w", err)
	}

	name := fmt.Sprintf("%s-session-%s", project, c.Session)
	// Fossil repo path forks on --scope. Default (session): each
	// session owns its own repo at .sesh/sessions/<label>.repo so the
	// publish hook fires natively from the session's own in-process
	// hub on every commit; cross-session convergence happens via NATS
	// autosync on the project-code subject. Opt-in (project): all
	// sessions in this project open the same .sesh/project.repo — one
	// SQLite file, synchronous cross-session reads, write contention
	// queued by busy_timeout. JetStream store stays per-session
	// regardless: each sesh up runs its own embedded NATS server and
	// can't share its store dir with peers.
	scope := SeshScope(c.Scope)
	repoPath := repoPathFor(scope, cwd, c.Session)
	storeDir := storeDirFor(cwd, c.Session)

	// Fresh = no per-session repo file yet → bootstrap this once.
	freshRepo := false
	if _, err := os.Stat(repoPath); errors.Is(err, os.ErrNotExist) {
		freshRepo = true
	}

	// Bootstrap fork: when the hub already has content from a peer
	// session's earlier autosync, this session's Fossil clones from the
	// hub via SeedFromUpstream so per-session repos start convergent.
	// When the hub is empty (this is the first session in the project),
	// SeedFromUpstream stays empty and we seed-from-cwd after NewHub.
	// Always-pass would be wrong: cloning from an empty hub would still
	// succeed but leave us seeding the same cwd snapshot from multiple
	// sessions, producing divergent commits with the same content.
	seedFromUpstream := ""
	if freshRepo {
		hubFossilURL, hubHasContent, hcErr := detectHubContent()
		if hcErr != nil {
			slog.Warn("hub-content probe failed; falling back to seed-from-cwd",
				"err", hcErr)
		} else if hubHasContent {
			seedFromUpstream = hubFossilURL
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h, err := hub.NewHub(ctx, hub.Config{
		RepoPath:         repoPath,
		ServerName:       name,
		NATSStoreDir:     storeDir,
		FossilHTTPPort:   c.HTTPPort,
		NATSClientPort:   c.NATSClientPort,
		NATSLeafPort:     c.NATSLeafPort,
		LeafUpstream:     leafURL,
		ProjectCode:      projectCode,
		SeedFromUpstream: seedFromUpstream,
	})
	if err != nil {
		return fmt.Errorf("sesh up: %w", err)
	}

	if err := updateSessionState(c.Session, SessionState{
		PID:       os.Getpid(),
		NATSURL:   h.NATSURL(),
		LeafURL:   h.LeafURL(),
		FossilURL: "http://" + h.HTTPAddr() + "/",
	}); err != nil {
		_ = h.Stop()
		return fmt.Errorf("publish session URLs: %w", err)
	}

	switch {
	case !freshRepo:
		slog.Info("fossil repo pre-existed; not re-seeding", "path", repoPath)
	case seedFromUpstream != "":
		slog.Info("fossil repo cloned from hub upstream",
			"path", repoPath, "upstream", seedFromUpstream)
	default:
		if err := seedFromGitWorktree(ctx, h, cwd, SeedMode(c.Seed)); err != nil {
			slog.Warn("fossil seed failed (continuing without seed)", "err", err)
		}
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
func ensureHubRunning(projectCode string) (string, error) {
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

	if err := spawnHub(projectCode); err != nil {
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

// detectHubContent reads ~/.sesh/hub.fossil.url and queries the hub's
// Fossil for any check-in events. Returns the hub URL plus whether the
// hub already holds at least one commit. Sessions use this to choose
// between two bootstrap paths:
//
//   - hubHasContent=false → first session in the project; seed-from-cwd
//     after NewHub.
//   - hubHasContent=true  → peer session has already committed and the
//     hub mirrored it via autosync; clone-from-hub via SeedFromUpstream
//     so the new session starts in convergent state.
//
// SQLite WAL mode allows the read here to coexist with the hub's open
// writer; the read-only flag avoids any contention. Errors are
// non-fatal: caller falls back to seed-from-cwd, the safer default
// (worst case is a duplicate seed that the autosync layer reconciles).
func detectHubContent() (hubURL string, hasContent bool, err error) {
	urlPath, err := hubFossilURLPath()
	if err != nil {
		return "", false, err
	}
	urlBytes, err := os.ReadFile(urlPath)
	if err != nil {
		return "", false, fmt.Errorf("read hub.fossil.url: %w", err)
	}
	hubURL = stringTrim(urlBytes)
	if hubURL == "" {
		return "", false, errors.New("hub.fossil.url is empty")
	}

	repoPath, err := hubRepoPath()
	if err != nil {
		return hubURL, false, err
	}
	if _, err := os.Stat(repoPath); err != nil {
		// Hub URL is published but repo file missing — treat as empty
		// and let the bootstrap fall back to seed-from-cwd.
		return hubURL, false, nil
	}

	// Read-only open via the modernc/sqlite driver registered in
	// cmd/sesh/main.go. WAL mode lets us read while the hub's writer
	// holds the file. Schema is libfossil's; event.type='ci' marks a
	// check-in row.
	db, err := sql.Open("sqlite", "file:"+repoPath+"?_pragma=mode(ro)")
	if err != nil {
		return hubURL, false, fmt.Errorf("open hub repo: %w", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(
		"SELECT count(*) FROM event WHERE type='ci'",
	).Scan(&count); err != nil {
		return hubURL, false, fmt.Errorf("count check-ins: %w", err)
	}
	return hubURL, count > 0, nil
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
func spawnHub(projectCode string) error {
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
	// SESH_PROJECT_CODE makes the hub's Fossil repo subscribe to the
	// same EdgeSync sync subject as the spawning project's leaf repos.
	cmd.Env = append(os.Environ(), "SESH_PROJECT_CODE="+projectCode)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("spawn hub: %w", err)
	}
	// Don't wait — the daemon owns the log file from here.
	_ = cmd.Process.Release()
	return nil
}
