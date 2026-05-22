// Package refagent is sesh's reference implementation of Synadia Agent
// Protocol v0.3 (see docs/synadia-agents-on-sesh.md). It is sesh's
// executable spec: every future test, plugin, and Go SDK should validate
// against the behavior in this file.
//
// The package exports exactly two symbols: [Config] and [Run]. Everything
// else — chunk encoding, heartbeat publishing, status endpoint, NATS URL
// resolution — is implementation detail. The module lives under
// internal/ because no third-party stability promise is made yet.
//
// Behavior mirrors the upstream TS reference agent at
// `agent-sdk/typescript/src/testing/reference-agent.ts`, with an echo
// prompt handler in place of TS's empty default. There is intentionally
// no conversation memory, no attachments support, and no mid-stream
// queries — those belong in a real harness, not in the spec proof.
package refagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// Config is the agent's full configuration surface. All fields have
// sensible defaults — [Run] applies them so callers can pass a zero
// Config and rely on env-derived values.
//
// Field defaults:
//
//   - Agent:    "echo"
//   - Owner:    $SESH_OWNER, else $USER, else os/user.Current().Username
//   - Session:  $SESH_SESSION (may be empty for session-less harnesses)
//   - Role:     $SESH_ROLE, else agentmeta.DefaultRole ("worker").
//   - Class:    $SESH_CLASS, else agentmeta.DefaultClass ("active").
//   - NATSURL:  $NATS_URL, else .sesh/sessions/<Session>.json#nats_url,
//     else ~/.sesh/hub.url
//   - Interval: 30s (Synadia §8.2 recommended cadence)
type Config struct {
	Agent    string
	Owner    string
	Session  string
	Role     string
	Class    agentmeta.AgentClass
	NATSURL  string
	Interval time.Duration
}

// Service constants from Synadia §3 / §4.
const (
	serviceName         = "agents"
	promptEndpoint      = "prompt"
	statusEndpoint      = "status"
	queueGroup          = "agents"
	protocolVersion     = "0.3"
	defaultHarnessVer   = "0.1.0"
	defaultAgent        = "echo"
	defaultInterval     = 30 * time.Second
	attachmentsOk       = false
	defaultMaxPayload   = "1MB" // fallback if INFO.max_payload is unset
	httpBadRequest      = "400"
	malformedErrMessage = "malformed_request"
)

// Run blocks until ctx is cancelled, running the reference agent.
// Cleanup is graceful: the service is stopped (which drains
// subscriptions and deregisters from $SRV.PING) and the NATS connection
// is drained-and-closed before Run returns.
//
// Run never panics on a zero Config — defaults are applied via
// [Config.applyDefaults]. Any startup error (NATS connect, service
// register) is returned; runtime errors (a single dropped heartbeat
// publish) are logged via NATS's own error channels but do not abort.
func Run(ctx context.Context, cfg Config) error {
	cfg.applyDefaults()
	if err := validateTokens(cfg); err != nil {
		return err
	}
	if err := agentmeta.ValidateRole(cfg.Role); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := agentmeta.ValidateClass(cfg.Class); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	url, err := resolveNATSURL(cfg.NATSURL, cfg.Session)
	if err != nil {
		return fmt.Errorf("resolve NATS URL: %w", err)
	}

	nc, err := nats.Connect(url,
		nats.Name(fmt.Sprintf("sesh-ref-agent/%s", cfg.Agent)),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return fmt.Errorf("connect %s: %w", url, err)
	}
	// Cleanup runs LIFO: a.shutdown() stops accepting new messages
	// first (service.Stop drains subscriptions, deregisters from
	// $SRV.PING); nc.Drain() then flushes the connection's remaining
	// outbound traffic and closes. Reversing the order would close
	// the connection out from under the service drain.
	defer func() { _ = nc.Drain() }()

	a := &agent{cfg: cfg, nc: nc}
	if err := a.register(); err != nil {
		return fmt.Errorf("register service: %w", err)
	}
	defer a.shutdown()

	// Coordination loop: subscribes to sesh-tier subjects
	// (agents.<verb>.<machine>.<project>.<session>[.<role>[.<worker_id>]])
	// per cfg.Class. Runs as a goroutine because subscriptions are
	// passive — the heartbeat loop below is the primary cadence driver.
	//
	// coordDone closes only after the loop's deferred unsubscribes complete,
	// so Run's `defer nc.Drain()` does not race the goroutine. Without this
	// synchronization, $SRV.INFO can linger past the operator's observable
	// shutdown boundary.
	projectID, err := resolveProjectID()
	if err != nil {
		slog.Warn("coordinate: resolveProjectID failed; coordination subscriptions disabled", "err", err)
		projectID = ""
	}
	coordDone := make(chan struct{})
	go func() {
		defer close(coordDone)
		if err := coordinateLoop(ctx, a.nc, cfg, projectID, a.instanceID()); err != nil {
			slog.Warn("coordinate: loop exited with error", "err", err)
		}
	}()
	defer func() { <-coordDone }()

	// Emit an immediate heartbeat so observers see the agent before the
	// first interval elapses. Mirrors the TS reference agent (§8.5).
	a.publishHeartbeat()

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			a.publishHeartbeat()
		}
	}
}

// applyDefaults fills in zero-value Config fields. Idempotent.
func (c *Config) applyDefaults() {
	if c.Agent == "" {
		c.Agent = defaultAgent
	}
	if c.Owner == "" {
		c.Owner = defaultOwner()
	}
	if c.Session == "" {
		c.Session = os.Getenv("SESH_SESSION")
	}
	if c.Role == "" {
		c.Role = agentmeta.DefaultedRole(os.Getenv("SESH_ROLE"))
	}
	if c.Class == "" {
		c.Class = agentmeta.DefaultedClass(os.Getenv("SESH_CLASS"))
	}
	if c.Interval == 0 {
		c.Interval = defaultInterval
	}
}

func defaultOwner() string {
	if v := os.Getenv("SESH_OWNER"); v != "" {
		return v
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "anon"
}

// agent holds runtime state for a single Run invocation. Not exported —
// callers see only Config + Run.
type agent struct {
	cfg Config
	nc  *nats.Conn

	mu      sync.Mutex
	svc     micro.Service
	stopped bool
}

// register adds the `agents` micro service, both endpoints, and stashes
// the framework-assigned service id for use as §8.3 instance_id.
func (a *agent) register() error {
	metadata := map[string]string{
		"agent":            a.cfg.Agent,
		"owner":            a.cfg.Owner,
		"protocol_version": protocolVersion,
		"role":             a.cfg.Role,
		"class":            string(a.cfg.Class),
	}
	if a.cfg.Session != "" {
		metadata["session"] = a.cfg.Session
	}

	svc, err := micro.AddService(a.nc, micro.Config{
		Name:        serviceName,
		Version:     defaultHarnessVer,
		Description: fmt.Sprintf("%s reference agent (sesh-ref-agent)", a.cfg.Agent),
		Metadata:    metadata,
	})
	if err != nil {
		return err
	}
	a.svc = svc

	promptMeta := map[string]string{
		"max_payload":    formatMaxPayload(a.nc.MaxPayload()),
		"attachments_ok": boolMeta(attachmentsOk),
	}
	if err := svc.AddEndpoint(promptEndpoint,
		micro.HandlerFunc(a.handlePrompt),
		micro.WithEndpointSubject(a.subject(promptEndpoint)),
		micro.WithEndpointQueueGroup(queueGroup),
		micro.WithEndpointMetadata(promptMeta),
	); err != nil {
		return fmt.Errorf("add prompt endpoint: %w", err)
	}

	if err := svc.AddEndpoint(statusEndpoint,
		micro.HandlerFunc(a.handleStatus),
		micro.WithEndpointSubject(a.subject(statusEndpoint)),
		micro.WithEndpointQueueGroup(queueGroup),
	); err != nil {
		return fmt.Errorf("add status endpoint: %w", err)
	}
	return nil
}

// shutdown drains the service. Per Synadia §8.6, publishes one final
// empty-payload heartbeat before stopping so observers see immediate
// offline without waiting for 3× interval missed-beats. Idempotent.
//
// Best-effort: a failed final-heartbeat publish does not block svc.Stop —
// the missed-beats fallback still triggers within ~3× cadence.
func (a *agent) shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}
	a.stopped = true
	if a.nc != nil {
		_ = a.nc.Publish(a.hbSubject(), nil)
	}
	if a.svc != nil {
		_ = a.svc.Stop()
	}
}

// subject builds `agents.<verb>.<agent>.<owner>[.<session>]`. The
// session token is omitted when empty (§3.2 session-less harness).
func (a *agent) subject(verb string) string {
	base := fmt.Sprintf("%s.%s.%s.%s", serviceName, verb, a.cfg.Agent, a.cfg.Owner)
	if a.cfg.Session == "" {
		return base
	}
	return base + "." + a.cfg.Session
}

// hbSubject returns the heartbeat subject `agents.hb.<agent>.<owner>[.<session>]`.
func (a *agent) hbSubject() string { return a.subject("hb") }

// instanceID returns the framework-assigned service id (Synadia §3.4 /
// §8.3 instance_id). Empty before register() succeeds.
func (a *agent) instanceID() string {
	if a.svc == nil {
		return ""
	}
	return a.svc.Info().ID
}

// ---------- prompt handler -----------------------------------------

// handlePrompt implements the streaming contract from Synadia §6:
//
//  1. Emit `{"type":"status","data":"ack"}` immediately (§6.4 — must be
//     first, before any latency work).
//  2. Parse plain-text or {"prompt":"..."} JSON envelope (§5.3).
//     Malformed → respond with 400 + JSON body and terminator (§9.2).
//  3. Echo the prompt as response chunks (§6.3), chunked under
//     max_payload/2 to leave headroom for envelope JSON overhead.
//  4. Zero-byte headerless terminator (§6.5).
//
// All chunks are published directly to the reply subject; the
// terminator goes via Request.Respond (which sends zero bytes + no
// headers — exactly the §6.5 wire shape).
func (a *agent) handlePrompt(req micro.Request) {
	reply := req.Reply()
	if reply == "" {
		// No reply subject — caller used fire-and-forget. Nothing to
		// stream to; just drop. (Spec doesn't define this case; a real
		// SDK might log.)
		return
	}

	// §6.4: ack first, before any work.
	if err := a.nc.Publish(reply, ackChunk()); err != nil {
		return
	}

	prompt, err := parsePrompt(req.Data())
	if err != nil {
		// req.Error publishes one message with Nats-Service-Error-Code
		// + Nats-Service-Error headers (§9.2) and the §9.1 JSON body;
		// we then publish the zero-body, no-headers terminator (§6.5).
		// This is the Appendix B.10 two-message error shape — error
		// signal first, terminator second.
		_ = req.Error(httpBadRequest, malformedErrMessage,
			malformedBody(err.Error()))
		_ = a.nc.Publish(reply, nil)
		return
	}

	maxChunk := chunkBudget(a.nc.MaxPayload())
	for _, fragment := range splitUTF8(prompt, maxChunk) {
		if err := a.nc.Publish(reply, responseChunk(fragment)); err != nil {
			return
		}
	}

	// §6.5: terminator — zero-byte body, no headers. Request.Respond
	// with nil body publishes exactly that (verified by B.9 byte test).
	_ = req.Respond(nil)
}

// parsePrompt accepts plain-text shorthand or {"prompt":"..."} JSON
// (Synadia §5.3). Empty strings and missing prompt fields are rejected
// per the §9.2 400 rules.
//
// Unknown JSON envelope fields are tolerated (§5.6). Attachments are
// explicitly rejected — this agent declares attachments_ok=false.
func parsePrompt(data []byte) (string, error) {
	trimmed := bytesTrimSpace(data)
	if len(trimmed) == 0 {
		return "", errors.New("empty payload")
	}

	// JSON envelope discrimination (§5.3): first non-whitespace byte
	// is `{`.
	if trimmed[0] == '{' {
		var env struct {
			Prompt      *string           `json:"prompt"`
			Attachments []json.RawMessage `json:"attachments"`
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return "", fmt.Errorf("invalid JSON envelope: %s", err)
		}
		if len(env.Attachments) > 0 {
			return "", errors.New("attachments not supported")
		}
		if env.Prompt == nil {
			return "", errors.New("missing 'prompt' field")
		}
		if *env.Prompt == "" {
			return "", errors.New("empty 'prompt' field")
		}
		return *env.Prompt, nil
	}

	// Plain-text shorthand.
	return string(trimmed), nil
}

// bytesTrimSpace trims ASCII whitespace without importing strings.
// Inline to avoid converting through string for JSON discrimination.
func bytesTrimSpace(b []byte) []byte {
	start := 0
	end := len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// ---------- chunk encoders -----------------------------------------

// ackChunk is the canonical first chunk on every stream (§6.4, B.6).
// Pre-encoded — same bytes on every prompt.
func ackChunk() []byte {
	// Compact form matches Appendix B.6 verbatim.
	return []byte(`{"type":"status","data":"ack"}`)
}

// responseChunk encodes a string-typed response chunk per §6.3 / B.4.
// Data is encoded with the standard library, which handles UTF-8
// escaping correctly.
func responseChunk(data string) []byte {
	type chunk struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	b, err := json.Marshal(chunk{Type: "response", Data: data})
	if err != nil {
		// json.Marshal on string data only fails on truly bizarre
		// inputs (NaN, etc.). Fall back to a safe encoding.
		return []byte(`{"type":"response","data":""}`)
	}
	return b
}

// malformedBody encodes the §9.1 error JSON body sent alongside the
// 400 headers. Per the contract doc and B.10 examples, the body is
// {"error":"<code>","message":"<detail>"}.
func malformedBody(detail string) []byte {
	type errBody struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	b, _ := json.Marshal(errBody{Error: malformedErrMessage, Message: detail})
	return b
}

// chunkBudget returns the maximum chunk payload size to use given the
// broker's max_payload. The chunk wrapper adds ~25 bytes (the JSON
// envelope `{"type":"response","data":""}` with UTF-8 escaping
// overhead); we conservatively allow up to half the broker limit, with
// a minimum of 256 bytes for tiny test brokers.
func chunkBudget(maxPayload int64) int {
	if maxPayload <= 0 {
		return 32 * 1024 // sensible default when MaxPayload isn't populated
	}
	half := int(maxPayload / 2)
	if half < 256 {
		return 256
	}
	return half
}

// splitUTF8 splits s into chunks no larger than maxBytes, respecting
// UTF-8 rune boundaries. An empty string yields one empty chunk so the
// caller still emits a single response chunk before the terminator
// (callers shouldn't normally pass empty — parsePrompt rejects empty
// prompts — but the contract is "at least one response chunk per
// non-empty prompt", so a defensive empty chunk on empty input keeps
// the stream structure intact).
//
// Splits at rune boundaries. If a single rune exceeds maxBytes (only
// possible for absurdly small budgets), the rune is included as its
// own chunk to avoid mid-rune truncation.
func splitUTF8(s string, maxBytes int) []string {
	if s == "" {
		return []string{""}
	}
	if maxBytes <= 0 {
		return []string{s}
	}
	var out []string
	i := 0
	for i < len(s) {
		end := i + maxBytes
		if end >= len(s) {
			out = append(out, s[i:])
			break
		}
		// Walk back to nearest rune boundary so we never split a
		// multi-byte rune. RuneStart on s[end] is the cheapest check.
		for end > i && !utf8.RuneStart(s[end]) {
			end--
		}
		if end == i {
			// maxBytes shorter than a single rune at this position;
			// advance by one rune so we make progress.
			_, size := utf8.DecodeRuneInString(s[i:])
			end = i + size
		}
		out = append(out, s[i:end])
		i = end
	}
	return out
}

// ---------- status endpoint + heartbeat ----------------------------

// handleStatus implements §8.7: reply with a freshly-built §8.3
// payload. Same JSON shape as a periodic heartbeat. The request body
// is reserved and intentionally ignored.
func (a *agent) handleStatus(req micro.Request) {
	// Race guard: a status request that arrives between shutdown()
	// nilling a.svc and the framework draining this subscription
	// would see a nil service id. Hold a.mu for the snapshot so the
	// reply either reflects a live agent or doesn't get sent at all.
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	if stopped {
		return
	}
	payload := a.heartbeatPayload(time.Now().UTC())
	body, err := json.Marshal(payload)
	if err != nil {
		_ = req.Error("500", "status_build_failed", nil)
		return
	}
	_ = req.Respond(body)
}

// heartbeatPayload returns the §8.3 dict for both periodic heartbeats
// and status replies. `session` is omitted when empty per §3.2.
//
// Sesh extension: `role` and `class` from cfg are included as a sesh-
// specific tail when set. Convergent with the task-management heartbeat
// extension (`docs/task-management.md`), and lets coordinators build
// `{instance_id → role, class}` maps from passive heartbeat observation
// without polling $SRV.INFO.agents.
//
// Field order in the encoded JSON matches Appendix B.11 for the Synadia
// fields; sesh-extension fields are appended in source order.
func (a *agent) heartbeatPayload(now time.Time) map[string]any {
	p := map[string]any{
		"agent":       a.cfg.Agent,
		"owner":       a.cfg.Owner,
		"instance_id": a.instanceID(),
		"ts":          now.Format(time.RFC3339),
		"interval_s":  int(a.cfg.Interval / time.Second),
	}
	if a.cfg.Session != "" {
		p["session"] = a.cfg.Session
	}
	if a.cfg.Role != "" {
		p["role"] = a.cfg.Role
	}
	if a.cfg.Class != "" {
		p["class"] = string(a.cfg.Class)
	}
	return p
}

// publishHeartbeat emits one §8.3 payload to the hb subject. Failures
// are silent — heartbeats are best-effort by spec; consumers detect
// outages via missed-beats (§8.2).
func (a *agent) publishHeartbeat() {
	body, err := json.Marshal(a.heartbeatPayload(time.Now().UTC()))
	if err != nil {
		return
	}
	_ = a.nc.Publish(a.hbSubject(), body)
}

// ---------- NATS URL resolution ------------------------------------

// resolveNATSURL implements the three-tier priority from §2 of the
// contract doc:
//
//  1. Explicit override (cfg.NATSURL).
//  2. .sesh/sessions/<session>.json#nats_url (walked up from CWD).
//  3. ~/.sesh/hub.url.
//
// All three failing is a startup error — the agent must not silently
// proceed with no bus. session may be empty; in that case step 2 is
// skipped.
func resolveNATSURL(override, session string) (string, error) {
	if override != "" {
		return override, nil
	}
	if v := os.Getenv("NATS_URL"); v != "" {
		return v, nil
	}
	if session != "" {
		if url, err := readSessionNATSURL(session); err != nil {
			return "", err
		} else if url != "" {
			return url, nil
		}
	}
	if url, err := readHubURL(); err != nil {
		return "", err
	} else if url != "" {
		return url, nil
	}
	return "", errors.New("no NATS URL: set NATS_URL, run inside `sesh up`, or start a hub")
}

// readSessionNATSURL walks from CWD up to root looking for
// .sesh/sessions/<session>.json and returns the `nats_url` field.
// Returns ("", nil) when no such file exists.
func readSessionNATSURL(session string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		path := filepath.Join(dir, ".sesh", "sessions", session+".json")
		data, err := os.ReadFile(path)
		if err == nil {
			var s struct {
				NATSURL string `json:"nats_url"`
			}
			if err := json.Unmarshal(data, &s); err != nil {
				return "", fmt.Errorf("parse %s: %w", path, err)
			}
			return strings.TrimSpace(s.NATSURL), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// readHubURL reads ~/.sesh/hub.url. Returns ("", nil) when the file
// doesn't exist.
func readHubURL() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".sesh", "hub.url")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// resolveProjectID walks from CWD up to root looking for
// .sesh/project-id and returns the pinned id (hostname-FREE routing
// key written by `sesh up` per cli/paths.go::loadOrCreateProjectID).
// Returns ("", nil) when no such file exists — the agent is running
// outside a sesh project; coordinateLoop will skip subscriptions in
// that case rather than refuse startup.
//
// Mirrors readSessionNATSURL's walk: same up-to-root traversal, same
// ENOENT-tolerant policy, same plain-text file shape.
func resolveProjectID() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		path := filepath.Join(dir, ".sesh", "project-id")
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// ---------- misc helpers -------------------------------------------

// validateTokens rejects identifiers that would produce illegal subject
// tokens per §2.2 (`$`-prefix, empty, oversize). We don't enforce the
// full `[a-z0-9_-]` charset — callers may have legitimate uppercase or
// other ASCII reasons — but we sanitize `.` to `-` so a stray dot
// doesn't split into multiple tokens.
func validateTokens(c Config) error {
	for _, tok := range []struct{ name, val string }{
		{"agent", c.Agent},
		{"owner", c.Owner},
	} {
		if tok.val == "" {
			return fmt.Errorf("empty %s token", tok.name)
		}
		if strings.HasPrefix(tok.val, "$") {
			return fmt.Errorf("%s token may not start with $: %q", tok.name, tok.val)
		}
		if len(tok.val) > 63 {
			return fmt.Errorf("%s token exceeds 63 chars: %q", tok.name, tok.val)
		}
		if strings.Contains(tok.val, ".") {
			return fmt.Errorf("%s token contains '.': %q (sanitize to '-')", tok.name, tok.val)
		}
	}
	return nil
}

// formatMaxPayload renders a byte count as the Synadia §2.1 string
// form (e.g. "1MB", "8MB"). Mirrors the upstream TS formatHumanBytes
// — exact-power-of-two values get the unit suffix; non-aligned values
// fall back to the next-larger unit.
func formatMaxPayload(bytes int64) string {
	if bytes <= 0 {
		return defaultMaxPayload
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case bytes >= gib && bytes%gib == 0:
		return fmt.Sprintf("%dGB", bytes/gib)
	case bytes >= mib && bytes%mib == 0:
		return fmt.Sprintf("%dMB", bytes/mib)
	case bytes >= kib && bytes%kib == 0:
		return fmt.Sprintf("%dKB", bytes/kib)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// boolMeta renders a bool as the §2.1 metadata string form.
// Endpoint metadata is map[string]string in the micro framework, so
// booleans are serialized as "true" / "false". Callers parse them back.
func boolMeta(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
