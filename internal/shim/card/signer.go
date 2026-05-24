package card

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// p256CoordLen is the byte length of one P-256 affine coordinate.
// JWS ES256 signatures are R || S concat at this width, and JWKS
// publishes x/y at the same width left-padded with zeros.
const p256CoordLen = 32

// Signer holds the shim's ES256 keypair and signs L1+L2 AgentCards by
// JCS-canonicalizing the card (minus signatures), then producing a
// JWS ES256 signature attached as cards[0].signatures[0].
type Signer struct {
	priv *ecdsa.PrivateKey
	pub  *ecdsa.PublicKey
	kid  string
}

// LoadSigner reads a PEM-encoded EC private key from path. If kid is empty,
// it is auto-derived (SHA-256 of the marshaled public key, first 16 hex chars).
func LoadSigner(path, kid string) (*Signer, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key %s: %w", path, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("decode PEM: no block in %s", path)
	}
	var priv *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		priv, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return nil, fmt.Errorf("parse PKCS8: %w", e)
		}
		var ok bool
		priv, ok = k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("signing key is not an ECDSA private key")
		}
	default:
		return nil, fmt.Errorf("unexpected PEM block type %q", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("parse EC private key: %w", err)
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("expected P-256 (ES256) key, got %s", priv.Curve.Params().Name)
	}
	return newSigner(priv, kid)
}

// NewDevSigner generates an ephemeral P-256 keypair in memory. Intended
// for --dev mode only; the kid is auto-derived and the key is lost on
// process restart.
func NewDevSigner() (*Signer, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate dev signing key: %w", err)
	}
	return newSigner(priv, "")
}

func newSigner(priv *ecdsa.PrivateKey, kid string) (*Signer, error) {
	if kid == "" {
		pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("marshal public key: %w", err)
		}
		sum := sha256.Sum256(pubBytes)
		kid = fmt.Sprintf("%x", sum[:8])
	}
	return &Signer{priv: priv, pub: &priv.PublicKey, kid: kid}, nil
}

// Loaded reports whether the signer has a private key. Used by /healthz.
func (s *Signer) Loaded() bool { return s != nil && s.priv != nil }

// KID returns the key ID used in the JWS protected header and JWKS.
func (s *Signer) KID() string { return s.kid }

// JWKS returns the public-key JWKS document for /.well-known/jwks.json.
func (s *Signer) JWKS() ([]byte, error) {
	if s == nil || s.pub == nil {
		return nil, errors.New("signer has no public key")
	}
	xb := padLeft(s.pub.X.Bytes(), p256CoordLen)
	yb := padLeft(s.pub.Y.Bytes(), p256CoordLen)
	jwks := map[string]any{
		"keys": []any{
			map[string]any{
				"kty": "EC",
				"crv": "P-256",
				"alg": "ES256",
				"use": "sig",
				"kid": s.kid,
				"x":   base64.RawURLEncoding.EncodeToString(xb),
				"y":   base64.RawURLEncoding.EncodeToString(yb),
			},
		},
	}
	return json.Marshal(jwks)
}

// SignCard produces the bytes of the signed AgentCard. The input is mutated
// only in that its Signatures field is replaced with the new signature; if
// the caller needs the original, deep-copy beforehand.
func (s *Signer) SignCard(card *a2a.AgentCard) ([]byte, error) {
	if s == nil || s.priv == nil {
		return nil, errors.New("signer has no private key")
	}
	if card == nil {
		return nil, errors.New("nil card")
	}
	card.Signatures = nil
	unsignedJSON, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("marshal card: %w", err)
	}
	canonical, err := canonicalizeJSON(unsignedJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize: %w", err)
	}
	protected, signature, err := s.SignDetached(canonical)
	if err != nil {
		return nil, err
	}
	card.Signatures = []a2a.AgentCardSignature{{
		Protected: protected,
		Signature: signature,
	}}
	return json.Marshal(card)
}

// SignDetached signs canonicalized payload bytes as JWS ES256 with a
// detached payload. Returns the base64url-encoded protected header and
// signature strings. RFC 7515 + RFC 7518.
func (s *Signer) SignDetached(payload []byte) (protected, signature string, err error) {
	header := map[string]string{
		"alg": "ES256",
		"kid": s.kid,
		"typ": "JWT",
	}
	hdrBytes, err := json.Marshal(header)
	if err != nil {
		return "", "", fmt.Errorf("marshal protected header: %w", err)
	}
	protected = base64.RawURLEncoding.EncodeToString(hdrBytes)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := protected + "." + payloadB64
	sum := sha256.Sum256([]byte(signingInput))
	r, ss, err := ecdsa.Sign(rand.Reader, s.priv, sum[:])
	if err != nil {
		return "", "", fmt.Errorf("ecdsa sign: %w", err)
	}
	signature = base64.RawURLEncoding.EncodeToString(concatRS(r, ss, p256CoordLen))
	return protected, signature, nil
}

// VerifyDetached re-derives the JWS signing input from the canonicalized
// payload and verifies the signature with the signer's public key. Returns
// nil on success; used by tests and by external callers that want to
// validate cards signed by this Signer.
func (s *Signer) VerifyDetached(payload []byte, protected, signature string) error {
	if s == nil || s.pub == nil {
		return errors.New("signer has no public key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if len(sig) != 2*p256CoordLen {
		return fmt.Errorf("expected %d-byte ES256 signature, got %d", 2*p256CoordLen, len(sig))
	}
	r := new(big.Int).SetBytes(sig[:p256CoordLen])
	ss := new(big.Int).SetBytes(sig[p256CoordLen:])
	signingInput := protected + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	if !ecdsa.Verify(s.pub, sum[:], r, ss) {
		return errors.New("ecdsa verify failed")
	}
	return nil
}

func concatRS(r, s *big.Int, size int) []byte {
	out := make([]byte, 2*size)
	rb := r.Bytes()
	sb := s.Bytes()
	copy(out[size-len(rb):size], rb)
	copy(out[2*size-len(sb):], sb)
	return out
}

func padLeft(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}
