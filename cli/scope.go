package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// resolveScope returns the SeshScope a follow-on subcommand should use for
// the given session label. The chosen scope is whichever of these wins,
// in order of precedence:
//
//  1. flagScope is non-empty — the operator passed --scope=... explicitly.
//     We honor the override even if the session JSON disagrees (the
//     operator may be probing a different scope's repo intentionally).
//
//  2. The session JSON at <cwd>/.sesh/sessions/<label>.json has a Scope
//     field — this is the scope `sesh up --session=<label>` was brought
//     up under, written by publishSession (#84). Newer sessions get this
//     for free; the operator never needs to repeat --scope.
//
//  3. Default: ScopeSession. Backward-compat for session JSONs without a
//     Scope field, OR sessions that don't have a JSON yet (e.g., the
//     caller probing a label that's never been up).
//
// Closes the operator-UX gap from #84: sesh up --scope=project followed by
// sesh worktree <label> (no flag) used to fail because worktree defaulted
// to session-scope and looked at the wrong backing repo. With resolveScope,
// the second command picks up the recorded scope from session state.
//
// validateLabel runs upstream of every caller. resolveScope trusts its
// label input — it does not validate. (Path math against an unvalidated
// label would be the bug class #66 closed.)
func resolveScope(cwd, label, flagScope string) (SeshScope, error) {
	if flagScope != "" {
		if flagScope != string(ScopeSession) && flagScope != string(ScopeProject) {
			return "", fmt.Errorf("invalid --scope %q (want %q or %q)", flagScope, ScopeSession, ScopeProject)
		}
		return SeshScope(flagScope), nil
	}
	statePath := filepath.Join(projectSeshDir(cwd), "sessions", label+".json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ScopeSession, nil
		}
		return "", fmt.Errorf("read session state %s: %w", statePath, err)
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return "", fmt.Errorf("parse session state %s: %w", statePath, err)
	}
	if state.Scope == "" {
		return ScopeSession, nil
	}
	if state.Scope != string(ScopeSession) && state.Scope != string(ScopeProject) {
		return "", fmt.Errorf("session %s has invalid scope %q in state file", label, state.Scope)
	}
	return SeshScope(state.Scope), nil
}

// otherScopeRepoHint inspects the OTHER scope's backing repo and returns a
// short hint string if it exists. The missing-repo error for one scope is
// often the operator hitting the scope-drift gap pre-fix: they ran sesh up
// with one scope, then a follow-on subcommand defaulted to the other.
// Surfacing the hint lets the operator self-correct fast.
//
// Returns the empty string when the other scope's repo doesn't exist —
// no hint to add. Errors silently degrade to no-hint; this is a UX
// helper, not a load-bearing check.
func otherScopeRepoHint(cwd string, current SeshScope) string {
	var otherRepo string
	var otherName SeshScope
	if current == ScopeProject {
		otherRepo = filepath.Join(projectSeshDir(cwd), "sessions")
		otherName = ScopeSession
		entries, err := os.ReadDir(otherRepo)
		if err != nil {
			return ""
		}
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".repo" {
				return fmt.Sprintf("\n             note: %s/%s exists — did you mean --scope=%s?", otherRepo, e.Name(), otherName)
			}
		}
		return ""
	}
	otherRepo = filepath.Join(projectSeshDir(cwd), "project.repo")
	otherName = ScopeProject
	if _, err := os.Stat(otherRepo); err == nil {
		return fmt.Sprintf("\n             note: %s exists — did you mean --scope=%s?", otherRepo, otherName)
	}
	return ""
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
