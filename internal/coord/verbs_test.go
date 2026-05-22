package coord

import "testing"

func TestVerb_String(t *testing.T) {
	cases := []struct {
		v    Verb
		want string
	}{
		{VerbTask, "task"},
		{VerbBroadcast, "broadcast"},
		{VerbControl, "control"},
		{VerbAnnounce, "announce"},
		{VerbBlackboard, "blackboard"},
		{VerbReport, "report"},
	}
	for _, tc := range cases {
		if got := string(tc.v); got != tc.want {
			t.Errorf("Verb(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

// TestVerb_QueueGroupPolicy enforces the per-verb queue-group policy from
// the proposal's amendment §4. Work-stealing verbs (task, control) carry
// a role-keyed queue group so peers of the same role share work. Fan-out
// verbs (broadcast, announce, report, blackboard) carry no queue group so
// every subscriber sees every message.
func TestVerb_QueueGroupPolicy(t *testing.T) {
	cases := []struct {
		verb       Verb
		role       string
		wantQGroup string
	}{
		{VerbTask, "implementer", "implementer"},
		{VerbTask, "verifier", "verifier"},
		{VerbControl, "coordinator", "coordinator"},
		{VerbBroadcast, "anything", ""},
		{VerbAnnounce, "anything", ""},
		{VerbReport, "anything", ""},
		{VerbBlackboard, "anything", ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.verb)+"/"+tc.role, func(t *testing.T) {
			if got := tc.verb.QueueGroup(tc.role); got != tc.wantQGroup {
				t.Errorf("Verb(%q).QueueGroup(%q) = %q, want %q", tc.verb, tc.role, got, tc.wantQGroup)
			}
		})
	}
}

// TestVerb_QueueGroupEmptyRoleForWorkStealing verifies that a work-stealing
// verb called with an empty role returns an empty queue group rather than
// the bare string "" interpreted as a role. Callers that forget to set the
// role get fan-out semantics, not a string-typed bug.
func TestVerb_QueueGroupEmptyRoleForWorkStealing(t *testing.T) {
	if got := VerbTask.QueueGroup(""); got != "" {
		t.Errorf("VerbTask.QueueGroup('') = %q, want '' (fan-out fallback)", got)
	}
}

// TestVerbs_KnownLists exposes the canonical list of verbs so external
// callers (Phase 3's docs generator, hypothetical autocomplete) can
// iterate without re-spelling the constants. The proposal commits six
// verbs; adding `query` later is a one-line change here.
func TestVerbs_KnownLists(t *testing.T) {
	got := KnownVerbs()
	want := []Verb{VerbTask, VerbBroadcast, VerbControl, VerbAnnounce, VerbBlackboard, VerbReport}
	if len(got) != len(want) {
		t.Fatalf("KnownVerbs len = %d, want %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("KnownVerbs[%d] = %v, want %v", i, got[i], v)
		}
	}
}
