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
	"strings"
	"syscall"

	"golang.org/x/term"
)

// resolveHubURL returns the NATS URL of the external hub sesh should dial as
// a client. Resolution is explicit-only (Q2 = hard-fail) — there is NO
// embedded hub to spawn and NO silent localhost fallback:
//
//  1. $SESH_HUB_URL — the dedicated knob for pointing sesh at its hub.
//  2. $NATS_URL     — the URL the agent harness and the sesh-channels
//     adapters already standardize on; honored as a fallback so a single
//     export wires both sesh up and the spawned harness.
//  3. else          — a hard error naming the remediation env var.
//
// The resolved value is injected into the harness env as NATS_URL (what the
// harness and adapters read), so the child dials the same hub sesh up did.
func resolveHubURL() (string, error) {
	if v := strings.TrimSpace(os.Getenv("SESH_HUB_URL")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("NATS_URL")); v != "" {
		return v, nil
	}
	return "", errors.New("no hub configured; set SESH_HUB_URL (or NATS_URL) to the NATS URL of an external hub (e.g. nats://127.0.0.1:4222)")
}

// harnessEnv is the supporting struct for spawnHarness / Harness (Hybrid C).
// It carries the five canonical SESH_* values (plus NATS_URL) plus the
// Role/Class for coordination subjects. Extended here per decision A so the
// single env-construction site in spawnHarness is the only place that ever
// mentions the SESH_* names (ready for role/class Phase 4 without edit
// amplification elsewhere).
type harnessEnv struct {
	Session string
	Project string
	// ProjectID is the pinned 40-hex SHA1 project-id (hostname-free routing
	// key, distinct from the human-readable Project slug). Exported as
	// SESH_PROJECT_ID so the refagent reads its coordination-subject routing
	// key from injected env rather than re-deriving it by walking the
	// filesystem for a .sesh/project-id pin.
	ProjectID string
	NATSURL   string
	NATSWSURL string
	FossilURL string
	LeafURL   string
	Role      string
	Class     string
}

// harnessEnvVars renders the canonical SESH_* + NATS_* environment a spawned
// harness child receives, as a slice of "KEY=value" strings ready to append
// to os.Environ(). Pure (no globals, no I/O) so the exact wire env can be
// asserted in a unit test without spawning a subprocess — the single source
// of truth for which variables the agent harness sees.
func harnessEnvVars(env harnessEnv) []string {
	return []string{
		"SESH_SESSION=" + env.Session,
		"SESH_PROJECT=" + env.Project,
		"SESH_PROJECT_ID=" + env.ProjectID,
		"NATS_URL=" + env.NATSURL,
		"SESH_NATS_WS_URL=" + env.NATSWSURL,
		"SESH_FOSSIL_URL=" + env.FossilURL,
		"SESH_LEAF_URL=" + env.LeafURL,
		"SESH_ROLE=" + env.Role,
		"SESH_CLASS=" + env.Class,
	}
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
	Session string `optional:"" help:"Session label (free-form). Defaults to the cwd basename; if that label is already claimed, '-2', '-3', … is appended until a free slot is found. Held exclusively by this sesh up — a second sesh up --session=<same-label> in another shell will fail. Run multiple adapters in one session by passing a multiplex wrapper to --exec."`

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
	// (queued by busy_timeout). Modes
	// can mix in the same project — see cli/scope.go for the full
	// trade-off rationale.
	Scope string `help:"Fossil repo scope: session (per-session repo) or project (shared file)" enum:"session,project" default:"session"`

	// Exec, when non-empty, causes sesh up to spawn the given command as a
	// child harness after the session is ready (wrapper lifecycle by default).
	Exec string `name:"exec" help:"Run <cmd> as a child coding-agent harness after the session is ready. Passed verbatim to sh -c (full shell features: quoting, pipes, &&, globs, etc.). Example: --exec='claude --dangerously-skip-permissions'"`

	// Role and Class (locked decision A) — propagated to the harness for sesh.* coordination subjects.
	Role  string `name:"role"  help:"Role for coordination subjects (e.g. implementer, verifier, spy). Passed as SESH_ROLE to --exec child (falls back to 'worker')."`
	Class string `name:"class" help:"Class display tag for the mesh listing (e.g. active). Passed as SESH_CLASS to --exec child; not consulted for subscription routing." default:"active"`
}

// sanitizeLabelFromBasename strips characters that validateLabel rejects from a
// filesystem basename so it can be used as a session label. Drops every rune
// outside printable 7-bit ASCII (0x20–0x7e) and any path separator. Trims
// whitespace and leading dots, then caps at 128 bytes. Returns "" when nothing
// printable survives.
func sanitizeLabelFromBasename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e && r != '/' && r != '\\' {
			b.WriteRune(r)
		}
	}
	result := strings.TrimSpace(b.String())
	result = strings.TrimLeft(result, ".")
	if len(result) > 128 {
		result = result[:128]
	}
	return result
}

// deriveSessionName picks a session label from the cwd basename, incrementing
// a numeric suffix (-2, -3, …) until a slot whose claim file is absent is
// found. The check is best-effort (not atomic); the real ownership claim uses
// O_EXCL inside ClaimSession.
func deriveSessionName() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	base := sanitizeLabelFromBasename(filepath.Base(cwd))
	if base == "" || validateLabel(base) != nil {
		base = "session"
	}
	stateDir := filepath.Join(cwd, ".sesh", "sessions")
	candidate := base
	for i := 2; ; i++ {
		claimPath := filepath.Join(stateDir, candidate+".json")
		if _, err := os.Stat(claimPath); os.IsNotExist(err) {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func (c *UpCmd) Run() error {
	// If the caller omitted --session, derive a label from the cwd basename.
	// Stale claim files will be reaped by ClaimSession's reapStaleSessions,
	// so a file-present check is a conservative (safe) heuristic: at worst
	// we pick -2 when -1 would have been claimable.
	if c.Session == "" {
		derived, err := deriveSessionName()
		if err != nil {
			return fmt.Errorf("sesh up: could not derive session name: %w", err)
		}
		c.Session = derived
		slog.Info("sesh up: no --session given; derived from cwd", "session", c.Session)
	}

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

	// Resolve the external hub URL BEFORE any disk pinning. sesh is a NATS
	// client; with no hub configured there is nothing to bring up, so we
	// hard-fail here without claiming a session slot or writing project
	// identity files — a no-hub `sesh up` leaves .sesh/ untouched.
	hubURL, err := resolveHubURL()
	if err != nil {
		return fmt.Errorf("sesh up: %w", err)
	}

	s, err := NewStarter(c)
	if err != nil {
		return err
	}
	s.hubURL = hubURL
	defer s.Release()
	return s.Start(context.Background())
}

// Starter owns the bring-up of one `sesh up`. It captures everything
// derived from UpCmd + the project on disk, and exposes Start as the
// single entry point for the phase pipeline.
//
// sesh is a NATS CLIENT only — it does not embed or spawn a hub. The hub
// URL is resolved by UpCmd.Run (resolveHubURL: $SESH_HUB_URL / $NATS_URL,
// else hard-fail) BEFORE the session slot is claimed, so a no-hub run
// leaves .sesh/ untouched. Phase ordering (Start):
//
//  1. publish-session: Session.Publish writes the resolved URL into the
//     project-local state JSON so `sesh down` and clients can reach this
//     session.
//  2. serve: block until ctx cancels (SIGINT / SIGTERM) or — when --exec
//     was given — the child harness exits. An agent watcher dials the
//     external hub to keep the session JSON's agents[] current.
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
	name        string // session name; e.g. "myproj-session-alpha"
	repoPath    string
	storeDir    string
	freshRepo   bool
	projectCode string
	projectID   string

	hubURL string // external hub NATS URL; resolved by UpCmd.Run before claim
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
// scopes the signal-listening ctx that the serve loop and the harness
// observe. s.hubURL must already be resolved (by UpCmd.Run, before the
// session slot is claimed).
func (s *Starter) Start(parent context.Context) error {
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Auto-add .sesh/ to the project's .gitignore so our own runtime state
	// doesn't show up as untracked content in the project worktree (#86).
	// Idempotent; no-op outside a git repo. Logged + continued on any
	// failure — .gitignore wrangling shouldn't block bring-up.
	if err := ensureSeshGitignored(s.cwd); err != nil {
		slog.Warn("auto-gitignore of .sesh/ failed (continuing)", "err", err)
	}
	if err := s.publishSession(); err != nil {
		return err
	}
	return s.serve(ctx)
}

// publishSession writes the resolved external hub URL into the session
// state JSON so `sesh down` and clients can reach this session. The atomic
// claim from ClaimSession (in NewStarter) gates this write: the underlying
// file must still exist (i.e. the claim wasn't externally removed) or
// Publish refuses, protecting against writing for a session no live
// process owns.
func (s *Starter) publishSession() error {
	if err := s.sessHandle.Publish(SessionState{
		PID:     os.Getpid(),
		Scope:   string(s.scope),
		NATSURL: s.hubURL,
	}); err != nil {
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
// The harnessEnv carries the resolved external hub URL as NATS_URL so the
// child dials the same hub sesh up resolved. The embedded-hub-only fields
// (WebSocket / Fossil HTTP / leaf URLs) are empty now that sesh is a pure
// client — the child reaches the hub via NATS_URL alone.
func (s *Starter) maybeSpawnHarness(ctx context.Context) <-chan error {
	if s.execCmd == "" {
		return nil
	}
	env := harnessEnv{
		Session: s.cmd.Session,
		// Project is the stable, human-readable repo token — the cwd
		// basename, sanitized with the adapter's exact transform so the
		// claude-nats-channel <project> subject segment is reconstructable
		// by peers (it must NOT inherit the session de-dup suffix).
		Project: sanitizeProjectToken(filepath.Base(s.cwd)),
		// ProjectID is the pinned 40-hex routing key derived host-side at
		// first `sesh up` (loadOrCreateProjectID in NewStarter). Injecting it
		// here lets the refagent skip its own filesystem walk for the pin.
		ProjectID: s.projectID,
		NATSURL:   s.hubURL,
		Role:      s.role,
		Class:     s.class,
	}
	return spawnHarness(ctx, s.execCmd, env)
}

// serve blocks until ctx cancels (SIGINT / SIGTERM from the operator) or —
// when --exec was given — the child harness exits. Child death is an
// explicit shutdown trigger for the whole session. sesh holds no embedded
// hub, so there is nothing to Stop(); the only resource is the session
// claim, released by Starter.Release via the deferred Run path.
func (s *Starter) serve(ctx context.Context) error {
	slog.Info("sesh up running",
		"name", s.name,
		"project", s.project,
		"session", s.cmd.Session,
		"pid", os.Getpid(),
		"hub_url", s.hubURL,
	)

	// Start the agent watcher: dials the external hub, polls
	// $SRV.INFO.agents, and keeps agents[] in the session JSON current.
	// Best-effort — watcher errors are logged, not fatal. connectWithBackoff
	// tolerates the hub not being reachable the instant we dial.
	go runAgentWatcher(ctx, s.hubURL, s.sessHandle, s.cmd.Session)

	childErr := s.maybeSpawnHarness(ctx)

	select {
	case <-ctx.Done():
	case err := <-childErr:
		if err != nil {
			slog.Error("child harness exited", "err", err)
		}
		// err==nil is normal exit of the coding agent (e.g. user quit
		// claude); still shut the session down cleanly. Signal forwarding
		// (when the operator interrupts) is owned inside spawnHarness.
	}

	slog.Info("sesh up shutting down", "name", s.name)
	return nil
}

// spawnHarness fork-execs the user-provided --exec command string as an
// interactive child "harness" (the coding agent). It is the happy-path
// wrapper implementation of the hybrid Harness owner (locked C).
//
// sh -c (locked X), inherits stdio/TTY + cwd, builds the canonical
// SESH_* + NATS + ROLE/CLASS env (single site per decision A), uses
// Setpgid so the whole child tree can be signaled as a group.
//
// On the ctx (from the one NotifyContext in Start) cancelling, a forwarder
// goroutine does syscall.Kill(-pgid, SIGINT) to propagate the operator
// interrupt. This is deliberately the *only* Kill site and uses the
// existing ctx (no additional Notify) to avoid the dual-notify/orphan
// bugs called out in the proposal + Ousterhout audit.
//
// Returns a buffered 1-slot <-chan error that receives the Wait result
// (or start error) then closes. Callers select on it (wired in serve via
// maybeSpawnHarness).
// harnessSysProcAttr builds the SysProcAttr the wrapper-mode harness child
// must be started with. Always: Setpgid (so we can SIGINT the whole tree as
// a group). Additionally — and critically — when stdin is a real TTY, we set
// Foreground + Ctty so the child becomes the *foreground* pgrp on that
// terminal and isn't kernel-stopped by SIGTTIN on its first stdin read.
//
// We use term.IsTerminal rather than os.ModeCharDevice because /dev/null,
// pipes and other character devices that aren't terminals would otherwise
// pass a ModeCharDevice check and then make forkExec fail in tcsetpgrp with
// "operation not supported by device" (which is exactly what go test sees
// when stdin is wired to a non-terminal char dev).
//
// Extracted to keep the TTY-detection branch unit-testable without spawning
// a real subprocess.
func harnessSysProcAttr(stdin *os.File) *syscall.SysProcAttr {
	spa := &syscall.SysProcAttr{Setpgid: true}
	if stdin == nil {
		return spa
	}
	fd := int(stdin.Fd())
	if !term.IsTerminal(fd) {
		return spa
	}
	spa.Foreground = true
	spa.Ctty = fd
	return spa
}

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
	cmd.Env = append(base, harnessEnvVars(env)...)

	// Setpgid for process-group signal forwarding (wrapper path of Hybrid C).
	// The parent (sesh up) can later syscall.Kill(-pgid, sig) to deliver to
	// the whole tree. The actual forwarding logic (below) lives inside this
	// spawn site so it is owned by the harness path, not scattered across
	// serve().
	//
	// When stdin is a controlling TTY we ALSO promote the child's new pgrp
	// to be the foreground pgrp on that terminal (Foreground + Ctty). Without
	// this, the child is born into a *background* pgrp and the kernel sends
	// SIGTTIN the moment it tries to read stdin — interactive TUIs (claude,
	// pi, grok, ...) end up T-state (stopped) before they ever render, which
	// looks to the operator like sesh up is hung. Go's forkExec performs the
	// tcsetpgrp(Ctty, childPgid) under the hood when Foreground is set.
	cmd.SysProcAttr = harnessSysProcAttr(os.Stdin)

	if err := cmd.Start(); err != nil {
		ch <- fmt.Errorf("spawn harness: %w", err)
		close(ch)
		return ch
	}

	// Happy-path waiter goroutine. Always sends exactly once then closes.
	// The returned chan is what serve() will select on.
	go func() {
		ch <- cmd.Wait()
		close(ch)
	}()

	// Signal forwarding (the critical "hard part"). We react exclusively to
	// cancellation of the ctx passed in from serve() — which itself comes
	// from the *single* signal.NotifyContext installed in Starter.Start.
	// This centralizes all signal handling for the up process (no second
	// independent signal.Notify / NotifyContext anywhere in the child path).
	// On cancel we forward SIGINT (the interactive/Ctrl-C signal used by
	// tests and `sesh down`) to the child's pgid. ESRCH is normal if the
	// child already exited (race with natural death or prior signal); we
	// ignore it. The goroutine is detached so spawn returns immediately.
	// This lives here (creation + cmd) rather than in serve or a separate
	// forwarder, keeping ownership with the Harness spawn logic per the
	// Ousterhout guidance and audit fix for "obscure dependencies".
	pgid := cmd.Process.Pid
	go func() {
		<-ctx.Done()
		slog.Info("forwarding SIGINT to harness pgid (reacting to ctx cancel from outer NotifyContext)", "pgid", pgid)
		if err := syscall.Kill(-pgid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
			slog.Warn("forward SIGINT to harness pgid failed (non-ESRCH)", "pgid", pgid, "err", err)
		}
	}()

	return ch
}
