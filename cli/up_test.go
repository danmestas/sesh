package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// TestNewStarter_ResolvesProjectIdentityWithoutHub verifies that NewStarter
// — the pre-serve setup phase — derives the project identity (project-code
// + project-id pins) and claims the session slot WITHOUT any hub contact.
// sesh is a NATS client now: there is no probe / plan / hub-acquire phase,
// and NewStarter must succeed against an isolated HOME with no hub running.
func TestNewStarter_ResolvesProjectIdentityWithoutHub(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(cwd)

	c := &UpCmd{Session: "alpha", Scope: "session"}
	s, err := NewStarter(c)
	if err != nil {
		t.Fatalf("NewStarter: %v", err)
	}
	t.Cleanup(s.Release)

	if !isValidProjectCode(s.projectCode) {
		t.Errorf("projectCode = %q, want a 40-hex pin", s.projectCode)
	}
	if !isValidProjectCode(s.projectID) {
		t.Errorf("projectID = %q, want a 40-hex pin", s.projectID)
	}
	// The session claim file must exist after NewStarter (atomic O_EXCL claim).
	claim := filepath.Join(cwd, ".sesh", "sessions", "alpha.json")
	if _, err := os.Stat(claim); err != nil {
		t.Errorf("session claim file %s not created by NewStarter: %v", claim, err)
	}
}

// TestUpCmd_FlagsAccepted is the Step-1 TDD sentinel (per locked plan):
// constructs kong over a root CLI wrapper embedding UpCmd (as done in
// cmd/sesh/main.go) and parses the new flags alongside --session.
// Written first so it fails (fields missing on UpCmd); after Step 2 it
// passes. Uses --exec='echo hi' (sh -c parsing is later), --role and
// --class per role-propagation decision A.
func TestUpCmd_FlagsAccepted(t *testing.T) {
	var root struct {
		Up UpCmd `cmd:"" help:"Bring a session up"`
	}
	k, err := kong.New(&root)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	// Parse as the "up" subcommand; optional Session + the three new flags.
	_, err = k.Parse([]string{"up", "--session=foo", "--exec=echo hi", "--role=implementer", "--class=active"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if got := root.Up.Session; got != "foo" {
		t.Errorf("Session = %q, want %q", got, "foo")
	}
	if got := root.Up.Exec; got != "echo hi" {
		t.Errorf("Exec = %q, want %q", got, "echo hi")
	}
	if got := root.Up.Role; got != "implementer" {
		t.Errorf("Role = %q, want %q", got, "implementer")
	}
	if got := root.Up.Class; got != "active" {
		t.Errorf("Class = %q, want %q", got, "active")
	}
}

// TestUpCmd_DeadEmbeddedHubFlagsRejected pins the F1 scope-cut: the inert
// embedded-NATS-server / fossil-seed flags were removed when sesh became a
// pure NATS client. Each must now be an unknown flag (parse error), proving
// the dead surface is gone rather than silently accepted-and-ignored.
func TestUpCmd_DeadEmbeddedHubFlagsRejected(t *testing.T) {
	dead := []string{
		"--http-port=8080",
		"--nats-client-port=4222",
		"--nats-leaf-port=7422",
		"--nats-ws-port=8081",
		"--disable-ws",
		"--seed=all",
	}
	for _, flag := range dead {
		var root struct {
			Up UpCmd `cmd:"" help:"Bring a session up"`
		}
		k, err := kong.New(&root)
		if err != nil {
			t.Fatalf("kong.New: %v", err)
		}
		if _, err := k.Parse([]string{"up", "--session=foo", flag}); err == nil {
			t.Errorf("flag %q parsed successfully; want unknown-flag error (dead surface)", flag)
		}
	}
}

// TestSpawnHarness_ReturnsChan is the TDD skeleton sentinel for Task 3
// (per implementation plan + locked Hybrid C). Written first so it fails
// until spawnHarness exists with the right signature and returns a closed
// chan for the no-op case. Exercises the Harness + harnessEnv types too.
func TestSpawnHarness_ReturnsChan(t *testing.T) {
	ch := spawnHarness(context.Background(), "", harnessEnv{})
	_, ok := <-ch
	if ok {
		t.Error("chan should be closed for no-op / empty cmdStr")
	}
}

// TestSpawnHarness_HappyPathEnvAndWait verifies the real happy-path spawn:
// uses sh -c (X), inherits stdio (silent here), Setpgid, builds the canonical
// env (5 SESH_* + NATS + ROLE/CLASS from harnessEnv), waiter goroutine.
// Child is a pure test expr that exits 0 only if env was correctly injected.
// This is the "compile + run" verification before wiring into Starter.serve.
func TestSpawnHarness_HappyPathEnvAndWait(t *testing.T) {
	env := harnessEnv{
		Session: "t3-sess",
		NATSURL: "nats://127.0.0.1:4222",
		Role:    "implementer",
		Class:   "active",
	}
	// The cmdStr is passed verbatim to sh -c; the test expression succeeds
	// only when the injected vars match exactly what we put in harnessEnv.
	// The embedded-hub URL vars (SESH_NATS_WS_URL / SESH_FOSSIL_URL /
	// SESH_LEAF_URL) are gone now that sesh is a pure NATS client — the
	// child reaches the hub via NATS_URL alone.
	cmdStr := `[ "$SESH_SESSION" = "t3-sess" ] && [ "$NATS_URL" = "nats://127.0.0.1:4222" ] && [ "$SESH_ROLE" = "implementer" ] && [ "$SESH_CLASS" = "active" ] && exit 0 || exit 77`

	ch := spawnHarness(context.Background(), cmdStr, env)
	err := <-ch
	if err != nil {
		t.Fatalf("spawnHarness happy path: child exited non-zero (env injection or sh -c failed): %v", err)
	}
}

// TestHarnessSysProcAttr_NonTTYStdinSkipsForeground pins the regression for
// the SIGTTIN hang: when the parent's stdin is not a TTY (the test process
// case, and also any piped/redirected sesh up), we must set Setpgid but
// must NOT set Foreground/Ctty — setting Foreground on a non-TTY descriptor
// would cause forkExec to fail in tcsetpgrp.
func TestHarnessSysProcAttr_NonTTYStdinSkipsForeground(t *testing.T) {
	r, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	spa := harnessSysProcAttr(r)
	if spa == nil {
		t.Fatal("harnessSysProcAttr returned nil")
	}
	if !spa.Setpgid {
		t.Error("Setpgid = false; want true (group-signal forwarding requires it)")
	}
	if spa.Foreground {
		t.Error("Foreground = true on non-TTY stdin; want false (would crash forkExec)")
	}
	if spa.Ctty != 0 {
		t.Errorf("Ctty = %d on non-TTY stdin; want 0 (unset)", spa.Ctty)
	}
}

// TestHarnessSysProcAttr_NilStdinFallsBack guards against a future refactor
// passing nil for the stdin file — must still return a valid Setpgid-only
// SysProcAttr without panicking.
func TestHarnessSysProcAttr_NilStdinFallsBack(t *testing.T) {
	spa := harnessSysProcAttr(nil)
	if spa == nil {
		t.Fatal("harnessSysProcAttr(nil) returned nil")
	}
	if !spa.Setpgid {
		t.Error("Setpgid = false for nil stdin; want true")
	}
	if spa.Foreground || spa.Ctty != 0 {
		t.Error("Foreground/Ctty must remain unset for nil stdin")
	}
}

// TestHarnessEnvVars_CarriesProject pins the wire contract that the harness
// child receives SESH_PROJECT (the peer-discoverability fix) alongside the
// pre-existing SESH_SESSION / SESH_ROLE. harnessEnvVars is the single source
// of truth for the spawned agent's env, so asserting its output is equivalent
// to asserting cmd.Env without spawning a subprocess.
func TestHarnessEnvVars_CarriesProject(t *testing.T) {
	env := harnessEnv{
		Session: "sesh-talk-2",
		Project: "sesh",
		NATSURL: "nats://127.0.0.1:4222",
		Role:    "orch",
		Class:   "active",
	}
	got := harnessEnvVars(env)

	has := func(want string) bool {
		for _, kv := range got {
			if kv == want {
				return true
			}
		}
		return false
	}

	// The fix: project is exported and is the stable token, NOT the
	// de-duped session label.
	if !has("SESH_PROJECT=sesh") {
		t.Errorf("harnessEnvVars missing SESH_PROJECT=sesh; got %v", got)
	}
	if has("SESH_PROJECT=sesh-talk-2") {
		t.Error("SESH_PROJECT must not equal the de-duped session label")
	}
	// Pre-existing vars must still be present.
	if !has("SESH_SESSION=sesh-talk-2") {
		t.Errorf("harnessEnvVars missing SESH_SESSION; got %v", got)
	}
	if !has("SESH_ROLE=orch") {
		t.Errorf("harnessEnvVars missing SESH_ROLE; got %v", got)
	}
	// The embedded-hub URL exports are gone now that sesh is a pure NATS
	// client; the child must NOT receive three empty SESH_*_URL exports.
	hasPrefix := func(prefix string) bool {
		for _, kv := range got {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}
	for _, dead := range []string{"SESH_NATS_WS_URL=", "SESH_FOSSIL_URL=", "SESH_LEAF_URL="} {
		if hasPrefix(dead) {
			t.Errorf("harnessEnvVars must not emit dead embedded-hub var %q; got %v", dead, got)
		}
	}
}

// TestHarnessEnvVars_CarriesProjectID pins the C1 wire contract: the harness
// child receives the pinned 40-hex projectID under SESH_PROJECT_ID, distinct
// from the human-readable SESH_PROJECT slug. This is the routing key the
// refagent uses for the 4th token of its coordination subjects; without it the
// refagent would have to re-derive it by walking the filesystem.
func TestHarnessEnvVars_CarriesProjectID(t *testing.T) {
	const pinnedID = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	env := harnessEnv{
		Session:   "sesh-talk-2",
		Project:   "sesh",
		ProjectID: pinnedID,
		NATSURL:   "nats://127.0.0.1:4222",
	}
	got := harnessEnvVars(env)

	has := func(want string) bool {
		for _, kv := range got {
			if kv == want {
				return true
			}
		}
		return false
	}

	if !has("SESH_PROJECT_ID=" + pinnedID) {
		t.Errorf("harnessEnvVars missing SESH_PROJECT_ID=%s; got %v", pinnedID, got)
	}
	// The pinned 40-hex id must be distinct from the human-readable slug.
	if has("SESH_PROJECT_ID=sesh") {
		t.Error("SESH_PROJECT_ID must carry the 40-hex id, not the human slug")
	}
}

// TestSanitizeLabelFromBasename covers the stripping rules applied to cwd
// basenames before they're used as session labels.
func TestSanitizeLabelFromBasename(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sesh", "sesh"},
		{"my-project", "my-project"},
		{"my project", "my project"}, // spaces are printable ASCII, kept
		{".hidden", "hidden"},        // leading dot stripped
		{"a/b", "ab"},                // path separator removed
		{"a\\b", "ab"},               // backslash removed
		{"\x00nul", "nul"},           // NUL stripped
		{"\x1b[red]", "[red]"},       // control chars stripped
		{"café", "caf"},              // non-ASCII stripped
		{"", ""},                     // empty stays empty
		{"..", ""},                   // all-dot → empty after strip
		{strings.Repeat("x", 200), strings.Repeat("x", 128)}, // capped at 128
	}
	for _, tc := range cases {
		got := sanitizeLabelFromBasename(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeLabelFromBasename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDeriveSessionName_UsesBasename verifies that deriveSessionName returns
// the cwd basename when no claim file exists.
func TestDeriveSessionName_UsesBasename(t *testing.T) {
	dir := t.TempDir()
	// Rename to a predictable basename.
	named := filepath.Join(filepath.Dir(dir), "myproject")
	if err := os.Rename(dir, named); err != nil {
		t.Skipf("rename temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(named) })
	t.Chdir(named)

	got, err := deriveSessionName()
	if err != nil {
		t.Fatalf("deriveSessionName: %v", err)
	}
	if got != "myproject" {
		t.Errorf("deriveSessionName = %q, want %q", got, "myproject")
	}
}

// TestDeriveSessionName_IncrementsOnConflict verifies that deriveSessionName
// appends -2, -3, … when claim files exist for earlier candidates.
func TestDeriveSessionName_IncrementsOnConflict(t *testing.T) {
	dir := t.TempDir()
	named := filepath.Join(filepath.Dir(dir), "myproject")
	if err := os.Rename(dir, named); err != nil {
		t.Skipf("rename temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(named) })
	t.Chdir(named)

	// Plant claim files for "myproject" and "myproject-2".
	stateDir := filepath.Join(named, ".sesh", "sessions")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir stateDir: %v", err)
	}
	for _, name := range []string{"myproject", "myproject-2"} {
		if err := os.WriteFile(filepath.Join(stateDir, name+".json"), []byte(`{"pid":999999}`), 0o644); err != nil {
			t.Fatalf("plant claim %s: %v", name, err)
		}
	}

	got, err := deriveSessionName()
	if err != nil {
		t.Fatalf("deriveSessionName: %v", err)
	}
	if got != "myproject-3" {
		t.Errorf("deriveSessionName = %q, want %q", got, "myproject-3")
	}
}

// TestUpCmd_Run_DerivesSessionFromCwd verifies that UpCmd.Run populates
// c.Session from the cwd when --session is omitted. We invoke Run and
// expect it to fail past the label-derivation point (no hub) but confirm
// the label was set before the failure.
func TestUpCmd_Run_DerivesSessionFromCwd(t *testing.T) {
	dir := t.TempDir()
	named := filepath.Join(filepath.Dir(dir), "myrunproject")
	if err := os.Rename(dir, named); err != nil {
		t.Skipf("rename temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(named) })
	t.Setenv("HOME", t.TempDir())
	t.Chdir(named)

	c := &UpCmd{Scope: "session"}
	// Run will fail (no hub, no git repo), but Session must be set before it does.
	_ = c.Run()
	if c.Session == "" {
		t.Error("UpCmd.Session was not populated from cwd; want non-empty")
	}
	if c.Session != "myrunproject" {
		t.Errorf("UpCmd.Session = %q, want %q", c.Session, "myrunproject")
	}
}
