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
// Originally (Slice 5) the (Agent, Owner, Name) triple addressed the
// adapter directly via the v0.3 `agents.card.get.<agent>.<owner>.<name>`
// subject. The clean v0.4 scheme reshapes that addressing to
// (Machine, Project, Session); during the cutover this struct remains
// the matcher key against $SRV.INFO metadata (where adapters still
// advertise these fields), and the subject for fetchL3 is built via
// the positional punt agentKeyAsCoord. Slice 3C threads real
// (Machine, Project, Session) state into the Composer and removes
// the punt.
type AgentKey struct {
	Agent string
	Owner string
	Name  string
}

// agentKeyAsCoord maps an AgentKey to a subject.Coord positionally so
// the v0.4 5-token Card/Cardx subjects can be built without yet
// threading a separate (Machine, Project, Session) tuple through the
// Composer surface. The mapping is byte-shape continuity only — there
// is NO claim that the (Agent, Owner, Name) values mean the same
// thing as (Machine, Project, Session). Slice 3C (per
// docs/plans/2026-05-26-clean-subject-scheme-cutover.md) replaces
// this punt by giving the Composer real coordinate state at
// construction time and dropping the AgentKey-keyed subject build.
//
// TODO(slice-3c): remove once Composer holds a subject.Coord directly.
func agentKeyAsCoord(key AgentKey) subject.Coord {
	return subject.Coord{
		Machine: key.Agent,
		Project: key.Owner,
		Session: key.Name,
	}
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
// request to agents.card.<machine>.<project>.<session> (the clean v0.4
// subject; the AgentKey is positionally mapped through agentKeyAsCoord
// until Slice 3C threads a real Coord). Returns a card even on full L2
// and L3 absence — a card is always producible.
//
// L3 subject tokens come from the discovered adapter's $SRV.INFO
// metadata, not from `key` directly. The operator's `--agent` flag
// (e.g. "claude-code") may differ from the token the adapter
// advertises (e.g. "cc"). Using `key.Agent` here would aim L3 at a
// subject no responder owns. See sesh#122.
func (c *Composer) Compose(ctx context.Context, key AgentKey) (*a2a.AgentCard, error) {
	card := c.l1Card()
	info, found := c.discover(ctx, key)
	if !found {
		c.log.Warn("composer: no $SRV.INFO match", "agent", key.Agent, "owner", key.Owner, "name", key.Name)
		card.Name = key.Agent
		return card, nil
	}
	c.applyL2(card, info)
	resolved := resolveSubjectTokens(info, key)
	if partial, ok := c.fetchL3(ctx, resolved, false); ok {
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
// (the clean v0.4 extended-card subject) and returns the parsed
// cardPartial on a successful reply. Caller passes the result through
// ApplyPartial to overlay onto a previously composed base card.
//
// Per sesh#122 the L3 subject must be built from the adapter's
// advertised tokens, not the operator's `key`. Discover first; if a
// match is found, use its metadata for the subject. If no $SRV.INFO
// match, fall back to the caller's key tokens — preserves the legacy
// path for adapters that haven't been observed via $SRV.INFO yet.
// Coord derivation still goes through agentKeyAsCoord during the
// cutover; see Slice 3C in the cutover plan.
func (c *Composer) FetchExtended(ctx context.Context, key AgentKey) (cardPartial, bool) {
	resolved := key
	if info, found := c.discover(ctx, key); found {
		resolved = resolveSubjectTokens(info, key)
	}
	return c.fetchL3(ctx, resolved, true)
}

// resolveSubjectTokens returns the (Agent, Owner, Name) triple that
// the L3 subject should be built from. Prefers the adapter's
// $SRV.INFO metadata so the resulting subject matches whatever the
// adapter registered its card endpoint under. Falls back to the
// caller's key when a particular metadata field is absent.
//
// For the Agent token specifically, we prefer `metadata.role` over
// `metadata.agent`. The adapter's L3 card endpoint is positionally
// keyed on the (Agent, Owner, Name) triple during the v0.4 cutover
// (see agentKeyAsCoord); pre-cutover this was the subject's third
// token directly. <subject-token> is the abbreviated form (e.g. "cc"),
// not the canonical agent ID (e.g. "claude-code"). The canonical ID
// lives in metadata.agent for L1+L2 composition; the subject token
// lives in metadata.role. Using metadata.agent for the subject (as
// #123 did) produces a subject no responder owns. See sesh#124.
//
// Preserving the caller's key as the final fallback lets the operator
// override discovery: if --agent was deliberately set to the right
// subject token (the rig pattern), it wins when discovery omits role.
func resolveSubjectTokens(info microInfo, fallback AgentKey) AgentKey {
	out := fallback
	if v := info.Metadata["role"]; v != "" {
		out.Agent = v
	} else if v := info.Metadata["agent"]; v != "" {
		out.Agent = v
	}
	if v := info.Metadata["owner"]; v != "" {
		out.Owner = v
	}
	if v := info.Metadata["name"]; v != "" {
		out.Name = v
	}
	return out
}

// ApplyPartial overlays partial onto card per the L3 merge rules
// (non-empty wins). Exported so the extended-card handler can compose
// base + apply without going through Compose's public-L3 path.
func (c *Composer) ApplyPartial(card *a2a.AgentCard, partial cardPartial) {
	c.applyL3(card, partial)
}

// fetchL3 issues one nats.Request to agents.card.<m>.<p>.<s> (extended=false)
// or agents.cardx.<m>.<p>.<s> (extended=true) and decodes the reply as
// a cardPartial. Subject coordinates come from agentKeyAsCoord — the
// v0.4 cutover positional punt — until Slice 3C threads a real Coord.
// Wall time is bounded by queryWindow.
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
	coord := agentKeyAsCoord(key)
	if extended {
		subj, err = subject.Cardx(coord)
	} else {
		subj, err = subject.Card(coord)
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
