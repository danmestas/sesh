package push

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKey returns a deterministic 32-byte key for tests.
func testKey(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("decode test key: %v", err)
	}
	return b
}

// TestEncryptDecrypt_RoundTrip walks the major plaintext shapes —
// empty, ASCII, unicode, 1 KB — and confirms decrypt(encrypt(x)) == x.
// Critical correctness backbone: a regression here corrupts every
// webhook delivery.
func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := testKey(t)
	cases := []struct {
		name      string
		plaintext string
	}{
		{"empty", ""},
		{"ascii", "hunter2"},
		{"unicode", "pwd-π-字-🔑"},
		{"1KB", strings.Repeat("A", 1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := EncryptCredentials(tc.plaintext, key)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			got, err := DecryptCredentials(env, key)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if got != tc.plaintext {
				t.Errorf("got %q, want %q", got, tc.plaintext)
			}
		})
	}
}

// TestEncrypt_EmptyReturnsEmpty pins the "no auth" carve-out:
// encrypting "" produces "" so callers don't have to special-case
// the no-credentials path. Symmetric on the decrypt side via
// the empty-input early-return.
func TestEncrypt_EmptyReturnsEmpty(t *testing.T) {
	key := testKey(t)
	env, err := EncryptCredentials("", key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if env != "" {
		t.Errorf("encrypt(\"\") = %q, want \"\"", env)
	}
}

// TestEncrypt_NonceIsRandom encrypts the same plaintext twice and
// confirms the envelopes differ. AES-GCM with a fixed nonce is
// catastrophically broken; this test catches a regression that
// hardwires the nonce (e.g., for "determinism in tests").
func TestEncrypt_NonceIsRandom(t *testing.T) {
	key := testKey(t)
	a, err := EncryptCredentials("same", key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncryptCredentials("same", key)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("nonce reused: both encryptions produced %q", a)
	}
}

// TestEncrypt_HasEnvelopePrefix locks the wire shape — the "enc:"
// prefix is what DecryptCredentials uses to dispatch new-envelope vs
// legacy plaintext. Drift would break every Decrypt call on
// previously-Set records.
func TestEncrypt_HasEnvelopePrefix(t *testing.T) {
	key := testKey(t)
	env, err := EncryptCredentials("hunter2", key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env, "enc:") {
		t.Errorf("envelope %q missing enc: prefix", env)
	}
}

// TestDecrypt_LegacyPlaintext_ReturnsSentinel covers the pre-Slice-6
// migration path: a raw value (no "enc:" prefix) decrypts to itself
// + ErrLegacyPlaintext. Callers WARN-log and use the value as-is so
// existing deployments don't break on the cutover.
func TestDecrypt_LegacyPlaintext_ReturnsSentinel(t *testing.T) {
	key := testKey(t)
	got, err := DecryptCredentials("hunter2", key)
	if !errors.Is(err, ErrLegacyPlaintext) {
		t.Errorf("err = %v, want ErrLegacyPlaintext", err)
	}
	if got != "hunter2" {
		t.Errorf("got %q, want %q", got, "hunter2")
	}
}

// TestDecrypt_TamperedCiphertext_Fails flips one ciphertext byte and
// confirms GCM rejects the modified envelope. Authenticity is the
// reason we picked GCM over plain CTR — this test pins it.
func TestDecrypt_TamperedCiphertext_Fails(t *testing.T) {
	key := testKey(t)
	env, err := EncryptCredentials("hunter2", key)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a character inside the base64 body (after "enc:").
	bs := []byte(env)
	pos := len("enc:") + 5
	if bs[pos] == 'A' {
		bs[pos] = 'B'
	} else {
		bs[pos] = 'A'
	}
	_, err = DecryptCredentials(string(bs), key)
	if err == nil {
		t.Fatal("expected GCM open error on tampered envelope")
	}
	if errors.Is(err, ErrLegacyPlaintext) {
		t.Errorf("tampered envelope must not be treated as legacy: %v", err)
	}
}

// TestDecrypt_WrongKey_Fails confirms a 32-byte key mismatch fails
// (rather than silently producing garbage). Defense against a bad
// key-rotation rollout reading prior-key envelopes with a new key.
func TestDecrypt_WrongKey_Fails(t *testing.T) {
	keyA := testKey(t)
	env, err := EncryptCredentials("hunter2", keyA)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := hex.DecodeString("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecryptCredentials(env, keyB)
	if err == nil {
		t.Fatal("expected error decrypting with wrong key")
	}
}

// TestLoadKey_Hex_Literal pins the env-var path: a 64-char hex
// string resolves to the raw 32-byte key.
func TestLoadKey_Hex_Literal(t *testing.T) {
	literal := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	k, err := LoadKey(literal)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if len(k) != 32 {
		t.Errorf("len = %d, want 32", len(k))
	}
}

// TestLoadKey_File loads a key from a file whose contents are a
// 64-char hex literal possibly trailed by newlines. Covers the
// --push-encryption-key=/path/to/file path.
func TestLoadKey_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "push.key")
	body := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if len(k) != 32 {
		t.Errorf("len = %d, want 32", len(k))
	}
}

// TestLoadKey_File_RefusesLoosePerms — a key file with group/world
// read bits set is refused. The operator's `chmod 644 push.key`
// during a debug session would otherwise void the entire encryption
// envelope without warning.
func TestLoadKey_File_RefusesLoosePerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "push.key")
	body := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path); err == nil {
		t.Fatal("expected error on 0644 key file")
	}
}

// TestLoadKey_ShortHex_Rejected — a 63-char hex literal is not a
// valid 256-bit key. LoadKey rejects rather than zero-padding or
// hashing, since silent normalization would mask an operator typo.
func TestLoadKey_ShortHex_Rejected(t *testing.T) {
	// 63 chars - one short of a literal, won't be parsed as such.
	short := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde"
	_, err := LoadKey(short)
	if err == nil {
		t.Fatal("expected error on short hex literal")
	}
	// The literal branch's length check fails → falls to file path,
	// which also fails (no such file). Either error is acceptable;
	// just confirm the call surfaces *something*.
}

// TestNewDevKey_Returns32BytesAndIsUnique calls NewDevKey twice and
// confirms each invocation yields a fresh 32-byte buffer. Catches a
// stub regression (e.g., a zeroed key shipped for tests).
func TestNewDevKey_Returns32BytesAndIsUnique(t *testing.T) {
	a, err := NewDevKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 {
		t.Errorf("len(a) = %d, want 32", len(a))
	}
	b, err := NewDevKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 32 {
		t.Errorf("len(b) = %d, want 32", len(b))
	}
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Errorf("NewDevKey returned the same bytes twice: %x", a)
	}
}
