package coord

import "testing"

// TestProjectTaskSubject reproduces the proposal's example from
// "Project-level tasking (exclude spies automatically)".
func TestProjectTaskSubject(t *testing.T) {
	got := ProjectTaskSubject("_local", "a3f2c1d8", "workers", "implementer")
	want := "sesh.task._local.project.a3f2c1d8.workers.implementer"
	if got.String() != want {
		t.Errorf("ProjectTaskSubject = %q, want %q", got.String(), want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("ProjectTaskSubject did not validate: %v", err)
	}
	if got.QueueGroup() != "implementer" {
		t.Errorf("QueueGroup = %q, want implementer", got.QueueGroup())
	}
}

// TestWorkflowBlackboardSubject reproduces the proposal's example
// from "Blackboard / shared findings".
func TestWorkflowBlackboardSubject(t *testing.T) {
	got := WorkflowBlackboardSubject("_local", "a1b2c3d4", "findings", "research")
	want := "sesh.blackboard._local.workflow.a1b2c3d4.findings.research"
	if got.String() != want {
		t.Errorf("WorkflowBlackboardSubject = %q, want %q", got.String(), want)
	}
	if got.QueueGroup() != "" {
		t.Errorf("QueueGroup = %q, want empty (blackboard is fan-out)", got.QueueGroup())
	}
}

// TestProjectTaskFilter_AllRoles reproduces the proposal's
// "Only worker traffic in the project" example, scoped to local-host.
func TestProjectTaskFilter_AllRoles(t *testing.T) {
	got := ProjectTaskFilter("_local", "a3f2c1d8", "workers", WildOne)
	want := "sesh.task._local.project.a3f2c1d8.workers.*"
	if got.String() != want {
		t.Errorf("ProjectTaskFilter = %q, want %q", got.String(), want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("ProjectTaskFilter did not validate: %v", err)
	}
}

// TestProjectTaskFilter_AnyHost reproduces the proposal's multi-host
// "Same role, all hosts" example.
func TestProjectTaskFilter_AnyHost(t *testing.T) {
	got := ProjectTaskFilter(WildOne, "a3f2c1d8", "workers", "implementer")
	want := "sesh.task.*.project.a3f2c1d8.workers.implementer"
	if got.String() != want {
		t.Errorf("ProjectTaskFilter = %q, want %q", got.String(), want)
	}
}

// TestProjectBlackboardFilter_All reproduces the proposal's
// "All blackboard updates for a project" example.
func TestProjectBlackboardFilter_All(t *testing.T) {
	got := ProjectBlackboardFilter(WildOne, "a3f2c1d8")
	want := "sesh.blackboard.*.project.a3f2c1d8.>"
	if got.String() != want {
		t.Errorf("ProjectBlackboardFilter = %q, want %q", got.String(), want)
	}
}

// TestObserverFilter_NoTaskingTraffic verifies the spy-exclusion contract:
// observers subscribe to report.* and blackboard.* under a project, never
// task.* or control.*. This is the load-bearing assertion behind the
// "exclude spies automatically" pattern.
func TestObserverFilter_NoTaskingTraffic(t *testing.T) {
	filters := ObserverFilters(WildOne, "a3f2c1d8")
	if len(filters) == 0 {
		t.Fatalf("ObserverFilters returned 0 filters, want ≥1")
	}
	for _, f := range filters {
		got := f.String()
		if contains(got, ".task.") || contains(got, ".control.") {
			t.Errorf("ObserverFilter %q subscribes to task/control traffic — violates spy-exclusion contract", got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
