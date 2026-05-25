package push

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Resolver looks up a hostname to a list of IPs. Default implementation
// wraps net.DefaultResolver.LookupIPAddr; tests inject deterministic
// stubs (including DNS-rebinding doubles where the first lookup
// returns a public IP and the second returns a private one).
type Resolver func(host string) ([]net.IP, error)

// DefaultResolver is the production resolver — net.DefaultResolver
// with a 2-second timeout per lookup. Callers that need a different
// timeout build their own Resolver.
func DefaultResolver(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// ValidateURL rejects webhook targets that would let a malicious
// caller pivot through the shim. Hard rules:
//
//   - URL must parse and have a non-empty host.
//   - Scheme MUST be "https" in production. When devAllowLocalhost is
//     true (mirrors --dev), "http" is allowed only when the host is
//     literally "localhost", "127.0.0.1", or "::1".
//   - The host (if a literal IP) and EVERY resolved IP (if a hostname)
//     must NOT be in the blocked set: loopback, link-local, RFC1918,
//     ULA, multicast, unspecified.
//
// DNS-rebinding mitigation: the caller MUST call ValidateURL at BOTH
// Create-time AND each delivery — a hostname that resolved to a
// public IP yesterday may resolve to a private IP today. The Resolver
// argument is the lever tests pull to demonstrate this.
//
// Returns nil on accept, a descriptive error on reject (Code-level
// callers wrap into jsonrpc.ErrInvalidParams).
func ValidateURL(raw string, devAllowLocalhost bool, resolve Resolver) error {
	if raw == "" {
		return errors.New("url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url has no host")
	}

	// Scheme gate. "https" always OK. "http" only on the dev whitelist.
	switch strings.ToLower(u.Scheme) {
	case "https":
		// Continue to IP checks.
	case "http":
		if !(devAllowLocalhost && isLocalhostLiteral(host)) {
			return fmt.Errorf("http scheme not allowed (host=%q, devAllowLocalhost=%v)", host, devAllowLocalhost)
		}
		// Localhost literal in dev mode: skip IP check (we already
		// know the answer; resolving "localhost" via the system
		// resolver introduces flakes on hosts with weird /etc/hosts).
		return nil
	default:
		return fmt.Errorf("unsupported scheme %q (only https; http with --dev for localhost)", u.Scheme)
	}

	// Literal IP host? Check it directly without DNS.
	if ip := net.ParseIP(host); ip != nil {
		if devAllowLocalhost && isLocalhostLiteral(host) {
			return nil
		}
		if isBlockedIP(ip) {
			return fmt.Errorf("ip %s is in blocked range (loopback/link-local/private/multicast/unspecified)", ip)
		}
		return nil
	}

	// Hostname → resolve, check every IP.
	if resolve == nil {
		resolve = DefaultResolver
	}
	ips, err := resolve(host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve %q: no addresses", host)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("host %s resolves to blocked ip %s", host, ip)
		}
	}
	return nil
}

// isLocalhostLiteral matches the dev-mode whitelist exactly. Brackets
// on "[::1]" are stripped by url.Hostname() so we compare to the bare
// form.
func isLocalhostLiteral(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// isBlockedIP returns true for any IP that should not be a webhook
// target. Uses Go 1.17+ net.IP.IsPrivate() which covers RFC 1918
// (10/8, 172.16/12, 192.168/16) AND RFC 4193 ULA (fc00::/7) in one
// call.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	if ip.IsMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	return false
}
