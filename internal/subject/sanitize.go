package subject

import (
	"strings"
	"unicode/utf16"
)

// SanitizeToken normalizes a raw string into a single NATS subject
// token under the canonical Axis-A subject-token rule:
//
//  1. Replace each disallowed char (NOT in [A-Za-z0-9_-]) with one '-'
//     PER UTF-16 code unit it occupies: 1 for a BMP char, 2 for an
//     astral/supplementary-plane char (e.g. emoji). So "a😀b" → "a--b".
//  2. Lowercase (ASCII).
//  3. Trim leading/trailing '-' (one or more). Internal '-' runs are
//     preserved — they are NOT collapsed.
//
// An empty result is returned empty; callers apply their own per-token
// fallback (e.g. "local", "default", "worker"). This function does not
// substitute a fallback itself.
//
// This is the single canonical Axis-A subject-token sanitizer for the
// Go side. It is byte-identical to the deployed TypeScript SDK's
// sanitizeSubjectToken, which uses the regex /[^a-zA-Z0-9_-]/g WITHOUT
// the `u` flag — so the regex matches UTF-16 code units, and a
// surrogate-pair char emits two '-'. The per-code-unit rule above is
// what reproduces that exactly; iterating runes (one '-' per disallowed
// rune) would diverge on astral chars. The shared golden vectors in
// testdata/sanitize-vectors.json pin this contract for both stacks.
//
// CROSS-STACK NOTE: a future TS consolidation MUST NOT add the `u` flag
// (which would switch TS to code-point semantics, one '-' per astral
// char) without simultaneously updating this Go impl AND the shared
// fixture. The non-`u`/per-code-unit behavior is the contract.
//
// NOTE: bucket names use a DIFFERENT rule (see sesh-ops/scope) — do not
// route bucket-name sanitization through this function.
func SanitizeToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		// Disallowed: emit one '-' per UTF-16 code unit, matching the
		// non-`u` TS regex. Invalid runes (RuneLen == -1) map to a
		// single '-', mirroring JS's U+FFFD replacement (one code unit).
		n := utf16.RuneLen(r)
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			b.WriteByte('-')
		}
	}
	return strings.Trim(strings.ToLower(b.String()), "-")
}
