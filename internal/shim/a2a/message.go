package a2a

import (
	"encoding/json"
	"fmt"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/danmestas/sesh-ops/messages"
)

// FromWireMessage decodes inbound A2A Message JSON into a sesh-ops
// *messages.Message, translating Role at the boundary. Parts pass
// through structurally — both a2a-go v2 (after its MarshalJSON
// flattens Part.Content) and sesh-ops use FLAT Part JSON (no
// `kind` discriminator, no `content` wrapper), so the only field
// transform is Role.
//
// JSON field names are identical on both sides (messageId, taskId,
// contextId, role, parts, extensions, referenceTaskIds, metadata)
// so the decode is a single Unmarshal pass into the sesh-ops type,
// followed by a role swap from the wire constants
// "ROLE_USER"/"ROLE_AGENT" to the storage constants "user"/"agent".
func FromWireMessage(b []byte) (*messages.Message, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("FromWireMessage: empty input")
	}
	var m messages.Message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("FromWireMessage: %w", err)
	}
	// The wire bytes carry "ROLE_USER" etc.; map them to the short form.
	m.Role = ToSeshRole(a2a.MessageRole(m.Role))
	return &m, nil
}

// wireMessage mirrors the A2A v1.0 Message shape exactly — only fields
// the spec defines, with SCREAMING_SNAKE role. The sesh-ops Message
// struct carries internal bookkeeping (`v`, `createdAt`) that strict
// A2A clients (Pydantic extra=forbid, codegen schemas) reject; this
// projection drops those before the wire write.
type wireMessage struct {
	MessageID        string          `json:"messageId"`
	TaskID           string          `json:"taskId,omitempty"`
	ContextID        string          `json:"contextId,omitempty"`
	Role             a2a.MessageRole `json:"role"`
	Parts            []messages.Part `json:"parts"`
	Extensions       []string        `json:"extensions,omitempty"`
	ReferenceTaskIDs []string        `json:"referenceTaskIds,omitempty"`
	Metadata         map[string]any  `json:"metadata,omitempty"`
}

// ToWireMessage is a Slice-1 compatibility shim: delegates to a
// zero-value Translator (GatewayURL=="" ⇒ obj:// pass-through). New
// callers should instantiate *Translator at boot via NewTranslator and
// use the method form — see internal/shim/server for the wiring.
//
// Deprecated: use Translator.ToWireMessage so obj:// URLs get the
// Slice-7 gateway rewrite.
func ToWireMessage(m *messages.Message) (json.RawMessage, error) {
	return (&Translator{}).ToWireMessage(m)
}
