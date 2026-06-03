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
// Defense-in-depth: the first gate sits at operator entrypoints (up,
// down); this second gate exists so a future entrypoint cannot silently
// re-introduce traversal by forgetting the entrypoint-level check.
func repoPathFor(scope SeshScope, cwd, session string) (string, error) {
	if err := validateLabel(session); err != nil {
		return "", fmt.Errorf("invalid label %q: %w", session, err)
	}
	if scope == ScopeProject {
		return filepath.Join(projectSeshDir(cwd), "project.repo"), nil
	}
	return filepath.Join(projectSeshDir(cwd), "sessions", session+".repo"), nil
}

// storeDirFor returns the per-session JetStream store directory path. The
// embedded NATS server is gone (sesh is a client now), so this path is
// retained only as the historical per-session messaging location; the
// validation gate matters more than the path today.
//
// Defense-in-depth: same label-validation gate as repoPathFor, same
// rationale.
func storeDirFor(cwd, session string) (string, error) {
	if err := validateLabel(session); err != nil {
		return "", fmt.Errorf("invalid label %q: %w", session, err)
	}
	return filepath.Join(projectSeshDir(cwd), "sessions", session+".messaging"), nil
}
