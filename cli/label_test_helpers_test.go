package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fingerprintTree walks root and returns a deterministic string keyed on
// (relative-path, mode-perm-bits, regular-file-content-hash). Used by the
// up/down tier-1 traversal tests to assert hostile inputs never mutate the
// .sesh/ tree. Errors during the walk are folded into the fingerprint as
// `ERR:<path>:<msg>` lines so the diff is human-readable when the
// assertion fails.
func fingerprintTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			lines = append(lines, "ERR:"+path+":"+walkErr.Error())
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			lines = append(lines, "D:"+rel)
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			lines = append(lines, "ERR:"+rel+":"+infoErr.Error())
			return nil
		}
		if !d.Type().IsRegular() {
			lines = append(lines, "S:"+rel+":"+info.Mode().String())
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			lines = append(lines, "ERR:"+rel+":"+readErr.Error())
			return nil
		}
		sum := sha256.Sum256(data)
		lines = append(lines, "F:"+rel+":"+info.Mode().Perm().String()+":"+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// Shared hostile-label inputs for the tier-1 traversal tests:
//
//   - TestSeshUp_RejectsLabelTraversal
//   - TestSeshDown_RejectsLabelTraversal
//
// validateLabel is the single safety gate every label-consuming entrypoint
// funnels through; the same matrix MUST fail at every door. Keeping the
// inputs in one table — instead of five drifting copies — guarantees that
// adding a hostile case (e.g. a new Unicode confusable) instantly covers
// every entrypoint.
//
// Each test still owns its own tier-1-paths-survive invariant via
// fingerprintTree; this helper only shares the inputs.
//
// Unicode rows pin the ASCII-only stance: the validator iterates runes
// with no canonical normalisation, so visually-similar lookalikes
// (U+2025 two-dot-leader; U+FF0E fullwidth full stop) fall through the
// same code path that rejects raw "..". Asserting the rejection prevents
// a future "be friendly to international users" PR from silently
// relaxing the gate.
var hostileLabelInputs = []struct {
	Name  string
	Label string
}{
	{"empty", ""},
	{"dot", "."},
	{"dotdot", ".."},
	{"slash_prefix", "/etc"},
	{"slash_embedded", "foo/bar"},
	{"backslash_embedded", "foo\\bar"},
	{"dotdot_embedded", "alpha/../beta"},
	{"dotdot_only_embedded", "x..y"},
	{"nul_byte", "alpha\x00beta"},
	{"leading_dot", ".sessions"},
	{"whitespace_only", "   "},
	{"control_char", "alpha\x01"},
	{"newline", "alpha\nbeta"},
	{"parent_sessions", "../sessions"},
	{"deeper_traversal", "../../foo"},
	// Unicode confusables — visually similar to ".." / "." but the
	// validator runs on bytes-via-rune-iteration with no canonical
	// normalisation, so the ASCII-only regex rejects them by the
	// same code path that rejects raw "..".
	{"unicode_two_dot_leader", "alpha‥beta"},   // U+2025 two-dot-leader
	{"unicode_fullwidth_dot_prefix", "．alpha"}, // U+FF0E fullwidth full stop
	{"unicode_fullwidth_dot_embedded", "alpha．beta"},
}
