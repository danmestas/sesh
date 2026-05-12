package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/danmestas/EdgeSync/hub"
)

// SeedMode controls what seedFromGitWorktree sends to Fossil at sesh up.
//
//   - "tracked": files in the git index (i.e., what `git ls-files` returns).
//   - "all":     tracked + untracked but not gitignored
//     (`git ls-files -co --exclude-standard`). Default.
//   - "none":    skip seeding entirely.
//
// When cwd is not a git repo, all modes degrade to a no-op with a slog
// note. Sesh's Fossil is an agent scratchpad on top of git, not a parallel
// SCM — it only carries the worktree snapshot, not git history.
type SeedMode string

const (
	SeedTracked SeedMode = "tracked"
	SeedAll     SeedMode = "all"
	SeedNone    SeedMode = "none"
)

// seedFromGitWorktree commits the cwd's worktree to the Hub's Fossil repo
// as a single initial commit. Called once at sesh up if the repo is fresh.
// No-op (with a log note) when cwd is not a git repo or mode is "none".
func seedFromGitWorktree(ctx context.Context, h *hub.Hub, cwd string, mode SeedMode) error {
	if mode == SeedNone {
		slog.Info("fossil seed skipped (--seed=none)")
		return nil
	}
	if !isGitWorktree(cwd) {
		slog.Info("fossil seed skipped (cwd is not a git worktree)", "cwd", cwd)
		return nil
	}

	files, err := gitFiles(cwd, mode)
	if err != nil {
		return fmt.Errorf("enumerate files: %w", err)
	}
	if len(files) == 0 {
		slog.Info("fossil seed skipped (no files to seed)")
		return nil
	}

	toCommit, err := readFilesForCommit(cwd, files)
	if err != nil {
		return fmt.Errorf("read files: %w", err)
	}

	headSha, _ := gitHeadSha(cwd)
	msg := "seed: worktree at sesh up"
	if headSha != "" {
		msg = fmt.Sprintf("seed: worktree at sesh up (git@%s)", short(headSha))
	}

	rev, err := h.Commit(ctx, hub.CommitOpts{
		Files:   toCommit,
		Message: msg,
		Author:  "sesh-seed",
	})
	if err != nil {
		return fmt.Errorf("hub commit: %w", err)
	}

	slog.Info("seeded fossil from worktree",
		"files", len(toCommit),
		"rev", rev,
		"git_sha", headSha,
		"mode", string(mode))
	return nil
}

// isGitWorktree returns true if cwd is inside a git working tree.
func isGitWorktree(cwd string) bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// gitFiles returns paths (relative to cwd) of files git considers part of
// the worktree, according to mode.
func gitFiles(cwd string, mode SeedMode) ([]string, error) {
	var args []string
	switch mode {
	case SeedTracked:
		args = []string{"ls-files", "-z"}
	case SeedAll, "":
		args = []string{"ls-files", "-c", "-o", "--exclude-standard", "-z"}
	default:
		return nil, fmt.Errorf("unknown seed mode %q", mode)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	// -z produces NUL-separated entries, no trailing record separator.
	raw := bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0})
	files := make([]string, 0, len(raw))
	for _, b := range raw {
		if len(b) == 0 {
			continue
		}
		p := string(b)
		// Sesh's own runtime state lives under .sesh/ in the cwd. Even
		// when the user hasn't gitignored it, we never want to seed it
		// into Fossil — it's transient state, not project content.
		if p == ".sesh" || strings.HasPrefix(p, ".sesh/") {
			continue
		}
		files = append(files, p)
	}
	return files, nil
}

// gitHeadSha returns HEAD's sha. Returns empty string if HEAD is unborn
// (no commits yet) — the caller treats that as "no sha to record."
func gitHeadSha(cwd string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// readFilesForCommit reads each file from disk, detects the executable
// bit, and returns a FileToCommit slice ready for hub.Commit.
func readFilesForCommit(cwd string, files []string) ([]hub.FileToCommit, error) {
	var errs []error
	toCommit := make([]hub.FileToCommit, 0, len(files))
	for _, rel := range files {
		abs := filepath.Join(cwd, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			errs = append(errs, fmt.Errorf("stat %s: %w", rel, err))
			continue
		}
		// Skip directories and symlinks. Symlinks could be encoded as
		// libfossil "l" perm but reading the target is risky for the
		// agent scratchpad use case — skip for now.
		if info.IsDir() {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			slog.Debug("skipping symlink in seed", "path", rel)
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", rel, err))
			continue
		}
		perm := ""
		if info.Mode().Perm()&0o111 != 0 {
			perm = "x"
		}
		toCommit = append(toCommit, hub.FileToCommit{
			Name:    rel,
			Content: content,
			Perm:    perm,
		})
	}
	if len(errs) > 0 && len(toCommit) == 0 {
		return nil, errors.Join(errs...)
	}
	for _, e := range errs {
		slog.Warn("seed: skipping file", "err", e)
	}
	return toCommit, nil
}

// short truncates a sha for log readability.
func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
