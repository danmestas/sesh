package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// leaseKind discriminates the three flavors of HubGuard claim. It is
// internal; callers read it through IsSpawner / Publish / Release.
type leaseKind int

const (
	leaseNone leaseKind = iota
	leaseSpawner
	leaseDaemon
)

// Lease represents a HubGuard claim. Three flavors:
//
//   - leaseSpawner: returned by AcquireOrReuse when this caller is the
//     elected hub-spawner. The flock on hub.spawn.lock is held until
//     Release. The caller is responsible for spawning the hub daemon and
//     calling Release once the published URL is observable.
//   - leaseDaemon: returned by RegisterDaemon. hub.url is held open with
//     O_EXCL; Publish writes the leaf URL into it; Release closes the
//     descriptor and removes the file.
//   - leaseNone: returned by AcquireOrReuse when an existing hub was found
//     (either on the fast path or under the spawn lock after a re-probe).
//     Release is a no-op.
type Lease struct {
	kind    leaseKind
	handle  *os.File
	cleanup func()
	closed  bool
}

// IsSpawner reports whether the caller was elected to spawn the hub.
func (l *Lease) IsSpawner() bool {
	return l != nil && l.kind == leaseSpawner
}

// Release closes any held file descriptor (releasing flock for spawner
// leases) and runs cleanup (removing hub.url for daemon leases). Safe to
// call on a nil or already-released lease.
func (l *Lease) Release() error {
	if l == nil || l.closed {
		return nil
	}
	l.closed = true
	var err error
	if l.handle != nil {
		err = l.handle.Close()
	}
	if l.cleanup != nil {
		l.cleanup()
	}
	return err
}

// Publish writes the hub's leaf URL into the daemon-claimed hub.url slot.
// Valid only on daemon leases (returned by RegisterDaemon). Call exactly
// once per daemon lease, after the hub's listeners are bound.
func (l *Lease) Publish(leafURL string) error {
	if l == nil || l.kind != leaseDaemon || l.handle == nil {
		return errors.New("hubguard: Publish requires a daemon lease")
	}
	if _, err := fmt.Fprintln(l.handle, leafURL); err != nil {
		return fmt.Errorf("write hub.url: %w", err)
	}
	return nil
}

// AcquireOrReuse implements the fast/slow path for the
// exactly-one-hub-per-user bring-up dance. Returns the daemon's leaf URL
// when a hub is already running (so callers can solicit into it) or "" with
// a spawner lease when the caller must spawn the daemon themselves.
//
// Fast path: read hub.url; if its host:port is reachable, return that URL
// and a non-spawner lease without touching the flock.
//
// Slow path: flock hub.spawn.lock, re-probe. If a hub became reachable
// during the wait, release the flock and return its URL with a
// non-spawner lease. Otherwise remove any stale hub.url, return "" and a
// spawner lease (the flock is held inside the lease). The caller is
// responsible for spawning the hub daemon, polling for the daemon's
// published URL, and calling lease.Release() once the URL is observable —
// that's what unblocks concurrent racers waiting on the flock.
//
// Only hub.url participates in the bring-up dance; hub.nats.url and
// hub.fossil.url are surfaced separately via ReadHubInfo when callers
// need them.
//
// The bind-vs-accept race from #15 is closed daemon-side: hub_serve.go
// gates URL publication on EdgeSync's hub.Ready() channel, so consumers
// only see hub.url / hub.fossil.url after the HTTP listener is accepting.
func AcquireOrReuse(stateDir string) (leafURL string, lease *Lease, err error) {
	if url, ok := readPrimaryIfReachable(stateDir); ok {
		return url, &Lease{kind: leaseNone}, nil
	}

	lockPath := filepath.Join(stateDir, "hub.spawn.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "", nil, fmt.Errorf("open hub spawn lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return "", nil, fmt.Errorf("flock hub spawn lock: %w", err)
	}

	if url, ok := readPrimaryIfReachable(stateDir); ok {
		_ = lockFile.Close()
		return url, &Lease{kind: leaseNone}, nil
	}

	_ = os.Remove(filepath.Join(stateDir, "hub.url"))
	return "", &Lease{kind: leaseSpawner, handle: lockFile}, nil
}

// RegisterDaemon claims hub.url for the calling daemon via O_EXCL. The
// returned lease must be released at shutdown to clean up the file path.
// Use lease.Publish(leafURL) once listeners are bound so racing
// AcquireOrReuse callers can see the hub is alive.
//
// Three collision cases distinguished by inspecting any existing file:
//   - URL present and reachable → another hub is running; refuse.
//   - URL present but empty → another hub is mid-boot between O_EXCL
//     claim and Publish; refuse.
//   - URL present but unreachable → previous daemon died without cleanup;
//     remove and re-claim.
func RegisterDaemon(stateDir string) (*Lease, error) {
	urlPath := filepath.Join(stateDir, "hub.url")
	file, err := tryClaim(urlPath)
	if err == nil {
		return daemonLease(file, urlPath), nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("acquire hub.url: %w", err)
	}

	existing, _ := os.ReadFile(urlPath)
	trimmed := stringTrim(existing)
	switch {
	case trimmed == "":
		return nil, errors.New("another sesh hub serve is mid-boot (hub.url present but unwritten)")
	case reachable(trimmed):
		return nil, fmt.Errorf("hub already running at %s", trimmed)
	}

	if err := os.Remove(urlPath); err != nil {
		return nil, fmt.Errorf("remove stale hub.url: %w", err)
	}
	file, err = tryClaim(urlPath)
	if err != nil {
		return nil, fmt.Errorf("acquire hub.url after stale takeover: %w", err)
	}
	return daemonLease(file, urlPath), nil
}

func tryClaim(urlPath string) (*os.File, error) {
	return os.OpenFile(urlPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

func daemonLease(file *os.File, urlPath string) *Lease {
	return &Lease{
		kind:   leaseDaemon,
		handle: file,
		cleanup: func() {
			_ = os.Remove(urlPath)
		},
	}
}

// readPrimaryIfReachable returns the daemon's leaf URL (hub.url) iff the
// file is present, non-empty, and the host:port is currently reachable.
// Drives AcquireOrReuse's fast path.
func readPrimaryIfReachable(stateDir string) (string, bool) {
	url, exists, err := ReadPrimaryURL(stateDir)
	if err != nil || !exists || url == "" {
		return "", false
	}
	if !reachable(url) {
		return "", false
	}
	return url, true
}
