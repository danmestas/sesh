package cli

import "path/filepath"

// SeshScope selects where a session's Fossil repo lives on disk.
//
//   - ScopeSession (default): per-session repo at
//     <cwd>/.sesh/sessions/<label>.repo. Same-project sessions converge
//     via NATS autosync on the project-code subject. No SQLite write
//     contention because each session has a single writer (its own
//     in-process libfossil handle).
//
//   - ScopeProject: shared repo at <cwd>/.sesh/project.repo. All
//     sessions in the project open the same SQLite file. Cross-session
//     commits are visible synchronously to readers on the same file
//     (no autosync round-trip needed for cohabiting sessions), but
//     concurrent writers contend on the SQLite WAL lock. busy_timeout
//     (set by EdgeSync hub via applySQLiteTuning) makes contending
//     writers queue rather than fail fast.
//
// Modes can mix in the same project: a --scope=project session and a
// --scope=session session co-exist on different repo files. NATS
// autosync still propagates commits between them via the shared
// project-code subject.
type SeshScope string

const (
	ScopeSession SeshScope = "session"
	ScopeProject SeshScope = "project"
)

// repoPathFor returns the Fossil repo path for the given scope.
func repoPathFor(scope SeshScope, cwd, session string) string {
	if scope == ScopeProject {
		return filepath.Join(cwd, ".sesh", "project.repo")
	}
	return filepath.Join(cwd, ".sesh", "sessions", session+".repo")
}

// storeDirFor returns the JetStream store directory for this session.
// JetStream storage is always per-session even in project scope: each
// sesh up runs its own embedded NATS server in its own process and
// JetStream's on-disk layout cannot be shared across processes.
func storeDirFor(cwd, session string) string {
	return filepath.Join(cwd, ".sesh", "sessions", session+".messaging")
}
