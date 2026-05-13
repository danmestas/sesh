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
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SessionState is the project-local JSON at <cwd>/.sesh/sessions/<label>.json.
//
// Written in two phases: claimSessionState creates the file O_EXCL with PID
// only (the atomic ownership claim), then updateSessionState overwrites with
// the URLs once the hub has bound its ports. Sub-leaves and NATS clients
// read NATSURL/LeafURL to attach without grepping logs.
type SessionState struct {
	PID      int    `json:"pid"`
	NATSURL  string `json:"nats_url,omitempty"`  // for NATS clients under this session
	LeafURL  string `json:"leaf_url,omitempty"`  // for EdgeSync leaves to solicit upstream
	FossilURL string `json:"fossil_url,omitempty"` // hub HTTP xfer endpoint; sub-leaves use as --seed-from-upstream
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

// writeAtomic writes data to path via tmpfile+rename so readers never
// see a partial file. Used for hub.url, hub.nats.url, and similar.
func writeAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
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
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// updateSessionState replaces the session JSON with state. The session must
// already be claimed (file exists) — guards against writing state for a
// session no live process owns. Atomic via tempfile+rename in the same dir.
func updateSessionState(label string, state SessionState) error {
	path, err := sessionStatePath(label)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("update session state: %w", err)
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

// hubURLPath returns ~/.sesh/hub.url — the hub's leafnode listener URL,
// written by the hub at bind for sesh up to discover the hub and solicit
// upstream into it.
func hubURLPath() (string, error) {
	dir, err := seshHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.url"), nil
}

// hubNATSURLPath returns ~/.sesh/hub.nats.url — the hub's NATS client
// URL, written by the hub at bind. Clients that need to operate on the
// hub's JetStream domain (hub/project/workflow-scoped shared memory)
// connect to this URL rather than to a session's NATS URL. Each session
// runs its own JetStream domain; the hub's is the shared one.
func hubNATSURLPath() (string, error) {
	dir, err := seshHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.nats.url"), nil
}

// hubRepoPath returns ~/.sesh/hub.repo.
func hubRepoPath() (string, error) {
	dir, err := seshHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.repo"), nil
}

// hubSpawnLockPath returns ~/.sesh/hub.spawn.lock — the flock target that
// serializes concurrent `sesh up` invocations so only one ever fork-execs
// a hub. The file content is irrelevant; flock semantics operate on the
// inode.
func hubSpawnLockPath() (string, error) {
	dir, err := seshHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.spawn.lock"), nil
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

// deriveProjectCode produces a deterministic 40-char lowercase hex
// project-code from (hostname, projectName). Used only as a SEED for
// loadOrCreateProjectCode on the first sesh up in a project — the
// returned code is pinned to <cwd>/.sesh/project-code and subsequent
// runs read the pinned value back, so the project-code survives
// hostname changes (VM clones, container migrations, dotfiles sync to
// a new laptop) that would otherwise silently break Fossil cross-leaf
// sync. See loadOrCreateProjectCode for the pinning dance.
func deriveProjectCode(projectName string) string {
	host, _ := os.Hostname()
	return deriveProjectCodeFromHost(host, projectName)
}

// deriveProjectCodeFromHost is the pure form of deriveProjectCode — the
// hostname is passed in rather than read from the OS. Factored out so
// tests can verify behavior across hostname changes without needing to
// shell out or mock os.Hostname.
func deriveProjectCodeFromHost(host, projectName string) string {
	sum := sha1.Sum([]byte("sesh:" + host + ":" + projectName))
	return hex.EncodeToString(sum[:])
}

// projectCodePath returns <cwd>/.sesh/project-code — the pinned
// project-code file written on first sesh up.
func projectCodePath(cwd string) string {
	return filepath.Join(cwd, ".sesh", "project-code")
}

// loadOrCreateProjectCode returns the project-code for cwd, reading
// the pinned value from <cwd>/.sesh/project-code when present, or
// deriving a fresh one via deriveProjectCode(projectName) and atomically
// writing it to disk for future runs. Pinning the code at first sesh up
// means subsequent invocations survive hostname changes — VM clones,
// container migrations, dotfiles sync, manual rename — which would
// otherwise re-derive a different hash and silently break Fossil
// cross-leaf sync for the project. The file is plain text: the 40-hex
// code plus a trailing newline.
//
// Backward-compat: existing projects already running (no project-code
// file yet) get seeded from deriveProjectCode(projectName), which is
// exactly what the previous run computed — the hub.repo subscription
// keeps working without disruption.
//
// If the file is present but doesn't validate as 40 lowercase hex
// chars, returns an error rather than silently overwriting. A mangled
// project-code file signals something else is wrong and the user
// should see it.
func loadOrCreateProjectCode(cwd, projectName string) (string, error) {
	path := projectCodePath(cwd)

	data, err := os.ReadFile(path)
	if err == nil {
		code := strings.TrimSpace(string(data))
		if !isValidProjectCode(code) {
			return "", fmt.Errorf("invalid project-code in %s: expected 40 lowercase hex chars", path)
		}
		return code, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read project-code %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	code := deriveProjectCode(projectName)
	if err := writeAtomic(path, code+"\n"); err != nil {
		return "", fmt.Errorf("seed project-code %s: %w", path, err)
	}
	return code, nil
}

// isValidProjectCode checks that s is exactly 40 lowercase hex chars,
// matching the output shape of deriveProjectCode.
func isValidProjectCode(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
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
