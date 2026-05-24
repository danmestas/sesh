package a2a

// TaskState wire constants. Mirror a2a-go/v2 a2a.TaskState
// (a2a/core.go:283-298). Centralized here so cancel.go, list.go, and
// any future status-aware handler reference one source of truth.
const (
	TaskStateUnspecified   = ""
	TaskStateAuthRequired  = "TASK_STATE_AUTH_REQUIRED"
	TaskStateCanceled      = "TASK_STATE_CANCELED"
	TaskStateCompleted     = "TASK_STATE_COMPLETED"
	TaskStateFailed        = "TASK_STATE_FAILED"
	TaskStateInputRequired = "TASK_STATE_INPUT_REQUIRED"
	TaskStateRejected      = "TASK_STATE_REJECTED"
	TaskStateSubmitted     = "TASK_STATE_SUBMITTED"
	TaskStateWorking       = "TASK_STATE_WORKING"
)

// knownTaskStates indexes the eight named + the unspecified sentinel.
// Used by IsKnownTaskState for ListTasks status-filter validation.
var knownTaskStates = map[string]struct{}{
	TaskStateUnspecified:   {},
	TaskStateAuthRequired:  {},
	TaskStateCanceled:      {},
	TaskStateCompleted:     {},
	TaskStateFailed:        {},
	TaskStateInputRequired: {},
	TaskStateRejected:      {},
	TaskStateSubmitted:     {},
	TaskStateWorking:       {},
}

// terminalTaskStates mirrors a2a-go/v2 TaskState.Terminal() (a2a/core.go:332-337):
// Completed, Canceled, Failed, Rejected. AUTH_REQUIRED is recoverable
// (the client resolves auth and resumes) and is intentionally absent.
var terminalTaskStates = map[string]struct{}{
	TaskStateCompleted: {},
	TaskStateCanceled:  {},
	TaskStateFailed:    {},
	TaskStateRejected:  {},
}

// IsTerminalTaskState reports whether s is one of the four A2A
// terminal states. Empty string returns false (treat unspecified as
// non-terminal so CancelTask can re-encode and CAS-write).
func IsTerminalTaskState(s string) bool {
	_, ok := terminalTaskStates[s]
	return ok
}

// IsKnownTaskState reports whether s is one of the nine A2A-spec-defined
// TaskState wire values (eight named plus "" for unspecified). Used by
// ListTasks status-filter validation.
func IsKnownTaskState(s string) bool {
	_, ok := knownTaskStates[s]
	return ok
}
