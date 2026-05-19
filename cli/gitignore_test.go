package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureSeshGitignored_FreshGitProject covers the headline case
// from sesh#86: a git-init'd project without a .gitignore at all.
// Calling ensureSeshGitignored must create .gitignore containing
// ".sesh/".
func TestEnsureSeshGitignored_FreshGitProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	project := t.TempDir()
	mustGitInit(t, project)

	if err := ensureSeshGitignored(project); err != nil {
		t.Fatalf("ensureSeshGitignored: %v", err)
	}

	gi := filepath.Join(project, ".gitignore")
	got, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(got), ".sesh/") {
		t.Errorf(".gitignore missing .sesh/ entry; got:\n%s", got)
	}
}

// TestEnsureSeshGitignored_PreservesExisting verifies we append to an
// existing .gitignore without losing operator-authored content. Acceptance
// criterion from sesh#86: previous content preserved + .sesh/ appended.
func TestEnsureSeshGitignored_PreservesExisting(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	project := t.TempDir()
	mustGitInit(t, project)
	existing := "node_modules/\n*.log\n"
	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := ensureSeshGitignored(project); err != nil {
		t.Fatalf("ensureSeshGitignored: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(project, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(got), "node_modules/") || !strings.Contains(string(got), "*.log") {
		t.Errorf("pre-existing entries lost:\n%s", got)
	}
	if !strings.Contains(string(got), ".sesh/") {
		t.Errorf(".sesh/ entry not appended:\n%s", got)
	}
	// The appended block should not have merged into a previous line —
	// "*.log.sesh/" would be a smell.
	if strings.Contains(string(got), "*.log.sesh/") {
		t.Errorf("appended .sesh/ collided with prior trailing line; got:\n%s", got)
	}
}

// TestEnsureSeshGitignored_NoDuplicate is the idempotency check: re-running
// against a project whose .gitignore already excludes .sesh/ must NOT
// append a second entry. Tests both the exact-match form (".sesh/") and
// the bare form (".sesh").
func TestEnsureSeshGitignored_NoDuplicate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, form := range []string{".sesh\n", ".sesh/\n", "# comment\n.sesh/\n", "/.sesh/\n"} {
		form := form
		t.Run(strings.TrimSpace(strings.ReplaceAll(form, "\n", "|")), func(t *testing.T) {
			project := t.TempDir()
			mustGitInit(t, project)
			gi := filepath.Join(project, ".gitignore")
			if err := os.WriteFile(gi, []byte(form), 0o644); err != nil {
				t.Fatalf("seed .gitignore: %v", err)
			}
			before, _ := os.ReadFile(gi)

			if err := ensureSeshGitignored(project); err != nil {
				t.Fatalf("ensureSeshGitignored: %v", err)
			}
			if err := ensureSeshGitignored(project); err != nil {
				t.Fatalf("ensureSeshGitignored (second run): %v", err)
			}

			after, err := os.ReadFile(gi)
			if err != nil {
				t.Fatalf("read .gitignore: %v", err)
			}
			if string(before) != string(after) {
				t.Errorf("idempotency violated for form %q:\nbefore=%q\nafter=%q",
					form, before, after)
			}
			// Defensive: count .sesh occurrences as token-on-its-own-line.
			n := 0
			for _, ln := range strings.Split(string(after), "\n") {
				trim := strings.TrimSpace(ln)
				trim = strings.TrimPrefix(trim, "/")
				trim = strings.TrimSuffix(trim, "/")
				if trim == ".sesh" {
					n++
				}
			}
			if n > 1 {
				t.Errorf("duplicate .sesh entries for form %q (n=%d):\n%s", form, n, after)
			}
		})
	}
}

// TestEnsureSeshGitignored_NonGitProject covers the "no .git directory"
// branch: ensureSeshGitignored must NOT create .gitignore in a directory
// that isn't a git repo. We don't want to be the reason a non-git dir
// suddenly grows a stray .gitignore file.
func TestEnsureSeshGitignored_NonGitProject(t *testing.T) {
	project := t.TempDir()
	if err := ensureSeshGitignored(project); err != nil {
		t.Fatalf("ensureSeshGitignored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf(".gitignore created in non-git project (stat err=%v)", err)
	}
}

// TestEnsureSeshGitignored_RecognizesWildcardMatch verifies the
// `git check-ignore`-based detection: if an operator's existing
// .gitignore matches .sesh/ via a wildcard pattern (".se*"), we must
// NOT also append a literal ".sesh/" line. Avoids fighting the operator's
// existing conventions.
func TestEnsureSeshGitignored_RecognizesWildcardMatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	project := t.TempDir()
	mustGitInit(t, project)
	gi := filepath.Join(project, ".gitignore")
	if err := os.WriteFile(gi, []byte(".se*\n"), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}
	if err := ensureSeshGitignored(project); err != nil {
		t.Fatalf("ensureSeshGitignored: %v", err)
	}
	got, _ := os.ReadFile(gi)
	if strings.Contains(string(got), ".sesh/") {
		t.Errorf("wildcard match not recognized — appended literal .sesh/:\n%s", got)
	}
}

func mustGitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}
