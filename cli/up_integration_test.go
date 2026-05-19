package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestUp_PopulatesSessionURLs is the topology pre-req: a running `sesh up`
// must surface its NATS client URL and leafnode URL in the project state
// file so sub-leaves and clients can attach without grepping logs.
//
// Builds the sesh binary into a tmpdir, isolates HOME and the project cwd,
// spawns `sesh up`, polls for the JSON to gain URLs, TCP-dials both, then
// SIGINTs and verifies the state file is reaped.
func TestUp_PopulatesSessionURLs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: builds binary + spawns subprocess")
	}

	bin := buildSesh(t)

	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	cmd := exec.Command(bin, "up", "--session=alpha")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sesh up: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})

	statePath := filepath.Join(project, ".sesh", "sessions", "alpha.json")
	state := waitForURLs(t, statePath, 15*time.Second)

	if state.PID != cmd.Process.Pid {
		t.Errorf("state PID = %d, want %d", state.PID, cmd.Process.Pid)
	}
	dial(t, state.NATSURL, "NATSURL")
	dial(t, state.LeafURL, "LeafURL")

	// nats_ws_url is the WebSocket endpoint for browser / CF Workers
	// clients. WS is enabled by default; the field must be present and
	// the underlying TCP socket dialable. (NATS-level WS pub/sub
	// round-trip is covered by EdgeSync's hub package tests; sesh's
	// job here is just to confirm the URL surfaces in the session JSON
	// and the socket is bound end-to-end.)
	if state.NATSWSURL == "" {
		t.Fatal("nats_ws_url missing from session JSON (WS default-enabled)")
	}
	if !strings.HasPrefix(state.NATSWSURL, "ws://") {
		t.Errorf("nats_ws_url = %q, want ws:// scheme", state.NATSWSURL)
	}
	dial(t, state.NATSWSURL, "NATSWSURL")

	// hub.nats.url is the hub's client NATS URL — clients doing
	// hub/project/workflow-scoped KV work connect here so their KV
	// buckets live in the shared (hub) JetStream domain, not in a
	// session's domain.
	hubNATSURLPath := filepath.Join(home, ".sesh", "hub.nats.url")
	hubNATSURL := readTrimmed(t, hubNATSURLPath)
	dial(t, hubNATSURL, "hub.nats.url")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Logf("sesh up exit: %v", err)
	}

	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file lingered after shutdown: %v", err)
	}
	// hub.nats.url is cleaned up by the hub process on its auto-shutdown
	// (~500ms after sesh up exits), not by sesh up itself — same as
	// hub.url. Not asserted here; race-prone.
}

// TestSeshUp_RejectsLabelTraversal is the tier-1 safety test for the
// up entrypoint. It exercises validateLabel through `sesh up --session`
// with the hostile inputs that would otherwise let the label escape its
// slot under .sesh/. The same matrix as the worktree / materialize
// retrofit tests — validateLabel sits ABOVE the path math in every
// label-consuming subcommand.
//
// We seed a known canary under .sesh/messaging/ and fingerprint the
// .sesh/ tree before and after the hostile-input runs. Each invocation
// must exit non-zero, never bring a session up, and never mutate any
// existing path under .sesh/.
func TestSeshUp_RejectsLabelTraversal(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	// .sesh/ tier-1 paths we want to prove are not touched by the
	// hostile-input runs. We do NOT bring a real session up — the
	// validator must reject the label before any path math touches
	// disk.
	seshDir := filepath.Join(project, ".sesh")
	if err := os.MkdirAll(filepath.Join(seshDir, "sessions"), 0o755); err != nil {
		t.Fatalf("seed .sesh/sessions: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(seshDir, "messaging"), 0o755); err != nil {
		t.Fatalf("seed .sesh/messaging: %v", err)
	}
	canary := filepath.Join(seshDir, "messaging", "canary.txt")
	if err := os.WriteFile(canary, []byte("tier-1\n"), 0o644); err != nil {
		t.Fatalf("seed canary: %v", err)
	}
	before := fingerprintTree(t, seshDir)

	for _, tc := range hostileLabelInputs {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			// Per-case deadline. validateLabel must reject and
			// exit non-zero in well under a second; a regression
			// that lets the label through would otherwise hang
			// the test by booting a daemon. CommandContext +
			// SIGKILL keeps the test bounded so we see a clean
			// failure rather than a 4-minute test-timeout panic.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "up", "--session="+tc.Label)
			cmd.Dir = project
			cmd.Env = append(os.Environ(), "HOME="+home)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err == nil {
				t.Fatalf("sesh up accepted hostile label %q; stdout=%q stderr=%q",
					tc.Label, stdout.String(), stderr.String())
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				// Ctx-killed means sesh up didn't fail-fast on
				// the hostile label — it ran long enough that
				// the deadline elapsed. That's the regression
				// signature we're guarding against.
				t.Fatalf("sesh up failed to fail-fast on hostile label %q (deadline-killed); stdout=%q stderr=%q",
					tc.Label, stdout.String(), stderr.String())
			}
			// Either Kong rejects the flag (empty label), our
			// validator rejects it, or os/exec refuses the
			// arg outright (NUL byte case — argv can't carry
			// NULs on POSIX). All three are acceptable
			// provided the exit is non-zero and tier-1 .sesh/
			// paths survive (asserted at the end of the
			// parent test). The cue check is best-effort —
			// for the NUL case Go's exec rejects with
			// "invalid argument" before the binary ever
			// runs, so stderr is empty.
			combined := strings.ToLower(stderr.String() + stdout.String() + err.Error())
			if !strings.Contains(combined, "label") && !strings.Contains(combined, "session") && !strings.Contains(combined, "invalid argument") {
				t.Errorf("hostile label %q rejected but no 'label'/'session'/'invalid argument' cue; err=%v stderr=%s",
					tc.Label, err, stderr.String())
			}
		})
	}

	after := fingerprintTree(t, seshDir)
	if before != after {
		t.Errorf("tier-1 .sesh/ tree fingerprint drifted after hostile-input up runs:\nbefore=%s\nafter=%s",
			before, after)
	}
	if got, err := os.ReadFile(canary); err != nil || string(got) != "tier-1\n" {
		t.Errorf("canary %s mutated by hostile-input up runs; got=%q err=%v",
			canary, string(got), err)
	}
}

// TestSeshUp_RegistersOperatorInFossilUserTable is the regression for
// sesh#77 — `sesh up` must register the operator's $USER in the seeded
// fossil repo's user table so subsequent `fossil commit` invocations
// from a worker (or operator) checkout do not hit "cannot determine
// user".
//
// The check is end-to-end via the fossil CLI (not libfossil-direct)
// because the gap only surfaces on the CLI path: `cli/seed.go` commits
// through libfossil's hub writer which bypasses the user-table check,
// while real workers spawned by `orch-spawn` use the fossil binary
// which does enforce it.
//
// Shape:
//
//  1. `sesh up alpha` against a fresh git project — seeds the project
//     repo and (with the fix) registers $USER.
//  2. `sesh worktree alpha` materializes a checkout.
//  3. Inside the checkout, `fossil add` + `fossil commit` MUST succeed
//     under the operator's natural $USER without any `fossil user new`
//     priming step.
//  4. Sanity: `fossil timeline -n 1` shows the new commit.
//
// Pre-fix evidence: this test FAILS at step 3 with "cannot determine
// user" exit-1.
func TestSeshUp_RegistersOperatorInFossilUserTable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: builds binary + spawns subprocess + uses fossil CLI")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}

	// The fossil CLI uses $USER to pick the committer login. Pin it
	// explicitly so the assertion below is stable across CI hosts where
	// $USER might be empty or "root".
	user := os.Getenv("USER")
	if user == "" {
		user = "sesh-op-test"
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	sesh, stderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, sesh, stderr)
	waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)
	if !waitForSlog(t, stderr, "seeded fossil from worktree", 10*time.Second) {
		t.Fatalf("alpha never seeded:\n%s", stderr.String())
	}

	checkout := mustRunWorktree(t, bin, home, project, "alpha")
	if checkout == "" {
		t.Fatalf("worktree printed empty stdout")
	}

	// Write a new file inside the checkout and try to commit via the
	// fossil CLI. This is the path real workers take (per the
	// fossil-worker accessory). Pre-fix this exits non-zero with
	// "cannot determine user".
	if err := os.WriteFile(filepath.Join(checkout, "from-op.txt"), []byte("operator-was-here\n"), 0o644); err != nil {
		t.Fatalf("write from-op.txt: %v", err)
	}
	runFossil := func(args ...string) (string, error) {
		cmd := exec.Command("fossil", args...)
		cmd.Dir = checkout
		cmd.Env = append(os.Environ(), "USER="+user, "HOME="+home)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		return out.String(), err
	}
	if out, err := runFossil("add", "from-op.txt"); err != nil {
		t.Fatalf("fossil add from-op.txt: %v\n%s", err, out)
	}
	out, err := runFossil("commit", "-m", "operator commit", "--no-warnings")
	if err != nil {
		// The exact pre-fix error string from fossil 2.x.
		if strings.Contains(strings.ToLower(out), "cannot determine") ||
			strings.Contains(strings.ToLower(out), "user") {
			t.Fatalf("fossil commit rejected operator user %q (sesh#77 regression): %v\n%s",
				user, err, out)
		}
		t.Fatalf("fossil commit failed for unrelated reason: %v\n%s", err, out)
	}

	// Sanity: the commit landed in the trunk timeline.
	tlOut, err := runFossil("timeline", "-n", "5", "-t", "ci")
	if err != nil {
		t.Fatalf("fossil timeline: %v\n%s", err, tlOut)
	}
	if !strings.Contains(tlOut, "operator commit") {
		t.Errorf("expected 'operator commit' in fossil timeline; got:\n%s", tlOut)
	}
}

// readTrimmed reads a file and trims trailing whitespace. The hub
// writes its NATS URL with a trailing newline; clients trim before use.
func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(data)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\r' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// stateOnDisk mirrors cli.SessionState — keeping the integration test in
// package cli_test (external) means we can't reference the unexported
// claim helpers, but the JSON schema is the public contract.
type stateOnDisk struct {
	PID       int    `json:"pid"`
	NATSURL   string `json:"nats_url"`
	NATSWSURL string `json:"nats_ws_url"`
	LeafURL   string `json:"leaf_url"`
	FossilURL string `json:"fossil_url"`
}

func waitForURLs(t *testing.T, path string, timeout time.Duration) stateOnDisk {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last stateOnDisk
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(data, &last); err == nil {
				if last.NATSURL != "" && last.LeafURL != "" {
					return last
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for URLs in %s; last=%+v", path, last)
	return last
}

func dial(t *testing.T, u, label string) {
	t.Helper()
	if u == "" {
		t.Fatalf("%s empty", label)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse %s %q: %v", label, u, err)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s %s: %v", label, parsed.Host, err)
	}
	_ = conn.Close()
}

func buildSesh(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	bin := filepath.Join(t.TempDir(), "sesh")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/sesh")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}
