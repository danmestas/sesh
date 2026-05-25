package push

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// stubResolver returns a Resolver that always yields the given IPs.
// Used to drive the hostname-resolution branch without touching DNS.
func stubResolver(ips ...string) Resolver {
	return func(host string) ([]net.IP, error) {
		out := make([]net.IP, 0, len(ips))
		for _, s := range ips {
			ip := net.ParseIP(s)
			if ip == nil {
				return nil, errors.New("test bug: bad ip " + s)
			}
			out = append(out, ip)
		}
		return out, nil
	}
}

// TestValidateURL_Table walks the SSRF surface as one table. Each row
// is a discrete acceptance/rejection rule from the plan:
//   - scheme gates (https always; http only on dev whitelist),
//   - literal-IP blocking (loopback, RFC1918, link-local, ULA,
//     metadata addr 169.254.169.254, unspecified, multicast),
//   - hostname blocking via injected resolver,
//   - malformed/empty inputs.
//
// Keeping the cases together makes drift visible (one expected-to-
// reject row flipping to accept is loud in the diff).
func TestValidateURL_Table(t *testing.T) {
	publicResolve := stubResolver("93.184.216.34") // example.com canonical IP
	loopbackResolve := stubResolver("127.0.0.1")
	rebindResolve := stubResolver("10.0.0.1") // public hostname → private IP

	cases := []struct {
		name              string
		url               string
		devAllowLocalhost bool
		resolve           Resolver
		wantOK            bool
	}{
		// https + public ⇒ accept.
		{"https-public-ip", "https://93.184.216.34/hook", false, nil, true},
		{"https-public-host", "https://example.com/hook", false, publicResolve, true},

		// https + private IP ⇒ reject.
		{"https-loopback-ip-v4", "https://127.0.0.1/hook", false, nil, false},
		{"https-loopback-ip-v6", "https://[::1]/hook", false, nil, false},
		{"https-rfc1918-10", "https://10.0.0.1/hook", false, nil, false},
		{"https-rfc1918-192-168", "https://192.168.1.1/hook", false, nil, false},
		{"https-rfc1918-172-16", "https://172.16.5.5/hook", false, nil, false},
		{"https-link-local", "https://169.254.169.254/hook", false, nil, false}, // EC2 metadata
		{"https-link-local-host", "https://fe80::1/hook", false, nil, false},
		{"https-unspecified", "https://0.0.0.0/hook", false, nil, false},
		{"https-multicast", "https://224.0.0.1/hook", false, nil, false},
		{"https-ula", "https://[fd00::1]/hook", false, nil, false},

		// http in prod (no dev override) ⇒ reject regardless of host.
		{"http-prod-localhost", "http://localhost:8080/hook", false, nil, false},
		{"http-prod-public", "http://example.com/hook", false, publicResolve, false},

		// http in dev + localhost literal ⇒ accept (any port).
		{"http-dev-localhost", "http://localhost:8080/hook", true, nil, true},
		{"http-dev-127", "http://127.0.0.1:9999/hook", true, nil, true},
		{"http-dev-v6", "http://[::1]:80/hook", true, nil, true},

		// http in dev + non-localhost host ⇒ reject (dev whitelist is
		// strict; we don't permit "http://example.com" even with --dev).
		{"http-dev-public", "http://example.com/hook", true, publicResolve, false},

		// DNS-rebinding: hostname resolves to a private IP ⇒ reject.
		{"dns-rebind-private", "https://evil.example.com/hook", false, rebindResolve, false},

		// Hostname resolves to loopback ⇒ reject (even with the dev flag,
		// because the hostname isn't on the literal whitelist).
		{"hostname-resolves-loopback", "https://example.com/hook", true, loopbackResolve, false},

		// Bad scheme.
		{"ftp", "ftp://example.com/hook", false, publicResolve, false},

		// Empty / malformed.
		{"empty", "", false, nil, false},
		{"no-host", "https://", false, nil, false},
		{"garbage", "not a url at all", false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(tc.url, tc.devAllowLocalhost, tc.resolve)
			ok := err == nil
			if ok != tc.wantOK {
				t.Errorf("ValidateURL(%q, dev=%v) ok=%v err=%v, want ok=%v",
					tc.url, tc.devAllowLocalhost, ok, err, tc.wantOK)
			}
		})
	}
}

// TestValidateURL_ResolverError surfaces the resolver's error rather
// than blanket-rejecting. Operators reading the error message should
// be able to tell "DNS down" from "resolved to private IP".
func TestValidateURL_ResolverError(t *testing.T) {
	resolve := func(host string) ([]net.IP, error) {
		return nil, errors.New("simulated DNS failure")
	}
	err := ValidateURL("https://example.com/hook", false, resolve)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "simulated DNS failure") {
		t.Errorf("err = %v, want it to wrap the resolver error", err)
	}
}

// TestValidateURL_ResolverEmpty rejects hostnames that resolve to no
// addresses. A zero-IP result is treated as "can't validate" and
// dropped on the safe side (deny).
func TestValidateURL_ResolverEmpty(t *testing.T) {
	resolve := func(host string) ([]net.IP, error) { return nil, nil }
	err := ValidateURL("https://example.com/hook", false, resolve)
	if err == nil {
		t.Fatal("expected error on empty resolver result")
	}
}
