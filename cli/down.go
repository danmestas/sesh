package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// DownCmd ends a session by SIGINTing its sesh up process. Reads the PID
// from <cwd>/.sesh/sessions/<label>.json. sesh up's signal handler does
// the actual cleanup (Stop, leaf disconnect, hub auto-shutdown if last).
type DownCmd struct {
	Session string `required:"" help:"Session label to bring down"`

	WaitTimeout time.Duration `help:"How long to wait for sesh up to exit after SIGINT" default:"15s"`
}

func (c *DownCmd) Run() error {
	path, err := sessionStatePath(c.Session)
	if err != nil {
		return err
	}
	state, err := readSessionState(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("session already down (no state file)", "session", c.Session)
			return nil
		}
		return fmt.Errorf("read session state: %w", err)
	}

	proc, err := os.FindProcess(state.PID)
	if err != nil {
		// On unix FindProcess always succeeds; this only fires on weirder
		// platforms. Treat as already-gone.
		_ = os.Remove(path)
		return nil
	}
	if err := proc.Signal(syscall.SIGINT); err != nil {
		// Likely "no such process" — clean up stale state and return success.
		_ = os.Remove(path)
		return nil
	}

	// Wait for sesh up's PID to actually exit. Polling the JSON file isn't
	// reliable — removal happens at the end of sesh up's defer chain, after
	// libfossil's WAL TRUNCATE checkpoint, which can take several seconds
	// under load. Checking the PID directly is more honest.
	deadline := time.Now().Add(c.WaitTimeout)
	for time.Now().Before(deadline) {
		if !alive(state.PID) {
			// Best-effort cleanup of any leftover state file.
			_ = os.Remove(path)
			slog.Info("session down", "session", c.Session, "pid", state.PID)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("session %q (pid %d) didn't exit within %s", c.Session, state.PID, c.WaitTimeout)
}
