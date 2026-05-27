package subject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sanitizeVectorFile is the shared golden-vector fixture. The future
// TS conformance test reuses this exact JSON, so any change here must
// keep both implementations in lockstep.
type sanitizeVectorFile struct {
	Rule    string           `json:"rule"`
	Vectors []sanitizeVector `json:"vectors"`
}

type sanitizeVector struct {
	In  string `json:"in"`
	Out string `json:"out"`
}

func TestSanitizeToken_GoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sanitize-vectors.json"))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var f sanitizeVectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal golden vectors: %v", err)
	}
	if len(f.Vectors) == 0 {
		t.Fatal("golden vector file contained no vectors")
	}
	for _, v := range f.Vectors {
		got := SanitizeToken(v.In)
		if got != v.Out {
			t.Errorf("SanitizeToken(%q) = %q; want %q", v.In, got, v.Out)
		}
	}
}

func TestSanitizeToken_Idempotent(t *testing.T) {
	inputs := []string{
		"M4-MacBook.local",
		"My Proj",
		"feature/X",
		"-leading-and-trailing-",
		"a..b",
		"UPPER_snake",
	}
	for _, in := range inputs {
		once := SanitizeToken(in)
		twice := SanitizeToken(once)
		if twice != once {
			t.Errorf("SanitizeToken not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

func TestSanitizeToken_DirectAssertions(t *testing.T) {
	cases := map[string]string{
		"MyProj":  "myproj",
		"My Proj": "my-proj",
		"a..b":    "a--b", // internal '-' runs preserved, not collapsed
		"":        "",
	}
	for in, want := range cases {
		if got := SanitizeToken(in); got != want {
			t.Errorf("SanitizeToken(%q) = %q; want %q", in, got, want)
		}
	}
}
