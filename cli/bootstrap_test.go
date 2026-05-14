package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakePlan exercises the decision tree end-to-end. The five
// functional cases plus the SeedTracked variant and a sanity check
// that drift is only meaningful at fresh bring-up.
func TestMakePlan(t *testing.T) {
	const (
		localCode = "1111111111111111111111111111111111111111"
		hubCode   = "2222222222222222222222222222222222222222"
		hubURL    = "http://127.0.0.1:54321/"
	)

	type want struct {
		source   Source
		hubURL   string
		seedMode SeedMode
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
			name: "fresh + hub content + codes drift → hub clone (adoption resolves drift at Execute time)",
			world: World{
				FreshRepo:        true,
				LocalProjectCode: localCode,
				HubFossilURL:     hubURL,
				HubProjectCode:   hubCode,
				SeedMode:         SeedAll,
			},
			want: want{source: SourceHub, hubURL: hubURL},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MakePlan(tc.world)
			if err != nil {
				t.Fatalf("MakePlan: %v", err)
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

// TestMakePlan_HubURLWithoutCode covers a probe-invariant edge: if a
// caller supplies HubFossilURL without HubProjectCode (the probe keeps
// these paired today, but a future caller might not), MakePlan still
// returns SourceHub. Adoption is then a no-op at Execute time.
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
	if p.Source != SourceHub {
		t.Errorf("Source = %v, want SourceHub", p.Source)
	}
	if p.HubFossilURL != "http://hub.example/" {
		t.Errorf("HubFossilURL = %q, want http://hub.example/", p.HubFossilURL)
	}
}

// TestExecute_AdoptsHubProjectCode is the issue #34 acceptance test:
// given a local pin = A and a hub project-code = B, Execute writes B
// into <cwd>/.sesh/project-code so EdgeSync's seedFromUpstream sees
// agreement on the next hub.NewHub call.
func TestExecute_AdoptsHubProjectCode(t *testing.T) {
	const (
		localCode = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hubCode   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	cwd := t.TempDir()
	seedProjectCodeFile(t, cwd, localCode)

	plan := Plan{Source: SourceHub, HubFossilURL: "http://hub.example/"}
	err := Execute(plan, Deps{Cwd: cwd, RepoPath: "", HubProjectCode: hubCode})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := readProjectCodeFile(t, cwd)
	if got != hubCode {
		t.Errorf("project-code = %q, want %q", got, hubCode)
	}
}

// TestExecute_AdoptionIdempotent confirms repeat invocations with a
// matching local file are no-ops — file unchanged, no error. This is
// the property that lets us call Execute unconditionally at the
// SourceHub branch in up.go without re-writing on every sesh up.
func TestExecute_AdoptionIdempotent(t *testing.T) {
	const hubCode = "cccccccccccccccccccccccccccccccccccccccc"
	cwd := t.TempDir()
	seedProjectCodeFile(t, cwd, hubCode)

	plan := Plan{Source: SourceHub, HubFossilURL: "http://hub.example/"}
	for i := range 2 {
		if err := Execute(plan, Deps{Cwd: cwd, RepoPath: "", HubProjectCode: hubCode}); err != nil {
			t.Fatalf("Execute (call %d): %v", i+1, err)
		}
	}
	if got := readProjectCodeFile(t, cwd); got != hubCode {
		t.Errorf("project-code = %q, want %q (unchanged)", got, hubCode)
	}
}

// TestExecute_NoAdoptWhenHubCodeEmpty covers the probe-invariant edge
// for Execute: an empty HubProjectCode (hub has no content yet) leaves
// the local pin alone.
func TestExecute_NoAdoptWhenHubCodeEmpty(t *testing.T) {
	const localCode = "dddddddddddddddddddddddddddddddddddddddd"
	cwd := t.TempDir()
	seedProjectCodeFile(t, cwd, localCode)

	plan := Plan{Source: SourceHub, HubFossilURL: "http://hub.example/"}
	if err := Execute(plan, Deps{Cwd: cwd, RepoPath: "", HubProjectCode: ""}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := readProjectCodeFile(t, cwd); got != localCode {
		t.Errorf("project-code = %q, want %q (unchanged)", got, localCode)
	}
}

func seedProjectCodeFile(t *testing.T, cwd, code string) {
	t.Helper()
	dir := filepath.Join(cwd, ".sesh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project-code"), []byte(code+"\n"), 0o644); err != nil {
		t.Fatalf("seed project-code: %v", err)
	}
}

func readProjectCodeFile(t *testing.T, cwd string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cwd, ".sesh", "project-code"))
	if err != nil {
		t.Fatalf("read project-code: %v", err)
	}
	return strings.TrimSpace(string(data))
}
