// Package push implements the shim-side credential encryption,
// SSRF defense, and JetStream-backed delivery worker for A2A
// PushNotificationConfig (Slice 6).
//
// Crypto contract.
//
// EncryptCredentials wraps plaintext in an "enc:" envelope using
// AES-256-GCM with a 32-byte shim-held key:
//
//	"enc:" + base64.StdEncoding.Encode(nonce || ciphertext || tag)
//
// The 4-byte "enc:" prefix is the smallest forward-compat marker —
// distinguishes new code path from legacy raw plaintext that sesh-ops
// stored before Slice 6. DecryptCredentials tolerates pre-Slice-6
// values by returning the original string plus ErrLegacyPlaintext;
// callers WARN-log once and pass the value through. Future envelope
// shapes extend with "enc2:" rather than mutating "enc:".
//
// Stdlib only — no third-party crypto.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// envelopePrefix is the 4-byte marker for a Slice-6+ AES-GCM envelope.
// Kept as a const so the cost of changing it is grep-able (every site
// that reads it must change together).
const envelopePrefix = "enc:"

// keySize is the required AES key length (256-bit / 32 bytes).
const keySize = 32

// ErrLegacyPlaintext is returned by DecryptCredentials when the input
// lacks the "enc:" prefix. The caller receives the original string
// alongside this sentinel so the legacy value can still be used for
// delivery while ops gets a WARN signal that the record predates
// Slice 6 and should be re-Set to migrate.
var ErrLegacyPlaintext = errors.New("push: legacy plaintext credentials (no enc: prefix)")

// EncryptCredentials returns "enc:<base64(nonce||ct||tag)>". An empty
// plaintext returns "" (so empty Auth.Credentials round-trips
// transparently — callers don't have to special-case "no auth").
func EncryptCredentials(plaintext string, key []byte) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) != keySize {
		return "", fmt.Errorf("push: key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("push: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("push: new gcm: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("push: rand nonce: %w", err)
	}
	// Seal appends ciphertext+tag to the nonce buffer in one shot.
	out := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return envelopePrefix + base64.StdEncoding.EncodeToString(out), nil
}

// DecryptCredentials reverses EncryptCredentials. On legacy values
// (no "enc:" prefix), returns the input verbatim plus ErrLegacyPlaintext
// so the caller can pass the value through to the webhook while
// surfacing the migration signal once.
func DecryptCredentials(envelope string, key []byte) (string, error) {
	if envelope == "" {
		return "", nil
	}
	if !strings.HasPrefix(envelope, envelopePrefix) {
		return envelope, ErrLegacyPlaintext
	}
	if len(key) != keySize {
		return "", fmt.Errorf("push: key must be %d bytes, got %d", keySize, len(key))
	}
	body := envelope[len(envelopePrefix):]
	blob, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", fmt.Errorf("push: decode envelope: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("push: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("push: new gcm: %w", err)
	}
	ns := aead.NonceSize()
	if len(blob) < ns+aead.Overhead() {
		return "", errors.New("push: envelope too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("push: gcm open: %w", err)
	}
	return string(plain), nil
}

// LoadKey resolves a key reference into the 32 raw bytes. src is
// either:
//
//   - a 64-char hex literal (when the operator wired the key via env);
//   - a path to a file whose first line is a 64-char hex literal
//     (when the operator wired --push-encryption-key as a file path).
//
// The heuristic: 64 hex chars after trimming whitespace ⇒ literal;
// otherwise treat as file path. This means a 64-char filename of
// only hex digits would be misread as a literal — operationally
// implausible (operators name key files like push.key) and rejecting
// it here would force the env vs file branching up into the CLI.
func LoadKey(src string) ([]byte, error) {
	s := strings.TrimSpace(src)
	if s == "" {
		return nil, errors.New("push: empty key source")
	}
	if len(s) == 2*keySize && isHex(s) {
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("push: decode hex literal: %w", err)
		}
		return b, nil
	}
	// Refuse to load a key file readable by group or other (mode bits
	// 0o077). The shim's only durable confidentiality control is this
	// key; loose perms void the entire envelope. Match the ssh /
	// kubectl precedent.
	info, statErr := os.Stat(s)
	if statErr != nil {
		return nil, fmt.Errorf("push: stat key file %q: %w", s, statErr)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("push: key file %q has loose permissions %o (refuses group/world readable; chmod 0600)", s, mode)
	}
	body, err := os.ReadFile(s)
	if err != nil {
		return nil, fmt.Errorf("push: read key file %q: %w", s, err)
	}
	// First non-empty line; trims trailing newlines that text editors add.
	first := strings.TrimSpace(string(body))
	// If the file embeds extra lines (comments, key id), take only the
	// first whitespace-bounded token. Operators expecting "header lines"
	// will surface a hex-decode error and re-format — we don't silently
	// scan further.
	if i := strings.IndexAny(first, " \t\n\r"); i >= 0 {
		first = first[:i]
	}
	if len(first) != 2*keySize || !isHex(first) {
		return nil, fmt.Errorf("push: key file %q: expected %d-char hex, got %d chars", s, 2*keySize, len(first))
	}
	b, err := hex.DecodeString(first)
	if err != nil {
		return nil, fmt.Errorf("push: decode key file: %w", err)
	}
	return b, nil
}

// NewDevKey returns 32 fresh bytes from crypto/rand. The shim CLI uses
// this when --dev is set AND no --push-encryption-key was provided,
// so dev loops don't need a persisted key. WARN'd by main with a
// kid hash so re-deploys without a stable key are visible.
func NewDevKey() ([]byte, error) {
	b := make([]byte, keySize)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("push: rand dev key: %w", err)
	}
	return b, nil
}

// isHex returns true when every rune in s is in [0-9a-fA-F]. Inlined
// because hex.DecodeString accepts the same set but its error path
// (returning a typed error) is less convenient for the length-first
// dispatch in LoadKey.
func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
