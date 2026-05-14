package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// HubURLs is the published-state surface of a running hub:
//   - Primary: the hub's leaf-NATS URL (~/.sesh/hub.url), the URL `sesh up`
//     solicits its session's leaf into.
//   - NATS:    the hub's NATS client URL (~/.sesh/hub.nats.url), the entry
//     point for hub-scoped JetStream operations.
//   - Fossil:  the hub's Fossil HTTP xfer endpoint (~/.sesh/hub.fossil.url),
//     read by `sesh up` to decide bootstrap path (clone-from-hub vs.
//     seed-from-cwd).
//
// NATS and Fossil are best-effort: when the daemon is mid-boot and has
// published Primary only, those fields come back empty rather than blocking.
type HubURLs struct {
	Primary string
	NATS    string
	Fossil  string
}

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
// exactly-one-hub-per-user bring-up dance.
//
// Fast path: read hub.url; if its host:port is reachable, return its URLs
// and a non-spawner lease without touching the flock.
//
// Slow path: flock hub.spawn.lock, re-probe. If a hub became reachable
// during the wait, release the flock and return its URLs with a
// non-spawner lease. Otherwise remove any stale hub.url, return empty
// URLs and a spawner lease (the flock is held inside the lease). The
// caller is responsible for spawning the hub daemon, polling for the
// daemon's published URL, and calling lease.Release() once the URL is
// observable — that's what unblocks concurrent racers waiting on the flock.
//
// The bind-vs-accept race from #15 is closed daemon-side: hub_serve.go
// gates URL publication on EdgeSync's hub.Ready() channel, so consumers
// only see hub.url / hub.fossil.url after the HTTP listener is accepting.
func AcquireOrReuse(stateDir string) (HubURLs, *Lease, error) {
	urlPath := filepath.Join(stateDir, "hub.url")

	if urls, ok := readURLsIfReachable(stateDir, urlPath); ok {
		return urls, &Lease{kind: leaseNone}, nil
	}

	lockPath := filepath.Join(stateDir, "hub.spawn.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return HubURLs{}, nil, fmt.Errorf("open hub spawn lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return HubURLs{}, nil, fmt.Errorf("flock hub spawn lock: %w", err)
	}

	if urls, ok := readURLsIfReachable(stateDir, urlPath); ok {
		_ = lockFile.Close()
		return urls, &Lease{kind: leaseNone}, nil
	}

	_ = os.Remove(urlPath)
	return HubURLs{}, &Lease{kind: leaseSpawner, handle: lockFile}, nil
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

// readURLsIfReachable returns the published URL set if hub.url is present
// and its host:port is currently reachable. NATS / Fossil URLs are
// best-effort: missing files just leave those fields empty.
func readURLsIfReachable(stateDir, urlPath string) (HubURLs, bool) {
	data, err := os.ReadFile(urlPath)
	if err != nil {
		return HubURLs{}, false
	}
	primary := stringTrim(data)
	if primary == "" || !reachable(primary) {
		return HubURLs{}, false
	}
	natsURL, _ := readTrimmed(filepath.Join(stateDir, "hub.nats.url"))
	fossilURL, _ := readTrimmed(filepath.Join(stateDir, "hub.fossil.url"))
	return HubURLs{Primary: primary, NATS: natsURL, Fossil: fossilURL}, true
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return stringTrim(data), nil
}
