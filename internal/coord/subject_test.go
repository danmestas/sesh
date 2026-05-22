package coord

import (
	"strings"
	"testing"
)

func TestSubject_String(t *testing.T) {
	cases := []struct {
		name string
		s    Subject
		want string
	}{
		{
			"project-level task — workers/implementer",
			Subject{
				Verb:    VerbTask,
				Machine: MachineLocal,
				Scope:   ScopeProject,
				ScopeID: "a3f2c1d8",
				Target:  "workers",
				Role:    "implementer",
			},
			"sesh.task._local.project.a3f2c1d8.workers.implementer",
		},
		{
			"workflow blackboard — findings",
			Subject{
				Verb:    VerbBlackboard,
				Machine: MachineLocal,
				Scope:   ScopeWorkflow,
				ScopeID: "a1b2c3d4",
				Target:  "findings",
				Role:    "research",
			},
			"sesh.blackboard._local.workflow.a1b2c3d4.findings.research",
		},
		{
			"agent-level report — status",
			Subject{
				Verb:    VerbReport,
				Machine: MachineLocal,
				Scope:   ScopeAgent,
				ScopeID: "claude-123",
				Target:  "self",
				Role:    "status",
			},
			"sesh.report._local.agent.claude-123.self.status",
		},
		{
			"multi-host — gpu-rig-7 swarm",
			Subject{
				Verb:    VerbTask,
				Machine: "gpu-rig-7",
				Scope:   ScopeProject,
				ScopeID: "a3f2c1d8",
				Target:  "swarm-alpha",
				Role:    "worker",
			},
			"sesh.task.gpu-rig-7.project.a3f2c1d8.swarm-alpha.worker",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Errorf("Subject.String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubject_Validate(t *testing.T) {
	good := Subject{
		Verb: VerbTask, Machine: "_local", Scope: ScopeProject,
		ScopeID: "a3f2c1d8", Target: "workers", Role: "implementer",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good.Validate() = %v, want nil", err)
	}

	cases := []struct {
		name   string
		s      Subject
		errSub string
	}{
		{"bad verb", Subject{Verb: "tsk", Machine: "_local", Scope: ScopeProject, ScopeID: "a3", Target: "w", Role: "i"}, "verb"},
		{"empty verb", Subject{Machine: "_local", Scope: ScopeProject, ScopeID: "a3", Target: "w", Role: "i"}, "verb"},
		{"bad scope", Subject{Verb: VerbTask, Machine: "_local", Scope: "proj", ScopeID: "a3", Target: "w", Role: "i"}, "scope"},
		{"empty machine", Subject{Verb: VerbTask, Scope: ScopeProject, ScopeID: "a3", Target: "w", Role: "i"}, "machine"},
		{"empty scopeID", Subject{Verb: VerbTask, Machine: "_local", Scope: ScopeProject, Target: "w", Role: "i"}, "scope-id"},
		{"empty target", Subject{Verb: VerbTask, Machine: "_local", Scope: ScopeProject, ScopeID: "a3", Role: "i"}, "target"},
		{"empty role", Subject{Verb: VerbTask, Machine: "_local", Scope: ScopeProject, ScopeID: "a3", Target: "w"}, "role"},
		{"dot in scopeID", Subject{Verb: VerbTask, Machine: "_local", Scope: ScopeProject, ScopeID: "a3.f2", Target: "w", Role: "i"}, "scope-id"},
		{"wildcard in role", Subject{Verb: VerbTask, Machine: "_local", Scope: ScopeProject, ScopeID: "a3", Target: "w", Role: "*"}, "role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.errSub)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("Validate() err = %v, want substring %q", err, tc.errSub)
			}
		})
	}
}

func TestSubject_QueueGroup_DelegatesToVerb(t *testing.T) {
	s := Subject{
		Verb: VerbTask, Machine: "_local", Scope: ScopeProject,
		ScopeID: "a3", Target: "workers", Role: "implementer",
	}
	if got := s.QueueGroup(); got != "implementer" {
		t.Errorf("Subject{Verb:task,Role:implementer}.QueueGroup() = %q, want implementer", got)
	}

	s.Verb = VerbBroadcast
	if got := s.QueueGroup(); got != "" {
		t.Errorf("Subject{Verb:broadcast}.QueueGroup() = %q, want ''", got)
	}
}
