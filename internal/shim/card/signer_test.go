package card

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestDevSigner_LoadedAndKID(t *testing.T) {
	s, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Loaded() {
		t.Fatal("dev signer not Loaded")
	}
	if s.KID() == "" || len(s.KID()) != 16 {
		t.Errorf("expected 16-hex-char kid, got %q", s.KID())
	}
}

func TestJWKS_Shape(t *testing.T) {
	s, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatalf("JWKS not valid JSON: %v", err)
	}
	keys, ok := got["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("expected keys[1], got %+v", got)
	}
	k := keys[0].(map[string]any)
	for _, field := range []string{"kty", "crv", "alg", "use", "kid", "x", "y"} {
		if _, ok := k[field]; !ok {
			t.Errorf("missing JWKS field %q", field)
		}
	}
	if k["kty"] != "EC" || k["crv"] != "P-256" || k["alg"] != "ES256" {
		t.Errorf("wrong key params: %+v", k)
	}
}

func TestSignDetached_RoundTrip(t *testing.T) {
	s, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"a":1,"b":"two"}`)
	prot, sig, err := s.SignDetached(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyDetached(payload, prot, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Tamper the payload — must fail.
	if err := s.VerifyDetached([]byte(`{"a":2,"b":"two"}`), prot, sig); err == nil {
		t.Fatal("tampered payload verified — expected failure")
	}
}

func TestSignDetached_ProtectedHeader(t *testing.T) {
	s, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	prot, _, err := s.SignDetached([]byte(`"x"`))
	if err != nil {
		t.Fatal(err)
	}
	hdrBytes, err := base64.RawURLEncoding.DecodeString(prot)
	if err != nil {
		t.Fatalf("protected not base64url: %v", err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		t.Fatalf("protected not JSON: %v", err)
	}
	if hdr["alg"] != "ES256" || hdr["typ"] != "JWT" || hdr["kid"] != s.KID() {
		t.Errorf("header = %+v", hdr)
	}
}

func TestSignDetached_StdlibVerifyCrossCheck(t *testing.T) {
	// Independently re-implement R||S decode + ecdsa.Verify to confirm we
	// emit the ES256 RFC 7518 format (R||S concat, not ASN.1 DER).
	s, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"ok":true}`)
	prot, sig, err := s.SignDetached(payload)
	if err != nil {
		t.Fatal(err)
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(rawSig) != 64 {
		t.Fatalf("expected 64-byte signature, got %d", len(rawSig))
	}
	r := new(big.Int).SetBytes(rawSig[:32])
	ss := new(big.Int).SetBytes(rawSig[32:])
	signingInput := prot + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	if !ecdsa.Verify(s.pub, sum[:], r, ss) {
		t.Fatal("hand-rolled ecdsa.Verify failed — signature format wrong")
	}
}

func TestSignCard_RoundTrip(t *testing.T) {
	s, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	card := &a2a.AgentCard{
		Name:    "echo",
		Version: "0.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface("https://shim.example.com/a2a", a2a.TransportProtocolJSONRPC),
		},
		Capabilities:       a2a.AgentCapabilities{},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []a2a.AgentSkill{},
	}
	signed, err := s.SignCard(card)
	if err != nil {
		t.Fatal(err)
	}

	// Parse the signed card; pull the signature off; re-canonicalize the
	// card-minus-signatures; verify against the signer's public key.
	var raw map[string]any
	if err := json.Unmarshal(signed, &raw); err != nil {
		t.Fatalf("signed card not JSON: %v", err)
	}
	sigsAny, ok := raw["signatures"].([]any)
	if !ok || len(sigsAny) != 1 {
		t.Fatalf("expected one signature, got %+v", raw["signatures"])
	}
	sig := sigsAny[0].(map[string]any)
	prot := sig["protected"].(string)
	sigStr := sig["signature"].(string)

	delete(raw, "signatures")
	rebuilt, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalizeJSON(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyDetached(canonical, prot, sigStr); err != nil {
		t.Fatalf("card signature failed verification: %v", err)
	}
}

func TestSignCard_NilSigner(t *testing.T) {
	var s *Signer
	_, err := s.SignCard(&a2a.AgentCard{})
	if err == nil || !strings.Contains(err.Error(), "no private key") {
		t.Errorf("nil signer should error, got: %v", err)
	}
}

func TestSignCard_NilCard(t *testing.T) {
	s, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignCard(nil); err == nil {
		t.Error("expected error for nil card")
	}
}
