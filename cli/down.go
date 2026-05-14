package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"
)

// DownCmd ends a session by SIGINTing its `sesh up` process. The owning
// PID lives in <cwd>/.sesh/sessions/<label>.json. sesh up's own signal
// handler performs the cleanup (Stop, leaf disconnect, hub auto-shutdown
// if last).
type DownCmd struct {
	Session string `required:"" help:"Session label to bring down"`

	WaitTimeout time.Duration `help:"How long to wait for sesh up to exit after SIGINT" default:"15s"`
}

func (c *DownCmd) Run() error {
	stateDir, err := projectStateDir()
	if err != nil {
		return err
	}
	state, readErr := ReadSession(stateDir, c.Session)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return fmt.Errorf("read session state: %w", readErr)
	}
	if errors.Is(readErr, fs.ErrNotExist) {
		slog.Info("session already down (no state file)", "session", c.Session)
		return nil
	}
	if err := Terminate(stateDir, c.Session, c.WaitTimeout); err != nil {
		return err
	}
	slog.Info("session down", "session", c.Session, "pid", state.PID)
	return nil
}
