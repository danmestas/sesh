package a2a

import "testing"

func TestIsTerminalTaskState(t *testing.T) {
	terminal := []string{
		TaskStateCompleted,
		TaskStateCanceled,
		TaskStateFailed,
		TaskStateRejected,
	}
	for _, s := range terminal {
		if !IsTerminalTaskState(s) {
			t.Errorf("IsTerminalTaskState(%q) = false, want true", s)
		}
	}
	// AUTH_REQUIRED is NOT terminal (a2a-go TaskState.Terminal excludes it).
	nonTerminal := []string{
		TaskStateUnspecified,
		TaskStateAuthRequired,
		TaskStateInputRequired,
		TaskStateSubmitted,
		TaskStateWorking,
		"garbage",
	}
	for _, s := range nonTerminal {
		if IsTerminalTaskState(s) {
			t.Errorf("IsTerminalTaskState(%q) = true, want false", s)
		}
	}
}

func TestIsKnownTaskState(t *testing.T) {
	known := []string{
		TaskStateUnspecified,
		TaskStateAuthRequired,
		TaskStateCanceled,
		TaskStateCompleted,
		TaskStateFailed,
		TaskStateInputRequired,
		TaskStateRejected,
		TaskStateSubmitted,
		TaskStateWorking,
	}
	for _, s := range known {
		if !IsKnownTaskState(s) {
			t.Errorf("IsKnownTaskState(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"GARBAGE", "task_state_submitted", "TASK_STATE_UNKNOWN"} {
		if IsKnownTaskState(s) {
			t.Errorf("IsKnownTaskState(%q) = true, want false", s)
		}
	}
}
