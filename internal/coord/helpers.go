package coord

// ProjectTaskSubject constructs the publish subject for a task targeting
// the given role within a project on a specific machine.
//
//	sesh.task.<machine>.project.<projectID>.<target>.<role>
//
// Use coord.Machine() to resolve the local machine identity. For
// single-host deployments, MachineLocal is the default.
//
// Example:
//
//	subj := coord.ProjectTaskSubject(coord.Machine(), pid, "workers", "implementer")
//	nc.Publish(subj.String(), payload)
//
// QueueGroup() on the returned Subject is "implementer" (work-stealing).
func ProjectTaskSubject(machine, projectID, target, role string) Subject {
	return Subject{
		Verb:    VerbTask,
		Machine: machine,
		Scope:   ScopeProject,
		ScopeID: projectID,
		Target:  target,
		Role:    role,
	}
}

// WorkflowBlackboardSubject constructs the publish subject for a
// blackboard update inside a workflow.
//
//	sesh.blackboard.<machine>.workflow.<workflowID>.<target>.<role>
//
// QueueGroup() returns "" (fan-out — all watchers see every update).
func WorkflowBlackboardSubject(machine, workflowID, target, role string) Subject {
	return Subject{
		Verb:    VerbBlackboard,
		Machine: machine,
		Scope:   ScopeWorkflow,
		ScopeID: workflowID,
		Target:  target,
		Role:    role,
	}
}

// ProjectTaskFilter constructs a subscribe filter for task traffic
// inside a project. Each argument may be WildOne to wildcard a single
// segment; pass WildOne for `machine` to subscribe across hosts.
//
// Examples:
//
//	// One implementer on this host:
//	coord.ProjectTaskFilter(coord.MachineLocal, pid, "workers", "implementer")
//	// → sesh.task._local.project.<pid>.workers.implementer
//
//	// Any host, any role under workers:
//	coord.ProjectTaskFilter(coord.WildOne, pid, "workers", coord.WildOne)
//	// → sesh.task.*.project.<pid>.workers.*
//
//	// All task traffic in the project across hosts (one-liner for
//	// orchestrators):
//	coord.ProjectTaskFilter(coord.WildOne, pid, coord.WildTail, "")
//	// → sesh.task.*.project.<pid>.>
func ProjectTaskFilter(machine, projectID, target, role string) Filter {
	return Filter{
		Verb:    string(VerbTask),
		Machine: machine,
		Scope:   string(ScopeProject),
		ScopeID: projectID,
		Target:  target,
		Role:    role,
	}
}

// ProjectBlackboardFilter constructs a subscribe filter covering every
// blackboard update under a project. The trailing `>` collects target
// + role in one wildcard so callers don't have to spell them.
//
//	sesh.blackboard.<machine>.project.<projectID>.>
func ProjectBlackboardFilter(machine, projectID string) Filter {
	return Filter{
		Verb:    string(VerbBlackboard),
		Machine: machine,
		Scope:   string(ScopeProject),
		ScopeID: projectID,
		Target:  WildTail,
	}
}

// ObserverFilters returns the set of filters an observer-class agent
// (e.g. a spy / monitor) should subscribe to for a given project. The
// list is exhaustive: nothing else under sesh.* is appropriate for an
// observer.
//
// Returned filters cover exactly the report.* and blackboard.* lanes
// — observers MUST NOT subscribe to task.* or control.*, by spec.
// The TestObserverFilter_NoTaskingTraffic test in helpers_test.go
// enforces this property.
func ObserverFilters(machine, projectID string) []Filter {
	return []Filter{
		{
			Verb:    string(VerbReport),
			Machine: machine,
			Scope:   string(ScopeProject),
			ScopeID: projectID,
			Target:  WildTail,
		},
		{
			Verb:    string(VerbBlackboard),
			Machine: machine,
			Scope:   string(ScopeProject),
			ScopeID: projectID,
			Target:  WildTail,
		},
	}
}
