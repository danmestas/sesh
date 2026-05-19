package cli

import (
	"fmt"
	"path/filepath"
)

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
//     (no autosync round-trip needed for cohabiting sessions).
//     Concurrent writers serialize at BEGIN IMMEDIATE on the SQLite
//     WAL lock and queue via busy_timeout.
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
//
// Under ScopeProject the session label does not appear in the returned
// path (the repo is shared across sessions), but we still validate it
// because (a) every caller passes a label and (b) defense-in-depth: a
// future scope variant that does interpolate the label gets the gate
// for free.
//
// Defense-in-depth: see checkoutDir's doc for the rationale. The first
// gate sits at operator entrypoints (up, down, worktree, materialize,
// worker-cwd); this second gate exists so a future entrypoint cannot
// silently re-introduce traversal by forgetting the entrypoint-level
// check.
func repoPathFor(scope SeshScope, cwd, session string) (string, error) {
	if err := validateLabel(session); err != nil {
		return "", fmt.Errorf("invalid label %q: %w", session, err)
	}
	if scope == ScopeProject {
		return filepath.Join(projectSeshDir(cwd), "project.repo"), nil
	}
	return filepath.Join(projectSeshDir(cwd), "sessions", session+".repo"), nil
}

// storeDirFor returns the JetStream store directory for this session.
// JetStream storage is always per-session even in project scope: each
// sesh up runs its own embedded NATS server in its own process and
// JetStream's on-disk layout cannot be shared across processes.
//
// Defense-in-depth: see checkoutDir's doc. Same gate, same rationale.
func storeDirFor(cwd, session string) (string, error) {
	if err := validateLabel(session); err != nil {
		return "", fmt.Errorf("invalid label %q: %w", session, err)
	}
	return filepath.Join(projectSeshDir(cwd), "sessions", session+".messaging"), nil
}
