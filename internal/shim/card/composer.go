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
)

// AgentKey identifies which adapter a composed AgentCard describes.
// In Slice 1 the shim runs single-agent and the key is fixed; Slice 5
// resolves it per request when L3 lands.
type AgentKey struct {
	Agent string
	Owner string
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

// Compose builds the L1+L2 merged card. Returns the L1 skeleton (and logs
// WARN) if no $SRV.INFO reply matches. A card is always producible.
func (c *Composer) Compose(ctx context.Context, key AgentKey) (*a2a.AgentCard, error) {
	card := c.l1Card()
	info, found := c.discover(ctx, key)
	if !found {
		c.log.Warn("composer: no $SRV.INFO match", "agent", key.Agent, "owner", key.Owner)
		card.Name = key.Agent
		return card, nil
	}
	c.applyL2(card, info)
	return card, nil
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
