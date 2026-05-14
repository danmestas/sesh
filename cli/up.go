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

	// Bootstrap planning happens before the hub bring-up so a
	// project-code drift between the local pin and the hub's on-disk
	// repo (issue #26) aborts with an actionable message rather than
	// fatal-ing inside EdgeSync's clone path. Probe failures degrade
	// to "no hub content" — the safer default, since autosync
	// reconciles any duplicate seed.
	hubFossilURL, hubProjectCode, probeErr := ProbeHub()
	if probeErr != nil {
		slog.Warn("hub-content probe failed; falling back to seed-from-cwd", "err", probeErr)
		hubFossilURL, hubProjectCode = "", ""
	}
	plan, err := MakePlan(World{
		LocalProjectCode: projectCode,
		HubFossilURL:     hubFossilURL,
		HubProjectCode:   hubProjectCode,
		FreshRepo:        freshRepo,
		SeedMode:         SeedMode(c.Seed),
	})
	if err != nil {
		return fmt.Errorf("bootstrap plan: %w", err)
	}
	if plan.Conflict != nil {
		return errors.New(plan.Conflict.Message)
	}

	leafURL, err := ensureHubRunning(projectCode)
	if err != nil {
		return fmt.Errorf("hub bring-up: %w", err)
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
		SeedFromUpstream: plan.HubFossilURL,
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

	if err := Execute(plan, Deps{Ctx: ctx, Hub: h, Cwd: cwd, RepoPath: repoPath}); err != nil {
		slog.Warn("fossil seed failed (continuing without seed)", "err", err)
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

// ensureHubRunning returns the hub's leaf URL. Reuses any existing hub via
// HubGuard's fast/slow path; on a spawner lease it fork-execs `sesh hub
// serve` then polls for the daemon's published URL. The flock-serialized
// spawn dance (so concurrent `sesh up` invocations never fork-exec
// competing hubs) lives entirely inside HubGuard now.
func ensureHubRunning(projectCode string) (string, error) {
	stateDir, err := seshHome()
	if err != nil {
		return "", err
	}

	urls, lease, err := AcquireOrReuse(stateDir)
	if err != nil {
		return "", err
	}
	if !lease.IsSpawner() {
		_ = lease.Release()
		return urls.Primary, nil
	}
	defer lease.Release()

	if err := spawnHub(projectCode); err != nil {
		return "", err
	}

	// Poll for the spawned hub to publish its URL. 15s covers slow
	// JetStream replay on a warm store. Lease (and the flock it holds) is
	// kept alive until we return — racing AcquireOrReuse callers block
	// here, so they wake up to a published URL rather than racing into a
	// second spawn.
	urlPath := filepath.Join(stateDir, "hub.url")
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
