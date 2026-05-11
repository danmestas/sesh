// Package cli implements sesh's command-line surface: up, down, and hub serve.
//
// Design captured during simple-brainstorm (2026-05-11):
//
//   - Hub lives at user level under ~/.sesh/. Auto-lifecycle: spawned by the
//     first `sesh up` when no hub is running, exits when the last leaf
//     disconnects (unless --keepalive).
//   - Project state at <cwd>/.sesh/sessions/<label>.json — git-style hidden
//     dir, ships with the project. JSON for future metadata; PID-only today.
//   - Connection-state on the hub IS the registry — no explicit register
//     protocol, no JetStream KV, no TTL, no renewer.
//   - Hub discovery: ~/.sesh/hub.url written O_EXCL by hub at bind, read by
//     sesh up at startup. Race-resolution = O_EXCL (one writer wins). Stale
//     URL handled by "try connect → fail → remove → respawn."
//   - Local lockfile is replaced by O_EXCL on the project state file itself.
//     Same fast-fail behavior, one fewer file.
package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SessionState is the project-local JSON at <cwd>/.sesh/sessions/<label>.json.
// PID is the only field today; the structure is JSON to leave room for
// metadata without breaking parsers later.
type SessionState struct {
	PID int `json:"pid"`
}

// projectStateDir returns <cwd>/.sesh/sessions, creating it if needed.
func projectStateDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cwd, ".sesh", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// sessionStatePath returns the JSON path for a session label.
func sessionStatePath(label string) (string, error) {
	dir, err := projectStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, label+".json"), nil
}

// claimSessionState atomically claims (cwd, label) by O_EXCL-creating the
// state JSON. Stale entries (file exists, PID is dead) are reaped first.
// Returns a release function that removes the file.
func claimSessionState(label string, pid int) (release func(), err error) {
	path, err := sessionStatePath(label)
	if err != nil {
		return nil, err
	}

	if existing, err := readSessionState(path); err == nil {
		if alive(existing.PID) {
			return nil, fmt.Errorf("session %q already held by pid %d (%s)", label, existing.PID, path)
		}
		_ = os.Remove(path)
	}

	payload, err := json.Marshal(SessionState{PID: pid})
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
	f.Close()

	return func() { _ = os.Remove(path) }, nil
}

// readSessionState decodes a session JSON file.
func readSessionState(path string) (SessionState, error) {
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
func alive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// newSessionID returns a time-prefixed random id. Sortable by start time.
// Used internally if a future flag wants auto-generated session labels;
// today --session is required by the CLI.
func newSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b)
}

// seshHome returns ~/.sesh, creating it if missing.
func seshHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".sesh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// hubURLPath returns ~/.sesh/hub.url.
func hubURLPath() (string, error) {
	dir, err := seshHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.url"), nil
}

// hubRepoPath returns ~/.sesh/hub.repo.
func hubRepoPath() (string, error) {
	dir, err := seshHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.repo"), nil
}

// hubLogPath returns ~/.sesh/hub.log — where the auto-spawned hub's stderr
// goes for debugging.
func hubLogPath() (string, error) {
	dir, err := seshHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.log"), nil
}

// defaultProject returns filepath.Base(cwd) — the convention is "the project
// is the directory you're in." Returns an error if cwd is unavailable.
func defaultProject() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	base := filepath.Base(cwd)
	// Defensive: strip any leading/trailing whitespace, never empty
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("cannot derive project name from cwd %q", cwd)
	}
	return base, nil
}
