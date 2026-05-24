package card

import (
	"bytes"
	"testing"
)

func TestCanonicalize_KeyOrdering(t *testing.T) {
	got, err := canonicalizeJSON([]byte(`{"b":1,"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":2,"b":1}` {
		t.Fatalf("got %s", got)
	}
}

func TestCanonicalize_NestedSort(t *testing.T) {
	got, err := canonicalizeJSON([]byte(`{"z":{"y":1,"x":[3,2,1]},"a":true}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":true,"z":{"x":[3,2,1],"y":1}}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestCanonicalize_NoWhitespace(t *testing.T) {
	got, err := canonicalizeJSON([]byte("{\n\t\"a\": 1,\n\t\"b\": [ 2, 3 ]\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsAny(got, " \t\n") {
		t.Fatalf("output contains whitespace: %s", got)
	}
}

func TestCanonicalize_Idempotent(t *testing.T) {
	in := []byte(`{"hello":"world","arr":[1,2,3],"nested":{"k":"v"}}`)
	once, err := canonicalizeJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := canonicalizeJSON(once)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("not idempotent: once=%s twice=%s", once, twice)
	}
}

func TestCanonicalize_Numbers(t *testing.T) {
	cases := []struct{ in, want string }{
		{`0`, `0`},
		{`-0`, `0`},
		{`0.0`, `0`},
		{`1`, `1`},
		{`1.0`, `1`},
		{`-1`, `-1`},
		{`1.5`, `1.5`},
		{`100`, `100`},
		{`1000000`, `1000000`},
		{`0.0000001`, `1e-7`},
		{`1e10`, `10000000000`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := canonicalizeJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("canonicalize(%q): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonicalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalize_Numbers_Boundary1e21(t *testing.T) {
	// At/above 1e21 ECMAScript switches to exponent form. normalizeExponent
	// strips the '+' so the canonical form is "1e21" (no sign).
	got, err := canonicalizeJSON([]byte(`1e21`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "1e21" {
		t.Fatalf("got %q, want 1e21", got)
	}
}

func TestCanonicalize_StringEscapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"hello"`, `"hello"`},
		{`"with \"quotes\""`, `"with \"quotes\""`},
		{`"back\\slash"`, `"back\\slash"`},
		{`"line\nbreak"`, `"line\nbreak"`},
		{`"tab\there"`, `"tab\there"`},
		{`"unicode: héllo 🌍"`, `"unicode: héllo 🌍"`},
		{`"<&>"`, `"<&>"`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := canonicalizeJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("canonicalize(%q): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCanonicalize_ControlChars(t *testing.T) {
	in := []byte{0x22, 0x5c, 0x75, 0x30, 0x30, 0x30, 0x31, 0x22}
	got, err := canonicalizeJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x22, 0x5c, 0x75, 0x30, 0x30, 0x30, 0x31, 0x22}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCanonicalize_Arrays(t *testing.T) {
	got, err := canonicalizeJSON([]byte(`[3,1,2]`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[3,1,2]` {
		t.Fatalf("array order changed: %s", got)
	}
}

func TestCanonicalize_BoolNull(t *testing.T) {
	got, err := canonicalizeJSON([]byte(`{"t":true,"f":false,"n":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"f":false,"n":null,"t":true}` {
		t.Fatalf("got %s", got)
	}
}

func TestCanonicalize_Invalid(t *testing.T) {
	cases := []string{``, `not json`, `{`, `{,}`, `{"a":1`, `1 2`}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := canonicalizeJSON([]byte(c)); err == nil {
				t.Fatalf("expected error for %q", c)
			}
		})
	}
}

func TestCanonicalize_AgentCardLike(t *testing.T) {
	in := []byte(`{
        "version": "1.0",
        "name": "echo",
        "supportedInterfaces": [
            {"url": "https://shim.example.com:8443/a2a", "protocolBinding": "JSON_RPC_2_0", "protocolVersion": "1.0"}
        ],
        "capabilities": {"streaming": false, "pushNotifications": false, "extendedAgentCard": false}
    }`)
	once, err := canonicalizeJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := canonicalizeJSON(once)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("agent-card-like not idempotent: %s vs %s", once, twice)
	}
	if !bytes.HasPrefix(once, []byte(`{"capabilities":`)) {
		t.Fatalf("expected first key 'capabilities', got: %s", once)
	}
}
