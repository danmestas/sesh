package methods

import (
	"context"
	"encoding/json"

	"github.com/danmestas/sesh/internal/shim/jsonrpc"
)

// getExtendedAgentCard always returns the
// AuthenticatedExtendedCardNotConfigured error in Slice 1. Slice 5
// implements the real fetch path.
func (d *Dispatcher) getExtendedAgentCard(_ context.Context, _ json.RawMessage) (any, *jsonrpc.Error) {
	return nil, jsonrpc.ErrExtendedCardNotConfigured
}
