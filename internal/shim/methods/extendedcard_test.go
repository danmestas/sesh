package methods

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/auth/authtest"
	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/subject"
)

// withCardDeps decorates testDeps with a Composer + Signer wired
// against the broker testDeps already brought up. Returns the enriched
// Deps and the *nats.Conn so callers can register L3 stubs.
func withCardDeps(t *testing.T) (Deps, *nats.Conn) {
	t.Helper()
	deps, nc, _ := testDeps(t)
	signer, err := card.NewDevSigner()
	if err != nil {
		t.Fatalf("dev signer: %v", err)
	}
	composer := card.NewComposer(nc, card.L1Defaults{
		GatewayURL:         "https://shim.test/a2a",
		ProtocolVersion:    "1.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}, 200*time.Millisecond, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	deps.Composer = composer
	deps.Signer = signer
	return deps, nc
}

// extendedScopedCtx returns a context carrying a principal with the
// agent.read.extended scope.
func extendedScopedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := mustCtx(t)
	t.Cleanup(cancel)
	return authtest.WithPrincipal(ctx, auth.Principal{
		Sub:    "alice",
		Scopes: []string{"agent.read", "agent.write", extendedReadScope},
	})
}

// noExtendedScopeCtx carries a principal that's authenticated but lacks
// the extended-read scope.
func noExtendedScopeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := mustCtx(t)
	t.Cleanup(cancel)
	return authtest.WithPrincipal(ctx, auth.Principal{
		Sub:    "alice",
		Scopes: []string{"agent.read"},
	})
}

func stubExtendedReply(t *testing.T, nc *nats.Conn, key card.AgentKey, body []byte) *int32 {
	t.Helper()
	subj, err := subject.CardExtended(key.Agent, key.Owner, key.Name)
	if err != nil {
		t.Fatalf("CardExtended: %v", err)
	}
	var n int32
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		atomic.AddInt32(&n, 1)
		_ = m.Respond(body)
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subj, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &n
}

func TestGetExtendedAgentCard_HappyPath(t *testing.T) {
	deps, nc := withCardDeps(t)
	stubExtendedReply(t, nc, deps.AgentKey, []byte(`{
		"description": "extended echo card",
		"skills": [{"id":"echo.privileged","name":"Privileged","description":"d","tags":["t"]}]
	}`))

	disp := NewDispatcher(deps)
	res, jerr := disp.getExtendedAgentCard(extendedScopedCtx(t), nil)
	if jerr != nil {
		t.Fatalf("getExtendedAgentCard: %+v", jerr)
	}
	raw, ok := res.(json.RawMessage)
	if !ok {
		t.Fatalf("result type = %T, want json.RawMessage", res)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode signed card: %v", err)
	}
	if card["description"] != "extended echo card" {
		t.Errorf("description = %v, want extended echo card", card["description"])
	}
	if _, ok := card["signatures"]; !ok {
		t.Errorf("signed card missing signatures: %s", raw)
	}
}

func TestGetExtendedAgentCard_MissingScope_ReturnsNotConfigured(t *testing.T) {
	deps, nc := withCardDeps(t)
	// Even if the adapter is present, missing scope must yield -32007.
	stubExtendedReply(t, nc, deps.AgentKey, []byte(`{"description":"x"}`))

	disp := NewDispatcher(deps)
	_, jerr := disp.getExtendedAgentCard(noExtendedScopeCtx(t), nil)
	if jerr == nil || jerr.Code != -32007 {
		t.Fatalf("jerr = %+v, want -32007", jerr)
	}
}

func TestGetExtendedAgentCard_NoPrincipal_ReturnsNotConfigured(t *testing.T) {
	deps, _ := withCardDeps(t)
	disp := NewDispatcher(deps)
	ctx, cancel := mustCtx(t)
	defer cancel()
	_, jerr := disp.getExtendedAgentCard(ctx, nil)
	if jerr == nil || jerr.Code != -32007 {
		t.Fatalf("jerr = %+v, want -32007", jerr)
	}
}

func TestGetExtendedAgentCard_AdapterNoReply_ReturnsNotConfigured(t *testing.T) {
	deps, _ := withCardDeps(t)
	// No stub registered — adapter is silent.
	disp := NewDispatcher(deps)
	start := time.Now()
	_, jerr := disp.getExtendedAgentCard(extendedScopedCtx(t), nil)
	elapsed := time.Since(start)
	if jerr == nil || jerr.Code != -32007 {
		t.Fatalf("jerr = %+v, want -32007", jerr)
	}
	if elapsed > 1*time.Second {
		t.Errorf("waited %s past queryWindow", elapsed)
	}
}

func TestGetExtendedAgentCard_PerRequestNoCache(t *testing.T) {
	deps, nc := withCardDeps(t)

	// Mutate the reply body between calls; the second call must see
	// the new body (i.e. no extended-card cache wedge).
	var current atomic.Pointer[[]byte]
	first := []byte(`{"description":"first body"}`)
	second := []byte(`{"description":"second body"}`)
	current.Store(&first)

	subj, err := subject.CardExtended(deps.AgentKey.Agent, deps.AgentKey.Owner, deps.AgentKey.Name)
	if err != nil {
		t.Fatalf("CardExtended: %v", err)
	}
	sub, err := nc.Subscribe(subj, func(m *nats.Msg) {
		body := *current.Load()
		_ = m.Respond(body)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	disp := NewDispatcher(deps)

	res1, jerr := disp.getExtendedAgentCard(extendedScopedCtx(t), nil)
	if jerr != nil {
		t.Fatalf("first call: %+v", jerr)
	}
	raw1 := res1.(json.RawMessage)

	current.Store(&second)
	res2, jerr := disp.getExtendedAgentCard(extendedScopedCtx(t), nil)
	if jerr != nil {
		t.Fatalf("second call: %+v", jerr)
	}
	raw2 := res2.(json.RawMessage)

	var c1, c2 map[string]any
	if err := json.Unmarshal(raw1, &c1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw2, &c2); err != nil {
		t.Fatal(err)
	}
	if c1["description"] != "first body" {
		t.Errorf("first description = %v", c1["description"])
	}
	if c2["description"] != "second body" {
		t.Errorf("second description = %v (cache wedge — bug)", c2["description"])
	}
}

func TestGetExtendedAgentCard_ComposerUnwired_ReturnsNotConfigured(t *testing.T) {
	deps, _, _ := testDeps(t)
	// Composer + Signer left nil — handler should NOT panic and must
	// return -32007 (externally indistinguishable from "no extended").
	disp := NewDispatcher(deps)
	_, jerr := disp.getExtendedAgentCard(extendedScopedCtx(t), nil)
	if jerr == nil || jerr.Code != -32007 {
		t.Fatalf("jerr = %+v, want -32007", jerr)
	}
}
