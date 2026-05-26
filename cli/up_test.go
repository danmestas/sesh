package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// TestStarter_PreHubBootstrap_NoHub exercises the pre-hub phase against
// a freshly-isolated HOME where no hub is running. Expected pipeline:
// ProbeHub returns Present=false, MakePlan picks SourceGitWorktree
// (fresh repo, no hub content), no project-code adoption fires
// (probe.ProjectCode is empty). Pins phase one of the
// "pre-hub adoption → hub-acquire → post-hub seed → publish-session"
// ordering by isolating the steps that ought to run before any hub
// work.
func TestStarter_PreHubBootstrap_NoHub(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(cwd)

	c := &UpCmd{Session: "alpha", Seed: "all", Scope: "session"}
	s, err := NewStarter(c)
	if err != nil {
		t.Fatalf("NewStarter: %v", err)
	}
	t.Cleanup(s.Release)

	if err := s.preHubBootstrap(); err != nil {
		t.Fatalf("preHubBootstrap: %v", err)
	}

	if s.probe.Present {
		t.Errorf("probe.Present = true on empty HOME; want false")
	}
	if s.plan.Source != SourceGitWorktree {
		t.Errorf("plan.Source = %v, want SourceGitWorktree", s.plan.Source)
	}
	if s.plan.SeedMode != SeedAll {
		t.Errorf("plan.SeedMode = %q, want %q", s.plan.SeedMode, SeedAll)
	}
	if !isValidProjectCode(s.projectCode) {
		t.Errorf("projectCode = %q, want a 40-hex pin", s.projectCode)
	}
}

// TestStarter_PreHubBootstrap_AdoptsDriftedHubCode covers the SourceHub
// branch of phase one: the hub has published both a Fossil URL and a
// project-code that disagrees with the local pin. preHubBootstrap
// must rewrite the local pin to the hub's code (issue #34 acceptance)
// and update its in-memory projectCode for downstream phases.
//
// Setup writes ~/.sesh/hub.fossil.url plus a hub.repo with the desired
// project-code so ReadHubProjectCode returns the hub's value via its
// real SQL path. The hub.fossil.url need not be reachable; ProbeHub
// itself doesn't dial.
func TestStarter_PreHubBootstrap_AdoptsDriftedHubCode(t *testing.T) {
	const (
		localCode = "1111111111111111111111111111111111111111"
		hubCode   = "2222222222222222222222222222222222222222"
	)
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(cwd)

	if err := os.MkdirAll(filepath.Join(cwd, ".sesh"), 0o755); err != nil {
		t.Fatalf("mkdir .sesh: %v", err)
	}
	if err := os.WriteFile(projectCodePath(cwd), []byte(localCode+"\n"), 0o644); err != nil {
		t.Fatalf("seed local project-code: %v", err)
	}

	seshDir := filepath.Join(home, ".sesh")
	if err := os.MkdirAll(seshDir, 0o755); err != nil {
		t.Fatalf("mkdir ~/.sesh: %v", err)
	}
	if err := os.WriteFile(hubFossilURLPath(seshDir), []byte("http://hub.example/\n"), 0o644); err != nil {
		t.Fatalf("seed hub.fossil.url: %v", err)
	}
	seedHubRepoWithProjectCode(t, hubRepoPath(seshDir), hubCode)

	c := &UpCmd{Session: "alpha", Seed: "all", Scope: "session"}
	s, err := NewStarter(c)
	if err != nil {
		t.Fatalf("NewStarter: %v", err)
	}
	t.Cleanup(s.Release)

	if err := s.preHubBootstrap(); err != nil {
		t.Fatalf("preHubBootstrap: %v", err)
	}

	if s.plan.Source != SourceHub {
		t.Errorf("plan.Source = %v, want SourceHub", s.plan.Source)
	}
	if s.projectCode != hubCode {
		t.Errorf("in-memory projectCode = %q, want %q (adoption did not run)", s.projectCode, hubCode)
	}
	pinned, err := os.ReadFile(projectCodePath(cwd))
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if got := stringTrim(pinned); got != hubCode {
		t.Errorf("pinned project-code = %q, want %q", got, hubCode)
	}
}

// TestStarter_PostHubBootstrap_SourceNoneIsNoOp verifies that the
// post-hub phase tolerates SourceNone with no real hub — the
// SourceNone branch of Apply logs and returns without touching h.
// Locks down the contract that postHubBootstrap is safe to call for
// any plan source without exploding on a nil-hub for the
// no-bootstrap cases.
func TestStarter_PostHubBootstrap_SourceNoneIsNoOp(t *testing.T) {
	s := &Starter{
		cwd:      t.TempDir(),
		repoPath: "/dev/null/x.repo",
		plan:     Plan{Source: SourceNone},
		// h intentionally left nil — Apply must not deref for SourceNone.
	}
	s.postHubBootstrap(context.Background())
}

// seedHubRepoWithProjectCode minimally seeds a libfossil-shaped SQLite
// file at path so ReadHubProjectCode returns code. Mirrors the schema
// bits TestReadHubProjectCode_SeededRepo uses.
func seedHubRepoWithProjectCode(t *testing.T, path, code string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open seeded hub.repo: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE config(name TEXT UNIQUE, value, mtime INTEGER)`); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE event(type TEXT, mtime DATETIME, objid INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create event: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO config(name, value, mtime) VALUES('project-code', ?, 0)`, code); err != nil {
		t.Fatalf("seed project-code: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO event(type, mtime, objid) VALUES('ci', 0, 1)`); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded db: %v", err)
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
		Session:   "t3-sess",
		NATSURL:   "nats://127.0.0.1:4222",
		NATSWSURL: "ws://127.0.0.1:8080",
		FossilURL: "http://127.0.0.1:8081/",
		LeafURL:   "nats://127.0.0.1:7422",
		Role:      "implementer",
		Class:     "active",
	}
	// The cmdStr is passed verbatim to sh -c; the test expression succeeds
	// only when the injected vars match exactly what we put in harnessEnv.
	cmdStr := `[ "$SESH_SESSION" = "t3-sess" ] && [ "$NATS_URL" = "nats://127.0.0.1:4222" ] && [ "$SESH_NATS_WS_URL" = "ws://127.0.0.1:8080" ] && [ "$SESH_FOSSIL_URL" = "http://127.0.0.1:8081/" ] && [ "$SESH_LEAF_URL" = "nats://127.0.0.1:7422" ] && [ "$SESH_ROLE" = "implementer" ] && [ "$SESH_CLASS" = "active" ] && exit 0 || exit 77`

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

	c := &UpCmd{Seed: "all", Scope: "session"}
	// Run will fail (no hub, no git repo), but Session must be set before it does.
	_ = c.Run()
	if c.Session == "" {
		t.Error("UpCmd.Session was not populated from cwd; want non-empty")
	}
	if c.Session != "myrunproject" {
		t.Errorf("UpCmd.Session = %q, want %q", c.Session, "myrunproject")
	}
}
