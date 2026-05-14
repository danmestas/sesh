package cli

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Round-trip: write a HubInfo, seed hub.url separately (simulating
// HubGuard's Publish step), read back and confirm every field.
func TestHubInfo_RoundTrip(t *testing.T) {
	stateDir := t.TempDir()

	in := HubInfo{
		NATSURL:   "nats://127.0.0.1:4222",
		FossilURL: "http://127.0.0.1:9000/",
	}
	if err := WriteHubInfo(stateDir, in); err != nil {
		t.Fatalf("WriteHubInfo: %v", err)
	}

	primary := "nats://hub:7422"
	if err := os.WriteFile(filepath.Join(stateDir, "hub.url"), []byte(primary+"\n"), 0o644); err != nil {
		t.Fatalf("seed hub.url: %v", err)
	}

	got, exists, err := ReadHubInfo(stateDir)
	if err != nil {
		t.Fatalf("ReadHubInfo: %v", err)
	}
	if !exists {
		t.Errorf("exists=false; want true when hub.url present")
	}
	if got.PrimaryURL != primary {
		t.Errorf("PrimaryURL = %q, want %q", got.PrimaryURL, primary)
	}
	if got.NATSURL != in.NATSURL {
		t.Errorf("NATSURL = %q, want %q", got.NATSURL, in.NATSURL)
	}
	if got.FossilURL != in.FossilURL {
		t.Errorf("FossilURL = %q, want %q", got.FossilURL, in.FossilURL)
	}
}

// Empty stateDir: ReadHubInfo returns zero info, exists=false, no error.
func TestReadHubInfo_EmptyDir(t *testing.T) {
	stateDir := t.TempDir()
	info, exists, err := ReadHubInfo(stateDir)
	if err != nil {
		t.Fatalf("ReadHubInfo on empty dir: %v", err)
	}
	if exists {
		t.Errorf("exists=true on empty dir; want false")
	}
	if info != (HubInfo{}) {
		t.Errorf("got %+v, want zero HubInfo", info)
	}
}

// Partial publication: only hub.url present (HubGuard claimed but the
// daemon died before writing nats/fossil). ReadHubInfo returns the
// primary with the other fields empty and exists=true.
func TestReadHubInfo_OnlyPrimaryPublished(t *testing.T) {
	stateDir := t.TempDir()
	primary := "nats://hub:7422"
	if err := os.WriteFile(filepath.Join(stateDir, "hub.url"), []byte(primary+"\n"), 0o644); err != nil {
		t.Fatalf("seed hub.url: %v", err)
	}

	info, exists, err := ReadHubInfo(stateDir)
	if err != nil {
		t.Fatalf("ReadHubInfo: %v", err)
	}
	if !exists {
		t.Errorf("exists=false; want true")
	}
	if info.PrimaryURL != primary {
		t.Errorf("PrimaryURL = %q, want %q", info.PrimaryURL, primary)
	}
	if info.NATSURL != "" {
		t.Errorf("NATSURL = %q, want empty (file absent)", info.NATSURL)
	}
	if info.FossilURL != "" {
		t.Errorf("FossilURL = %q, want empty (file absent)", info.FossilURL)
	}
}

// hub.url absent but nats/fossil present (impossible in practice but
// well-defined): exists=false because hub.url is the canonical "daemon
// claimed" signal; nats/fossil fields still populate.
func TestReadHubInfo_PrimaryMissing(t *testing.T) {
	stateDir := t.TempDir()
	if err := WriteHubInfo(stateDir, HubInfo{NATSURL: "nats://x", FossilURL: "http://y"}); err != nil {
		t.Fatalf("WriteHubInfo: %v", err)
	}
	info, exists, err := ReadHubInfo(stateDir)
	if err != nil {
		t.Fatalf("ReadHubInfo: %v", err)
	}
	if exists {
		t.Errorf("exists=true with hub.url absent; want false")
	}
	if info.NATSURL != "nats://x" || info.FossilURL != "http://y" {
		t.Errorf("partial fields not surfaced: %+v", info)
	}
}

// WriteHubInfo with empty fields touches no files. Lets callers publish
// only the URLs they have without disturbing previously-written ones.
func TestWriteHubInfo_EmptyFieldsSkip(t *testing.T) {
	stateDir := t.TempDir()
	if err := WriteHubInfo(stateDir, HubInfo{}); err != nil {
		t.Fatalf("WriteHubInfo: %v", err)
	}
	for _, name := range []string{"hub.url", "hub.nats.url", "hub.fossil.url"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("WriteHubInfo with zero info created %s (err=%v)", name, err)
		}
	}
}

// WriteHubInfo ignores PrimaryURL — that file is owned by HubGuard's
// daemon lease and must not be written through this path. Test asserts
// the field is silently dropped rather than racing the O_EXCL claim.
func TestWriteHubInfo_IgnoresPrimaryURL(t *testing.T) {
	stateDir := t.TempDir()
	if err := WriteHubInfo(stateDir, HubInfo{PrimaryURL: "nats://leak"}); err != nil {
		t.Fatalf("WriteHubInfo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "hub.url")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("hub.url created via WriteHubInfo; should only be written by HubGuard.Publish")
	}
}

// ClearHubInfo removes all three URL files; idempotent on a clean dir.
func TestClearHubInfo_RemovesAll(t *testing.T) {
	stateDir := t.TempDir()
	files := []string{"hub.url", "hub.nats.url", "hub.fossil.url"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(stateDir, f), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", f, err)
		}
	}
	if err := ClearHubInfo(stateDir); err != nil {
		t.Fatalf("ClearHubInfo: %v", err)
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(stateDir, f)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s present after Clear (err=%v)", f, err)
		}
	}
	if err := ClearHubInfo(stateDir); err != nil {
		t.Errorf("second ClearHubInfo on clean dir: %v", err)
	}
}

// Atomicity: a writer cycles NATSURL between two values while readers
// hammer ReadHubInfo. A reader must never see a partial URL — only
// the empty string (race with absent file) or one of the two written
// values. writeAtomic uses rename, so torn reads are impossible on
// POSIX; this test enforces the contract.
func TestWriteHubInfo_AtomicConcurrent(t *testing.T) {
	stateDir := t.TempDir()
	valA := "nats://aaaaaaaa:4222"
	valB := "nats://bbbbbbbb:4222"

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = WriteHubInfo(stateDir, HubInfo{NATSURL: valA})
			_ = WriteHubInfo(stateDir, HubInfo{NATSURL: valB})
		}
	}()

	deadline := time.Now().Add(150 * time.Millisecond)
	reads := 0
	for time.Now().Before(deadline) {
		info, _, err := ReadHubInfo(stateDir)
		if err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("ReadHubInfo: %v", err)
		}
		switch info.NATSURL {
		case "", valA, valB:
		default:
			stop.Store(true)
			wg.Wait()
			t.Fatalf("torn NATSURL read: %q", info.NATSURL)
		}
		reads++
	}
	stop.Store(true)
	wg.Wait()
	if reads < 10 {
		t.Errorf("only %d reads completed — test may be too short to exercise the race", reads)
	}
}

// ProjectCode returns ("", nil) when stateDir has no hub.repo at all.
func TestProjectCode_NoHubRepo(t *testing.T) {
	stateDir := t.TempDir()
	code, err := ProjectCode(stateDir)
	if err != nil {
		t.Fatalf("ProjectCode: %v", err)
	}
	if code != "" {
		t.Errorf("ProjectCode = %q on empty dir, want empty", code)
	}
}

// ProjectCode reads from the libfossil config table when a hub.repo with
// at least one check-in exists. Schema seeded manually to avoid pulling
// in a real EdgeSync hub bring-up just for one read.
func TestProjectCode_SeededRepo(t *testing.T) {
	stateDir := t.TempDir()
	repoPath := filepath.Join(stateDir, "hub.repo")
	want := "abc123abc123abc123abc123abc123abc123abc1"

	db, err := sql.Open("sqlite", "file:"+repoPath)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE config(name TEXT UNIQUE, value, mtime INTEGER)`); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE event(type TEXT, mtime DATETIME, objid INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create event: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO config(name, value, mtime) VALUES('project-code', ?, 0)`, want); err != nil {
		t.Fatalf("seed project-code: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO event(type, mtime, objid) VALUES('ci', 0, 1)`); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded db: %v", err)
	}

	got, err := ProjectCode(stateDir)
	if err != nil {
		t.Fatalf("ProjectCode: %v", err)
	}
	if got != want {
		t.Errorf("ProjectCode = %q, want %q", got, want)
	}
}

// hub.repo with zero check-ins → empty code, no error. Mirrors the prior
// ProbeHub behavior so callers treating "" as "no canonical content"
// keep working.
func TestProjectCode_EmptyRepo(t *testing.T) {
	stateDir := t.TempDir()
	repoPath := filepath.Join(stateDir, "hub.repo")

	db, err := sql.Open("sqlite", "file:"+repoPath)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE config(name TEXT UNIQUE, value, mtime INTEGER)`); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE event(type TEXT, mtime DATETIME, objid INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create event: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO config(name, value, mtime) VALUES('project-code', 'unused', 0)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := ProjectCode(stateDir)
	if err != nil {
		t.Fatalf("ProjectCode: %v", err)
	}
	if got != "" {
		t.Errorf("ProjectCode = %q on repo with 0 check-ins, want empty", got)
	}
}
