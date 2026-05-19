package cli_test

// Shared hostile-label inputs for the five tier-1 traversal tests:
//
//   - TestSeshUp_RejectsLabelTraversal
//   - TestSeshDown_RejectsLabelTraversal
//   - TestSeshWorktree_RejectsLabelTraversal
//   - TestSeshMaterialize_RejectsLabelTraversal
//   - TestSeshWorkerCwd_RejectsLabelTraversal
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
