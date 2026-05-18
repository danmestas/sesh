package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	// Imported for its side effect: registers the modernc SQLite driver
	// libfossil requires before any Repo.Open succeeds. Mirrors the
	// rationale in worktree.go.
	_ "github.com/danmestas/EdgeSync/hub"

	libfossil "github.com/danmestas/libfossil"
)

// MaterializeCmd reads the fossil trunk HEAD for the given label and
// overlays its files into a git worktree (or any output dir). It is the
// fossil→git bridge for mission-complete materialization (issue #64,
// Slice 3): once the swarm has converged on the fossil trunk, the
// operator's sesh-level agent runs `sesh materialize <label>` to make
// the trunk-tip visible to the git PR flow.
//
// Contract:
//
//	sesh materialize <label>                       — overlay trunk-tip into cwd
//	sesh materialize <label> --scope=project       — back by .sesh/project.repo
//	sesh materialize <label> --output=<dir>        — overlay into <dir> (must exist)
//	sesh materialize <label> --git-add             — run `git add .` after writing
//	sesh materialize <label> --allow-dirty         — proceed even with uncommitted git state
//
// Overlay semantics (deliberate, conservative):
//
//   - Files in fossil trunk are written into the output dir, creating
//     parent directories as needed.
//   - Files present in the output dir but absent from fossil trunk are
//     LEFT ALONE. The git worktree's history, .git/ dir, build
//     artifacts, and local-only files survive untouched.
//   - File modes (executable bit) are preserved as recorded by
//     libfossil's checkout extract primitive.
//   - Binary files are written byte-for-byte. No line-ending conversion.
//
// "Mirror mode" (deleting non-fossil files from the output) is
// explicitly out of scope for v1. It is the natural follow-up if a
// mission needs whole-tree re-baselining, but the safe primitive is
// overlay; mirror is the optimization.
//
// Atomicity: the trunk-tip is first extracted to a tempdir under the
// output filesystem (so any rename is intra-volume), then files are
// copied into the output dir one-by-one. If a copy fails partway
// through, files already written remain in place — the operator sees a
// half-applied overlay in their git status and can `git checkout -- .`
// or `git reset --hard` to roll back. The alternative — extract first,
// rename-into-place atomically — would require an all-or-nothing dir
// swap that doesn't compose with overlay semantics (we'd have to read
// the output dir first, splice fossil files in, then swap; the
// resulting code is denser without changing the failure mode operators
// actually see).
//
// Tier-1 safety: validateLabel is the first call in Run. No
// os.Remove* call sites in this file. Overlay semantics specifically
// avoid deletion.
type MaterializeCmd struct {
	Label string `arg:"" required:"" help:"Session/checkout label. Must match a session brought up via 'sesh up --session=<label>' (or, with --scope=project, must match any session that mounted the shared project.repo)."`

	Scope string `help:"Backing repo scope: 'session' uses .sesh/sessions/<label>.repo; 'project' uses the shared .sesh/project.repo." enum:"session,project" default:"session"`

	Output string `name:"output" help:"Output directory (must exist). Defaults to cwd."`

	GitAdd bool `name:"git-add" help:"Run 'git add .' in the output dir after writing. Useful for chaining into 'git commit -m \"materialize <label>\"'."`

	AllowDirty bool `name:"allow-dirty" help:"Proceed even if the output dir is a git repo with uncommitted changes. Default: refuse, to protect the operator's in-flight work."`
}

// materializeResult is the structured outcome of a materialize call.
// Captured as a struct so the integration tests can assert on individual
// fields without parsing stdout.
type materializeResult struct {
	OutputDir string
	Files     int
	Bytes     int64
}

func (c *MaterializeCmd) Run() error {
	// Tier-1 safety: validate the label before ANY path math. The
	// rest of the function reads .sesh/sessions/<label>.repo and
	// extracts to .sesh/checkouts/.materialize-<label>-<random>/; a
	// hostile label would let either of those step outside .sesh/.
	if err := validateLabel(c.Label); err != nil {
		return fmt.Errorf("invalid label: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	scope := SeshScope(c.Scope)
	repoPath := repoPathFor(scope, cwd, c.Label)
	if _, err := os.Stat(repoPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf(
				"backing repo missing at %s — run 'sesh up --session=%s%s' first",
				repoPath, c.Label, scopeHint(scope))
		}
		return fmt.Errorf("stat backing repo %s: %w", repoPath, err)
	}

	outDir, err := c.resolveOutputDir(cwd)
	if err != nil {
		return err
	}

	if !c.AllowDirty {
		if err := refuseIfGitDirty(outDir); err != nil {
			return err
		}
	}

	result, err := materializeOverlay(repoPath, outDir)
	if err != nil {
		return err
	}

	if c.GitAdd {
		if err := runGitAdd(outDir); err != nil {
			return fmt.Errorf("git add: %w", err)
		}
	}

	slog.Info("materialized fossil trunk",
		"label", c.Label,
		"scope", scope,
		"repo", repoPath,
		"output", result.OutputDir,
		"files", result.Files,
		"bytes", result.Bytes,
	)
	fmt.Printf("materialized %d files (%d bytes) to %s\n", result.Files, result.Bytes, result.OutputDir)
	return nil
}

// resolveOutputDir returns the absolute output directory, defaulting to
// cwd. When --output is set, the directory must exist; we do NOT create
// it. Rationale: materialize is a heavyweight operation that should land
// where the operator explicitly pointed it. Auto-creating a typo'd path
// would scatter trunk-tip files across the filesystem.
func (c *MaterializeCmd) resolveOutputDir(cwd string) (string, error) {
	if c.Output == "" {
		return cwd, nil
	}
	abs, err := filepath.Abs(c.Output)
	if err != nil {
		return "", fmt.Errorf("resolve --output %q: %w", c.Output, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("--output %s does not exist (materialize never creates the output dir)", abs)
		}
		return "", fmt.Errorf("stat --output %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--output %s is not a directory", abs)
	}
	return abs, nil
}

// refuseIfGitDirty checks `git status --porcelain` in dir. If the dir
// is a git repo and the status is non-empty, returns an error naming the
// dirty files and pointing at `git stash`. If the dir is NOT a git repo
// (e.g., a fresh tempdir for inspection), the check is silently skipped:
// that's a legitimate materialize target where no operator work is at
// risk.
func refuseIfGitDirty(dir string) error {
	if _, err := exec.LookPath("git"); err != nil {
		// No git binary available — the operator can't have a git
		// worktree to dirty. Best-effort skip.
		return nil
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// Not a git work tree — skip the check.
		return nil
	}

	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = dir
	var out bytes.Buffer
	statusCmd.Stdout = &out
	statusCmd.Stderr = &out
	if err := statusCmd.Run(); err != nil {
		return fmt.Errorf("git status in %s: %w (output: %s)", dir, err, out.String())
	}
	porcelain := out.String()
	if porcelain == "" {
		return nil
	}
	return fmt.Errorf(
		"refusing to materialize into dirty git worktree %s\n"+
			"uncommitted files:\n%s"+
			"stash with `git stash`, or re-run with --allow-dirty to overlay anyway",
		dir, porcelain)
}

// runGitAdd executes `git add .` in dir. Idempotent: if no diff,
// git-add is a no-op. We do not gate this on a prior git-repo check —
// if the operator passed --git-add against a non-git dir, the
// surfaced error from git itself is the right signal.
func runGitAdd(dir string) error {
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, stderr.String())
	}
	return nil
}

// materializeOverlay extracts the trunk-tip of repoPath into a tempdir,
// then copies each file into outDir preserving relative paths and modes.
// The tempdir is removed on success (via defer); on copy failure, the
// tempdir is also removed but files already copied into outDir remain
// (overlay semantics: half-applied state is recoverable via git).
//
// The tempdir lives under outDir's parent so the eventual rename-or-
// copy stays on a single filesystem (avoids EXDEV). We use os.MkdirTemp
// with a deterministic prefix so a crashed run leaves a discoverable
// artifact rather than a random orphan under /tmp.
func materializeOverlay(repoPath, outDir string) (*materializeResult, error) {
	parent := filepath.Dir(outDir)
	tmpDir, err := os.MkdirTemp(parent, ".sesh-materialize-")
	if err != nil {
		return nil, fmt.Errorf("create materialize tempdir under %s: %w", parent, err)
	}
	// Defer cleanup. Note: NOT os.RemoveAll of anything inside outDir
	// — only the transient extract tempdir we just created. Tier-1
	// safety: this Remove targets a path we made ourselves, named
	// with a `.sesh-materialize-` prefix, never an operator path.
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return nil, fmt.Errorf("open backing repo %s: %w", repoPath, err)
	}
	defer repo.Close()

	co, err := repo.CreateCheckout(tmpDir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		return nil, fmt.Errorf("create transient checkout: %w", err)
	}
	defer co.Close()

	rid, _, err := co.Version()
	if err != nil {
		return nil, fmt.Errorf("read checkout version: %w", err)
	}
	if rid == 0 {
		return nil, fmt.Errorf("backing repo %s has no commits on trunk to materialize", repoPath)
	}

	if err := co.Extract(rid, libfossil.ExtractOpts{}); err != nil {
		return nil, fmt.Errorf("extract trunk-tip: %w", err)
	}

	result := &materializeResult{OutputDir: outDir}
	if err := overlayCopyTree(tmpDir, outDir, result); err != nil {
		return nil, err
	}
	return result, nil
}

// overlayCopyTree walks src and copies every regular file to dst,
// preserving relative paths and modes. Directories in dst are created
// as needed (0o755). The libfossil checkout marker `.fslckout` is
// skipped — it's a libfossil bookkeeping file, not part of the trunk's
// tracked content. result.Files / result.Bytes are accumulated as the
// walk progresses.
//
// Symlinks within the trunk: walked as their target type. libfossil's
// Extract writes symlinks as regular files containing the target path
// when the repo's allow-symlinks config is off (the default), so the
// typical trunk has no live symlinks to handle.
func overlayCopyTree(src, dst string, result *materializeResult) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("relpath %s: %w", path, err)
		}
		// Skip the libfossil checkout marker. Skipping the dir form
		// in case a future libfossil ever stamps a dir instead of a
		// file.
		if rel == ".fslckout" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		dstPath := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dstPath, err)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Non-regular, non-directory (device, named pipe,
			// socket, symlink-with-allow-symlinks). Fossil trunks
			// shouldn't carry these in practice; skip with a slog
			// rather than refuse the whole materialize.
			slog.Info("skipping non-regular file in trunk overlay",
				"path", rel, "type", d.Type().String())
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		n, err := copyFilePreservingMode(path, dstPath, info.Mode())
		if err != nil {
			return fmt.Errorf("copy %s -> %s: %w", path, dstPath, err)
		}
		result.Files++
		result.Bytes += n
		return nil
	})
}

// copyFilePreservingMode opens src, creates dst (creating parents),
// streams bytes through, and chmods dst to match the source's mode.
// Returns the number of bytes written. Used by the overlay walk above.
//
// dst is opened with O_TRUNC so overlaying a file that already exists
// in the output dir replaces the content (overlay semantics: trunk-tip
// wins on overlap). The mode is applied AFTER the close so the perms
// reflect the source even if the umask would have masked them on
// create.
func copyFilePreservingMode(src, dst string, mode fs.FileMode) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir parent of %s: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if closeErr != nil {
		return n, closeErr
	}
	// Re-apply mode. We only honor the perm bits (lower 9); type bits
	// (setuid/setgid/sticky) are dropped — libfossil's checkout
	// doesn't record those and we don't want a future trunk recording
	// them to silently grant elevated privileges in a materialize.
	if err := os.Chmod(dst, mode.Perm()); err != nil {
		return n, fmt.Errorf("chmod %s: %w", dst, err)
	}
	return n, nil
}
