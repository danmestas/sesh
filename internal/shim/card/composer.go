package card

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/subject"
)

// cardPartial is the wire shape of the adapter's L3 contribution. MUST
// stay byte-identical with the TypeScript SDK's `AgentCardPartial`
// (sesh-channels/sdk/src/card.ts). Drift breaks cross-stack wire
// compatibility — verify via TestComposer_DecodesSDKWireShape and the
// SDK source comment that test points at.
//
// All fields are optional on the wire (omitempty respected on encode;
// absent fields decode to zero values and applyL3 skips them per the
// "non-empty overlay" rule from plan D1).
type cardPartial struct {
	Description      string           `json:"description,omitempty"`
	Skills           []a2a.AgentSkill `json:"skills,omitempty"`
	IconURL          string           `json:"iconUrl,omitempty"`
	DocumentationURL string           `json:"documentationUrl,omitempty"`
	Capabilities     *partialCaps     `json:"capabilities,omitempty"`
}

// partialCaps mirrors the `capabilities` sub-object of the TS
// AgentCardPartial. Only Extensions is meaningful for L3 today; the
// other AgentCapabilities fields (Streaming, PushNotifications,
// ExtendedAgentCard) are operator-decided at L1 and would lose meaning
// if an adapter could override them.
type partialCaps struct {
	Extensions []a2a.AgentExtension `json:"extensions,omitempty"`
}

// AgentKey identifies which adapter a composed AgentCard describes. The
// (Agent, Owner, Name) triple matches the third-token slot of
// agents.card.get.<agent>.<owner>.<name> introduced in Slice 5. Slice 1
// shipped only Agent+Owner; Name was added when the L3 fetch path
// landed.
type AgentKey struct {
	Agent string
	Owner string
	Name  string
}

// L1Defaults are the operator-controlled shim defaults that never vary
// per adapter. Sourced from CLI flags in Slice 1; from shim.toml later.
type L1Defaults struct {
	GatewayURL         string
	ProtocolVersion    string
	Provider           *a2a.AgentProvider
	SecuritySchemes    a2a.NamedSecuritySchemes
	Capabilities       a2a.AgentCapabilities
	DefaultInputModes  []string
	DefaultOutputModes []string
}

// Composer composes L1 (operator defaults) with L2 (adapter $SRV.INFO
// metadata fallback). Per audit P1.4, Slice 1 implements only the
// $SRV.INFO fallback path; the rich `agents.card.get.*` fetch lands in
// Slice 5.
type Composer struct {
	nc          *nats.Conn
	l1          L1Defaults
	queryWindow time.Duration
	log         *slog.Logger
}

func NewComposer(nc *nats.Conn, l1 L1Defaults, queryWindow time.Duration, log *slog.Logger) *Composer {
	if queryWindow <= 0 {
		queryWindow = 500 * time.Millisecond
	}
	if log == nil {
		log = slog.Default()
	}
	return &Composer{nc: nc, l1: l1, queryWindow: queryWindow, log: log}
}

// Compose builds the L1+L2+L3 merged card. L1 is the operator-controlled
// skeleton; L2 comes from $SRV.INFO; L3 comes from a window-bounded
// request to agents.card.get.<agent>.<owner>.<name>. Returns a card
// even on full L2 and L3 absence — a card is always producible.
func (c *Composer) Compose(ctx context.Context, key AgentKey) (*a2a.AgentCard, error) {
	card, err := c.ComposeBase(ctx, key)
	if err != nil {
		return nil, err
	}
	if partial, found := c.fetchL3(ctx, key, false); found {
		c.applyL3(card, partial)
	}
	return card, nil
}

// ComposeBase returns the L1+L2 card without any L3 contribution. Used
// by the extended-card path so it can build a per-request card with the
// extended L3 overlay without double-fetching the public L3 first.
func (c *Composer) ComposeBase(ctx context.Context, key AgentKey) (*a2a.AgentCard, error) {
	card := c.l1Card()
	info, found := c.discover(ctx, key)
	if !found {
		c.log.Warn("composer: no $SRV.INFO match", "agent", key.Agent, "owner", key.Owner, "name", key.Name)
		card.Name = key.Agent
		return card, nil
	}
	c.applyL2(card, info)
	return card, nil
}

// FetchExtended issues a request against agents.card.extended.* and
// returns the parsed cardPartial on a successful reply. Caller passes
// the result through ApplyPartial to overlay onto a previously composed
// base card.
func (c *Composer) FetchExtended(ctx context.Context, key AgentKey) (cardPartial, bool) {
	return c.fetchL3(ctx, key, true)
}

// ApplyPartial overlays partial onto card per the L3 merge rules
// (non-empty wins). Exported so the extended-card handler can compose
// base + apply without going through Compose's public-L3 path.
func (c *Composer) ApplyPartial(card *a2a.AgentCard, partial cardPartial) {
	c.applyL3(card, partial)
}

// fetchL3 issues one nats.Request to agents.card.get.* (extended=false)
// or agents.card.extended.* (extended=true) and decodes the reply as a
// cardPartial. Wall time is bounded by queryWindow.
//
// L3 absence is NOT an error — pre-v0.4 adapters never register the
// service, so a timeout is the expected steady state. We log INFO
// (not WARN) so production logs don't flood. Decode failures DO log
// WARN since they indicate adapter-side bugs.
//
// nats.Request handles inbox subscription + cleanup internally on
// timeout; we deliberately use it over RequestWithContext because the
// caller's ctx may outlive queryWindow (e.g. an HTTP request with a 30s
// deadline shouldn't block the card fetch for 30s).
func (c *Composer) fetchL3(ctx context.Context, key AgentKey, extended bool) (cardPartial, bool) {
	// Honor caller cancellation pre-emptively — saves the round trip
	// when the HTTP request was already aborted.
	if err := ctx.Err(); err != nil {
		return cardPartial{}, false
	}
	var (
		subj string
		err  error
	)
	if extended {
		subj, err = subject.CardExtended(key.Agent, key.Owner, key.Name)
	} else {
		subj, err = subject.CardGet(key.Agent, key.Owner, key.Name)
	}
	if err != nil {
		c.log.Warn("composer: build card subject failed",
			"agent", key.Agent, "owner", key.Owner, "name", key.Name,
			"extended", extended, "err", err)
		return cardPartial{}, false
	}

	msg, err := c.nc.Request(subj, nil, c.queryWindow)
	if err != nil {
		// nats.ErrTimeout is the steady-state "no adapter responded
		// within the window" outcome. Log INFO only — pre-v0.4
		// adapters never register the service, and even v0.4+
		// adapters may be momentarily unreachable.
		c.log.Info("composer: no L3 reply",
			"subject", subj, "extended", extended,
			"window", c.queryWindow, "err", err)
		return cardPartial{}, false
	}

	var p cardPartial
	if err := json.Unmarshal(msg.Data, &p); err != nil {
		c.log.Warn("composer: L3 decode failed",
			"subject", subj, "extended", extended, "err", err, "bytes", len(msg.Data))
		return cardPartial{}, false
	}
	return p, true
}

// applyL3 overlays partial onto card per the "adapter wins per-field"
// rule from plan D1. Empty fields on the wire mean "no contribution"
// and preserve L1+L2 values. Skills is replace-not-append: an L3 list
// of length>0 replaces the L1+L2 skills list entirely (currently always
// empty at L2; revisit if a future L2 source populates skills).
func (c *Composer) applyL3(card *a2a.AgentCard, p cardPartial) {
	if card == nil {
		return
	}
	if p.Description != "" {
		card.Description = p.Description
	}
	if len(p.Skills) > 0 {
		card.Skills = p.Skills
	}
	if p.IconURL != "" {
		card.IconURL = p.IconURL
	}
	if p.DocumentationURL != "" {
		card.DocumentationURL = p.DocumentationURL
	}
	if p.Capabilities != nil && len(p.Capabilities.Extensions) > 0 {
		card.Capabilities.Extensions = p.Capabilities.Extensions
	}
}

func (c *Composer) l1Card() *a2a.AgentCard {
	return &a2a.AgentCard{
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(c.l1.GatewayURL, a2a.TransportProtocolJSONRPC),
		},
		Capabilities:       c.l1.Capabilities,
		DefaultInputModes:  c.l1.DefaultInputModes,
		DefaultOutputModes: c.l1.DefaultOutputModes,
		Provider:           c.l1.Provider,
		SecuritySchemes:    c.l1.SecuritySchemes,
		Skills:             []a2a.AgentSkill{},
	}
}

type microInfo struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Version  string            `json:"version"`
	Metadata map[string]string `json:"metadata"`
}

// discover issues one $SRV.INFO.agents request and returns the first
// microInfo whose metadata matches key. Window-bounded; deduplicates by ID.
func (c *Composer) discover(ctx context.Context, key AgentKey) (microInfo, bool) {
	inbox := c.nc.NewInbox()
	replies := make(chan *nats.Msg, 64)
	sub, err := c.nc.ChanSubscribe(inbox, replies)
	if err != nil {
		c.log.Warn("composer: subscribe inbox failed", "err", err)
		return microInfo{}, false
	}
	defer sub.Unsubscribe()

	if err := c.nc.PublishRequest("$SRV.INFO.agents", inbox, nil); err != nil {
		c.log.Warn("composer: publish INFO failed", "err", err)
		return microInfo{}, false
	}

	deadline := time.Now().Add(c.queryWindow)
	seen := make(map[string]struct{})
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return microInfo{}, false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return microInfo{}, false
		case <-timer.C:
			return microInfo{}, false
		case msg, ok := <-replies:
			timer.Stop()
			if !ok {
				return microInfo{}, false
			}
			var info microInfo
			if err := json.Unmarshal(msg.Data, &info); err != nil {
				continue
			}
			if _, dup := seen[info.ID]; dup {
				continue
			}
			seen[info.ID] = struct{}{}
			if matches(info, key) {
				return info, true
			}
		}
	}
}

func matches(info microInfo, key AgentKey) bool {
	if key.Agent != "" && info.Metadata["agent"] != key.Agent {
		return false
	}
	if key.Owner != "" && info.Metadata["owner"] != key.Owner {
		return false
	}
	return true
}

func (c *Composer) applyL2(card *a2a.AgentCard, info microInfo) {
	if name := strings.TrimSpace(info.Metadata["agent"]); name != "" {
		card.Name = name
	} else if info.Name != "" {
		card.Name = info.Name
	}
	if v := strings.TrimSpace(info.Metadata["harness_ver"]); v != "" {
		card.Version = v
	} else if info.Version != "" {
		card.Version = info.Version
	} else {
		card.Version = "0.0.0"
	}
	role := info.Metadata["role"]
	class := info.Metadata["class"]
	if role != "" || class != "" {
		card.Description = fmt.Sprintf("%s/%s", role, class)
	}
}
