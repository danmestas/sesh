package a2a

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/danmestas/sesh-ops/messages"
	"github.com/danmestas/sesh-ops/objects"
)

// ObjectPathPrefix is the gateway path under which obj:// URLs are
// re-served. The Translator emits `<gateway>%s<kind>/<id>/<task>/<art>`
// and the server mounts the route at this same prefix — sharing the
// const prevents silent drift between writer and reader.
const ObjectPathPrefix = "/obj/"

// Translator owns the outbound-Message serialisation boundary. It folds
// two responsibilities that were previously a free function plus a TODO:
//  1. the Slice-1 wire-shape projection (Role swap, drop storage-only
//     fields), and
//  2. the Slice-7 obj:// → HTTPS rewrite so remote clients can fetch
//     bytes they could never reach over NATS.
//
// A single Translator instance is constructed at server boot and shared
// by every outbound path (JSON-RPC results + SSE bridge). GatewayURL is
// read-only after construction; warnOnce gates the empty-gateway INFO
// log so it fires once per process rather than once per Part.
type Translator struct {
	GatewayURL string
	Log        *slog.Logger

	warnOnce sync.Once
}

// NewTranslator constructs a Translator with the provided gateway URL
// and logger. A trailing "/" on gatewayURL is stripped so the join in
// translatePart can always insert exactly one "/". An empty GatewayURL
// is legal — translatePart falls through to pass-through with a one-shot
// INFO log so pre-v0.4 native clients keep working.
func NewTranslator(gatewayURL string, log *slog.Logger) *Translator {
	if log == nil {
		log = slog.Default()
	}
	return &Translator{
		GatewayURL: strings.TrimRight(gatewayURL, "/"),
		Log:        log,
	}
}

// ToWireMessage serialises m for the A2A wire. Defensively copies Parts
// before any rewrite so the caller's *messages.Message (typically the
// pointer in a WatchEvent or KV-cached struct) is never mutated.
func (t *Translator) ToWireMessage(m *messages.Message) (json.RawMessage, error) {
	if m == nil {
		return nil, fmt.Errorf("ToWireMessage: nil message")
	}
	parts := m.Parts
	if len(parts) > 0 {
		cp := make([]messages.Part, len(parts))
		copy(cp, parts)
		for i := range cp {
			t.translatePart(&cp[i])
		}
		parts = cp
	}
	w := wireMessage{
		MessageID:        m.ID,
		TaskID:           m.TaskID,
		ContextID:        m.ContextID,
		Role:             ToA2ARole(m.Role),
		Parts:            parts,
		Extensions:       m.Extensions,
		ReferenceTaskIDs: m.ReferenceTaskIDs,
		Metadata:         m.Metadata,
	}
	b, err := json.Marshal(&w)
	if err != nil {
		return nil, fmt.Errorf("ToWireMessage: %w", err)
	}
	return b, nil
}

// translatePart rewrites a single Part in place. The caller owns the
// defensive copy — translatePart mutates whatever pointer it receives.
//
// Decision matrix:
//   - URL does not start with "obj://"          → no-op
//   - URL is "obj://..." and GatewayURL == ""   → pass-through, one INFO
//   - URL is "obj://..." and ParseURI fails     → pass-through, one WARN
//     per call (ops visibility
//     for malformed adapter
//     output without dropping
//     the Part on the floor)
//   - URL is valid obj://                       → rewrite to HTTPS
func (t *Translator) translatePart(p *messages.Part) {
	if p == nil || !strings.HasPrefix(p.URL, "obj://") {
		return
	}
	if t.GatewayURL == "" {
		t.warnOnce.Do(func() {
			t.Log.Info("a2a.Translator: GatewayURL empty; obj:// URLs pass through unchanged")
		})
		return
	}
	kind, id, task, art, err := objects.ParseURI(p.URL)
	if err != nil {
		t.Log.Warn("a2a.Translator: malformed obj:// URL, passing through",
			"url", p.URL, "err", err)
		return
	}
	p.URL = t.GatewayURL + ObjectPathPrefix +
		url.PathEscape(kind) + "/" +
		url.PathEscape(id) + "/" +
		url.PathEscape(task) + "/" +
		url.PathEscape(art)
}
