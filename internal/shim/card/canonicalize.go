// Package card composes, signs, and caches A2A AgentCards.
//
// canonicalizeJSON implements RFC 8785 (JCS) — JSON Canonicalization
// Scheme — so that AgentCard JWS signatures are stable across emitters
// that re-serialize the card. Kept stdlib-only (~150 LOC) to avoid an
// extra dependency for a small, well-specified primitive.
package card

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ErrInvalidJSON is returned by canonicalizeJSON when input is not valid JSON.
var ErrInvalidJSON = errors.New("card: input is not valid JSON")

// canonicalizeJSON returns the RFC 8785 (JCS) canonical form of input.
// Input must be valid JSON; otherwise ErrInvalidJSON is returned.
//
// Implementation notes:
//   - Object keys sort by UTF-16 code-unit ordering per RFC 8785 §3.2.3.
//   - Numbers use ECMAScript Number.prototype.toString form (RFC 8785 §3.2.2.3):
//     shortest round-trip decimal; integers without fractional part; exponent
//     only when shorter; lowercase 'e'; no '+' sign in exponent.
//   - Strings emit minimal escapes (RFC 8785 §3.2.2.2): only \", \\, and
//     control characters U+0000..U+001F (using \uXXXX for the rest of those).
//     All other code points emitted as literal UTF-8.
func canonicalizeJSON(input []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidJSON)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, x)
	case json.Number:
		s, err := canonicalNumber(string(x))
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return utf16Less(keys[i], keys[j])
		})
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("card: unexpected type %T", v)
	}
	return nil
}

func utf16Less(a, b string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	n := len(ua)
	if len(ub) < n {
		n = len(ub)
	}
	for i := 0; i < n; i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// writeCanonicalString emits a JSON string with RFC 8785 §3.2.2.2 escaping:
// \" and \\ for those two, \b \f \n \r \t for the named controls,
// \u00XX for any other U+0000..U+001F, literal UTF-8 for everything else.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// canonicalNumber returns the RFC 8785 §3.2.2.3 canonical form of a JSON
// number. The input is the raw token from json.Number (already validated
// as a JSON number by the decoder).
func canonicalNumber(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("card: empty number")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("card: parse number %q: %w", s, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("card: non-finite number %q not representable in JSON", s)
	}
	if f == 0 {
		return "0", nil
	}

	abs := math.Abs(f)

	// Integer fast path.
	if f == math.Trunc(f) && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}

	// ECMAScript Number.prototype.toString uses fixed notation when
	// 1e-6 <= abs(value) < 1e21, otherwise exponential. strconv 'g'
	// approximates this; we normalize the result to JCS form below.
	var out string
	if abs >= 1e-6 && abs < 1e21 {
		out = strconv.FormatFloat(f, 'f', -1, 64)
	} else {
		out = strconv.FormatFloat(f, 'e', -1, 64)
	}
	return normalizeExponent(out), nil
}

// normalizeExponent rewrites Go's exponent form into JCS form:
//   - lowercase 'e'
//   - no '+' sign on positive exponents
//   - no leading zeros in the exponent
//   - mantissa keeps a single digit before the decimal point; drop the
//     decimal point entirely if the mantissa has no fractional digits
//     (so "1.e10" → "1e10").
func normalizeExponent(s string) string {
	idx := strings.IndexAny(s, "eE")
	if idx < 0 {
		return s
	}
	mant := s[:idx]
	exp := s[idx+1:]

	// Trim trailing fractional zeros and a trailing '.' from the mantissa.
	if strings.Contains(mant, ".") {
		mant = strings.TrimRight(mant, "0")
		mant = strings.TrimRight(mant, ".")
	}

	// Normalize exponent: drop '+', strip leading zeros (preserving sign).
	neg := false
	if strings.HasPrefix(exp, "+") {
		exp = exp[1:]
	} else if strings.HasPrefix(exp, "-") {
		neg = true
		exp = exp[1:]
	}
	exp = strings.TrimLeft(exp, "0")
	if exp == "" {
		exp = "0"
	}
	if neg {
		exp = "-" + exp
	}
	return mant + "e" + exp
}
