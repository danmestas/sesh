package cli

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startReachable spins up a tiny TCP listener and returns a URL whose host
// resolves to it. Anything that calls reachable() on that URL will see it
// alive until the cleanup runs.
func startReachable(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return "http://" + lis.Addr().String() + "/"
}

func writeURLFile(t *testing.T, stateDir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stateDir, name), []byte(contents+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// Fast path: hub.url published and reachable. AcquireOrReuse returns the
// existing URLs and a non-spawner lease without touching the flock.
func TestAcquireOrReuse_FastPath(t *testing.T) {
	stateDir := t.TempDir()
	primary := startReachable(t)
	natsURL := "nats://127.0.0.1:4222"
	fossilURL := "http://127.0.0.1:9000/"
	writeURLFile(t, stateDir, "hub.url", primary)
	writeURLFile(t, stateDir, "hub.nats.url", natsURL)
	writeURLFile(t, stateDir, "hub.fossil.url", fossilURL)

	urls, lease, err := AcquireOrReuse(stateDir)
	if err != nil {
		t.Fatalf("AcquireOrReuse: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	if lease.IsSpawner() {
		t.Errorf("fast path returned spawner lease; want non-spawner")
	}
	if urls.Primary != primary {
		t.Errorf("Primary = %q, want %q", urls.Primary, primary)
	}
	if urls.NATS != natsURL {
		t.Errorf("NATS = %q, want %q", urls.NATS, natsURL)
	}
	if urls.Fossil != fossilURL {
		t.Errorf("Fossil = %q, want %q", urls.Fossil, fossilURL)
	}

	// Sanity: hub.spawn.lock should not have been created on the fast path.
	if _, err := os.Stat(filepath.Join(stateDir, "hub.spawn.lock")); err == nil {
		t.Errorf("hub.spawn.lock created on fast path; want untouched")
	}
}

// Slow path: no hub running. First caller returns a spawner lease with
// empty URLs and holds the flock. A second concurrent caller blocks on
// the flock until the spawner publishes hub.url and releases.
func TestAcquireOrReuse_SlowPathBlocksUntilRelease(t *testing.T) {
	stateDir := t.TempDir()

	urls1, lease1, err := AcquireOrReuse(stateDir)
	if err != nil {
		t.Fatalf("first AcquireOrReuse: %v", err)
	}
	if !lease1.IsSpawner() {
		t.Fatalf("first lease IsSpawner=false on empty stateDir; want true")
	}
	if urls1.Primary != "" {
		t.Errorf("spawner got non-empty Primary=%q; want empty", urls1.Primary)
	}

	type acquireResult struct {
		urls  HubURLs
		lease *Lease
		err   error
	}
	resCh := make(chan acquireResult, 1)
	go func() {
		u, l, err := AcquireOrReuse(stateDir)
		resCh <- acquireResult{urls: u, lease: l, err: err}
	}()

	select {
	case r := <-resCh:
		t.Fatalf("second AcquireOrReuse returned while first still holds spawner lease: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}

	primary := startReachable(t)
	writeURLFile(t, stateDir, "hub.url", primary)
	if err := lease1.Release(); err != nil {
		t.Fatalf("release lease1: %v", err)
	}

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("second AcquireOrReuse: %v", r.err)
		}
		if r.lease.IsSpawner() {
			t.Errorf("second lease IsSpawner=true after URL was published; want false")
		}
		if r.urls.Primary != primary {
			t.Errorf("second Primary = %q, want %q", r.urls.Primary, primary)
		}
		if err := r.lease.Release(); err != nil {
			t.Errorf("release lease2: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second AcquireOrReuse did not return after spawner released")
	}
}

// N goroutines call AcquireOrReuse concurrently against an initially-empty
// state dir. Exactly one returns a spawner lease (it publishes hub.url
// before releasing, simulating a real spawn). The others fall through to
// non-spawner leases pointing at the published hub.
func TestAcquireOrReuse_ExactlyOneSpawnerAmongRacers(t *testing.T) {
	const n = 8
	stateDir := t.TempDir()
	primary := startReachable(t)

	var spawnerCount atomic.Int64
	var nonSpawnerCount atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			urls, lease, err := AcquireOrReuse(stateDir)
			if err != nil {
				t.Errorf("AcquireOrReuse: %v", err)
				return
			}
			if lease.IsSpawner() {
				spawnerCount.Add(1)
				// Simulate the caller spawning the daemon: publish URL,
				// give racers a moment to observe it, then release.
				writeURLFile(t, stateDir, "hub.url", primary)
				time.Sleep(50 * time.Millisecond)
			} else {
				nonSpawnerCount.Add(1)
				if urls.Primary != primary {
					t.Errorf("non-spawner Primary = %q, want %q", urls.Primary, primary)
				}
			}
			if err := lease.Release(); err != nil {
				t.Errorf("release: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := spawnerCount.Load(); got != 1 {
		t.Errorf("spawner count = %d, want exactly 1", got)
	}
	if got := nonSpawnerCount.Load(); got != n-1 {
		t.Errorf("non-spawner count = %d, want %d", got, n-1)
	}
}

// RegisterDaemon is called by `sesh hub serve` to O_EXCL-claim hub.url. A
// second concurrent daemon must be rejected.
func TestRegisterDaemon_RejectsConcurrentDaemon(t *testing.T) {
	stateDir := t.TempDir()

	lease1, err := RegisterDaemon(stateDir)
	if err != nil {
		t.Fatalf("first RegisterDaemon: %v", err)
	}
	t.Cleanup(func() { _ = lease1.Release() })

	if _, err := RegisterDaemon(stateDir); err == nil {
		t.Fatal("second RegisterDaemon: want error (claim already held), got nil")
	}
}

// A daemon that died without cleaning up hub.url leaves a stale, unreachable
// URL on disk. The next RegisterDaemon should take over.
func TestRegisterDaemon_TakesOverStaleURL(t *testing.T) {
	stateDir := t.TempDir()
	// Write a URL pointing at a port we know is closed.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	writeURLFile(t, stateDir, "hub.url", "http://"+addr+"/")

	lease, err := RegisterDaemon(stateDir)
	if err != nil {
		t.Fatalf("RegisterDaemon over stale URL: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })
}
