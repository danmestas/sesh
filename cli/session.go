package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// AgentRef is a snapshot of one live agent entry in the session's agents[]
// array. It mirrors the fields callers need from $SRV.INFO.agents without
// requiring a NATS round-trip.
//
// Fields are derived from the micro service's INFO response:
//   - Agent      ← metadata.agent
//   - Owner      ← metadata.owner
//   - InstanceID ← service id (the framework-assigned opaque string)
//   - Subject    ← the "prompt" endpoint's subject (first endpoint named "prompt",
//     or the first endpoint if none is named "prompt")
type AgentRef struct {
	Agent      string `json:"agent"`
	Owner      string `json:"owner"`
	InstanceID string `json:"instance_id"`
	Subject    string `json:"subject"`
}

// SessionState is the project-local JSON at <cwd>/.sesh/sessions/<label>.json.
//
// Written in two phases by the Session module: ClaimSession creates the file
// O_EXCL with PID only (the atomic ownership claim), then Session.Publish
// overwrites with the URLs once the hub has bound its ports. Sub-leaves and
// NATS clients read NATSURL/LeafURL to attach without grepping logs.
//
// Agents[] is updated by the session watcher whenever agents register or
// deregister on $SRV.INFO.agents. It is eventual (best-effort, ~1s lag)
// and defaults to [] when absent — backward-compatible with older files
// that did not include the field.
type SessionState struct {
	PID       int        `json:"pid"`
	Scope     string     `json:"scope,omitempty"`       // "session" or "project" — the scope this session was brought up under. Read by sesh worktree / sesh materialize / sesh worker-cwd to auto-detect scope without flag repetition. Empty for backward compat with older session JSONs; consumers fall back to "session" in that case. (#84)
	NATSURL   string     `json:"nats_url,omitempty"`    // for NATS clients under this session
	NATSWSURL string     `json:"nats_ws_url,omitempty"` // WebSocket NATS endpoint (ws://, loopback, no_tls); for browser / Cloudflare Workers clients via @nats-io/transport-websockets. Present iff the embedded NATS server has WebSocket enabled.
	LeafURL   string     `json:"leaf_url,omitempty"`    // for EdgeSync leaves to solicit upstream
	FossilURL string     `json:"fossil_url,omitempty"`  // hub HTTP xfer endpoint; sub-leaves use as --seed-from-upstream
	Agents    []AgentRef `json:"agents,omitempty"`      // live agents in this session; eventual, updated by watcher
}

// Session owns a project-local state file at <stateDir>/<label>.json. It
// represents the lifecycle of a single `sesh up` between claim and release:
// sesh up creates one at startup (atomic O_EXCL claim), publishes its bound
// URLs once the hub is ready, runs until SIGINT, then removes the file.
// sesh down (via package-level Terminate) reads the published PID and
// signals the owner.
//
// The file is the registry — no in-process state, no shared bus. A live PID
// in the file means a live session; a missing file means none; a dead PID
// in the file is a stale claim that gets reaped on the next ClaimSession or
// Terminate.
type Session struct {
	stateDir string
	label    string
}

// ClaimSession atomically claims (stateDir, label) by O_EXCL-creating the
// state JSON file with the current process's PID. A stale file owned by a
// dead PID is reaped first. Returns an error iff a live PID already owns
// the slot.
//
// The returned Session must be released via Session.Release (typically
// deferred) so the file does not outlive the owning process.
func ClaimSession(stateDir, label string) (*Session, error) {
	path := sessionFilePath(stateDir, label)
	if existing, err := readSessionFile(path); err == nil {
		if alive(existing.PID) {
			return nil, fmt.Errorf("session %q already held by pid %d (%s)", label, existing.PID, path)
		}
		_ = os.Remove(path)
	}
	payload, err := json.Marshal(SessionState{PID: os.Getpid()})
	if err != nil {
		return nil, fmt.Errorf("marshal session state: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create session state %s: %w", path, err)
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &Session{stateDir: stateDir, label: label}, nil
}

// Publish overwrites the session file with state. The file must still
// exist (i.e. the claim must still hold) — guards against publishing for
// a session whose state file has been externally removed. Atomic via
// tempfile+rename in the same directory.
func (s *Session) Publish(state SessionState) error {
	path := sessionFilePath(s.stateDir, s.label)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("publish session state: %w", err)
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal session state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("temp session state: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename session state: %w", err)
	}
	return nil
}

// UpdateAgents atomically rewrites the agents[] field of the session JSON.
// It reads the current file, replaces the Agents slice with agents, and
// writes back via tempfile+rename. Returns fs.ErrNotExist if the session
// file has been removed (i.e. the session has ended).
//
// UpdateAgents is NOT safe for concurrent invocation; callers MUST serialize
// via a single writer per session (in practice, the watcher goroutine started
// by Starter.serve()). Concurrent readers on any goroutine are safe — the
// temp-file-and-rename never exposes a partial file.
func (s *Session) UpdateAgents(agents []AgentRef) error {
	path := sessionFilePath(s.stateDir, s.label)
	state, err := readSessionFile(path)
	if err != nil {
		return err // includes fs.ErrNotExist when session is gone
	}
	if agents == nil {
		agents = []AgentRef{}
	}
	state.Agents = agents
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal session state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("temp session state: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename session state: %w", err)
	}
	return nil
}

// Release removes the session state file. Best-effort; an already-removed
// file is not an error. Intended use is `defer s.Release()` immediately
// after a successful ClaimSession.
func (s *Session) Release() error {
	path := sessionFilePath(s.stateDir, s.label)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ReadSession returns the current persisted state for (stateDir, label).
// Used by sibling commands that don't own the slot (sesh down, future
// status/ls). Returns fs.ErrNotExist when no session is claimed.
func ReadSession(stateDir, label string) (SessionState, error) {
	return readSessionFile(sessionFilePath(stateDir, label))
}

// Terminate brings down the sesh up that owns (stateDir, label) by sending
// SIGINT and waiting up to timeout for it to exit. The state file is
// reaped on success.
//
//   - No file → no error (already down).
//   - File exists but owner PID is dead → reap, no error.
//   - Owner exits within timeout → reap, no error.
//   - Owner survives past timeout → error with PID surfaced.
//
// The caller blocks while waiting; this function does not background.
// sesh up's own SIGINT handler does the heavy cleanup (hub stop, leaf
// disconnect, JetStream/Fossil WAL checkpoint).
func Terminate(stateDir, label string, timeout time.Duration) error {
	path := sessionFilePath(stateDir, label)
	state, err := readSessionFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read session state: %w", err)
	}
	if !alive(state.PID) {
		_ = os.Remove(path)
		return nil
	}
	proc, err := os.FindProcess(state.PID)
	if err != nil {
		_ = os.Remove(path)
		return nil
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		// PID was alive a microsecond ago; treat the race as already-gone.
		_ = os.Remove(path)
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(state.PID) {
			_ = os.Remove(path)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("session %q (pid %d) didn't exit within %s", label, state.PID, timeout)
}

// sessionFilePath returns <stateDir>/<label>.json. The dir must already
// exist — callers using the cwd convention create it via projectStateDir.
func sessionFilePath(stateDir, label string) string {
	return filepath.Join(stateDir, label+".json")
}

// readSessionFile decodes a session JSON at path.
func readSessionFile(path string) (SessionState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionState{}, err
	}
	var s SessionState
	if err := json.Unmarshal(data, &s); err != nil {
		return SessionState{}, fmt.Errorf("parse session state: %w", err)
	}
	return s, nil
}

// alive returns true if a process with pid is reachable by signal 0.
// Used to discriminate stale state files (dead PID; reap silently) from
// live sessions (active PID; refuse re-claim, signal on Terminate).
func alive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
