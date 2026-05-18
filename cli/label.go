package cli

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// validateLabel rejects label inputs that would cause path traversal,
// hidden-state collision, or filesystem-encoding ambiguity when the
// label is interpolated into paths like
//
//	<cwd>/.sesh/sessions/<label>.repo
//	<cwd>/.sesh/checkouts/<label>/
//
// Every operator entrypoint that consumes a `<label>` argument MUST call
// validateLabel BEFORE any path math. The validator is the tier-1 safety
// gate: adjacent state under .sesh/ (sessions/, checkouts/, messaging/)
// is only safe so long as a label can never escape its slot.
//
// Rules:
//
//   - non-empty after TrimSpace
//   - valid UTF-8, no NUL bytes
//   - no path separators ('/' or '\\')
//   - not "." or ".."
//   - no leading '.' (forbid dotfile labels — they collide with .fslckout,
//     .git, and similar; also blocks "..foo" etc.)
//   - no embedded ".." (defense in depth — a future change that joined
//     the label with a sibling segment would otherwise re-introduce
//     traversal)
//   - length cap at 128 chars (filesystem-portable; the JetStream
//     storeDir under .sesh/sessions/<label>.messaging adds suffix bytes,
//     so we leave headroom under ext4's 255-byte basename limit)
//   - no whitespace or control characters
//
// Returns a wrapped error naming the offending input. Callers may surface
// the message directly to operators.
func validateLabel(label string) error {
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("label is empty")
	}
	if label != strings.TrimSpace(label) {
		return fmt.Errorf("label %q has leading or trailing whitespace", label)
	}
	if !utf8.ValidString(label) {
		return fmt.Errorf("label %q is not valid UTF-8", label)
	}
	if strings.ContainsRune(label, 0x00) {
		return fmt.Errorf("label %q contains a NUL byte", label)
	}
	if len(label) > 128 {
		return fmt.Errorf("label is %d bytes; max 128", len(label))
	}
	if strings.ContainsAny(label, "/\\") {
		return fmt.Errorf("label %q contains a path separator", label)
	}
	if label == "." || label == ".." {
		return fmt.Errorf("label %q is a reserved path segment", label)
	}
	if strings.HasPrefix(label, ".") {
		return fmt.Errorf("label %q starts with '.' (dotfile labels collide with .fslckout, .git, etc.)", label)
	}
	if strings.Contains(label, "..") {
		return fmt.Errorf("label %q contains '..'", label)
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("label %q contains a control character (rune %U)", label, r)
		}
	}
	return nil
}
