package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuf is a goroutine-safe wrapper around bytes.Buffer. Necessary
// because os/exec writes stderr to the buffer from a goroutine while
// the test reads from it in a polling loop.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestUp_SeedsFossilFromGitWorktree confirms that `sesh up` in a git
// worktree commits the worktree's files to the session's Fossil repo
// as a single seed commit.
//
// Sets up: a tmpdir initialized as a git repo with 3 tracked files
// (one executable) + 1 untracked-but-not-gitignored file + 1 gitignored
// file. Runs `sesh up`, captures stderr, asserts the slog line
// "seeded fossil from worktree" appears with files=4 (the gitignored
// file should be excluded; default seed mode is "all" = tracked +
// untracked-but-not-gitignored).
func TestUp_SeedsFossilFromGitWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: builds binary + spawns subprocess + needs git")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()

	setupGitWorktree(t, project)

	cmd := exec.Command(bin, "up", "--session=seed")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = os.Stderr
	var stderr syncBuf
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh up: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})

	statePath := filepath.Join(project, ".sesh", "sessions", "seed.json")
	_ = waitForURLs(t, statePath, 15*time.Second)

	// Wait for the seed log line (commit is async-ish).
	deadline := time.Now().Add(10 * time.Second)
	var seedLine string
	for time.Now().Before(deadline) {
		seedLine = findSlogLine(stderr.String(), "seeded fossil from worktree")
		if seedLine != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seedLine == "" {
		t.Fatalf("did not see seed log line in stderr\n%s", stderr.String())
	}

	got := parseSlogField(seedLine, "files")
	if got != "5" {
		t.Errorf("files = %q in seed line, want 5\nline: %s", got, seedLine)
	}

	mode := parseSlogField(seedLine, "mode")
	if mode != "all" {
		t.Errorf("mode = %q, want all", mode)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Logf("sesh up exit: %v", err)
	}
}

// TestUp_SkipsSeedingWhenCwdIsNotGit confirms the no-op path when the
// cwd is just a plain directory.
func TestUp_SkipsSeedingWhenCwdIsNotGit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir() // not a git repo

	cmd := exec.Command(bin, "up", "--session=nogit")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = os.Stderr
	var stderr syncBuf
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh up: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})

	statePath := filepath.Join(project, ".sesh", "sessions", "nogit.json")
	_ = waitForURLs(t, statePath, 15*time.Second)

	// Brief wait so any seed attempt would have logged.
	time.Sleep(500 * time.Millisecond)

	if line := findSlogLine(stderr.String(), "seeded fossil from worktree"); line != "" {
		t.Errorf("did not expect a seed log line in non-git dir, got: %s", line)
	}
	if line := findSlogLine(stderr.String(), "fossil seed skipped (cwd is not a git worktree)"); line == "" {
		t.Errorf("expected skip-log line, got none\nstderr:\n%s", stderr.String())
	}

	_ = cmd.Process.Signal(syscall.SIGINT)
	_, _ = cmd.Process.Wait()
}

// setupGitWorktree builds a tiny git repo at dir with a known file set.
//
// Tracked + committed (4 files, one executable):
//   - .gitignore (rules: .sesh/ and ignored.log)
//   - hello.txt
//   - subdir/data.json
//   - script.sh (mode 0755)
//
// Untracked but not gitignored (1 file): agent-note.md
// Gitignored (1 file): ignored.log
//
// Plus sesh's own .sesh/ runtime state (gitignored AND filtered by
// seed code as belt-and-suspenders).
//
// Total expected by SeedAll: 5 (4 tracked + 1 untracked-not-ignored).
func setupGitWorktree(t *testing.T, dir string) {
	t.Helper()
	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		// Use isolated git config so user identity isn't required.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}

	mustWrite := func(rel, content string, mode os.FileMode) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	run("git", "init", "-q", "-b", "main")
	mustWrite(".gitignore", ".sesh/\nignored.log\n", 0o644)
	mustWrite("hello.txt", "hello\n", 0o644)
	mustWrite("subdir/data.json", `{"k":"v"}`+"\n", 0o644)
	mustWrite("script.sh", "#!/bin/sh\necho hi\n", 0o755)
	run("git", "add", ".gitignore", "hello.txt", "subdir/data.json", "script.sh")
	run("git", "commit", "-q", "-m", "init")

	mustWrite("agent-note.md", "untracked\n", 0o644)
	mustWrite("ignored.log", "noise\n", 0o644) // gitignored — should NOT be seeded
}

// findSlogLine returns the first line of haystack containing needle,
// or "" if none.
func findSlogLine(haystack, needle string) string {
	for _, line := range splitLines(haystack) {
		if bytes.Contains([]byte(line), []byte(needle)) {
			return line
		}
	}
	return ""
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// parseSlogField extracts the value of key=... from a slog text line.
// Handles both unquoted (k=val) and quoted (k="val with spaces") forms.
func parseSlogField(line, key string) string {
	re := regexp.MustCompile(fmt.Sprintf(`\b%s=(?:"([^"]*)"|([^\s]+))`, regexp.QuoteMeta(key)))
	m := re.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// strconv referenced to keep imports clean if tests refer to numbers.
var _ = strconv.Atoi
