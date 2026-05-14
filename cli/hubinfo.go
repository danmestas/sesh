package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// HubInfo is the published state of a running hub on disk. Each non-empty
// field maps to one file under stateDir:
//
//   - PrimaryURL → stateDir/hub.url         (hub's leafnode URL — what
//     `sesh up` solicits its session's leaf into)
//   - NATSURL    → stateDir/hub.nats.url    (hub's NATS client URL — entry
//     point for hub-scoped JetStream)
//   - FossilURL  → stateDir/hub.fossil.url  (hub's Fossil HTTP xfer
//     endpoint — read by `sesh up` to decide clone-from-hub vs.
//     seed-from-cwd)
//
// Missing files on read become empty strings without an error: partial
// publication is an explicit state, not a failure.
//
// hub.url is the daemon's atomic ownership claim (O_EXCL via HubGuard's
// RegisterDaemon → Publish path). WriteHubInfo writes the other two via
// temp-then-rename so concurrent readers never see a half-written URL.
// Adding a new published URL is a one-line struct change plus a new
// case in WriteHubInfo and ReadHubInfo.
type HubInfo struct {
	PrimaryURL string
	NATSURL    string
	FossilURL  string
}

// WriteHubInfo atomically publishes hub.nats.url and hub.fossil.url under
// stateDir via writeAtomic. Empty fields are skipped — the underlying
// file is not touched at all, so callers can publish a partial set
// without disturbing the rest.
//
// PrimaryURL is intentionally a no-op here: hub.url is owned by HubGuard's
// daemon lease (RegisterDaemon → Publish) which holds the file open
// O_EXCL for the daemon's lifetime. Passing a non-empty PrimaryURL is
// allowed (and ignored) so callers can populate the full struct without
// branching.
func WriteHubInfo(stateDir string, info HubInfo) error {
	if info.NATSURL != "" {
		path := filepath.Join(stateDir, "hub.nats.url")
		if err := writeAtomic(path, info.NATSURL+"\n"); err != nil {
			return fmt.Errorf("write hub.nats.url: %w", err)
		}
	}
	if info.FossilURL != "" {
		path := filepath.Join(stateDir, "hub.fossil.url")
		if err := writeAtomic(path, info.FossilURL+"\n"); err != nil {
			return fmt.Errorf("write hub.fossil.url: %w", err)
		}
	}
	return nil
}

// ReadHubInfo returns the currently-published HubInfo for stateDir. The
// exists return is true iff hub.url is present, which is the canonical
// "a daemon has at least claimed the primary slot" signal — partial
// publication (hub.url present but nats/fossil not yet written) reports
// exists=true with the unfilled fields zero. A completely empty stateDir
// returns ({}, false, nil), not an error.
//
// Errors are reserved for unexpected I/O — a present-but-unreadable
// file, for instance.
func ReadHubInfo(stateDir string) (HubInfo, bool, error) {
	primary, primaryExists, err := readURLFile(filepath.Join(stateDir, "hub.url"))
	if err != nil {
		return HubInfo{}, false, err
	}
	natsURL, _, err := readURLFile(filepath.Join(stateDir, "hub.nats.url"))
	if err != nil {
		return HubInfo{}, false, err
	}
	fossilURL, _, err := readURLFile(filepath.Join(stateDir, "hub.fossil.url"))
	if err != nil {
		return HubInfo{}, false, err
	}
	return HubInfo{
		PrimaryURL: primary,
		NATSURL:    natsURL,
		FossilURL:  fossilURL,
	}, primaryExists, nil
}

// ClearHubInfo removes hub.url, hub.nats.url, and hub.fossil.url. ENOENT
// is swallowed so callers can defer Clear unconditionally without caring
// which subset of files actually got published before shutdown. Only
// throwaway tier-3 hub state is touched here — JetStream (~/.sesh/
// messaging/) and the Fossil hub repo (~/.sesh/hub.repo*) are never
// removed by this function.
func ClearHubInfo(stateDir string) error {
	paths := []string{
		filepath.Join(stateDir, "hub.url"),
		filepath.Join(stateDir, "hub.nats.url"),
		filepath.Join(stateDir, "hub.fossil.url"),
	}
	var firstErr error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ProjectCode reads the project-code from the hub's on-disk Fossil repo
// at stateDir/hub.repo via libfossil's SQLite schema. Returns ("", nil)
// for any "no canonical project-code to adopt" case: hub.repo absent,
// or hub.repo present but with zero check-ins. The zero-check-ins guard
// mirrors the prior ProbeHub behavior — a hub that bound and crashed
// before its first commit has nothing for a peer session to adopt.
//
// SQLite WAL mode lets this read coexist with the hub's open writer;
// the read-only pragma eliminates contention. Schema is libfossil's:
// event.type='ci' marks a check-in, and project-code lives in the
// config table.
func ProjectCode(stateDir string) (string, error) {
	repoPath := filepath.Join(stateDir, "hub.repo")
	if _, err := os.Stat(repoPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("stat hub repo: %w", err)
	}

	db, err := sql.Open("sqlite", "file:"+repoPath+"?_pragma=mode(ro)")
	if err != nil {
		return "", fmt.Errorf("open hub repo: %w", err)
	}
	defer db.Close()

	var commits int
	if err := db.QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&commits); err != nil {
		return "", fmt.Errorf("count check-ins: %w", err)
	}
	if commits == 0 {
		return "", nil
	}

	var code string
	if err := db.QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&code); err != nil {
		return "", fmt.Errorf("read project-code: %w", err)
	}
	return code, nil
}

// readURLFile reads and trims one of the hub's published URL files.
// Distinguishes "file absent" from "file present (possibly empty)" via
// the exists return so ReadHubInfo can report partial publication
// accurately — daemon mid-boot leaves hub.url present-but-empty between
// the O_EXCL claim and Publish.
func readURLFile(path string) (content string, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), true, nil
}
