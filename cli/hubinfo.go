package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// HubInfo is the URL-publication channel of the running hub's on-disk
// state — the URLs peer sessions need for hub-scoped JetStream and
// Fossil HTTP xfer. Each non-empty field maps to one file under stateDir:
//
//   - NATSURL    → stateDir/hub.nats.url    (hub's NATS client URL — entry
//     point for hub-scoped JetStream)
//   - FossilURL  → stateDir/hub.fossil.url  (hub's Fossil HTTP xfer
//     endpoint — read by `sesh up` to decide clone-from-hub vs.
//     seed-from-cwd)
//
// Missing files on read become empty strings without an error: partial
// publication is an explicit state, not a failure.
//
// stateDir/hub.url (the daemon's leafnode URL) is owned by the parallel
// ownership channel: HubGuard's daemon lease holds it O_EXCL for the
// daemon's lifetime and writes the URL via Lease.Publish. Read it via
// ReadPrimaryURL. WriteHubInfo / ReadHubInfo never touch hub.url —
// keeping the two channels separate means callers can't accidentally
// race the O_EXCL claim by passing the wrong field.
//
// Adding a new published URL is a one-line struct change plus a new
// case in WriteHubInfo and ReadHubInfo.
type HubInfo struct {
	NATSURL   string
	FossilURL string
}

// WriteHubInfo atomically publishes hub.nats.url and hub.fossil.url under
// stateDir via writeAtomic. Empty fields are skipped — the underlying
// file is not touched at all, so callers can publish a partial set
// without disturbing the rest.
//
// hub.url is not in scope; see ReadPrimaryURL / Lease.Publish.
func WriteHubInfo(stateDir string, info HubInfo) error {
	if info.NATSURL != "" {
		if err := writeAtomic(hubNATSURLPath(stateDir), info.NATSURL+"\n"); err != nil {
			return fmt.Errorf("write hub.nats.url: %w", err)
		}
	}
	if info.FossilURL != "" {
		if err := writeAtomic(hubFossilURLPath(stateDir), info.FossilURL+"\n"); err != nil {
			return fmt.Errorf("write hub.fossil.url: %w", err)
		}
	}
	return nil
}

// ReadHubInfo returns the currently-published hub.nats.url and
// hub.fossil.url under stateDir. Missing files are reported as empty
// strings, not errors — partial publication is an explicit state.
//
// To check whether a hub daemon has claimed stateDir, use
// ReadPrimaryURL: hub.url is the canonical "a daemon has claimed the
// slot" signal and lives in the parallel ownership channel.
//
// Errors are reserved for unexpected I/O — a present-but-unreadable
// file, for instance.
func ReadHubInfo(stateDir string) (HubInfo, error) {
	natsURL, _, err := readURLFile(hubNATSURLPath(stateDir))
	if err != nil {
		return HubInfo{}, err
	}
	fossilURL, _, err := readURLFile(hubFossilURLPath(stateDir))
	if err != nil {
		return HubInfo{}, err
	}
	return HubInfo{
		NATSURL:   natsURL,
		FossilURL: fossilURL,
	}, nil
}

// ReadPrimaryURL returns the daemon's published leaf URL (hub.url) plus
// a present flag. ("", false, nil) means no daemon has claimed stateDir;
// (url, true, nil) means a daemon is alive and has published its URL;
// ("", true, nil) means a daemon is mid-boot — it holds the O_EXCL claim
// on hub.url but has not yet called Lease.Publish to write the contents.
//
// hub.url is the canonical "a daemon has claimed the slot" signal.
// WriteHubInfo never touches it; HubGuard's daemon lease owns the
// claim/publish/release cycle.
func ReadPrimaryURL(stateDir string) (url string, exists bool, err error) {
	return readURLFile(hubURLPath(stateDir))
}

// ClearHubInfo removes hub.nats.url and hub.fossil.url — the two files
// WriteHubInfo manages. ENOENT is swallowed so callers can defer Clear
// unconditionally without caring which subset got published before
// shutdown. Only throwaway tier-3 hub state is touched here —
// JetStream (~/.sesh/messaging/) and the Fossil hub repo
// (~/.sesh/hub.repo*) are never removed by this function.
//
// hub.url is not in scope: it belongs to the daemon's lease and is
// removed by Lease.Release when the daemon exits.
func ClearHubInfo(stateDir string) error {
	paths := []string{
		hubNATSURLPath(stateDir),
		hubFossilURLPath(stateDir),
	}
	var firstErr error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReadHubProjectCode reads the project-code from the hub's on-disk Fossil
// repo at stateDir/hub.repo via libfossil's SQLite schema. Returns ("", nil)
// for any "no canonical project-code to adopt" case: hub.repo absent,
// or hub.repo present but with zero check-ins. The zero-check-ins guard
// mirrors the prior ProbeHub behavior — a hub that bound and crashed
// before its first commit has nothing for a peer session to adopt.
//
// SQLite WAL mode lets this read coexist with the hub's open writer;
// the read-only pragma eliminates contention. Schema is libfossil's:
// event.type='ci' marks a check-in, and project-code lives in the
// config table.
//
// Named with a Read* prefix to match ReadHubInfo and disambiguate from
// the ProjectCode field on hub.Config and the HubProjectCode struct
// fields on World / Deps.
func ReadHubProjectCode(stateDir string) (string, error) {
	repoPath := hubRepoPath(stateDir)
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
