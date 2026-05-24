// Package authtest exposes test-only helpers for installing a Principal
// into a context without booting the HTTP middleware. Importing this
// package from production code is a smell — the only legitimate callers
// are *_test.go files in sibling packages (e.g. internal/shim/methods).
package authtest

import (
	"context"

	"github.com/danmestas/sesh/internal/shim/auth"
)

// WithPrincipal returns a copy of ctx carrying p under the same
// context key auth.Middleware uses. Mirrors what Middleware does
// without an HTTP round-trip.
func WithPrincipal(ctx context.Context, p auth.Principal) context.Context {
	return auth.ExportForTest_InstallPrincipal(ctx, p)
}
