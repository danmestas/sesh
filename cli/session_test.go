package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSessionState_RoundTrip exercises JSON encode/decode with the full
// field set. Codifies the on-disk schema sub-leaves and clients consume.
func TestSessionState_RoundTrip(t *testing.T) {
	in := SessionState{
		PID:       12345,
		NATSURL:   "nats://127.0.0.1:54321",
		NATSWSURL: "ws://127.0.0.1:54323",
		LeafURL:   "nats-leaf://127.0.0.1:54322",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out SessionState
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestSessionState_NATSWSURLOmitEmpty pins the omitempty contract for the
// new nats_ws_url field. When WebSocket is disabled the URL is empty and
// the field must not appear in the on-disk JSON — backward compatibility
// for consumers that haven't learned about the field yet, and a clean
// signal to consumers that have ("missing" means "no WS available").
func TestSessionState_NATSWSURLOmitEmpty(t *testing.T) {
	in := SessionState{
		PID:     1,
		NATSURL: "nats://127.0.0.1:1",
		LeafURL: "nats-leaf://127.0.0.1:2",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "nats_ws_url") {
		t.Errorf("nats_ws_url present in JSON when URL is empty: %s", data)
	}
}

// TestSessionState_NATSWSURLPresent confirms the field IS emitted when
// non-empty — protects against accidentally tagging the field with a
// flag that suppresses output unconditionally.
func TestSessionState_NATSWSURLPresent(t *testing.T) {
	in := SessionState{
		PID:       1,
		NATSURL:   "nats://127.0.0.1:1",
		NATSWSURL: "ws://127.0.0.1:3",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"nats_ws_url":"ws://127.0.0.1:3"`) {
		t.Errorf("nats_ws_url missing or wrong shape in JSON: %s", data)
	}
}

// TestSessionState_BackCompat verifies a PID-only file written by an older
// sesh still parses cleanly — empty URL fields rather than parse failure.
// This keeps in-flight session files readable across a version bump.
func TestSessionState_BackCompat(t *testing.T) {
	old := []byte(`{"pid":99}`)
	var s SessionState
	if err := json.Unmarshal(old, &s); err != nil {
		t.Fatalf("legacy parse failed: %v", err)
	}
	if s.PID != 99 || s.NATSURL != "" || s.LeafURL != "" || s.NATSWSURL != "" {
		t.Fatalf("legacy parse got %+v", s)
	}
}

// TestSessionStatePersistsNATSURL verifies the session JSON written by
// Session.Publish carries the NATSURL field, so downstream tools can read
// it from <cwd>/.sesh/sessions/<label>.json#nats_url. The session JSON is
// the canonical per-session source for the hub's NATS URL — see F6 in the
// integration-rig findings and docs/synadia-agents-on-sesh.md
// "NATS URL discovery and lifecycle". This test is a regression guard:
// removing NATSURL from the SessionState publish path (or changing the
// JSON tag) would silently break every downstream tool that reads
// nats_url from the session JSON. Asserts both the raw JSON tag presence
// (the on-disk contract) and the round-tripped Go field.
func TestSessionStatePersistsNATSURL(t *testing.T) {
	dir := t.TempDir()

	sess, err := ClaimSession(dir, "f6-test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release() })

	wantURL := "nats://127.0.0.1:65535"
	if err := sess.Publish(SessionState{
		PID:     os.Getpid(),
		NATSURL: wantURL,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Raw-JSON assertion: the on-disk file MUST carry the nats_url field
	// so external readers (shell scripts, other-language tools) can pick
	// it up by key.
	raw, err := os.ReadFile(filepath.Join(dir, "f6-test.json"))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.Contains(string(raw), `"nats_url":"`+wantURL+`"`) {
		t.Fatalf("nats_url missing or wrong shape in JSON: %s", raw)
	}

	got, err := ReadSession(dir, "f6-test")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.NATSURL != wantURL {
		t.Fatalf("nats_url: got %q want %q", got.NATSURL, wantURL)
	}
}

// TestSession_PublishOverwritesFile claims a session, publishes URLs, and
// verifies the on-disk file matches. Same coverage the prior
// TestUpdateSessionState_OverwritesFile gave through the deleted helpers.
func TestSession_PublishOverwritesFile(t *testing.T) {
	dir := t.TempDir()

	sess, err := ClaimSession(dir, "alpha")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release() })

	want := SessionState{
		PID:     os.Getpid(),
		NATSURL: "nats://127.0.0.1:4222",
		LeafURL: "nats-leaf://127.0.0.1:7422",
	}
	if err := sess.Publish(want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "alpha.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got SessionState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("on-disk mismatch: got %+v want %+v", got, want)
	}
}

// TestSession_PublishRequiresClaim refuses to publish for a session whose
// state file no longer exists — protects against writing state for a
// session that no live process owns. (Original guard from
// updateSessionState, preserved through Session.Publish.)
func TestSession_PublishRequiresClaim(t *testing.T) {
	dir := t.TempDir()

	sess, err := ClaimSession(dir, "alpha")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Externally remove the claim. Publish should refuse rather than
	// silently re-create a file no process owns.
	if err := os.Remove(filepath.Join(dir, "alpha.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := sess.Publish(SessionState{PID: 1, NATSURL: "x"}); err == nil {
		t.Fatalf("expected error publishing after external removal, got nil")
	}
}

// TestClaimSession_RefusesLiveOwner is the same-machine same-name guard:
// a second claim for the same (stateDir, label) while the first owner is
// alive must fail rather than overwrite. Guards against accidental
// relaxation of the O_EXCL semantics — see docs/synadia-agents-on-sesh.md
// "Session ownership" for the rationale (single-owner-per-label is what
// makes the lifecycle deterministic for sesh down / status / watchers).
func TestClaimSession_RefusesLiveOwner(t *testing.T) {
	dir := t.TempDir()

	first, err := ClaimSession(dir, "alpha")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	if _, err := ClaimSession(dir, "alpha"); err == nil {
		t.Fatalf("expected second claim to fail while owner is alive")
	}
}

// TestClaimSession_ReapsStaleDeadPID ensures a leftover file owned by a
// dead PID does not block a fresh claim — sesh up should recover from a
// previous run that crashed without releasing.
func TestClaimSession_ReapsStaleDeadPID(t *testing.T) {
	dir := t.TempDir()

	deadPID := exitedSubprocessPID(t)
	payload, err := json.Marshal(SessionState{PID: deadPID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha.json"), payload, 0o644); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	sess, err := ClaimSession(dir, "alpha")
	if err != nil {
		t.Fatalf("claim over stale: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release() })
}

// TestClaimSession_ReapsStaleOtherLabels pins the multi-label reaper.
// When several sessions in the same stateDir have been kill -9'd / OOM'd
// (deferred Release never ran), the next ClaimSession — even for a fresh
// unrelated label — should sweep all dead-pid manifests in the dir, not
// just the one it's trying to claim.
func TestClaimSession_ReapsStaleOtherLabels(t *testing.T) {
	dir := t.TempDir()

	deadPID := exitedSubprocessPID(t)
	stalePayload, err := json.Marshal(SessionState{PID: deadPID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	livePayload, err := json.Marshal(SessionState{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Seed: two stale (alpha/beta), one live (gamma).
	for name, body := range map[string][]byte{
		"alpha.json": stalePayload,
		"beta.json":  stalePayload,
		"gamma.json": livePayload,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// Also a non-json file — must be ignored, not touched.
	if err := os.WriteFile(filepath.Join(dir, "alpha.repo"), []byte("opaque"), 0o644); err != nil {
		t.Fatalf("seed alpha.repo: %v", err)
	}

	// Claim a brand-new label — the reaper runs as a side effect.
	sess, err := ClaimSession(dir, "delta")
	if err != nil {
		t.Fatalf("claim delta: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release() })

	// alpha + beta should be gone.
	for _, n := range []string{"alpha.json", "beta.json"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s not reaped: stat err=%v", n, err)
		}
	}
	// gamma (live owner) must survive.
	if _, err := os.Stat(filepath.Join(dir, "gamma.json")); err != nil {
		t.Errorf("gamma.json (live) was reaped: %v", err)
	}
	// non-json sibling must survive.
	if _, err := os.Stat(filepath.Join(dir, "alpha.repo")); err != nil {
		t.Errorf("alpha.repo (non-json) was touched: %v", err)
	}
	// And of course our own claim went through.
	if _, err := os.Stat(filepath.Join(dir, "delta.json")); err != nil {
		t.Errorf("delta claim file missing: %v", err)
	}
}

// TestTerminate_NoFile: missing state file → no error, no-op.
func TestTerminate_NoFile(t *testing.T) {
	dir := t.TempDir()
	if err := Terminate(dir, "ghost", time.Second); err != nil {
		t.Fatalf("Terminate on missing file: %v", err)
	}
}

// TestTerminate_StaleDeadPID: file present, owner is dead → reap silently.
func TestTerminate_StaleDeadPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alpha.json")
	deadPID := exitedSubprocessPID(t)
	payload, err := json.Marshal(SessionState{PID: deadPID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if err := Terminate(dir, "alpha", time.Second); err != nil {
		t.Fatalf("Terminate on stale: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale file not reaped: stat err=%v", err)
	}
}

// TestTerminate_LivePIDExits: live owner that responds to SIGINT exits
// within timeout, file is reaped.
//
// Goroutine-Wait dance: a subprocess SIGINTed but never reaped stays in
// the kernel as a zombie, and zombies still return success on kill(pid,0)
// — so alive(pid) would stay true until we Wait. We launch a concurrent
// Wait so the zombie clears as soon as `sleep` dies; Terminate's poll
// then sees the PID gone and reaps the state file.
func TestTerminate_LivePIDExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alpha.json")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waitDone
	})

	payload, err := json.Marshal(SessionState{PID: cmd.Process.Pid})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if err := Terminate(dir, "alpha", 5*time.Second); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file not reaped after SIGINT: stat err=%v", err)
	}
}

// TestTerminate_LivePIDSurvivesTimeout: owner ignores SIGINT and Terminate
// returns an error surfacing the PID.
func TestTerminate_LivePIDSurvivesTimeout(t *testing.T) {
	helperPID, kill := spawnSigintIgnoringHelper(t)
	t.Cleanup(kill)

	dir := t.TempDir()
	path := filepath.Join(dir, "alpha.json")
	payload, err := json.Marshal(SessionState{PID: helperPID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	err = Terminate(dir, "alpha", 500*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(helperPID)) {
		t.Fatalf("error did not surface PID %d: %v", helperPID, err)
	}
}

// exitedSubprocessPID returns the PID of a child that ran `/bin/true` and
// has already been reaped. The OS could theoretically recycle that PID
// before the test uses it; in practice the window is microseconds and
// no test environment has enough churn to hit it.
func exitedSubprocessPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

// spawnSigintIgnoringHelper re-execs the test binary into
// TestHelperIgnoresSIGINT. Returns the helper's PID and a cleanup func
// the caller must call (Kill + Wait so no zombies leak past the test).
//
// The helper writes a sentinel file once its signal handler is installed;
// we block on that file so the test can't race past handler setup.
func spawnSigintIgnoringHelper(t *testing.T) (int, func()) {
	t.Helper()
	readyPath := filepath.Join(t.TempDir(), "ready")

	cmd := exec.Command(os.Args[0], "-test.run", "^TestHelperIgnoresSIGINT$")
	cmd.Env = append(os.Environ(),
		"GO_TEST_SIGINT_IGNORE=1",
		"GO_TEST_SIGINT_READY="+readyPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	cleanup := func() {
		_ = cmd.Process.Kill()
		<-waitDone
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return cmd.Process.Pid, cleanup
		}
		time.Sleep(20 * time.Millisecond)
	}
	cleanup()
	t.Fatalf("helper did not become ready within 2s")
	return 0, nil
}

// TestHelperIgnoresSIGINT is a re-exec target, not a real test assertion.
// When the test binary is invoked with GO_TEST_SIGINT_IGNORE=1, it
// installs a SIGINT-ignoring handler, touches the ready file, and blocks.
// The parent test kills it via Process.Kill at cleanup.
func TestHelperIgnoresSIGINT(t *testing.T) {
	if os.Getenv("GO_TEST_SIGINT_IGNORE") == "" {
		t.Skip("re-exec target")
	}
	signal.Ignore(syscall.SIGINT)
	if path := os.Getenv("GO_TEST_SIGINT_READY"); path != "" {
		_ = os.WriteFile(path, []byte("ok"), 0o644)
	}
	// Block well past any reasonable test runtime. Parent SIGKILLs us.
	time.Sleep(60 * time.Second)
}

// TestLoadOrCreateProjectCode_PinsOnFirstCall verifies that the project-code
// is generated once and then read back unchanged on subsequent calls — the
// core pinning invariant. A second call MUST NOT re-derive (i.e. mutating
// the on-disk file mid-test should be reflected by the second call, proving
// the second call read from disk rather than re-deriving).
func TestLoadOrCreateProjectCode_PinsOnFirstCall(t *testing.T) {
	tmp := t.TempDir()

	first, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !isValidProjectCode(first) {
		t.Fatalf("first call returned non-hex: %q", first)
	}

	second, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second != first {
		t.Fatalf("second call mismatch: got %q want %q", second, first)
	}

	// Mutate the pinned file to a different valid code and call again —
	// if the third call returns the mutated value (not a fresh derivation
	// from projectName), the function is reading the pin from disk.
	mutated := strings.Repeat("a", 40)
	if err := os.WriteFile(projectCodePath(tmp), []byte(mutated+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite project-code: %v", err)
	}
	third, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if third != mutated {
		t.Fatalf("third call did not read pin from disk: got %q want %q", third, mutated)
	}
}

// TestLoadOrCreateProjectCode_BackwardCompatSeedsFromHostname verifies the
// backward-compat path: on a fresh dir (no .sesh/project-code), the function
// seeds the file from deriveProjectCode(projectName) so existing projects
// already running with that derived code stay subscribed to the same
// fossil sync subject after this change rolls out.
func TestLoadOrCreateProjectCode_BackwardCompatSeedsFromHostname(t *testing.T) {
	tmp := t.TempDir()

	got, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("loadOrCreate: %v", err)
	}
	want := deriveProjectCode("myproj")
	if got != want {
		t.Fatalf("seed mismatch: got %q want %q", got, want)
	}

	// File should now exist with the same content.
	data, err := os.ReadFile(projectCodePath(tmp))
	if err != nil {
		t.Fatalf("read project-code: %v", err)
	}
	if strings.TrimSpace(string(data)) != want {
		t.Fatalf("file content mismatch: got %q want %q", string(data), want)
	}
}

// TestLoadOrCreateProjectCode_RejectsCorruptedFile verifies that a mangled
// project-code file surfaces a clear error rather than silently overwriting.
// Users with a corrupted file have bigger problems and need to see them.
func TestLoadOrCreateProjectCode_RejectsCorruptedFile(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".sesh"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"short", "abc123"},
		{"long", strings.Repeat("a", 41)},
		{"uppercase", strings.Repeat("A", 40)},
		{"non-hex", strings.Repeat("z", 40)},
		{"garbage", "this is not a project code at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(projectCodePath(tmp), []byte(tc.content), 0o644); err != nil {
				t.Fatalf("seed corrupted file: %v", err)
			}
			_, err := loadOrCreateProjectCode(tmp, "myproj")
			if err == nil {
				t.Fatalf("expected error for corrupted content %q, got nil", tc.content)
			}
			if !strings.Contains(err.Error(), "invalid project-code") {
				t.Fatalf("unexpected error message: %v", err)
			}
		})
	}
}

// TestLoadOrCreateProjectCode_StableAcrossHostnameChange simulates the bug
// scenario from issue #16: a project pinned its code on hostname A, then
// the machine is cloned / renamed to hostname B. The pinned code must
// survive — that's the whole point of the pinning.
//
// os.Hostname() reads from the OS rather than an env var, so we simulate
// the prior-host derivation by writing the file directly with the value
// deriveProjectCodeFromHost would have produced under hostname A, then
// verify loadOrCreateProjectCode returns that exact value regardless of
// the current host.
func TestLoadOrCreateProjectCode_StableAcrossHostnameChange(t *testing.T) {
	tmp := t.TempDir()
	pinned := deriveProjectCodeFromHost("old-hostname-A", "myproj")
	if err := os.MkdirAll(filepath.Join(tmp, ".sesh"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(projectCodePath(tmp), []byte(pinned+"\n"), 0o644); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	// Sanity: the pinned value should differ from the current host's
	// derivation (otherwise the test isn't actually exercising the
	// "different hostname" case). Skip the assertion if by some
	// astronomical coincidence the current host hashes to the same value.
	currentDerived := deriveProjectCode("myproj")
	if pinned == currentDerived {
		t.Skipf("current host happens to match 'old-hostname-A' derivation; test inconclusive")
	}

	got, err := loadOrCreateProjectCode(tmp, "myproj")
	if err != nil {
		t.Fatalf("loadOrCreate: %v", err)
	}
	if got != pinned {
		t.Fatalf("pin did not survive hostname change: got %q want %q (current-host derive: %q)", got, pinned, currentDerived)
	}
}
