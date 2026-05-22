package cli

import (
	"context"
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

// harnessEnv is the supporting struct for spawnHarness / Harness (Hybrid C).
// It carries the five canonical SESH_* values (plus NATS_URL) plus the
// Role/Class for coordination subjects. Extended here per decision A so the
// single env-construction site in spawnHarness is the only place that ever
// mentions the SESH_* names (ready for role/class Phase 4 without edit
// amplification elsewhere).
type harnessEnv struct {
	Session   string
	NATSURL   string
	NATSWSURL string
	FossilURL string
	LeafURL   string
	Role      string
	Class     string
}

// Harness is the (initially thin) owner of one child coding-agent harness
// process. Per locked Hybrid (C) it is introduced now inside cli/up.go even
// though v1 keeps most logic in the free func + waiter; the type gives the
// future daemon/wrapper modes, Done() chan, signal ownership, and extraction
// point (see Future Extraction Commitment). A second caller (sesh exec, etc.)
// will force the move to its own package.
type Harness struct {
	cmd  *exec.Cmd
	done chan error
	env  harnessEnv
}

// UpCmd brings a session up. Cwd-derived project; --session required.
//
// Run is intentionally thin: it constructs a Starter (which holds every
// piece of session-derived state and the lifecycle of the session
// claim) and asks it to Start. The phase pipeline lives behind Starter's
// methods so adding a new pipeline step (goal-management bootstrap,
// agent attach, etc.) means registering a step on Starter rather than
// editing this method.
type UpCmd struct {
	Session string `required:"" help:"Session label (free-form)"`

	HTTPPort          int `help:"Fossil HTTP port (0 = auto)" default:"0"`
	NATSClientPort    int `help:"NATS client port (0 = auto)" default:"0"`
	NATSLeafPort      int `help:"NATS leafnode port (0 = auto)" default:"0"`
	NATSWebSocketPort int `help:"NATS WebSocket port (0 = auto)" default:"0"`
	// DisableWebSocket lets the operator turn off the embedded NATS
	// WebSocket listener. Default is enabled — sesh's loopback-only
	// posture makes the WS endpoint dialable for browser / Cloudflare
	// Workers clients via @nats-io/transport-websockets without
	// surfacing any extra knob. Opt-out is for environments that want
	// to minimize listening sockets.
	DisableWebSocket bool `name:"disable-ws" help:"Disable the embedded NATS WebSocket listener (advertised as nats_ws_url in the session JSON). Default: enabled."`

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

	// Exec, when non-empty, causes sesh up to spawn the given command as a
	// child harness after the session is ready (wrapper lifecycle by default).
	Exec string `name:"exec" help:"Run <cmd> as a child coding-agent harness after the session is ready. Passed to sh -c. Example: --exec='claude --dangerously-skip-permissions'"`

	// Role and Class (locked decision A) — propagated to the harness for sesh.* coordination subjects.
	Role  string `name:"role"  help:"Role for coordination subjects (e.g. implementer, verifier, spy). Default: worker"`
	Class string `name:"class" help:"Class for coordination subjects: active (default) or observer" enum:"active,observer" default:"active"`
}

func (c *UpCmd) Run() error {
	// Tier-1 safety: validate the session label before ANY path math.
	// NewStarter computes <cwd>/.sesh/sessions/<label>.repo, the
	// JetStream storeDir at <cwd>/.sesh/sessions/<label>.messaging/,
	// and the claim file under .sesh/sessions/<label>.json. A hostile
	// label like "../sessions" would let those paths escape .sesh/
	// before we ever touched validateLabel. The validator MUST sit
	// above NewStarter — same contract as worktree.go / materialize.go.
	if err := validateLabel(c.Session); err != nil {
		return fmt.Errorf("sesh up: invalid label %q: %w", c.Session, err)
	}
	s, err := NewStarter(c)
	if err != nil {
		return err
	}
	defer s.Release()
	return s.Start(context.Background())
}

// Starter owns the bring-up of one `sesh up`. It captures everything
// derived from UpCmd + the project on disk, and exposes Start as the
// single entry point for the phase pipeline.
//
// Phase ordering (Start):
//
//  1. pre-hub bootstrap: probe the user-wide hub, decide MakePlan's
//     source, reconcile project-code drift via ResolveProjectCode.
//  2. hub-acquire: AcquireOrReuse → spawn-if-needed → return the leaf
//     URL the session's local hub will solicit into.
//  3. bind: hub.NewHub binds this session's libfossil + NATS + leaf.
//     SeedFromUpstream is keyed off the resolved project-code so the
//     SourceHub clone-from-upstream works without project-code
//     disagreement.
//  4. post-hub bootstrap: Apply commits seed-from-cwd through the
//     bound hub (SourceGitWorktree). SourceNone and SourceHub are
//     log-only at this point.
//  5. publish-session: Session.Publish writes the bound URLs into the
//     project-local state JSON so sub-leaves and `sesh down` can
//     reach this session.
//  6. serve: HTTP serve loop until ctx cancels (SIGINT / SIGTERM).
//
// The session claim is acquired by NewStarter and released by
// Release; Run defers Release so the claim never outlives the
// process.
type Starter struct {
	cmd     *UpCmd
	execCmd string // threaded from UpCmd.Exec (wrapper path; Harness later per hybrid C)
	role    string // threaded from UpCmd.Role (propagated per decision A)
	class   string // threaded from UpCmd.Class (propagated per decision A)
	project string
	cwd     string

	stateDir    string   // <cwd>/.sesh/sessions
	sessHandle  *Session // claimed in NewStarter
	scope       SeshScope
	name        string // EdgeSync server name; e.g. "myproj-session-alpha"
	repoPath    string
	storeDir    string
	freshRepo   bool
	projectCode string
	projectID   string

	probe HubProbe
	plan  Plan

	leafURL string
	h       *hub.Hub
}

// NewStarter prepares the session start: derives project + cwd + paths,
// claims the project-local session state slot via O_EXCL, and loads
// (or seeds) the project-code pin. No hub work, no network — failures
// here mean the caller can retry without leaking resources beyond what
// Release cleans up.
//
// Callers MUST defer s.Release() when NewStarter returns nil error.
func NewStarter(c *UpCmd) (*Starter, error) {
	project, err := defaultProject()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	stateDir, err := projectStateDir()
	if err != nil {
		return nil, err
	}
	sess, err := ClaimSession(stateDir, c.Session)
	if err != nil {
		return nil, err
	}
	projectCode, err := loadOrCreateProjectCode(cwd, project)
	if err != nil {
		_ = sess.Release()
		return nil, fmt.Errorf("project-code: %w", err)
	}
	projectID, err := loadOrCreateProjectID(cwd, project)
	if err != nil {
		_ = sess.Release()
		return nil, fmt.Errorf("project-id: %w", err)
	}

	scope := SeshScope(c.Scope)
	repoPath, err := repoPathFor(scope, cwd, c.Session)
	if err != nil {
		_ = sess.Release()
		return nil, fmt.Errorf("sesh up: %w", err)
	}
	storeDir, err := storeDirFor(cwd, c.Session)
	if err != nil {
		_ = sess.Release()
		return nil, fmt.Errorf("sesh up: %w", err)
	}
	freshRepo := false
	if _, err := os.Stat(repoPath); errors.Is(err, os.ErrNotExist) {
		freshRepo = true
	}

	return &Starter{
		cmd:         c,
		execCmd:     c.Exec,
		role:        c.Role,
		class:       c.Class,
		project:     project,
		cwd:         cwd,
		stateDir:    stateDir,
		sessHandle:  sess,
		scope:       scope,
		name:        fmt.Sprintf("%s-session-%s", project, c.Session),
		repoPath:    repoPath,
		storeDir:    storeDir,
		freshRepo:   freshRepo,
		projectCode: projectCode,
		projectID:   projectID,
	}, nil
}

// Release frees resources NewStarter / Start acquired. Currently the
// session-claim file; safe on nil / already-released starter.
func (s *Starter) Release() {
	if s == nil || s.sessHandle == nil {
		return
	}
	_ = s.sessHandle.Release()
}

// Start runs the phase pipeline described in the Starter doc. parent
// scopes the signal-listening ctx that all hub-bound work observes.
func (s *Starter) Start(parent context.Context) error {
	if err := s.preHubBootstrap(); err != nil {
		return err
	}
	if err := s.acquireHub(); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := s.bindHub(ctx); err != nil {
		return err
	}
	s.postHubBootstrap(ctx)
	// Auto-add .sesh/ to the project's .gitignore so `sesh materialize`
	// doesn't refuse-as-dirty over our own runtime state (#86). Idempotent;
	// no-op outside a git repo. Logged + continued on any failure — seed
	// shouldn't block on .gitignore wrangling.
	if err := ensureSeshGitignored(s.cwd); err != nil {
		slog.Warn("auto-gitignore of .sesh/ failed (continuing)", "err", err)
	}
	if err := s.publishSession(); err != nil {
		return err
	}
	return s.serve(ctx)
}

// preHubBootstrap probes the hub for content, plans the bootstrap, and
// reconciles project-code drift. Runs BEFORE hub.NewHub so the
// SeedFromUpstream path sees agreement between the local pin and the
// hub's libfossil project-code.
//
// Probe errors surface to the operator rather than silently degrade —
// a corrupt hub.repo reported as "no content" would let seed-from-cwd
// overwrite state. probe.Present=false on a clean "no hub yet" case is
// the expected steady-state.
func (s *Starter) preHubBootstrap() error {
	probe, err := ProbeHub()
	if err != nil {
		return fmt.Errorf("probe hub: %w", err)
	}
	s.probe = probe
	plan, err := MakePlan(World{
		LocalProjectCode: s.projectCode,
		HubFossilURL:     probe.FossilURL,
		HubProjectCode:   probe.ProjectCode,
		FreshRepo:        s.freshRepo,
		SeedMode:         SeedMode(s.cmd.Seed),
	})
	if err != nil {
		return fmt.Errorf("bootstrap plan: %w", err)
	}
	s.plan = plan

	if plan.Source == SourceHub {
		active, err := ResolveProjectCode(s.cwd, s.projectCode, probe.ProjectCode)
		if err != nil {
			return fmt.Errorf("resolve project-code: %w", err)
		}
		s.projectCode = active
	}
	return nil
}

// acquireHub uses HubGuard's fast/slow path to find or spawn the
// user-wide hub daemon, then captures the leaf URL the session will
// solicit into.
func (s *Starter) acquireHub() error {
	leafURL, err := ensureHubRunning(s.projectCode)
	if err != nil {
		return fmt.Errorf("hub bring-up: %w", err)
	}
	s.leafURL = leafURL
	return nil
}

// bindHub binds the session's libfossil + NATS server + leafnode
// solicit. EdgeSync's SeedFromUpstream fires here for SourceHub plans.
func (s *Starter) bindHub(ctx context.Context) error {
	h, err := hub.NewHub(ctx, hub.Config{
		RepoPath:          s.repoPath,
		ServerName:        s.name,
		NATSStoreDir:      s.storeDir,
		FossilHTTPPort:    s.cmd.HTTPPort,
		NATSClientPort:    s.cmd.NATSClientPort,
		NATSLeafPort:      s.cmd.NATSLeafPort,
		EnableWebSocket:   !s.cmd.DisableWebSocket,
		NATSWebSocketPort: s.cmd.NATSWebSocketPort,
		LeafUpstream:      s.leafURL,
		ProjectCode:       s.projectCode,
		SeedFromUpstream:  s.plan.HubFossilURL,
	})
	if err != nil {
		return fmt.Errorf("sesh up: %w", err)
	}
	s.h = h
	return nil
}

// postHubBootstrap dispatches the post-hub side of the bootstrap plan
// via Apply. SourceGitWorktree commits the worktree through the bound
// hub; SourceNone and SourceHub log and return. Seed failures are
// logged and swallowed — the session can run without seed; the
// alternative (refusing to start) is worse UX for a recoverable error.
func (s *Starter) postHubBootstrap(ctx context.Context) {
	if err := Apply(ctx, s.plan, s.h, Deps{Cwd: s.cwd, RepoPath: s.repoPath}); err != nil {
		slog.Warn("fossil seed failed (continuing without seed)", "err", err)
	}
}

// publishSession writes the bound URLs into the session state JSON so
// sub-leaves and `sesh down` can reach this session. The atomic claim
// from ClaimSession (in NewStarter) gates this write: the underlying
// file must still exist (i.e. the claim wasn't externally removed) or
// Publish refuses, protecting against writing for a session no live
// process owns.
func (s *Starter) publishSession() error {
	if err := s.sessHandle.Publish(SessionState{
		PID:       os.Getpid(),
		Scope:     string(s.scope),
		NATSURL:   s.h.NATSURL(),
		NATSWSURL: s.h.NATSWebSocketURL(),
		LeafURL:   s.h.LeafURL(),
		FossilURL: "http://" + s.h.HTTPAddr() + "/",
	}); err != nil {
		_ = s.h.Stop()
		return fmt.Errorf("publish session URLs: %w", err)
	}
	return nil
}

// maybeSpawnHarness returns the error chan from spawnHarness when
// --exec was supplied (first real usage of the Harness/spawnHarness path
// for the exec feature). When execCmd is empty it returns nil so the
// corresponding receive case in serve's select is inert (a nil chan is
// never ready for communication). This extracts the harness construction
// + env population into a small helper, keeping serve() focused on its
// lifecycle select rather than growing a second unrelated responsibility
// (per the Ousterhout audit guidance for this slice).
//
// The harnessEnv is populated from the already-bound hub (post-publish)
// using the exact same expressions as publishSession so the child
// receives identical SESH_* / NATS / ROLE/CLASS topology.
func (s *Starter) maybeSpawnHarness(ctx context.Context) <-chan error {
	if s.execCmd == "" {
		return nil
	}
	env := harnessEnv{
		Session:   s.cmd.Session,
		NATSURL:   s.h.NATSURL(),
		NATSWSURL: s.h.NATSWebSocketURL(),
		FossilURL: "http://" + s.h.HTTPAddr() + "/",
		LeafURL:   s.h.LeafURL(),
		Role:      s.role,
		Class:     s.class,
	}
	return spawnHarness(ctx, s.execCmd, env)
}

// serve runs the HTTP serve loop. Returns when ctx cancels (SIGINT /
// SIGTERM from the operator), the serve goroutine reports an error, or
// the child harness (when --exec was given) exits. The latter case makes
// child death an explicit shutdown trigger for the whole session (first
// exercising the exec path). h.Stop is called in all paths so the hub's
// NATS server, leaf, and JetStream WAL all shut down cleanly.
func (s *Starter) serve(ctx context.Context) error {
	slog.Info("sesh up running",
		"name", s.h.ServerName(),
		"project", s.project,
		"session", s.cmd.Session,
		"pid", os.Getpid(),
		"repo", s.repoPath,
		"hub_url", s.leafURL,
		"nats", s.h.NATSURL(),
		"http", "http://"+s.h.HTTPAddr(),
	)

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.h.ServeHTTP(ctx) }()

	// Start the agent watcher: polls $SRV.INFO.agents and keeps
	// agents[] in the session JSON current. Best-effort — watcher
	// errors are logged, not fatal.
	go runAgentWatcher(ctx, s.h.NATSURL(), s.sessHandle, s.cmd.Session)

	// First real usage of the exec harness (Task 4 slice).
	// Placed after watcher (and after publishSession readiness in Start)
	// so the hub is fully bound and URLs are live. Child exit will
	// unblock the select below and drive shutdown.
	childErr := s.maybeSpawnHarness(ctx)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			slog.Error("HTTP serve error", "err", err)
		}
	case err := <-childErr:
		if err != nil {
			slog.Error("child harness exited", "err", err)
		}
		// err==nil is normal exit of the coding agent (e.g. user quit
		// claude); still shut the session down cleanly. No signal
		// forwarding in this slice (future work).
	}

	slog.Info("sesh up shutting down", "name", s.name)
	return s.h.Stop()
}

// ensureHubRunning returns the hub's leaf URL. Reuses any existing hub via
// HubGuard's fast/slow path; on a spawner lease it fork-execs `sesh hub
// serve` then polls for the daemon's published URL. The flock-serialized
// spawn dance (so concurrent `sesh up` invocations never fork-exec
// competing hubs) lives entirely inside HubGuard.
func ensureHubRunning(projectCode string) (string, error) {
	stateDir, err := seshHome()
	if err != nil {
		return "", err
	}

	leafURL, lease, err := AcquireOrReuse(stateDir)
	if err != nil {
		return "", err
	}
	if !lease.IsSpawner() {
		_ = lease.Release()
		return leafURL, nil
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
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		url, exists, err := ReadPrimaryURL(stateDir)
		if err == nil && exists && url != "" && reachable(url) {
			return url, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", errors.New("hub didn't come up within 15s")
}

// spawnHub fork-execs `sesh hub serve` as a detached daemon. Stdout/stderr
// go to ~/.sesh/hub.log; stdin is /dev/null. setsid detaches from the
// controlling terminal so the daemon survives parent shutdown.
func spawnHub(projectCode string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("self executable: %w", err)
	}
	stateDir, err := seshHome()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(hubLogPath(stateDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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

// spawnHarness fork-execs the user-provided --exec command string as an
// interactive child "harness" (the coding agent). It is the happy-path
// wrapper implementation of the hybrid Harness owner (locked C).
//
// For the skeleton step (TDD): always returns a closed chan (no-op).
// Real body (sh -c, Setpgid, stdio inherit, env builder for the five
// canonical SESH_* + NATS_URL + SESH_ROLE/CLASS, waiter goroutine) is
// filled immediately after to keep the task atomic while still honoring
// "skeleton first".
//
// Returns a buffered 1-slot <-chan error that receives the Wait result
// (or start error) then closes. Callers select on it (future wiring in serve).
func spawnHarness(ctx context.Context, cmdStr string, env harnessEnv) <-chan error {
	ch := make(chan error, 1)

	if cmdStr == "" {
		close(ch)
		return ch
	}

	// Locked Parsing (X): pass the entire --exec value verbatim to sh -c.
	// This gives the operator full shell features (quotes, pipes, env vars,
	// globs, && etc.) exactly as documented in the UpCmd help and proposal.
	cmd := exec.Command("sh", "-c", cmdStr)

	// Wrapper UX: child inherits our stdio/TTY so interactive TUIs
	// (claude, pi, grok, ...) just work. Cwd is inherited automatically.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Single named site for *all* SESH_* + NATS_* injection (plus role/class
	// per decision A). Parent env is preserved (PATH, USER, TERM, etc.).
	// Later callers (role Phase 4, etc.) only edit this list.
	base := os.Environ()
	cmd.Env = append(base,
		"SESH_SESSION="+env.Session,
		"NATS_URL="+env.NATSURL,
		"SESH_NATS_WS_URL="+env.NATSWSURL,
		"SESH_FOSSIL_URL="+env.FossilURL,
		"SESH_LEAF_URL="+env.LeafURL,
		"SESH_ROLE="+env.Role,
		"SESH_CLASS="+env.Class,
	)

	// Setpgid for process-group signal forwarding (wrapper path of Hybrid C).
	// The parent (sesh up) can later syscall.Kill(-pgid, sig) to deliver to
	// the whole tree. Opposite of spawnHub's Setsid+daemon.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		ch <- fmt.Errorf("spawn harness: %w", err)
		close(ch)
		return ch
	}

	// Happy-path waiter goroutine. Always sends exactly once then closes.
	// The returned chan is what serve() will select on (future task).
	go func() {
		ch <- cmd.Wait()
		close(ch)
	}()

	return ch
}
