package subject

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPromptV2_Build(t *testing.T) {
	cases := []struct {
		name string
		in   Coord
		want string
	}{
		{
			name: "canonical with inst",
			in:   Coord{Machine: "m1", Project: "p1", Session: "s1", Role: "r1", Inst: "i1"},
			want: "agents.prompt.v2.m1.p1.s1.r1.i1",
		},
		{
			name: "no inst",
			in:   Coord{Machine: "m1", Project: "p1", Session: "s1", Role: "r1"},
			want: "agents.prompt.v2.m1.p1.s1.r1",
		},
		{
			name: "hyphenated tokens",
			in:   Coord{Machine: "laptop", Project: "sesh-channels", Session: "main", Role: "worker"},
			want: "agents.prompt.v2.laptop.sesh-channels.main.worker",
		},
		{
			name: "numeric inst",
			in:   Coord{Machine: "laptop", Project: "p", Session: "s", Role: "r", Inst: "1"},
			want: "agents.prompt.v2.laptop.p.s.r.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PromptV2(tc.in)
			if err != nil {
				t.Fatalf("PromptV2(%+v) err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("PromptV2(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPromptV2_RejectsBadTokens(t *testing.T) {
	cases := []struct {
		name string
		in   Coord
	}{
		{"dot in machine", Coord{Machine: "m.1", Project: "p", Session: "s", Role: "r"}},
		{"star in project", Coord{Machine: "m", Project: "*", Session: "s", Role: "r"}},
		{"gt in role", Coord{Machine: "m", Project: "p", Session: "s", Role: ">"}},
		{"space in role", Coord{Machine: "m", Project: "p", Session: "s", Role: "r "}},
		{"empty machine", Coord{Machine: "", Project: "p", Session: "s", Role: "r"}},
		{"bad inst", Coord{Machine: "m", Project: "p", Session: "s", Role: "r", Inst: "i.1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PromptV2(tc.in)
			if err == nil {
				t.Fatalf("PromptV2(%+v) want error, got nil", tc.in)
			}
			var ite *InvalidTokenError
			if !errors.As(err, &ite) {
				t.Fatalf("PromptV2(%+v) err type = %T, want *InvalidTokenError", tc.in, err)
			}
		})
	}

	t.Run("empty inst is omitted not rejected", func(t *testing.T) {
		got, err := PromptV2(Coord{Machine: "m", Project: "p", Session: "s", Role: "r"})
		if err != nil {
			t.Fatalf("empty Inst should be omitted, got err=%v", err)
		}
		if got != "agents.prompt.v2.m.p.s.r" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestPromptV2_RoundTrip(t *testing.T) {
	cases := []Coord{
		{Machine: "m1", Project: "p1", Session: "s1", Role: "r1", Inst: "i1"},
		{Machine: "m1", Project: "p1", Session: "s1", Role: "r1"},
		{Machine: "laptop", Project: "sesh-channels", Session: "main", Role: "worker"},
		{Machine: "laptop", Project: "p", Session: "s", Role: "r", Inst: "1"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%+v", c), func(t *testing.T) {
			s, err := PromptV2(c)
			if err != nil {
				t.Fatalf("PromptV2: %v", err)
			}
			got, err := ParsePromptV2(s)
			if err != nil {
				t.Fatalf("ParsePromptV2(%q): %v", s, err)
			}
			if got != c {
				t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, c)
			}
		})
	}
}

func TestParsePromptV2_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"two tokens", "foo.bar"},
		{"v1 prefix", "agents.prompt.v1.a.b.c.d"},
		{"too few tokens", "agents.prompt.v2.a.b.c"},
		{"too many tokens", "agents.prompt.v2.a.b.c.d.e.f"},
		{"empty token within", "agents.prompt.v2.a..s.r"},
		{"trailing dot", "agents.prompt.v2.a.b.c.d."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePromptV2(tc.in)
			if err == nil {
				t.Fatalf("ParsePromptV2(%q) want error, got nil", tc.in)
			}
		})
	}
}

func TestTaskStream(t *testing.T) {
	got, err := TaskStream("session", "abc123", "01HX")
	if err != nil {
		t.Fatalf("TaskStream: %v", err)
	}
	if want := "agents.task.stream.session.abc123.01HX"; got != want {
		t.Fatalf("TaskStream = %q, want %q", got, want)
	}

	if _, err := TaskStream("session", "", "01HX"); err == nil {
		t.Fatalf("empty scopeID should error")
	}
	if _, err := TaskStream("", "abc", "01HX"); err == nil {
		t.Fatalf("empty scopeKind should error")
	}
}

func TestCard_Builders(t *testing.T) {
	t.Run("CardGet canonical", func(t *testing.T) {
		got, err := CardGet("claude", "dmestas", "main")
		if err != nil {
			t.Fatalf("CardGet: %v", err)
		}
		if want := "agents.card.get.claude.dmestas.main"; got != want {
			t.Fatalf("CardGet = %q, want %q", got, want)
		}
	})
	t.Run("CardExtended canonical", func(t *testing.T) {
		got, err := CardExtended("claude", "dmestas", "main")
		if err != nil {
			t.Fatalf("CardExtended: %v", err)
		}
		if want := "agents.card.extended.claude.dmestas.main"; got != want {
			t.Fatalf("CardExtended = %q, want %q", got, want)
		}
	})

	badTokens := []string{".", "*", ">", "a.b", "x y", "\t", "\n", ""}
	for _, bad := range badTokens {
		t.Run("CardGet rejects "+fmt.Sprintf("%q", bad), func(t *testing.T) {
			if _, err := CardGet(bad, "dmestas", "main"); err == nil {
				t.Fatalf("CardGet(%q) want error", bad)
			}
		})
		t.Run("CardExtended rejects "+fmt.Sprintf("%q", bad), func(t *testing.T) {
			if _, err := CardExtended("claude", bad, "main"); err == nil {
				t.Fatalf("CardExtended(_, %q, _) want error", bad)
			}
		})
		t.Run("TaskStream rejects "+fmt.Sprintf("%q", bad), func(t *testing.T) {
			if _, err := TaskStream("session", "abc", bad); err == nil {
				t.Fatalf("TaskStream(_, _, %q) want error", bad)
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

func TestPromptV2QueueGroup(t *testing.T) {
	if PromptV2QueueGroup != "agents-prompt-v2" {
		t.Fatalf("PromptV2QueueGroup = %q, want %q", PromptV2QueueGroup, "agents-prompt-v2")
	}
}
