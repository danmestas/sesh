// Command sesh-shim is the HTTPS+JSON-RPC A2A gateway binary for Slice 1.
// It bridges remote A2A clients to one local sesh adapter agent over NATS.
//
// See docs/plans/2026-05-24-v0.4-slice-1-shim-skeleton.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/shim/server"
)

type CLI struct {
	Listen        string        `name:"listen" default:"0.0.0.0:8443" env:"SESH_SHIM_LISTEN" help:"HTTPS bind address"`
	NATSURL       string        `name:"nats" env:"NATS_URL" help:"NATS URL; falls back to ~/.sesh/hub.url"`
	TLSCert       string        `name:"tls-cert" env:"SESH_SHIM_TLS_CERT" help:"PEM TLS certificate; in --dev mode self-signed if empty"`
	TLSKey        string        `name:"tls-key" env:"SESH_SHIM_TLS_KEY" help:"PEM TLS key; in --dev mode self-signed if empty"`
	SigningKey    string        `name:"signing-key" env:"SESH_SHIM_SIGNING_KEY" help:"PEM ES256 private key for AgentCard JWS; in --dev mode auto-generated if empty"`
	KeyID         string        `name:"kid" env:"SESH_SHIM_KID" help:"Key ID published in JWKS; auto-derived if empty"`
	Auth          string        `name:"auth" default:"jwt" enum:"jwt,none-dev-only" env:"SESH_SHIM_AUTH" help:"Auth mode for /a2a"`
	JWKSURL       string        `name:"jwks-url" env:"SESH_SHIM_JWKS_URL" help:"Upstream JWKS URL for JWT validation; required when --auth=jwt"`
	Agent         string        `name:"agent" env:"SESH_SHIM_AGENT" help:"Adapter agent token to advertise (single-agent per shim, see Decision D1)"`
	Owner         string        `name:"owner" env:"SESH_SHIM_OWNER" help:"Owner token to advertise"`
	ScopeKind     string        `name:"scope-kind" default:"project" env:"SESH_SHIM_SCOPE_KIND" help:"Task scope kind for KV bucket naming"`
	ScopeID       string        `name:"scope-id" env:"SESH_SHIM_SCOPE_ID" help:"Task scope id for KV bucket naming"`
	GatewayURL    string        `name:"gateway-url" env:"SESH_SHIM_GATEWAY_URL" help:"Public-facing URL advertised in the AgentCard"`
	Machine       string        `name:"machine" env:"SESH_SHIM_MACHINE" help:"Machine token used as the first segment of agents.prompt.v2.*; falls back to os.Hostname() if empty"`
	Dev           bool          `name:"dev" env:"SESH_SHIM_DEV" help:"Enable dev mode: self-signed TLS + ephemeral signing key permitted"`
	ShutdownGrace time.Duration `name:"shutdown-grace" default:"5s" env:"SESH_SHIM_SHUTDOWN_GRACE" help:"Max drain/shutdown wait"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var cli CLI
	kctx := kong.Parse(&cli,
		kong.Name("sesh-shim"),
		kong.Description("HTTPS+JSON-RPC A2A gateway for sesh adapter agents."),
		kong.UsageOnError(),
		kong.BindTo(ctx, (*context.Context)(nil)),
	)

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(ctx, cli, log); err != nil {
		log.Error("sesh-shim: exit", "err", err)
		kctx.Exit(1)
	}
}

func run(ctx context.Context, cli CLI, log *slog.Logger) error {
	if cli.ScopeID == "" {
		return errors.New("--scope-id is required")
	}

	url, err := resolveNATSURL(cli.NATSURL)
	if err != nil {
		return fmt.Errorf("nats url: %w", err)
	}
	nc, err := nats.Connect(url,
		nats.Name("sesh-shim"),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return fmt.Errorf("nats connect %s: %w", url, err)
	}
	defer func() { _ = nc.Drain() }()

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	signer, err := buildSigner(cli, log)
	if err != nil {
		return fmt.Errorf("signer: %w", err)
	}

	validator, err := buildValidator(cli, log)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	composer := card.NewComposer(nc, card.L1Defaults{
		GatewayURL:         cli.GatewayURL,
		ProtocolVersion:    "1.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}, 500*time.Millisecond, log)
	cache := card.NewCache(composer, signer, 5*time.Minute, 64)

	machine := cli.Machine
	if machine == "" {
		if h, err := os.Hostname(); err == nil {
			machine = h
		}
	}
	// Sanitize for the subject-token contract (no `.`, no whitespace,
	// no `*`/`>`). macOS hostnames like "Dans-MacBook-Pro.local" would
	// otherwise silently break SendMessage's prompt publish path. Replace
	// reserved chars; if the result is still empty, fail fast.
	machine = sanitizeMachineToken(machine)
	if machine == "" {
		return errors.New("--machine resolved empty after sanitization; set SESH_SHIM_MACHINE explicitly")
	}

	cfg := server.Config{
		Listen:        cli.Listen,
		TLSCert:       cli.TLSCert,
		TLSKey:        cli.TLSKey,
		Dev:           cli.Dev,
		Auth:          validator,
		Card:          cache,
		Signer:        signer,
		NC:            nc,
		JetStream:     js,
		AgentKey:      card.AgentKey{Agent: cli.Agent, Owner: cli.Owner},
		ScopeKind:     cli.ScopeKind,
		ScopeID:       cli.ScopeID,
		Machine:       machine,
		Logger:        log,
		ShutdownGrace: cli.ShutdownGrace,
	}

	log.Info("sesh-shim: starting",
		"listen", cli.Listen,
		"dev", cli.Dev,
		"auth", cli.Auth,
		"scope", cli.ScopeKind+"/"+cli.ScopeID,
		"nats", url,
	)
	return server.Run(ctx, cfg)
}

func buildSigner(cli CLI, log *slog.Logger) (*card.Signer, error) {
	if cli.SigningKey != "" {
		return card.LoadSigner(cli.SigningKey, cli.KeyID)
	}
	if !cli.Dev {
		return nil, errors.New("--signing-key is required outside --dev mode")
	}
	s, err := card.NewDevSigner()
	if err != nil {
		return nil, err
	}
	log.Warn("sesh-shim: using ephemeral dev signing key", "kid", s.KID())
	return s, nil
}

func buildValidator(cli CLI, log *slog.Logger) (auth.Validator, error) {
	switch cli.Auth {
	case "none-dev-only":
		if !cli.Dev {
			return nil, errors.New("--auth=none-dev-only requires --dev")
		}
		log.Warn("sesh-shim: /a2a auth is disabled (--auth=none-dev-only)")
		return auth.NoopValidator{Logger: log}, nil
	case "jwt":
		if cli.JWKSURL == "" {
			return nil, errors.New("--jwks-url is required when --auth=jwt")
		}
		return auth.NewJWTValidator(cli.JWKSURL)
	default:
		return nil, fmt.Errorf("unknown --auth mode %q", cli.Auth)
	}
}

// resolveNATSURL implements: explicit flag → env NATS_URL → ~/.sesh/hub.url.
// The kong tag for NATSURL already wires NATS_URL env, so by the time we
// see cli.NATSURL=="" both the flag and env are empty and we fall back
// to the hub.url file.
func resolveNATSURL(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".sesh", "hub.url")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no NATS URL: set --nats, NATS_URL, or run `sesh up` so %s exists", path)
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	url := strings.TrimSpace(string(data))
	if url == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return url, nil
}

// sanitizeMachineToken replaces any rune that subject.validateToken
// rejects (., whitespace, *, >) with `-`. macOS `os.Hostname()` like
// "Dans-MacBook-Pro.local" otherwise yields a token that silently
// breaks SendMessage's NATS publish path.
func sanitizeMachineToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '.', r == '*', r == '>':
			b.WriteByte('-')
		case r == ' ', r == '\t', r == '\n', r == '\r':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}
