package methods

import (
	"context"
	"encoding/json"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

// extendedReadScope is the OAuth-style scope a principal must carry to
// see the auth-gated extended AgentCard. Mirrors the listTasks pattern
// (Slice 4 readScope) for consistency.
const extendedReadScope = "agent.read.extended"

// getExtendedAgentCard returns a signed extended AgentCard composed
// per-request from L1 + L2 (via Composer.ComposeBase) overlaid with
// the L3 extended-card partial from the adapter
// (agents.cardx.<machine>.<project>.<session>).
//
// Per plan D4 there is NO cache for the extended card: every
// authorized request re-composes and re-signs. The cost is one NATS
// round-trip + one JCS+ES256 sign per call (sub-50ms in practice),
// and it side-steps per-principal cache complexity.
//
// Behavior:
//   - No principal or principal without `agent.read.extended` →
//     ErrExtendedCardNotConfigured (-32007). Matches the A2A spec's
//     "the server reveals nothing about why" stance — don't leak
//     "this exists but you can't see it" vs "this doesn't exist".
//   - Adapter doesn't respond within the composer's queryWindow →
//     same -32007. There's no extended card to return.
//   - Compose or sign error → ErrInternal (-32603), logged.
func (d *Dispatcher) getExtendedAgentCard(ctx context.Context, _ json.RawMessage) (any, *jsonrpc.Error) {
	if d.deps.Composer == nil || d.deps.Signer == nil {
		// Misconfiguration — handler wired but collaborators absent.
		// Returning -32007 keeps the externally visible behavior
		// indistinguishable from "no extended card configured", which
		// is exactly what the operator gets.
		d.deps.Log.Warn("extendedcard: composer or signer not wired")
		return nil, jsonrpc.ErrExtendedCardNotConfigured
	}

	p, ok := auth.FromContext(ctx)
	if !ok || !HasScope(p, extendedReadScope) {
		return nil, jsonrpc.ErrExtendedCardNotConfigured
	}

	partial, found := d.deps.Composer.FetchExtended(ctx, d.deps.AgentKey)
	if !found {
		return nil, jsonrpc.ErrExtendedCardNotConfigured
	}

	base, err := d.deps.Composer.ComposeBase(ctx, d.deps.AgentKey)
	if err != nil {
		d.deps.Log.Error("extendedcard: compose base failed", "err", err,
			"agent", d.deps.AgentKey.Agent, "owner", d.deps.AgentKey.Owner, "name", d.deps.AgentKey.Name)
		return nil, jsonrpc.ErrInternal
	}
	d.deps.Composer.ApplyPartial(base, partial)

	signed, err := d.deps.Signer.SignCard(base)
	if err != nil {
		d.deps.Log.Error("extendedcard: sign failed", "err", err)
		return nil, jsonrpc.ErrInternal
	}
	return json.RawMessage(signed), nil
}
