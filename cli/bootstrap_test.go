package cli

import (
	"strings"
	"testing"
)

// TestMakePlan exercises the decision tree end-to-end. The acceptance
// criteria in issue #27 enumerate the five cases that have to land for
// the bootstrap refactor to be considered functionally complete; this
// table walks them plus the SeedTracked variant and a "drift only
// matters at fresh bring-up" sanity case.
func TestMakePlan(t *testing.T) {
	const (
		localCode = "1111111111111111111111111111111111111111"
		hubCode   = "2222222222222222222222222222222222222222"
		hubURL    = "http://127.0.0.1:54321/"
	)

	type want struct {
		source      Source
		hubURL      string
		seedMode    SeedMode
		hasConflict bool
	}

	cases := []struct {
		name  string
		world World
		want  want
	}{
		{
			name: "non-fresh repo: no bootstrap action",
			world: World{
				FreshRepo:        false,
				LocalProjectCode: localCode,
				HubFossilURL:     hubURL,
				HubProjectCode:   localCode,
				SeedMode:         SeedAll,
			},
			want: want{source: SourceNone},
		},
		{
			name: "non-fresh: drift ignored (only meaningful at first bring-up)",
			world: World{
				FreshRepo:        false,
				LocalProjectCode: localCode,
				HubFossilURL:     hubURL,
				HubProjectCode:   hubCode,
				SeedMode:         SeedAll,
			},
			want: want{source: SourceNone},
		},
		{
			name: "fresh + hub empty + seed=all → git worktree",
			world: World{
				FreshRepo:        true,
				LocalProjectCode: localCode,
				SeedMode:         SeedAll,
			},
			want: want{source: SourceGitWorktree, seedMode: SeedAll},
		},
		{
			name: "fresh + hub empty + seed=none → git worktree, seed=none (Execute no-ops)",
			world: World{
				FreshRepo:        true,
				LocalProjectCode: localCode,
				SeedMode:         SeedNone,
			},
			want: want{source: SourceGitWorktree, seedMode: SeedNone},
		},
		{
			name: "fresh + hub empty + seed=tracked → git worktree, seed=tracked",
			world: World{
				FreshRepo:        true,
				LocalProjectCode: localCode,
				SeedMode:         SeedTracked,
			},
			want: want{source: SourceGitWorktree, seedMode: SeedTracked},
		},
		{
			name: "fresh + hub content + codes match → hub clone",
			world: World{
				FreshRepo:        true,
				LocalProjectCode: localCode,
				HubFossilURL:     hubURL,
				HubProjectCode:   localCode,
				SeedMode:         SeedAll,
			},
			want: want{source: SourceHub, hubURL: hubURL},
		},
		{
			name: "fresh + hub content + codes drift → conflict",
			world: World{
				FreshRepo:        true,
				LocalProjectCode: localCode,
				HubFossilURL:     hubURL,
				HubProjectCode:   hubCode,
				SeedMode:         SeedAll,
			},
			want: want{hasConflict: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MakePlan(tc.world)
			if err != nil {
				t.Fatalf("MakePlan: %v", err)
			}
			if tc.want.hasConflict {
				if got.Conflict == nil {
					t.Fatalf("expected conflict; got plan %+v", got)
				}
				if got.Conflict.Kind != "project-code-drift" {
					t.Errorf("Conflict.Kind = %q, want %q", got.Conflict.Kind, "project-code-drift")
				}
				if got.Conflict.LocalPin != tc.world.LocalProjectCode {
					t.Errorf("Conflict.LocalPin = %q, want %q", got.Conflict.LocalPin, tc.world.LocalProjectCode)
				}
				if got.Conflict.HubPin != tc.world.HubProjectCode {
					t.Errorf("Conflict.HubPin = %q, want %q", got.Conflict.HubPin, tc.world.HubProjectCode)
				}
				if got.Source != SourceNone {
					t.Errorf("conflict plan should leave Source unset; got %v", got.Source)
				}
				return
			}
			if got.Conflict != nil {
				t.Fatalf("unexpected conflict: %+v", got.Conflict)
			}
			if got.Source != tc.want.source {
				t.Errorf("Source = %v, want %v", got.Source, tc.want.source)
			}
			if got.HubFossilURL != tc.want.hubURL {
				t.Errorf("HubFossilURL = %q, want %q", got.HubFossilURL, tc.want.hubURL)
			}
			if tc.want.source == SourceGitWorktree && got.SeedMode != tc.want.seedMode {
				t.Errorf("SeedMode = %q, want %q", got.SeedMode, tc.want.seedMode)
			}
		})
	}
}

// TestMakePlan_ConflictMessage codifies the user-visible contract from
// issue #26: the conflict message must surface both hashes, both file
// paths in their conventional display form, and both recovery commands
// so the operator can pick a side without guessing.
func TestMakePlan_ConflictMessage(t *testing.T) {
	const (
		localCode = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hubCode   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	p, err := MakePlan(World{
		FreshRepo:        true,
		LocalProjectCode: localCode,
		HubFossilURL:     "http://hub.example/",
		HubProjectCode:   hubCode,
		SeedMode:         SeedAll,
	})
	if err != nil {
		t.Fatalf("MakePlan: %v", err)
	}
	if p.Conflict == nil {
		t.Fatal("expected conflict; got nil")
	}
	msg := p.Conflict.Message
	mustContain := []string{
		// Both hashes appear verbatim.
		localCode,
		hubCode,
		// Both file paths in their conventional display form.
		".sesh/project-code",
		"~/.sesh/hub.repo",
		// Recovery 1: treat hub as canonical.
		"rm .sesh/project-code",
		// Recovery 2: treat local as canonical (start hub fresh).
		"sesh hub stop",
		"rm -rf ~/.sesh/hub.repo*",
		"sesh hub serve",
	}
	for _, want := range mustContain {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict message missing %q\n---message---\n%s\n---end---", want, msg)
		}
	}
}

// TestMakePlan_HubURLWithoutCode covers a probe-invariant edge: if a
// caller ever supplies HubFossilURL without HubProjectCode (the probe
// keeps these paired today, but a future caller might not), MakePlan
// treats the hub as having content but skips the drift check rather
// than fabricating a phantom conflict against the empty string.
func TestMakePlan_HubURLWithoutCode(t *testing.T) {
	const localCode = "1111111111111111111111111111111111111111"
	p, err := MakePlan(World{
		FreshRepo:        true,
		LocalProjectCode: localCode,
		HubFossilURL:     "http://hub.example/",
		HubProjectCode:   "",
		SeedMode:         SeedAll,
	})
	if err != nil {
		t.Fatalf("MakePlan: %v", err)
	}
	if p.Conflict != nil {
		t.Fatalf("unexpected conflict: %+v", p.Conflict)
	}
	if p.Source != SourceHub {
		t.Errorf("Source = %v, want SourceHub", p.Source)
	}
	if p.HubFossilURL != "http://hub.example/" {
		t.Errorf("HubFossilURL = %q, want http://hub.example/", p.HubFossilURL)
	}
}
