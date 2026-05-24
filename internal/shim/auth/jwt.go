// Package auth implements the JWT/Bearer middleware for the shim's
// /a2a endpoint. The exported Validator interface lets dev mode (no
// real auth) and production (JWT against a JWKS URL) share one
// middleware shape.
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Validator is what every auth mode satisfies.
type Validator interface {
	Validate(r *http.Request) (Principal, error)
}

// Principal is the authenticated identity attached to a request context.
type Principal struct {
	Sub    string
	Scopes []string
}

type ctxKey struct{}

// FromContext extracts the Principal a Middleware put into ctx.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// installPrincipal is the low-level context-write used by Middleware
// (and re-exported via ExportForTest_InstallPrincipal for the authtest
// sibling subpackage).
func installPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// ExportForTest_InstallPrincipal is a deliberately ugly name so any
// production caller stands out in code review. Only authtest should
// call it.
func ExportForTest_InstallPrincipal(ctx context.Context, p Principal) context.Context {
	return installPrincipal(ctx, p)
}

// AuthError carries the HTTP status that a Validator wants the middleware
// to surface. Defaults to 401 if not provided.
type AuthError struct {
	Status int
	Msg    string
}

func (e *AuthError) Error() string { return e.Msg }

// Middleware wraps an http.Handler. On validation failure it writes the
// status the Validator returned (default 401 with WWW-Authenticate: Bearer)
// and an empty body. On success it stores Principal in the request context.
func Middleware(v Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := v.Validate(r)
			if err != nil {
				status := http.StatusUnauthorized
				var ae *AuthError
				if errors.As(err, &ae) && ae.Status != 0 {
					status = ae.Status
				}
				if status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="sesh-shim"`)
				}
				http.Error(w, err.Error(), status)
				return
			}
			next.ServeHTTP(w, r.WithContext(installPrincipal(r.Context(), p)))
		})
	}
}

// NoopValidator accepts every request with a placeholder principal.
// Usable ONLY when --auth=none-dev-only. Logs WARN per accepted request
// so production misconfigurations are loud.
type NoopValidator struct {
	Logger *slog.Logger
}

func (n NoopValidator) Validate(r *http.Request) (Principal, error) {
	log := n.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Warn("auth: none-dev-only bypass", "path", r.URL.Path, "remote", r.RemoteAddr)
	return Principal{Sub: "dev", Scopes: []string{"agent.read", "agent.write"}}, nil
}

// JWTValidator validates Authorization: Bearer <jwt> against a JWKS URL.
// Uses an in-process JWKS cache refreshed on miss or signature failure.
type JWTValidator struct {
	JWKSURL  string
	Audience string
	Issuer   string
	HTTP     *http.Client

	cache *jwksCache
}

// NewJWTValidator creates a validator that fetches keys from jwksURL.
// Returns an error if the URL is empty or has no scheme.
func NewJWTValidator(jwksURL string) (*JWTValidator, error) {
	if jwksURL == "" {
		return nil, fmt.Errorf("JWKS URL is empty")
	}
	u, err := url.Parse(jwksURL)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("invalid JWKS URL %q", jwksURL)
	}
	return &JWTValidator{
		JWKSURL: jwksURL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		cache:   &jwksCache{ttl: 5 * time.Minute},
	}, nil
}

func (v *JWTValidator) Validate(r *http.Request) (Principal, error) {
	hdr := r.Header.Get("Authorization")
	if hdr == "" {
		return Principal{}, &AuthError{Status: http.StatusUnauthorized, Msg: "missing Authorization header"}
	}
	tokStr := strings.TrimSpace(strings.TrimPrefix(hdr, "Bearer "))
	if tokStr == hdr || tokStr == "" {
		return Principal{}, &AuthError{Status: http.StatusUnauthorized, Msg: "expected Bearer token"}
	}

	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithExpirationRequired(),
	}
	if v.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.Issuer))
	}
	parser := jwt.NewParser(opts...)

	tok, err := parser.Parse(tokStr, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, kerr := v.keyForKID(r.Context(), kid)
		if kerr != nil {
			return nil, kerr
		}
		return key, nil
	})
	if err != nil {
		// Distinguish "keys unavailable" (503) from "bad token" (401).
		var ku *keysUnavailableError
		if errors.As(err, &ku) {
			return Principal{}, &AuthError{Status: http.StatusServiceUnavailable, Msg: "JWKS unavailable"}
		}
		return Principal{}, &AuthError{Status: http.StatusUnauthorized, Msg: "invalid token: " + err.Error()}
	}
	if !tok.Valid {
		return Principal{}, &AuthError{Status: http.StatusUnauthorized, Msg: "invalid token"}
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return Principal{}, &AuthError{Status: http.StatusUnauthorized, Msg: "claims wrong shape"}
	}
	if v.Audience != "" && !audienceMatches(claims["aud"], v.Audience) {
		return Principal{}, &AuthError{Status: http.StatusUnauthorized, Msg: "aud mismatch"}
	}
	sub, _ := claims["sub"].(string)
	scopes := scopesFromClaims(claims)
	return Principal{Sub: sub, Scopes: scopes}, nil
}

// audienceMatches accepts the JWT-spec forms of `aud`: a single string,
// or an array of strings. Returns true when want is found.
func audienceMatches(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if s == want {
				return true
			}
		}
	}
	return false
}

func scopesFromClaims(c jwt.MapClaims) []string {
	if s, ok := c["scope"].(string); ok && s != "" {
		return strings.Fields(s)
	}
	if arr, ok := c["scope"].([]any); ok {
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// keysUnavailableError signals that we couldn't fetch the JWKS at all.
type keysUnavailableError struct{ err error }

func (e *keysUnavailableError) Error() string { return "JWKS unavailable: " + e.err.Error() }
func (e *keysUnavailableError) Unwrap() error { return e.err }

func (v *JWTValidator) keyForKID(ctx context.Context, kid string) (any, error) {
	if k, ok := v.cache.get(kid); ok {
		return k, nil
	}
	if err := v.refreshCache(ctx); err != nil {
		return nil, &keysUnavailableError{err: err}
	}
	if k, ok := v.cache.get(kid); ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

func (v *JWTValidator) refreshCache(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS GET %s: %s", v.JWKSURL, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}
	v.cache.replace(keys)
	return nil
}

// jwksCache is a tiny thread-safe map from kid to public key. ttl is
// applied at the cache level (one expiry stamp for the whole document).
type jwksCache struct {
	mu      sync.RWMutex
	keys    map[string]any
	loadedT time.Time
	ttl     time.Duration
}

func (c *jwksCache) get(kid string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.keys == nil || time.Since(c.loadedT) > c.ttl {
		return nil, false
	}
	k, ok := c.keys[kid]
	return k, ok
}

func (c *jwksCache) replace(keys map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = keys
	c.loadedT = time.Now()
}

type jwkSet struct {
	Keys []jsonWebKey `json:"keys"`
}

type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func parseJWKS(body []byte) (map[string]any, error) {
	var set jwkSet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	out := make(map[string]any, len(set.Keys))
	for _, k := range set.Keys {
		key, err := jwkToPublicKey(k)
		if err != nil {
			slog.Warn("auth: skipping unusable JWK", "kid", k.Kid, "kty", k.Kty, "err", err)
			continue
		}
		out[k.Kid] = key
	}
	if len(out) == 0 {
		return nil, errors.New("JWKS contained no usable keys")
	}
	return out, nil
}

func jwkToPublicKey(k jsonWebKey) (any, error) {
	switch k.Kty {
	case "RSA":
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("decode n: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("decode e: %w", err)
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: int(new(big.Int).SetBytes(eBytes).Int64()),
		}, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("EC curve %q not supported", k.Crv)
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("decode x: %w", err)
		}
		yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, fmt.Errorf("decode y: %w", err)
		}
		return &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported kty %q", k.Kty)
	}
}
