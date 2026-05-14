package cli

import (
	"context"
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

// TestResolveProjectCode_AdoptsOnDrift is the issue #34 acceptance:
// given a local pin = A and an authoritative hub code = B,
// ResolveProjectCode writes B into <cwd>/.sesh/project-code and returns
// B so EdgeSync's SeedFromUpstream sees agreement on the next
// hub.NewHub call.
func TestResolveProjectCode_AdoptsOnDrift(t *testing.T) {
	const (
		localCode = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hubCode   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	cwd := t.TempDir()
	seedProjectCodeFile(t, cwd, localCode)

	active, err := ResolveProjectCode(cwd, localCode, hubCode)
	if err != nil {
		t.Fatalf("ResolveProjectCode: %v", err)
	}
	if active != hubCode {
		t.Errorf("active = %q, want %q", active, hubCode)
	}

	got := readProjectCodeFile(t, cwd)
	if got != hubCode {
		t.Errorf("project-code = %q, want %q", got, hubCode)
	}
}

// TestResolveProjectCode_Idempotent confirms repeat invocations with a
// matching pin are no-ops — file unchanged, no error, active=local.
// Lets up.go's Starter call ResolveProjectCode unconditionally at the
// SourceHub branch without re-writing on every sesh up.
func TestResolveProjectCode_Idempotent(t *testing.T) {
	const hubCode = "cccccccccccccccccccccccccccccccccccccccc"
	cwd := t.TempDir()
	seedProjectCodeFile(t, cwd, hubCode)

	for i := range 2 {
		active, err := ResolveProjectCode(cwd, hubCode, hubCode)
		if err != nil {
			t.Fatalf("ResolveProjectCode (call %d): %v", i+1, err)
		}
		if active != hubCode {
			t.Errorf("call %d: active = %q, want %q", i+1, active, hubCode)
		}
	}
	if got := readProjectCodeFile(t, cwd); got != hubCode {
		t.Errorf("project-code = %q, want %q (unchanged)", got, hubCode)
	}
}

// TestResolveProjectCode_NoAdoptWhenHubCodeEmpty covers the
// probe-invariant edge: an empty hubCode (hub has no content yet)
// leaves the local pin alone and returns localCode.
func TestResolveProjectCode_NoAdoptWhenHubCodeEmpty(t *testing.T) {
	const localCode = "dddddddddddddddddddddddddddddddddddddddd"
	cwd := t.TempDir()
	seedProjectCodeFile(t, cwd, localCode)

	active, err := ResolveProjectCode(cwd, localCode, "")
	if err != nil {
		t.Fatalf("ResolveProjectCode: %v", err)
	}
	if active != localCode {
		t.Errorf("active = %q, want %q", active, localCode)
	}
	if got := readProjectCodeFile(t, cwd); got != localCode {
		t.Errorf("project-code = %q, want %q (unchanged)", got, localCode)
	}
}

// TestResolveProjectCode_NoAdoptWhenCodesMatch is the "agree-already"
// shortcut: matching codes skip the file write entirely. Cheap path
// for the common steady-state where hub and local agreed all along.
func TestResolveProjectCode_NoAdoptWhenCodesMatch(t *testing.T) {
	const code = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	cwd := t.TempDir()
	// Deliberately do NOT seed the file. If ResolveProjectCode
	// short-circuits on matching codes (as it should), no write
	// happens and the absent file stays absent.
	active, err := ResolveProjectCode(cwd, code, code)
	if err != nil {
		t.Fatalf("ResolveProjectCode: %v", err)
	}
	if active != code {
		t.Errorf("active = %q, want %q", active, code)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".sesh", "project-code")); !os.IsNotExist(err) {
		t.Errorf("project-code file created on no-drift path (stat err=%v)", err)
	}
}

// TestApply_SourceNone exercises the no-bootstrap branch: the session's
// repo file pre-existed, so Apply just logs and returns nil. The hub
// argument can be nil because no commit goes through it.
func TestApply_SourceNone(t *testing.T) {
	plan := Plan{Source: SourceNone}
	if err := Apply(context.Background(), plan, nil, Deps{Cwd: t.TempDir(), RepoPath: "/x/y.repo"}); err != nil {
		t.Fatalf("Apply(SourceNone): %v", err)
	}
}

// TestApply_SourceHub exercises the clone-already-done branch: the
// SourceHub clone fired inside hub.NewHub's SeedFromUpstream before
// Apply was called, so Apply just logs and returns nil. The hub
// argument is unused for this case.
func TestApply_SourceHub(t *testing.T) {
	plan := Plan{Source: SourceHub, HubFossilURL: "http://hub.example/"}
	if err := Apply(context.Background(), plan, nil, Deps{Cwd: t.TempDir(), RepoPath: "/x/y.repo"}); err != nil {
		t.Fatalf("Apply(SourceHub): %v", err)
	}
}

// TestApply_UnknownSource guards the switch: an unrecognized Source
// value yields an error rather than a silent no-op. Defends against
// a future Source const that forgets to add an Apply case.
func TestApply_UnknownSource(t *testing.T) {
	plan := Plan{Source: Source(99)}
	err := Apply(context.Background(), plan, nil, Deps{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("Apply with unknown source: want error, got nil")
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
