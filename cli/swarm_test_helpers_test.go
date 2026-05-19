package cli_test

import (
	"os"
	"path/filepath"
	"time"
)

// Shared helpers for the swarm integration tests
// (swarm_tbd_integration_test.go and swarm_tbd_conflict_test.go). Promoted
// here from swarm_tbd_integration_test.go once a second consumer arrived,
// per the helper-promotion gate in #71.

// awaitPeerSees pumps libfossil.Update on the peer's checkout and polls
// for the named file to appear with the expected contents. Returns true
// on success, false on timeout. The caller decides how to fail the test
// — most callers t.Fatalf so the failure message is specific to the
// missed direction.
func awaitPeerSees(peerCheckout, repoPath, name, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Best-effort update; transient SQLite contention is fine — we
		// retry on the next loop tick.
		_ = updateViaLibfossil(repoPath, peerCheckout)
		if got, err := os.ReadFile(filepath.Join(peerCheckout, name)); err == nil && string(got) == want {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// samePath resolves both inputs through EvalSymlinks and reports whether
// they refer to the same on-disk path. Used by the swarm tests to
// confirm `sesh worker-cwd` and `sesh worktree` agree on the checkout
// location.
func samePath(a, b string) (bool, error) {
	ar, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false, err
	}
	br, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false, err
	}
	return ar == br, nil
}
