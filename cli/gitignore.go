package cli

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ensureSeshGitignored makes sure <project>/.gitignore excludes the .sesh/
// runtime state directory. Called at sesh up after .sesh/ has been
// materialized; closes the circular-UX gap where `sesh up` creates .sesh/
// as untracked content and a subsequent `sesh materialize` refuses to
// operate on the resulting "dirty" worktree (issue #86).
//
// Contract:
//
//   - cwd must contain a .git directory (or .git linkfile) at the project
//     root: we only touch .gitignore when sesh is in a git repo. Non-git
//     projects are a silent no-op (slog.Debug). The check is the
//     existence of <cwd>/.git — a `git worktree`-linked tree carries a
//     .git FILE rather than a directory, and we treat both as "this is
//     git territory."
//
//   - If .gitignore already excludes .sesh/ (verified via `git check-ignore`
//     against a probe path under .sesh/), we make no change. This catches
//     pattern variants: ".sesh", ".sesh/", "/.sesh", "*sesh*", or a
//     parent-directory .gitignore further up the tree. Idempotent.
//
//   - If .gitignore does not exist, create it containing just ".sesh/\n".
//
//   - If .gitignore exists but does not match .sesh/, append ".sesh/\n"
//     with a leading newline if the existing file does not end in one.
//
// Tier-1 safety: no os.Remove* on anything. Only os.WriteFile / append.
func ensureSeshGitignored(cwd string) error {
	gitPath := filepath.Join(cwd, ".git")
	if _, err := os.Stat(gitPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Debug("auto-gitignore skipped: cwd is not a git repo", "cwd", cwd)
			return nil
		}
		return fmt.Errorf("stat .git: %w", err)
	}

	if seshAlreadyGitignored(cwd) {
		slog.Debug("auto-gitignore skipped: .sesh/ already matched by gitignore rules", "cwd", cwd)
		return nil
	}

	gitignorePath := filepath.Join(cwd, ".gitignore")
	existing, err := os.ReadFile(gitignorePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read .gitignore: %w", err)
	}

	// Belt-and-suspenders: even though seshAlreadyGitignored said no,
	// a plain-text scan for a ".sesh"/".sesh/" line catches the case
	// where `git check-ignore` is unavailable (older git or a non-
	// standard environment) and the file simply has the entry on a line
	// of its own.
	if existing != nil && seshLinePresent(existing) {
		slog.Debug("auto-gitignore skipped: .sesh line already present in .gitignore", "cwd", cwd)
		return nil
	}

	var out []byte
	if len(existing) == 0 {
		out = []byte(".sesh/\n")
	} else {
		out = existing
		if !strings.HasSuffix(string(out), "\n") {
			out = append(out, '\n')
		}
		out = append(out, ".sesh/\n"...)
	}
	if err := os.WriteFile(gitignorePath, out, 0o644); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}
	slog.Info("auto-gitignored .sesh/ in project gitignore", "path", gitignorePath)
	return nil
}

// seshAlreadyGitignored shells out to `git check-ignore` to ask git
// itself whether .sesh/ is excluded. This is the most robust matcher:
// it covers wildcard patterns ("*sesh*"), parent-tree .gitignore rules,
// global gitignore, and the exact-match cases. Probe path is .sesh/probe
// rather than .sesh/ directly because check-ignore evaluates paths as
// if they were files; using a non-existent inner path forces the
// directory-pattern semantics we care about.
//
// Returns false on any error (git missing, non-git repo, etc.) — callers
// fall back to the line-scan check.
func seshAlreadyGitignored(cwd string) bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	cmd := exec.Command("git", "check-ignore", "-q", ".sesh/probe")
	cmd.Dir = cwd
	err := cmd.Run()
	return err == nil // exit 0 = path IS ignored
}

// seshLinePresent scans .gitignore content for a bare ".sesh" or
// ".sesh/" entry. Whitespace-trimmed line match; ignores comments
// (lines starting with #). Does NOT recognize wildcard patterns —
// those are the job of seshAlreadyGitignored.
func seshLinePresent(content []byte) bool {
	sc := bufio.NewScanner(strings.NewReader(string(content)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Tolerate a leading "/" anchor (`/.sesh`, `/.sesh/`) — git
		// treats this as "match only at the gitignore's directory."
		// Trailing "/" is the directory-only marker.
		trimmed := strings.TrimPrefix(line, "/")
		trimmed = strings.TrimSuffix(trimmed, "/")
		if trimmed == ".sesh" {
			return true
		}
	}
	return false
}
