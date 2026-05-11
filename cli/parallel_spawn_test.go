package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestUp_ParallelSpawnNoRace fires N=5 `sesh up` invocations at once across
// 5 project tmpdirs sharing one HOME. Before the flock-based serialization
// fix in ensureHubRunning, each `sesh up` fork-execs its own `sesh hub
// serve`, the hubs clobber each other's hub.url under the empty-file/stale
// branch in hub_serve, contend on shared ~/.sesh fossil + JetStream
// storage, and none come up within the 5s deadline.
//
// With the fix:
//   - The first sesh up to enter the slow path acquires the spawn lock and
//     fork-execs the hub.
//   - The others block on the lock, wake up, see hub.url is reachable, and
//     return immediately. No second hub is ever spawned.
//
// Assertions:
//   - All 5 sesh up processes publish their state JSON with URLs.
//   - Only 1 `sesh hub serve` ever appears across the run (counted via the
//     hub.log startup-line count).
func TestUp_ParallelSpawnNoRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: spawns 5 subprocesses + a hub")
	}

	const n = 5

	bin := buildSesh(t)
	home := t.TempDir()

	type result struct {
		idx     int
		pid     int
		state   stateOnDisk
		spawnEr error
		waitErr error
	}

	cmds := make([]*exec.Cmd, n)
	projects := make([]string, n)
	for i := 0; i < n; i++ {
		projects[i] = t.TempDir()
	}

	t.Cleanup(func() {
		for _, c := range cmds {
			if c == nil || c.Process == nil {
				continue
			}
			if c.ProcessState == nil {
				_ = c.Process.Signal(syscall.SIGKILL)
				_, _ = c.Process.Wait()
			}
		}
	})

	results := make([]result, n)
	var startWG, doneWG sync.WaitGroup
	startGate := make(chan struct{})

	for i := 0; i < n; i++ {
		i := i
		startWG.Add(1)
		doneWG.Add(1)
		go func() {
			defer doneWG.Done()
			cmd := exec.Command(bin, "up", "--session=s"+string(rune('a'+i)))
			cmd.Dir = projects[i]
			cmd.Env = append(os.Environ(), "HOME="+home)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			cmds[i] = cmd
			startWG.Done()
			<-startGate
			results[i].idx = i
			if err := cmd.Start(); err != nil {
				results[i].spawnEr = err
				return
			}
			results[i].pid = cmd.Process.Pid
		}()
	}
	startWG.Wait()
	close(startGate)
	doneWG.Wait()

	for i := 0; i < n; i++ {
		if results[i].spawnEr != nil {
			t.Fatalf("sesh up #%d start failed: %v", i, results[i].spawnEr)
		}
	}

	// All 5 must publish URLs within 15s (covers worst-case JetStream replay
	// + flock-serialized boot of the one real hub).
	deadline := 20 * time.Second
	for i := 0; i < n; i++ {
		state := waitForURLs(t,
			filepath.Join(projects[i], ".sesh", "sessions", "s"+string(rune('a'+i))+".json"),
			deadline,
		)
		results[i].state = state
		if state.PID != results[i].pid {
			t.Errorf("sesh #%d state PID = %d, want %d", i, state.PID, results[i].pid)
		}
	}

	// Critical: only one hub should have started. hub.log appends one
	// startup line per hub.NewHub call; with the fix, exactly 1.
	hubLog := filepath.Join(home, ".sesh", "hub.log")
	hubStarts := countMatches(t, hubLog, "sesh hub running")
	if hubStarts != 1 {
		t.Errorf("expected exactly 1 hub startup in %s, got %d", hubLog, hubStarts)
	}

	// Tear down all 5 sesh ups; verify state files are reaped.
	for i := 0; i < n; i++ {
		if err := cmds[i].Process.Signal(syscall.SIGINT); err != nil {
			t.Logf("SIGINT sesh #%d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := cmds[i].Process.Wait(); err != nil {
			t.Logf("sesh #%d exit: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		sp := filepath.Join(projects[i], ".sesh", "sessions", "s"+string(rune('a'+i))+".json")
		if _, err := os.Stat(sp); !os.IsNotExist(err) {
			t.Errorf("state file lingered for sesh #%d: %v", i, err)
		}
	}
}

func countMatches(t *testing.T, path, needle string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	count := 0
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == needle {
			count++
		}
	}
	return count
}

