package coord

import (
	"fmt"
	"regexp"
)

// tokenIllegalCharRE matches any single character that is illegal inside a
// NATS subject token. We allow [A-Za-z0-9_-]; everything else (dot,
// space, control, `*`, `>`, `/`, `$`, ...) is forbidden. The check
// is per-character so we can report the offending input clearly.
//
// Note: '.' is rejected at the token level. NATS uses '.' as the
// token separator; a dot inside a token would split into two and
// silently corrupt the subject.
var tokenIllegalCharRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// validateToken returns nil iff s is a legal single token under the
// sesh coordination-subject rules:
//
//   - non-empty
//   - 1-63 bytes (subject limit per NATS, with headroom)
//   - chars in [A-Za-z0-9_-]
//   - no leading '$' (NATS reserves `$SYS.>`, `$SRV.>`, `$JS.>`)
//   - no whitespace
//   - no '.' (token separator)
//
// The 63-byte cap is intentional: NATS subject tokens have no documented
// hard limit, but 63 bytes gives ample room for UUIDs, hex IDs, and
// human-readable names while leaving space for the other six tokens in
// a subject whose total byte budget is 1024.
//
// Wildcard tokens (`*`, `>`) are NOT accepted here — those belong in
// Filter, which calls validateTokenOrWildcard.
func validateToken(s string) error {
	if s == "" {
		return fmt.Errorf("token is empty")
	}
	if len(s) > 63 {
		return fmt.Errorf("token %q is %d bytes; max 63", s, len(s))
	}
	if s[0] == '$' {
		return fmt.Errorf("token %q has leading '$' (reserved for NATS system subjects)", s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			return fmt.Errorf("token %q contains whitespace at byte %d", s, i)
		case c == '.':
			return fmt.Errorf("token %q contains dot at byte %d (subject separator, not a token char)", s, i)
		}
	}
	if loc := tokenIllegalCharRE.FindStringIndex(s); loc != nil {
		return fmt.Errorf("token %q contains illegal char %q at byte %d", s, s[loc[0]:loc[1]], loc[0])
	}
	return nil
}

// validateTokenOrWildcard is validateToken plus the NATS wildcard
// tokens `*` (single-token) and `>` (recursive, tail-only). Filter
// uses this; Subject uses validateToken.
//
// `>` is only legal in the final position of the subject; positional
// enforcement lives in Filter.Validate (which has the position
// context), not here.
func validateTokenOrWildcard(s string) error {
	if s == "*" || s == ">" {
		return nil
	}
	return validateToken(s)
}
