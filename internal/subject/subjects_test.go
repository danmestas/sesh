package subject

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestPrompt_Build pins the canonical byte strings the team-lead spec
// locks in. The shapes here MUST match the TS SDK's golden fixtures
// (sesh-channels/sdk/test/subjects.test.ts) byte-for-byte. Any change
// here is a cross-stack wire change and requires the paired TS PR.
func TestPrompt_Build(t *testing.T) {
	cases := []struct {
		name string
		in   Coord
		want string
	}{
		{
			name: "5-token session orch (empty role)",
			in:   Coord{Machine: "m1", Project: "p1", Session: "s1"},
			want: "agents.prompt.m1.p1.s1",
		},
		{
			name: "6-token role pool",
			in:   Coord{Machine: "m1", Project: "p1", Session: "s1", Role: "impl"},
			want: "agents.prompt.m1.p1.s1.impl",
		},
		{
			name: "7-token direct instance",
			in:   Coord{Machine: "m1", Project: "p1", Session: "s1", Role: "impl", Inst: "i7"},
			want: "agents.prompt.m1.p1.s1.impl.i7",
		},
		{
			name: "hyphenated tokens stay intact",
			in:   Coord{Machine: "laptop", Project: "sesh-channels", Session: "main", Role: "worker"},
			want: "agents.prompt.laptop.sesh-channels.main.worker",
		},
		{
			name: "numeric inst",
			in:   Coord{Machine: "laptop", Project: "p", Session: "s", Role: "r", Inst: "1"},
			want: "agents.prompt.laptop.p.s.r.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Prompt(tc.in)
			if err != nil {
				t.Fatalf("Prompt(%+v) err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Prompt(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPrompt_RejectsBadTokens(t *testing.T) {
	cases := []struct {
		name string
		in   Coord
	}{
		{"dot in machine", Coord{Machine: "m.1", Project: "p", Session: "s"}},
		{"star in project", Coord{Machine: "m", Project: "*", Session: "s"}},
		{"gt in session", Coord{Machine: "m", Project: "p", Session: ">"}},
		{"space in role", Coord{Machine: "m", Project: "p", Session: "s", Role: "r "}},
		{"empty machine", Coord{Machine: "", Project: "p", Session: "s"}},
		{"bad inst", Coord{Machine: "m", Project: "p", Session: "s", Role: "r", Inst: "i.1"}},
		{"inst without role", Coord{Machine: "m", Project: "p", Session: "s", Inst: "i1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Prompt(tc.in)
			if err == nil {
				t.Fatalf("Prompt(%+v) want error, got nil", tc.in)
			}
			var ite *InvalidTokenError
			if !errors.As(err, &ite) {
				t.Fatalf("Prompt(%+v) err type = %T, want *InvalidTokenError", tc.in, err)
			}
		})
	}
}

func TestHeartbeat(t *testing.T) {
	got, err := Heartbeat(Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Heartbeat err = %v", err)
	}
	if want := "agents.hb.m1.p1.s1"; got != want {
		t.Fatalf("Heartbeat = %q, want %q", got, want)
	}

	// Role and Inst are ignored — Heartbeat is always 5-token.
	gotIgnored, err := Heartbeat(Coord{Machine: "m1", Project: "p1", Session: "s1", Role: "r", Inst: "i"})
	if err != nil {
		t.Fatalf("Heartbeat with role+inst err = %v", err)
	}
	if gotIgnored != "agents.hb.m1.p1.s1" {
		t.Fatalf("Heartbeat ignored-tokens = %q, want %q", gotIgnored, "agents.hb.m1.p1.s1")
	}

	if _, err := Heartbeat(Coord{Machine: "", Project: "p", Session: "s"}); err == nil {
		t.Fatalf("empty machine should error")
	}
	if _, err := Heartbeat(Coord{Machine: "m", Project: "p.1", Session: "s"}); err == nil {
		t.Fatalf("dotted project should error")
	}
}

func TestStatus(t *testing.T) {
	got, err := Status(Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Status err = %v", err)
	}
	if want := "agents.status.m1.p1.s1"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
	if _, err := Status(Coord{Machine: "m", Project: "p", Session: ""}); err == nil {
		t.Fatalf("empty session should error")
	}
}

func TestCard(t *testing.T) {
	got, err := Card(Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Card err = %v", err)
	}
	if want := "agents.card.m1.p1.s1"; got != want {
		t.Fatalf("Card = %q, want %q", got, want)
	}
	if _, err := Card(Coord{Machine: "m.bad", Project: "p", Session: "s"}); err == nil {
		t.Fatalf("dotted machine should error")
	}
}

func TestCardx(t *testing.T) {
	got, err := Cardx(Coord{Machine: "m1", Project: "p1", Session: "s1"})
	if err != nil {
		t.Fatalf("Cardx err = %v", err)
	}
	if want := "agents.cardx.m1.p1.s1"; got != want {
		t.Fatalf("Cardx = %q, want %q", got, want)
	}
	if _, err := Cardx(Coord{Machine: "m", Project: "", Session: "s"}); err == nil {
		t.Fatalf("empty project should error")
	}
}

// TestVerbsAreSingleSegment is a structural guard: every session-scoped
// verb MUST occupy exactly one segment between `agents` and the first
// machine token. Compound verbs (e.g. `card.get`) would shift the
// 5/6/7-token tier boundary downstream consumers count on. The check
// runs against the canonical 5-token output of each builder.
func TestVerbsAreSingleSegment(t *testing.T) {
	c := Coord{Machine: "m", Project: "p", Session: "s"}
	cases := []struct {
		name string
		fn   func(Coord) (string, error)
	}{
		{"Heartbeat", Heartbeat},
		{"Status", Status},
		{"Card", Card},
		{"Cardx", Cardx},
		{"Prompt", Prompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn(c)
			if err != nil {
				t.Fatalf("%s err = %v", tc.name, err)
			}
			parts := strings.Split(got, ".")
			if len(parts) != 5 {
				t.Fatalf("%s = %q (%d parts), want 5 (single-segment verb tier)", tc.name, got, len(parts))
			}
			if parts[0] != "agents" {
				t.Fatalf("%s = %q, want first token = agents", tc.name, got)
			}
			if parts[1] == "" {
				t.Fatalf("%s = %q, empty verb token", tc.name, got)
			}
			if strings.Contains(parts[1], ".") {
				t.Fatalf("%s verb token %q contains a dot — verbs MUST be single-segment", tc.name, parts[1])
			}
		})
	}
}

func TestStream(t *testing.T) {
	got, err := Stream("session", "abc123", "01HX")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if want := "agents.task.stream.session.abc123.01HX"; got != want {
		t.Fatalf("Stream = %q, want %q", got, want)
	}

	if _, err := Stream("session", "", "01HX"); err == nil {
		t.Fatalf("empty scopeID should error")
	}
	if _, err := Stream("", "abc", "01HX"); err == nil {
		t.Fatalf("empty scopeKind should error")
	}

	badTokens := []string{".", "*", ">", "a.b", "x y", "\t", "\n", ""}
	for _, bad := range badTokens {
		t.Run("Stream rejects "+fmt.Sprintf("%q", bad), func(t *testing.T) {
			if _, err := Stream("session", "abc", bad); err == nil {
				t.Fatalf("Stream(_, _, %q) want error", bad)
			}
		})
	}
}

func TestValidateToken(t *testing.T) {
	negatives := []string{".", " ", "\t", "\n", "*", ">", "a.b", "x y", ""}
	for _, tok := range negatives {
		t.Run("neg/"+tok, func(t *testing.T) {
			err := validateToken(tok)
			if err == nil {
				t.Fatalf("validateToken(%q) want error", tok)
			}
		})
	}
	positives := []string{"m1", "claude", "01HX5K0", "dmestas-main"}
	for _, tok := range positives {
		t.Run("pos/"+tok, func(t *testing.T) {
			if err := validateToken(tok); err != nil {
				t.Fatalf("validateToken(%q) err = %v", tok, err)
			}
		})
	}

	t.Run("empty error message", func(t *testing.T) {
		err := validateToken("")
		var ite *InvalidTokenError
		if !errors.As(err, &ite) {
			t.Fatalf("err type = %T", err)
		}
		if ite.Reason != "empty" {
			t.Fatalf("reason = %q, want %q", ite.Reason, "empty")
		}
	})

	t.Run("reserved error message", func(t *testing.T) {
		err := validateToken("a.b")
		var ite *InvalidTokenError
		if !errors.As(err, &ite) {
			t.Fatalf("err type = %T", err)
		}
		const want = "contains reserved character (. whitespace * >)"
		if ite.Reason != want {
			t.Fatalf("reason = %q, want %q", ite.Reason, want)
		}
		// Cross-stack grep contract: full formatted message includes both the
		// type prefix and the reason text, so log scrapers can match either.
		full := err.Error()
		if !strings.Contains(full, "invalid subject token") || !strings.Contains(full, want) {
			t.Fatalf("Error() = %q, expected to contain both prefix and reason", full)
		}
	})
}

func TestPromptQueueGroup(t *testing.T) {
	// The literal value is retained from the v0.3 era for queue-group
	// continuity across the cutover — see PromptQueueGroup doc comment.
	if PromptQueueGroup != "agents-prompt-v2" {
		t.Fatalf("PromptQueueGroup = %q, want %q", PromptQueueGroup, "agents-prompt-v2")
	}
}
