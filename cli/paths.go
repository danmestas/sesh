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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// projectSeshDir returns <cwd>/.sesh — the project-local hidden state
// dir, mirror of seshHome for project-scoped state.
func projectSeshDir(cwd string) string {
	return filepath.Join(cwd, ".sesh")
}

// projectStateDir returns <cwd>/.sesh/sessions, creating it if needed.
func projectStateDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(projectSeshDir(cwd), "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
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

// Hub-state path helpers. All take the hub's stateDir (typically the
// result of seshHome()) and return a fully-qualified path. Centralizing
// every hub-state filename here makes "what files does sesh own under
// ~/.sesh/" answerable by reading one file; the alternative — inline
// filepath.Join calls — left the filenames scattered across hubinfo,
// hubguard, hub_serve, and up.

// hubRepoPath returns <stateDir>/hub.repo — the hub's libfossil repo file.
func hubRepoPath(stateDir string) string {
	return filepath.Join(stateDir, "hub.repo")
}

// checkoutDir returns <cwd>/.sesh/checkouts/<label> — the materialized
// fossil working directory for a worker (or operator) bound to the
// session-or-project repo identified by <label>. The label is the same
// disambiguator used by .sesh/sessions/<label>.repo (and by the worker's
// orch-spawned session identity), so .sesh/sessions/<label>.repo and
// .sesh/checkouts/<label>/ form a 1:1 pair under --scope=session. Under
// --scope=project, all checkouts share the single .sesh/project.repo
// repo file but keep distinct working dirs keyed on <label>.
//
// Tier-1 safety: .sesh/checkouts/<label>/ is the ONLY path under .sesh/
// that sesh worktree --force-recreate is permitted to remove. Adjacent
// trees — .sesh/sessions/, .sesh/messaging/ — are never touched by the
// worktree code path.
//
// Defense-in-depth: the label is re-validated here even though every
// operator entrypoint already validates at the top of Run(). The first
// gate is the primary defense; this second gate exists so a future
// entrypoint (or refactor that inlines path math) cannot silently
// re-introduce path traversal by forgetting the entrypoint-level check.
// The cost is a sub-microsecond regex; the consequence of a missed gate
// is the entire .sesh/ tier-1 safety boundary.
func checkoutDir(cwd, label string) (string, error) {
	if err := validateLabel(label); err != nil {
		return "", fmt.Errorf("invalid label %q: %w", label, err)
	}
	return filepath.Join(projectSeshDir(cwd), "checkouts", label), nil
}

// checkoutMarkerPath returns the absolute path to the .fslckout marker
// file libfossil writes inside a materialized checkout. Stat-ing this
// file is the cheapest way to ask "does this directory contain a live
// fossil checkout?" without opening the SQLite DB. On Windows the
// equivalent marker is _FOSSIL_ — sesh is POSIX-only today so the
// fixed-name check is fine; the helper centralizes the assumption so
// the inevitable Windows port has a single call site to fix.
func checkoutMarkerPath(checkoutDir string) string {
	return filepath.Join(checkoutDir, ".fslckout")
}

// hubURLPath returns <stateDir>/hub.url — the daemon's leafnode URL,
// owned O_EXCL by HubGuard's daemon lease.
func hubURLPath(stateDir string) string {
	return filepath.Join(stateDir, "hub.url")
}

// hubNATSURLPath returns <stateDir>/hub.nats.url — the hub's NATS client
// URL, written via WriteHubInfo's temp-then-rename.
func hubNATSURLPath(stateDir string) string {
	return filepath.Join(stateDir, "hub.nats.url")
}

// hubFossilURLPath returns <stateDir>/hub.fossil.url — the hub's Fossil
// HTTP xfer endpoint, written via WriteHubInfo's temp-then-rename.
func hubFossilURLPath(stateDir string) string {
	return filepath.Join(stateDir, "hub.fossil.url")
}

// hubSpawnLockPath returns <stateDir>/hub.spawn.lock — the flock target
// that serializes concurrent `sesh up` invocations so only one ever
// fork-execs a hub. The file content is irrelevant; flock semantics
// operate on the inode.
func hubSpawnLockPath(stateDir string) string {
	return filepath.Join(stateDir, "hub.spawn.lock")
}

// hubLogPath returns <stateDir>/hub.log — where the auto-spawned hub's
// stderr goes for debugging.
func hubLogPath(stateDir string) string {
	return filepath.Join(stateDir, "hub.log")
}

// hubStoreDir returns <stateDir>/messaging — the hub daemon's JetStream
// store directory. Session JetStream stores live elsewhere
// (see scope.storeDirFor); this one is the shared user-wide hub store.
func hubStoreDir(stateDir string) string {
	return filepath.Join(stateDir, "messaging")
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
	return filepath.Join(projectSeshDir(cwd), "project-code")
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

// ResolveProjectCode reconciles the local pin at <cwd>/.sesh/project-code
// against an authoritative upstream value (typically the hub's
// project-code from ReadHubProjectCode). If hubCode is empty (no hub
// content yet) or already agrees with localCode, returns localCode
// untouched. Otherwise the hub wins: ResolveProjectCode rewrites the
// pin to match hubCode and returns hubCode so EdgeSync's
// SeedFromUpstream sees agreement on both sides of the clone.
//
// Idempotent: a second call with the same args is a no-op. A missing
// or unreadable local file is treated the same as "current value is
// empty," which triggers a write to seed the pin.
//
// This is the pre-hub side of project-code resolution. The hub is
// shared across all sessions in the project and is the natural source
// of truth (issue #26 / #34); local pins follow.
func ResolveProjectCode(cwd, localCode, hubCode string) (string, error) {
	if hubCode == "" || hubCode == localCode {
		return localCode, nil
	}
	slog.Info("adopting hub project-code",
		"hub", hubCode, "was_pinned_to", localCode, "path", projectCodePath(cwd))
	if err := writeAtomic(projectCodePath(cwd), hubCode+"\n"); err != nil {
		return "", fmt.Errorf("adopt hub project-code: %w", err)
	}
	return hubCode, nil
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

// projectTokenRe matches every character that is NOT allowed in a NATS
// project subject token. The single-char class (no '+') is deliberate: each
// disallowed char is replaced by its own '-' so multi-separator runs survive
// as one dash each — matching the JS regex byte-for-byte.
var projectTokenRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeProjectToken mirrors the claude-nats-channel adapter's
// sanitizeSessionName (server.ts) byte-for-byte so the Go-exported
// SESH_PROJECT and the TS-sanitized subject segment always agree:
// (1) replace every char not in [A-Za-z0-9_-] with a single '-',
// (2) lowercase, (3) trim leading/trailing '-'. Order matters: replace
// before lowercasing before trimming. The transform is idempotent on
// already-sanitized input.
func sanitizeProjectToken(s string) string {
	s = projectTokenRe.ReplaceAllString(s, "-")
	s = strings.ToLower(s)
	return strings.Trim(s, "-")
}

// deriveProjectID produces a deterministic 40-char lowercase hex project-id
// from projectName alone — no hostname salt. Unlike project-code (which is
// hostname-salted to prevent fossil cross-leaf collisions), project-id is
// the same value across every host that works on the same project. It is
// the correct routing key for sesh.* coordination subjects scoped to a
// project.
//
// Pure delegate to deriveProjectIDFromName. The two-function shape matches
// the deriveProjectCode / deriveProjectCodeFromHost pair so the
// hostname-free property can be asserted directly in tests without
// shelling out to the OS.
func deriveProjectID(projectName string) string {
	return deriveProjectIDFromName(projectName)
}

// deriveProjectIDFromName is the pure form of deriveProjectID. Takes no
// host input — the hostname-free property is structural, not behavioral.
// Derivation: SHA1("sesh:project:" + projectName).
func deriveProjectIDFromName(projectName string) string {
	sum := sha1.Sum([]byte("sesh:project:" + projectName))
	return hex.EncodeToString(sum[:])
}

// projectIDPath returns <cwd>/.sesh/project-id — the pinned project-id file
// written on first sesh up.
func projectIDPath(cwd string) string {
	return filepath.Join(projectSeshDir(cwd), "project-id")
}

// loadOrCreateProjectID returns the project-id for cwd, reading the pinned
// value from <cwd>/.sesh/project-id when present, or deriving a fresh one
// via deriveProjectID(projectName) and atomically writing it to disk for
// future runs. Semantics mirror loadOrCreateProjectCode but without the
// hostname-adoption path — project-id is hostname-free by design.
//
// If the file is present but doesn't validate as 40 lowercase hex chars,
// returns an error rather than silently overwriting.
func loadOrCreateProjectID(cwd, projectName string) (string, error) {
	path := projectIDPath(cwd)

	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if !isValidProjectCode(id) { // same shape: 40 lowercase hex
			return "", fmt.Errorf("invalid project-id in %s: expected 40 lowercase hex chars", path)
		}
		return id, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read project-id %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	id := deriveProjectID(projectName)
	if err := writeAtomic(path, id+"\n"); err != nil {
		return "", fmt.Errorf("seed project-id %s: %w", path, err)
	}
	return id, nil
}
