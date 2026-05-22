package coord

import (
	"strings"
	"testing"
)

func TestFilter_String_ConcreteFilter(t *testing.T) {
	f := Filter{
		Verb:    string(VerbTask),
		Machine: "_local",
		Scope:   string(ScopeProject),
		ScopeID: "a3f2c1d8",
		Target:  "workers",
		Role:    "implementer",
	}
	want := "sesh.task._local.project.a3f2c1d8.workers.implementer"
	if got := f.String(); got != want {
		t.Errorf("Filter.String() = %q, want %q", got, want)
	}
}

func TestFilter_String_WithWildcards(t *testing.T) {
	cases := []struct {
		name string
		f    Filter
		want string
	}{
		{
			"all task across hosts in a project",
			Filter{Verb: string(VerbTask), Machine: WildOne, Scope: string(ScopeProject), ScopeID: "a3", Target: WildTail},
			"sesh.task.*.project.a3.>",
		},
		{
			"this host any verb",
			Filter{Verb: WildOne, Machine: "_local", Scope: WildTail},
			"sesh.*._local.>",
		},
		{
			"specific role across hosts",
			Filter{Verb: string(VerbTask), Machine: WildOne, Scope: string(ScopeProject), ScopeID: "a3", Target: "workers", Role: "implementer"},
			"sesh.task.*.project.a3.workers.implementer",
		},
		{
			"all workers regardless of role",
			Filter{Verb: string(VerbTask), Machine: WildOne, Scope: string(ScopeProject), ScopeID: "a3", Target: "workers", Role: WildOne},
			"sesh.task.*.project.a3.workers.*",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.String(); got != tc.want {
				t.Errorf("Filter.String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFilter_Validate(t *testing.T) {
	cases := []struct {
		name   string
		f      Filter
		errSub string
	}{
		{
			"good concrete",
			Filter{Verb: "task", Machine: "_local", Scope: "project", ScopeID: "a3", Target: "workers", Role: "implementer"},
			"",
		},
		{
			"good wildcarded",
			Filter{Verb: WildOne, Machine: "_local", Scope: WildTail},
			"",
		},
		{
			"good tail",
			Filter{Verb: "task", Machine: "_local", Scope: "project", ScopeID: "a3", Target: WildTail},
			"",
		},
		{
			"recursive wildcard mid-subject — illegal",
			Filter{Verb: "task", Machine: WildTail, Scope: "project", ScopeID: "a3", Target: "workers", Role: "implementer"},
			"recursive wildcard",
		},
		{
			"unknown verb non-wildcard",
			Filter{Verb: "tsk", Machine: "_local", Scope: "project", ScopeID: "a3", Target: "workers", Role: "implementer"},
			"verb",
		},
		{
			"unknown scope non-wildcard",
			Filter{Verb: "task", Machine: "_local", Scope: "proj", ScopeID: "a3", Target: "workers", Role: "implementer"},
			"scope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			if tc.errSub == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.errSub)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("Validate() err = %v, want substring %q", err, tc.errSub)
			}
		})
	}
}
