package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newEC() (*ecdsa.PrivateKey, string) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv, "test-kid"
}

func jwksFixture(t *testing.T, priv *ecdsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	xb := make([]byte, 32)
	yb := make([]byte, 32)
	priv.PublicKey.X.FillBytes(xb)
	priv.PublicKey.Y.FillBytes(yb)
	body := `{"keys":[{"kty":"EC","crv":"P-256","alg":"ES256","use":"sig","kid":"` + kid + `","x":"` +
		base64.RawURLEncoding.EncodeToString(xb) + `","y":"` +
		base64.RawURLEncoding.EncodeToString(yb) + `"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func signES256(t *testing.T, priv *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTValidator_HappyPath(t *testing.T) {
	priv, kid := newEC()
	srv := jwksFixture(t, priv, kid)

	v, err := NewJWTValidator(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tokStr := signES256(t, priv, kid, jwt.MapClaims{
		"sub":   "alice",
		"scope": "agent.read agent.write",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	})
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Bearer "+tokStr)

	p, err := v.Validate(req)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Sub != "alice" {
		t.Errorf("sub = %q", p.Sub)
	}
	if len(p.Scopes) != 2 || p.Scopes[0] != "agent.read" {
		t.Errorf("scopes = %v", p.Scopes)
	}
}

func TestJWTValidator_MissingHeader(t *testing.T) {
	priv, kid := newEC()
	srv := jwksFixture(t, priv, kid)
	v, _ := NewJWTValidator(srv.URL)

	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	_, err := v.Validate(req)
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Errorf("expected 401 AuthError, got %v", err)
	}
}

func TestJWTValidator_NotBearerScheme(t *testing.T) {
	priv, kid := newEC()
	srv := jwksFixture(t, priv, kid)
	v, _ := NewJWTValidator(srv.URL)

	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, err := v.Validate(req)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestJWTValidator_ExpiredToken(t *testing.T) {
	priv, kid := newEC()
	srv := jwksFixture(t, priv, kid)
	v, _ := NewJWTValidator(srv.URL)

	tokStr := signES256(t, priv, kid, jwt.MapClaims{
		"sub": "alice",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Bearer "+tokStr)

	_, err := v.Validate(req)
	if err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestJWTValidator_UnknownKID(t *testing.T) {
	priv, kid := newEC()
	srv := jwksFixture(t, priv, kid)
	v, _ := NewJWTValidator(srv.URL)

	tokStr := signES256(t, priv, "other-kid", jwt.MapClaims{
		"sub": "alice",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Bearer "+tokStr)

	_, err := v.Validate(req)
	if err == nil {
		t.Fatal("expected unknown-kid error")
	}
}

func TestJWTValidator_JWKSDown_Returns503(t *testing.T) {
	v, err := NewJWTValidator("http://127.0.0.1:1/nope")
	if err != nil {
		t.Fatal(err)
	}
	v.HTTP.Timeout = 200 * time.Millisecond

	// Forge a token; we can't sign it usefully, but the parser will try
	// to look up the key first.
	tokStr := "eyJhbGciOiJFUzI1NiIsImtpZCI6ImtpZCIsInR5cCI6IkpXVCJ9." +
		"e30." +
		"AAA"
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Bearer "+tokStr)

	_, err = v.Validate(req)
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %v (status=%d)", err, func() int {
			if ae != nil {
				return ae.Status
			}
			return -1
		}())
	}
}

func TestMiddleware_AttachesPrincipal(t *testing.T) {
	priv, kid := newEC()
	srv := jwksFixture(t, priv, kid)
	v, _ := NewJWTValidator(srv.URL)

	tokStr := signES256(t, priv, kid, jwt.MapClaims{
		"sub":   "bob",
		"scope": "agent.read",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	var seen Principal
	h := Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			t.Error("no principal in context")
		}
		seen = p
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Bearer "+tokStr)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if seen.Sub != "bob" {
		t.Errorf("Sub = %q", seen.Sub)
	}
}

func TestMiddleware_RejectsWithWWWAuthenticate(t *testing.T) {
	v := NoopFailingValidator{}
	h := Middleware(v)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream handler should not be called")
	}))
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("missing WWW-Authenticate header: %q", w.Header().Get("WWW-Authenticate"))
	}
}

func TestNoopValidator_Passes(t *testing.T) {
	v := NoopValidator{}
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	p, err := v.Validate(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Sub != "dev" {
		t.Errorf("Sub = %q", p.Sub)
	}
}

// NoopFailingValidator always denies; used to exercise the 401 path.
type NoopFailingValidator struct{}

func (NoopFailingValidator) Validate(r *http.Request) (Principal, error) {
	return Principal{}, &AuthError{Status: http.StatusUnauthorized, Msg: "denied"}
}
