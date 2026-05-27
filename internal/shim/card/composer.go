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

// AgentKey identifies which adapter a composed AgentCard describes.
// It is the matcher key against $SRV.INFO metadata: discover() filters
// advertised services by these fields to find the adapter this shim
// fronts. It is NOT the source of the L3 card subject — that comes from
// the Composer's own subject.Coord (set at construction). See
// docs/plans/2026-05-26-clean-subject-scheme-cutover.md, Slice 3C.
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
// metadata fallback) and L3 (the rich `agents.card.*` fetch). Per audit
// P1.4, Slice 1 implemented only the $SRV.INFO fallback path; the L3
// fetch landed in Slice 5.
type Composer struct {
	nc          *nats.Conn
	coord       subject.Coord // session this composer L3-binds to; empty disables L3
	l1          L1Defaults
	queryWindow time.Duration
	log         *slog.Logger
}

func NewComposer(nc *nats.Conn, coord subject.Coord, l1 L1Defaults, queryWindow time.Duration, log *slog.Logger) *Composer {
	if queryWindow <= 0 {
		queryWindow = 500 * time.Millisecond
	}
	if log == nil {
		log = slog.Default()
	}
	return &Composer{nc: nc, coord: coord, l1: l1, queryWindow: queryWindow, log: log}
}

// Compose builds the L1+L2+L3 merged card. L1 is the operator-controlled
// skeleton; L2 comes from $SRV.INFO; L3 comes from a window-bounded
// request to agents.card.<machine>.<project>.<session>, built from the
// Composer's own subject.Coord (set at construction, Slice 3C). Returns
// a card even on full L2 and L3 absence — a card is always producible.
//
// The `key` AgentKey is the $SRV.INFO matcher only: discover() uses it
// to find the adapter this shim fronts. The L3 subject does NOT derive
// from `key` — the operator's `--agent` flag (e.g. "claude-code") may
// differ from the adapter's advertised token, so addressing L3 off
// `key` would aim at a subject no responder owns (see sesh#122). The
// Coord is the authoritative session address instead.
func (c *Composer) Compose(ctx context.Context, key AgentKey) (*a2a.AgentCard, error) {
	card := c.l1Card()
	info, found := c.discover(ctx, key)
	if !found {
		c.log.Warn("composer: no $SRV.INFO match", "agent", key.Agent, "owner", key.Owner, "name", key.Name)
		card.Name = key.Agent
		return card, nil
	}
	c.applyL2(card, info)
	if partial, ok := c.fetchL3(ctx, false); ok {
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

// FetchExtended issues a request against agents.cardx.<machine>.<project>.<session>
// (the clean v0.4 extended-card subject, built from the Composer's own
// subject.Coord) and returns the parsed cardPartial on a successful
// reply. Caller passes the result through ApplyPartial to overlay onto
// a previously composed base card.
//
// The `key` param is retained for call-site uniformity with Compose
// (both public fetch methods take the AgentKey the dispatcher holds),
// but the L3 subject derives from c.coord, not key — see Compose for
// why addressing off `key` would miss the responder (sesh#122).
func (c *Composer) FetchExtended(ctx context.Context, _ AgentKey) (cardPartial, bool) {
	return c.fetchL3(ctx, true)
}

// ApplyPartial overlays partial onto card per the L3 merge rules
// (non-empty wins). Exported so the extended-card handler can compose
// base + apply without going through Compose's public-L3 path.
func (c *Composer) ApplyPartial(card *a2a.AgentCard, partial cardPartial) {
	c.applyL3(card, partial)
}

// fetchL3 issues one nats.Request to agents.card.<m>.<p>.<s> (extended=false)
// or agents.cardx.<m>.<p>.<s> (extended=true) and decodes the reply as
// a cardPartial. Subject coordinates come from the Composer's own
// subject.Coord (set at construction, Slice 3C). Wall time is bounded
// by queryWindow.
//
// An empty Coord disables L3: a Composer built with subject.Coord{}
// (or any partially-empty triple) returns (cardPartial{}, false) before
// touching NATS. This preserves the "card always producible" invariant
// for unit tests and any future caller that constructs a Composer
// without a bound session.
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
func (c *Composer) fetchL3(ctx context.Context, extended bool) (cardPartial, bool) {
	// No bound session ⇒ no L3 to fetch.
	if c.coord.Machine == "" || c.coord.Project == "" || c.coord.Session == "" {
		return cardPartial{}, false
	}
	// Honor caller cancellation pre-emptively — saves the round trip
	// when the HTTP request was already aborted.
	if err := ctx.Err(); err != nil {
		return cardPartial{}, false
	}
	var (
		subj string
		err  error
	)
	coord := c.coord
	if extended {
		subj, err = subject.Cardx(coord)
	} else {
		subj, err = subject.Card(coord)
	}
	if err != nil {
		c.log.Warn("composer: build card subject failed",
			"machine", coord.Machine, "project", coord.Project, "session", coord.Session,
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

// matches reports whether info is the adapter the caller's key refers
// to. The matcher is intentionally loose on the agent field because the
// shim's `--agent` flag is overloaded across the v0.4 deployment: some
// operators pass the canonical agent ID (e.g. "claude-code", matching
// `metadata.agent`) while integration rigs and short-form invocations
// pass the abbreviated subject token (e.g. "cc", matching the role
// token the adapter publishes as `metadata.role`). One flag cannot be
// two things at once, so we accept either form as a match. The owner
// check stays exact — owners are not abbreviated.
//
// See sesh#124 for the motivating rig failure: shim has --agent=cc but
// the adapter advertises metadata.agent="claude-code", metadata.role="cc".
// The strict canonical-only match left discover() returning "no match"
// and L3 + prompt routing both starved.
func matches(info microInfo, key AgentKey) bool {
	if key.Agent != "" {
		if info.Metadata["agent"] != key.Agent && info.Metadata["role"] != key.Agent {
			return false
		}
	}
	if key.Owner != "" && info.Metadata["owner"] != key.Owner {
		return false
	}
	return true
}

// DiscoverRoleToken returns the abbreviated subject token the adapter
// publishes as `metadata.role` in $SRV.INFO. The shim uses this to
// build the prompt subject's role token, which adapters subscribe with
// on agents.prompt.<machine>.<project>.<session>.<role>. Returns
// ("", false) when discovery times out or the matched adapter omits
// the role metadata — callers fall back to the operator's --agent flag.
//
// Decoupling the role token from --agent is required because --agent
// carries the canonical agent ID (e.g. "claude-code") whereas the v2
// prompt subject expects the adapter-defined abbreviation (e.g. "cc").
// See sesh#124.
func (c *Composer) DiscoverRoleToken(ctx context.Context, key AgentKey) (string, bool) {
	info, found := c.discover(ctx, key)
	if !found {
		return "", false
	}
	role := strings.TrimSpace(info.Metadata["role"])
	if role == "" {
		return "", false
	}
	return role, true
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
