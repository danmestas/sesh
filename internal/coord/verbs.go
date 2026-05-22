package coord

// Verb is the first segment after `sesh.` in a coordination subject. It
// declares intent: task assignment, broadcast notification, control
// command, etc. The verb also dictates queue-group behavior — see
// QueueGroup.
//
// Six verbs committed per docs/proposals/2026-05-20-sesh-parallel-coordination-subjects.md
// amendment §1 (verb vocabulary lock). `query` is a candidate addition
// deferred to a future proposal.
type Verb string

const (
	// VerbTask — work assignment. Subscribers within the same role share
	// a queue group so each task is processed by exactly one worker
	// (work-stealing semantics).
	VerbTask Verb = "task"

	// VerbBroadcast — fan-out notification. No queue group; every
	// subscriber receives every message.
	VerbBroadcast Verb = "broadcast"

	// VerbControl — directed command (start, stop, reconfigure). Like
	// VerbTask, work-stealing within a role so exactly one instance acts.
	VerbControl Verb = "control"

	// VerbAnnounce — pub/sub notification. Fan-out (no queue group);
	// distinct from VerbBroadcast in convention only — Announce is for
	// "thing happened" events, Broadcast is for "everyone needs to act
	// on this" prompts.
	VerbAnnounce Verb = "announce"

	// VerbBlackboard — shared-state updates (find-ings, partial results).
	// Fan-out so every watcher sees every update.
	VerbBlackboard Verb = "blackboard"

	// VerbReport — observability / monitoring traffic. Fan-out so
	// multiple independent observers can consume in parallel.
	VerbReport Verb = "report"
)

// KnownVerbs returns the canonical list of verbs in source order. Useful
// for documentation generators, autocomplete dictionaries, and validation
// loops. The slice is freshly allocated per call so callers may mutate.
func KnownVerbs() []Verb {
	return []Verb{
		VerbTask,
		VerbBroadcast,
		VerbControl,
		VerbAnnounce,
		VerbBlackboard,
		VerbReport,
	}
}

// QueueGroup returns the NATS queue group a subscriber for this verb
// should use, given the role under which the subscriber is operating.
// Two regimes:
//
//   - Work-stealing verbs (task, control): the role name is the queue
//     group. Two implementers subscribing to the same subject share
//     work; each message is delivered to exactly one.
//
//   - Fan-out verbs (broadcast, announce, report, blackboard): no
//     queue group ("" return). Every subscriber sees every message,
//     independent of role.
//
// Callers MUST honor the policy when subscribing. NATS Micro registers
// endpoints under a queue group by default; using the wrong group for
// a fan-out verb silently loses messages to peers. The helpers in
// helpers.go pass the right group to the NATS client; hand-rolled
// subscribers should call this method.
//
// An empty role on a work-stealing verb returns "" (fan-out fallback)
// rather than treating "" as a role name. This is a defense against
// callers who forget to set the role.
func (v Verb) QueueGroup(role string) string {
	switch v {
	case VerbTask, VerbControl:
		if role == "" {
			return ""
		}
		return role
	case VerbBroadcast, VerbAnnounce, VerbReport, VerbBlackboard:
		return ""
	default:
		// Unknown verbs default to fan-out — the safer choice. A typo'd
		// verb shouldn't silently swallow messages into a one-of-N group.
		return ""
	}
}

// Valid reports whether v is one of the six committed verbs. Used by
// Subject.Validate and Filter.Validate to reject typos.
func (v Verb) Valid() bool {
	switch v {
	case VerbTask, VerbBroadcast, VerbControl, VerbAnnounce, VerbBlackboard, VerbReport:
		return true
	default:
		return false
	}
}
