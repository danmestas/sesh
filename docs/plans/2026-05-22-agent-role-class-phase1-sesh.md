# Agent Role & Class Registration — Phase 1 (sesh core) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `role` and `class` fields to sesh's agent registration data model so coordination-subject consumers can route on them.

**Architecture:** Two new fields (`Role`, `Class`) flow from env vars (`SESH_ROLE`, `SESH_CLASS`) into `internal/refagent/agent.go`'s `Config`, into the NATS Micro service metadata at `register()`, then read back by `cli/agent_watcher.go` into `AgentRef` and persisted to session JSON. Defaults preserve backward compat: missing fields = `role="worker"`, `class="active"`.

**Tech Stack:** Go 1.22+, `github.com/nats-io/nats.go` micro framework, in-process `nats-server` for tests, existing `validateLabel` validator in `cli/label.go`.

**Source proposal:** `docs/proposals/2026-05-21-agent-role-registration.md`

---

## File Structure

**Create:**
- `internal/agentmeta/agentmeta.go` — `AgentClass` type, constants, defaults
- `internal/agentmeta/validate.go` — `ValidateRole`, `ValidateClass`, `DefaultedRole`, `DefaultedClass`
- `internal/agentmeta/agentmeta_test.go` — table-driven tests for validators + defaults

**Modify:**
- `internal/refagent/agent.go` — add `Role`, `Class` to `Config`; import `agentmeta` for types/validators
- `cli/session.go` — add `Role`, `Class` to `AgentRef` struct
- `cli/agent_watcher.go` — parse `metadata.role` / `metadata.class` via `agentmeta.DefaultedRole`/`DefaultedClass`
- `docs/swarm-workflow.md` — update session JSON example to include role/class

**Test:**
- `internal/refagent/agent_test.go` — Config env reads, metadata emission
- `cli/session_agents_test.go` — AgentRef JSON shape, agent_watcher parses role/class, back-compat defaults

---

## Task 0: Centralize role/class types in `internal/agentmeta/`

**Why:** Without this, `internal/refagent` and `cli/agent_watcher` each carry independent copies of the validators and defaults — change amplification across packages, defaults can drift. One package, one source of truth. (See `docs/plans/2026-05-22-agent-role-class-ousterhout-audit.md` P0 finding.)

**Files:**
- Create: `internal/agentmeta/agentmeta.go`, `internal/agentmeta/validate.go`, `internal/agentmeta/agentmeta_test.go`

- [ ] **Step 1: Write the failing table-driven test**

File: `internal/agentmeta/agentmeta_test.go`

```go
package agentmeta

import (
	"strings"
	"testing"
)

func TestValidateRole(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		wantOK bool
		errSub string
	}{
		{"worker", "worker", true, ""},
		{"implementer", "implementer", true, ""},
		{"hyphen-and-underscore_ok", "abc-def_ghi", true, ""},
		{"digits", "v2", true, ""},
		{"empty", "", false, "empty"},
		{"uppercase", "Worker", false, "must match"},
		{"space", "im plementer", false, "must match"},
		{"slash", "im/plementer", false, "must match"},
		{"too long", strings.Repeat("a", 64), false, "max 63"},
		{"63 ok", strings.Repeat("a", 63), true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRole(tc.role)
			if tc.wantOK && err != nil {
				t.Fatalf("ValidateRole(%q) = %v, want nil", tc.role, err)
			}
			if !tc.wantOK {
				if err == nil {
					t.Fatalf("ValidateRole(%q) = nil, want error containing %q", tc.role, tc.errSub)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("ValidateRole(%q) err = %v, want substring %q", tc.role, err, tc.errSub)
				}
			}
		})
	}
}

func TestValidateClass(t *testing.T) {
	if err := ValidateClass(ClassActive); err != nil {
		t.Errorf("ValidateClass(active) = %v, want nil", err)
	}
	if err := ValidateClass(ClassObserver); err != nil {
		t.Errorf("ValidateClass(observer) = %v, want nil", err)
	}
	if err := ValidateClass(AgentClass("passive")); err == nil {
		t.Errorf("ValidateClass(passive) = nil, want error")
	}
	if err := ValidateClass(AgentClass("")); err == nil {
		t.Errorf("ValidateClass(empty) = nil, want error")
	}
}

func TestDefaultedRole(t *testing.T) {
	if got := DefaultedRole(""); got != DefaultRole {
		t.Errorf("DefaultedRole(empty) = %q, want %q", got, DefaultRole)
	}
	if got := DefaultedRole("implementer"); got != "implementer" {
		t.Errorf("DefaultedRole(implementer) = %q, want implementer", got)
	}
}

func TestDefaultedClass(t *testing.T) {
	if got := DefaultedClass(""); got != DefaultClass {
		t.Errorf("DefaultedClass(empty) = %v, want %v", got, DefaultClass)
	}
	if got := DefaultedClass("observer"); got != ClassObserver {
		t.Errorf("DefaultedClass(observer) = %v, want observer", got)
	}
	if got := DefaultedClass("active"); got != ClassActive {
		t.Errorf("DefaultedClass(active) = %v, want active", got)
	}
	// Unknown values pass through unchanged — validation is the caller's job.
	if got := DefaultedClass("passive"); got != AgentClass("passive") {
		t.Errorf("DefaultedClass(passive) = %v, want passive (unchanged)", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/dmestas/projects/sesh && go test ./internal/agentmeta/ -v`

Expected: FAIL — `no Go files in /internal/agentmeta` or `undefined`.

- [ ] **Step 3: Create the types file**

File: `internal/agentmeta/agentmeta.go`

```go
// Package agentmeta is the canonical home for agent role/class types and
// validation. Both internal/refagent (emit side) and cli/agent_watcher
// (parse side) import this package so the rules live in one file.
//
// Rules are mirrored verbatim from docs/proposals/2026-05-21-agent-role-registration.md.
// Adapters in other languages MUST port the same rules — see that proposal's
// "Canonical role/class rules" section.
package agentmeta

// AgentClass is the on-the-wire class enum. Defined as a string type so
// JSON marshaling matches the wire format, while still letting the
// compiler reject typo'd assignments at every call site that uses the
// constants.
type AgentClass string

const (
	ClassActive   AgentClass = "active"
	ClassObserver AgentClass = "observer"

	DefaultRole  string     = "worker"
	DefaultClass AgentClass = ClassActive
)
```

- [ ] **Step 4: Create the validators file**

File: `internal/agentmeta/validate.go`

```go
package agentmeta

import (
	"fmt"
	"regexp"
)

var roleTokenRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidateRole returns nil if role matches ^[a-z0-9_-]+$ and is 1-63 bytes.
func ValidateRole(role string) error {
	if role == "" {
		return fmt.Errorf("role is empty")
	}
	if len(role) > 63 {
		return fmt.Errorf("role %q is %d bytes; max 63", role, len(role))
	}
	if !roleTokenRE.MatchString(role) {
		return fmt.Errorf("role %q must match ^[a-z0-9_-]+$", role)
	}
	return nil
}

// ValidateClass returns nil iff c is one of the canonical class values.
func ValidateClass(c AgentClass) error {
	if c != ClassActive && c != ClassObserver {
		return fmt.Errorf("class %q must be %q or %q", string(c), ClassActive, ClassObserver)
	}
	return nil
}

// DefaultedRole returns s, or DefaultRole if s is empty.
// Does NOT validate — caller decides whether to validate after defaulting.
func DefaultedRole(s string) string {
	if s == "" {
		return DefaultRole
	}
	return s
}

// DefaultedClass returns AgentClass(s), or DefaultClass if s is empty.
// Does NOT validate — used on the read path where unknown values should
// surface as-is so the caller can decide between defaulting and erroring.
func DefaultedClass(s string) AgentClass {
	if s == "" {
		return DefaultClass
	}
	return AgentClass(s)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/agentmeta/ -v`

Expected: PASS — 4 test functions, 14+ subtests.

- [ ] **Step 6: Checkpoint your progress**

Stage `internal/agentmeta/`. Commit: `feat(agentmeta): add canonical role/class types + validators (single source of truth)`.

---

## Task 1: Config gains Role and Class with env reads (typed via agentmeta)

**Files:**
- Modify: `internal/refagent/agent.go:40-54` (Config struct), `:130-160` (NewConfig defaults), import `agentmeta`
- Test: `internal/refagent/agent_test.go` (new file if absent, else append)

- [ ] **Step 1: Write the failing test for env-var reading and defaults**

File: `internal/refagent/agent_test.go` — add (or create):

```go
package refagent

import (
	"strings"
	"testing"

	"github.com/danmestas/sesh/internal/agentmeta"
)

func TestNewConfig_RoleAndClassFromEnv(t *testing.T) {
	t.Setenv("SESH_ROLE", "implementer")
	t.Setenv("SESH_CLASS", "active")

	var c Config
	c.applyDefaults()

	if c.Role != "implementer" {
		t.Errorf("Role = %q, want implementer", c.Role)
	}
	if c.Class != agentmeta.ClassActive {
		t.Errorf("Class = %v, want active", c.Class)
	}
}

func TestNewConfig_RoleAndClassDefaults(t *testing.T) {
	t.Setenv("SESH_ROLE", "")
	t.Setenv("SESH_CLASS", "")

	var c Config
	c.applyDefaults()

	if c.Role != agentmeta.DefaultRole {
		t.Errorf("Role default = %q, want %q", c.Role, agentmeta.DefaultRole)
	}
	if c.Class != agentmeta.DefaultClass {
		t.Errorf("Class default = %v, want %v", c.Class, agentmeta.DefaultClass)
	}
}

func TestNewConfig_RejectsBadClassAtBoot(t *testing.T) {
	t.Setenv("SESH_CLASS", "passive")
	t.Setenv("SESH_ROLE", "worker")

	_, err := NewConfig() // adjust to actual signature if needed
	if err == nil || !strings.Contains(err.Error(), "class") {
		t.Fatalf("NewConfig with SESH_CLASS=passive: err = %v, want class error", err)
	}
}

func TestNewConfig_RejectsBadRoleAtBoot(t *testing.T) {
	t.Setenv("SESH_ROLE", "Bad Role")
	t.Setenv("SESH_CLASS", "active")

	_, err := NewConfig()
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("NewConfig with SESH_ROLE=\"Bad Role\": err = %v, want role error", err)
	}
}
```

Note the import: `github.com/danmestas/sesh/internal/agentmeta` — adjust to the module's actual path. Find it via `head -1 go.mod`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/dmestas/projects/sesh && go test ./internal/refagent/ -run 'TestNewConfig_(RoleAndClass|RejectsBad)' -v`

Expected: FAIL — `Config has no field Role` or `cannot use ... as agentmeta.AgentClass`.

- [ ] **Step 3: Add typed fields and env reads**

Modify `internal/refagent/agent.go`. Update the `Config` struct (current lines 40-54):

```go
import "github.com/danmestas/sesh/internal/agentmeta"

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
```

Find `applyDefaults` (or the equivalent inline block at lines 130-160) and add env reads alongside the existing `SESH_OWNER` / `SESH_SESSION` block:

```go
	if c.Role == "" {
		c.Role = agentmeta.DefaultedRole(os.Getenv("SESH_ROLE"))
	}
	if c.Class == "" {
		c.Class = agentmeta.DefaultedClass(os.Getenv("SESH_CLASS"))
	}
```

- [ ] **Step 4: Run tests to verify they pass (env-read tests)**

Run: `go test ./internal/refagent/ -run 'TestNewConfig_RoleAndClass' -v`

Expected: PASS for env-read + defaults tests; rejection tests still FAIL (validation not wired yet).

- [ ] **Step 5: Wire validation into NewConfig / Run startup**

Find where `applyDefaults` is called (likely in `NewConfig` or at the top of `Run`). Immediately after defaults are applied, call validators:

```go
	if err := agentmeta.ValidateRole(c.Role); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := agentmeta.ValidateClass(c.Class); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
```

If `NewConfig` does not return an error today, change its signature to `(Config, error)` and update callers. The proposal says "errors on invalid (don't paper over)".

- [ ] **Step 6: Run all four Task 1 tests to verify they pass**

Run: `go test ./internal/refagent/ -run 'TestNewConfig_(RoleAndClass|RejectsBad)' -v`

Expected: PASS — all 4 tests.

- [ ] **Step 7: Checkpoint your progress**

Stage `internal/refagent/agent.go` and `internal/refagent/agent_test.go`. Commit: `feat(refagent): add Role and Class to Config (typed via agentmeta) with env reads + boot validation`.

---

## Task 2: Emit metadata.role and metadata.class in service registration

**Files:**
- Modify: `internal/refagent/agent.go` `register()` method (current lines ~162-190, metadata map at 173-179)
- Test: `internal/refagent/agent_test.go`

- [ ] **Step 1: Write the failing test that asserts metadata has role and class**

Append to `internal/refagent/agent_test.go`:

```go
import (
	"context"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func startInProcNATS(t *testing.T) (*server.Server, string) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(s.Shutdown)
	return s, s.ClientURL()
}

func TestRegister_EmitsRoleAndClassMetadata(t *testing.T) {
	_, url := startInProcNATS(t)

	t.Setenv("SESH_ROLE", "implementer")
	t.Setenv("SESH_CLASS", "active")
	t.Setenv("SESH_SESSION", "rc-test")

	cfg := Config{NATSURL: url} // applyDefaults will fill Role/Class
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, cfg) // assumes Run signature; adjust if it differs

	// Give registration ~500ms to complete.
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	msg, err := nc.Request("$SRV.INFO.agents", nil, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("INFO request: %v", err)
	}
	body := string(msg.Data)
	if !strings.Contains(body, `"role":"implementer"`) {
		t.Errorf("INFO body missing role=implementer:\n%s", body)
	}
	if !strings.Contains(body, `"class":"active"`) {
		t.Errorf("INFO body missing class=active:\n%s", body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/refagent/ -run TestRegister_EmitsRoleAndClassMetadata -v`

Expected: FAIL — `INFO body missing role=implementer`.

- [ ] **Step 3: Extend the metadata map in register()**

In `internal/refagent/agent.go`, find the `register()` method's metadata block (currently):

```go
	metadata := map[string]string{
		"agent":            a.cfg.Agent,
		"owner":            a.cfg.Owner,
		"protocol_version": protocolVersion,
	}
	if a.cfg.Session != "" {
		metadata["session"] = a.cfg.Session
	}
```

Replace with:

```go
	metadata := map[string]string{
		"agent":            a.cfg.Agent,
		"owner":            a.cfg.Owner,
		"protocol_version": protocolVersion,
		"role":             a.cfg.Role,
		"class":            a.cfg.Class,
	}
	if a.cfg.Session != "" {
		metadata["session"] = a.cfg.Session
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/refagent/ -run TestRegister_EmitsRoleAndClassMetadata -v`

Expected: PASS.

- [ ] **Step 5: Run the full refagent test suite**

Run: `go test ./internal/refagent/ -v`

Expected: all tests PASS. No regressions in existing Config / register / handler tests.

- [ ] **Step 6: Checkpoint your progress**

Commit: `feat(refagent): emit metadata.role and metadata.class on service registration`.

---

## Task 3: Add Role and Class to AgentRef struct

**Files:**
- Modify: `cli/session.go:24-29` (AgentRef struct)
- Test: `cli/session_agents_test.go`

- [ ] **Step 1: Write the failing JSON shape test**

Append to `cli/session_agents_test.go`:

```go
func TestAgentRef_JSONShapeIncludesRoleAndClass(t *testing.T) {
	ref := AgentRef{
		Agent:      "claude-code",
		Owner:      "dmestas",
		InstanceID: "ABC123",
		Subject:    "agents.prompt.cc.dmestas.foo",
		Role:       "implementer",
		Class:      "active",
	}
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []string{
		`"agent":"claude-code"`,
		`"owner":"dmestas"`,
		`"instance_id":"ABC123"`,
		`"subject":"agents.prompt.cc.dmestas.foo"`,
		`"role":"implementer"`,
		`"class":"active"`,
	}
	got := string(b)
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("marshaled AgentRef missing %s\nfull: %s", w, got)
		}
	}
}

func TestAgentRef_JSONOmitsEmptyRoleAndClass(t *testing.T) {
	ref := AgentRef{
		Agent:      "claude-code",
		Owner:      "dmestas",
		InstanceID: "ABC123",
		Subject:    "agents.prompt.cc.dmestas.foo",
	}
	b, _ := json.Marshal(ref)
	got := string(b)
	if strings.Contains(got, `"role"`) {
		t.Errorf("expected role to be omitted when empty: %s", got)
	}
	if strings.Contains(got, `"class"`) {
		t.Errorf("expected class to be omitted when empty: %s", got)
	}
}
```

If `strings` isn't already imported in this test file, add it.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/dmestas/projects/sesh && go test ./cli/ -run 'TestAgentRef_JSON' -v`

Expected: FAIL — `AgentRef has no field Role`.

- [ ] **Step 3: Add the fields to AgentRef**

Modify `cli/session.go:24-29`:

```go
type AgentRef struct {
	Agent      string `json:"agent"`
	Owner      string `json:"owner"`
	InstanceID string `json:"instance_id"`
	Subject    string `json:"subject"`
	Role       string `json:"role,omitempty"`
	Class      string `json:"class,omitempty"`
}
```

Also update the doc comment block above the struct (currently lines 14-23) — add two bullets for the new mappings:

```go
//   - Role       ← metadata.role  (defaults to "worker" when absent)
//   - Class      ← metadata.class (defaults to "active" when absent)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cli/ -run 'TestAgentRef_JSON' -v`

Expected: PASS — both tests pass.

- [ ] **Step 5: Run the full cli test suite to confirm no regressions**

Run: `go test ./cli/ -v`

Expected: all PASS. Existing tests that construct `AgentRef` literally (e.g., `session_agents_test.go:252,312`) still pass because the new fields are optional.

- [ ] **Step 6: Checkpoint your progress**

Commit: `feat(cli): add Role and Class to AgentRef struct with omitempty JSON tags`.

---

## Task 4: Parse metadata.role and metadata.class in agent_watcher

**Files:**
- Modify: `cli/agent_watcher.go` (queryAgents at lines 124-190, ref-construction around line 176)
- Test: `cli/session_agents_test.go`

- [ ] **Step 1: Write the failing watcher-parsing test**

Append to `cli/session_agents_test.go`:

```go
func TestAgentWatcher_PopulatesRoleAndClassFromMetadata(t *testing.T) {
	_, url := startTestNATSServer(t)

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	// Register two synthetic agents with explicit role/class metadata.
	subject1 := "agents.prompt.claude-code.dmestas.testlabel"
	svc1, err := micro.AddService(nc, micro.Config{
		Name:    "agents",
		Version: "0.1.0",
		Metadata: map[string]string{
			"agent":   "claude-code",
			"owner":   "dmestas",
			"session": "testlabel",
			"role":    "implementer",
			"class":   "active",
		},
	})
	if err != nil {
		t.Fatalf("svc1: %v", err)
	}
	defer svc1.Stop()
	_ = svc1.AddEndpoint("prompt", micro.HandlerFunc(func(m micro.Request) { _ = m.Respond(nil) }),
		micro.WithEndpointSubject(subject1))

	subject2 := "agents.prompt.pi.dmestas.testlabel"
	svc2, err := micro.AddService(nc, micro.Config{
		Name:    "agents",
		Version: "0.1.0",
		Metadata: map[string]string{
			"agent":   "pi",
			"owner":   "dmestas",
			"session": "testlabel",
			"role":    "spy",
			"class":   "observer",
		},
	})
	if err != nil {
		t.Fatalf("svc2: %v", err)
	}
	defer svc2.Stop()
	_ = svc2.AddEndpoint("prompt", micro.HandlerFunc(func(m micro.Request) { _ = m.Respond(nil) }),
		micro.WithEndpointSubject(subject2))

	// Give registration a moment.
	time.Sleep(100 * time.Millisecond)

	agents := queryAgents(nc, "testlabel", 500*time.Millisecond)
	if len(agents) != 2 {
		t.Fatalf("queryAgents returned %d agents, want 2", len(agents))
	}

	byAgent := map[string]AgentRef{}
	for _, a := range agents {
		byAgent[a.Agent] = a
	}
	if byAgent["claude-code"].Role != "implementer" {
		t.Errorf("claude-code Role = %q, want implementer", byAgent["claude-code"].Role)
	}
	if byAgent["claude-code"].Class != "active" {
		t.Errorf("claude-code Class = %q, want active", byAgent["claude-code"].Class)
	}
	if byAgent["pi"].Role != "spy" {
		t.Errorf("pi Role = %q, want spy", byAgent["pi"].Role)
	}
	if byAgent["pi"].Class != "observer" {
		t.Errorf("pi Class = %q, want observer", byAgent["pi"].Class)
	}
}

func TestAgentWatcher_DefaultsForMissingRoleClassMetadata(t *testing.T) {
	_, url := startTestNATSServer(t)

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	subject := "agents.prompt.legacy.dmestas.bclabel"
	svc, err := micro.AddService(nc, micro.Config{
		Name:    "agents",
		Version: "0.1.0",
		Metadata: map[string]string{
			"agent":   "legacy",
			"owner":   "dmestas",
			"session": "bclabel",
			// No role / class — back-compat.
		},
	})
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	defer svc.Stop()
	_ = svc.AddEndpoint("prompt", micro.HandlerFunc(func(m micro.Request) { _ = m.Respond(nil) }),
		micro.WithEndpointSubject(subject))

	time.Sleep(100 * time.Millisecond)

	agents := queryAgents(nc, "bclabel", 500*time.Millisecond)
	if len(agents) != 1 {
		t.Fatalf("queryAgents returned %d, want 1", len(agents))
	}
	if agents[0].Role != "worker" {
		t.Errorf("default Role = %q, want worker", agents[0].Role)
	}
	if agents[0].Class != "active" {
		t.Errorf("default Class = %q, want active", agents[0].Class)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/dmestas/projects/sesh && go test ./cli/ -run 'TestAgentWatcher_(Populates|Defaults)' -v`

Expected: FAIL — `Role = "", want implementer` (watcher doesn't read the new metadata yet).

- [ ] **Step 3: Extend queryAgents to read role and class via agentmeta**

In `cli/agent_watcher.go`, add the import (top of file):

```go
import "github.com/danmestas/sesh/internal/agentmeta"
```

Find the `ref := AgentRef{` block (around line 176) and replace with:

```go
			ref := AgentRef{
				Agent:      info.Metadata["agent"],
				Owner:      info.Metadata["owner"],
				InstanceID: info.ID,
				Role:       agentmeta.DefaultedRole(info.Metadata["role"]),
				Class:      string(agentmeta.DefaultedClass(info.Metadata["class"])),
			}
```

`AgentRef.Class` stays a `string` (JSON wire format), but the defaulting + canonical values come from `agentmeta` — no hardcoded strings in this file. If `agentmeta` ever changes the default, both emit and parse paths update together.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cli/ -run 'TestAgentWatcher_(Populates|Defaults)' -v`

Expected: PASS — both tests pass.

- [ ] **Step 5: Run the full cli test suite**

Run: `go test ./cli/ -v`

Expected: all PASS.

- [ ] **Step 6: Checkpoint your progress**

Commit: `feat(cli): parse metadata.role and metadata.class from $SRV.INFO.agents with safe defaults`.

---

## Task 5: Session-JSON round-trip test (distinct from Task 4's watcher assertion)

**Why distinct from Task 4:** Task 4 asserts the in-memory `[]AgentRef` returned by `queryAgents` has the right fields. Task 5 asserts that those fields *survive a `Session.UpdateAgents` write-and-reread*. This catches a JSON-tag bug that Task 4 doesn't (e.g., if someone accidentally sets `json:"-"` on `Role`).

**Files:**
- Test: `cli/session_agents_test.go` (append)

- [ ] **Step 1: Write the round-trip test using existing primitives**

The existing test file already constructs `*Session` and calls `sess.UpdateAgents(...)` (see lines around 252 and 312). Follow the same pattern — don't introduce a new `newTestSession` helper.

Append to `cli/session_agents_test.go`:

```go
func TestSessionJSON_RoleAndClassRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "rc.json")

	// Construct a Session using the same constructor existing tests use.
	// (Look at how lines ~250 and ~310 build their *Session — copy that exact
	// pattern. If the constructor is `OpenSession(path)` or similar, use it.)
	sess, err := OpenSession(sessPath) // adjust to match the actual constructor used in nearby tests
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	agents := []AgentRef{
		{
			Agent:      "claude-code",
			Owner:      "dmestas",
			InstanceID: "ABC",
			Subject:    "agents.prompt.cc.dmestas.rc",
			Role:       "implementer",
			Class:      "active",
		},
		{
			Agent:      "pi",
			Owner:      "dmestas",
			InstanceID: "XYZ",
			Subject:    "agents.prompt.pi.dmestas.rc",
			Role:       "spy",
			Class:      "observer",
		},
	}
	if err := sess.UpdateAgents(agents); err != nil {
		t.Fatalf("UpdateAgents: %v", err)
	}

	data, err := os.ReadFile(sessPath)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`"role":"implementer"`,
		`"class":"active"`,
		`"role":"spy"`,
		`"class":"observer"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session JSON missing %s\nfull:\n%s", want, body)
		}
	}
}
```

Note: the exact `Session` constructor name may be `OpenSession`, `NewSession`, or constructed inline as `&Session{path: ...}`. Look at lines 250 and 310 of the *current* `cli/session_agents_test.go` and copy the literal pattern used there. The plan deliberately does not introduce a new helper.

- [ ] **Step 2: Run the test**

Run: `go test ./cli/ -run TestSessionJSON_RoleAndClassRoundTrip -v`

Expected: PASS once Task 3 has run (the AgentRef fields exist) and `UpdateAgents` writes them. If FAIL, the JSON tags on AgentRef are wrong — re-check Task 3 Step 3.

- [ ] **Step 3: Checkpoint your progress**

Commit: `test(cli): session JSON round-trip preserves role and class`.

---

## Task 6: Update session manifest doc

**Files:**
- Modify: `docs/swarm-workflow.md` (search for the existing session JSON example) — add role/class fields. If no example exists there, add a new section to `docs/sesh-ref-agent.md` titled "Session manifest fields".

- [ ] **Step 1: Locate the existing session JSON example**

Run: `grep -rn '"agents"' docs/ | head -10`

Identify the canonical doc that shows the session manifest shape. Likely `docs/swarm-workflow.md` or `docs/synadia-agents-on-sesh.md`.

- [ ] **Step 2: Update the example to include role and class**

Replace the existing example with this exact block:

```json
{
  "pid": 42629,
  "scope": "session",
  "nats_url": "nats://127.0.0.1:65261",
  "agents": [
    {
      "agent": "claude-code",
      "owner": "dmestas",
      "instance_id": "ABC123",
      "subject": "agents.prompt.cc.dmestas.foo",
      "role": "implementer",
      "class": "active"
    },
    {
      "agent": "pi",
      "owner": "dmestas",
      "instance_id": "XYZ789",
      "subject": "agents.prompt.pi.dmestas.foo",
      "role": "spy",
      "class": "observer"
    }
  ]
}
```

Below the example add this paragraph:

> **`role`** is a free-form short token (`^[a-z0-9_-]+$`, 1–63 chars) identifying the function an agent plays in the swarm — e.g. `implementer`, `verifier`, `spy`, `planner`. Defaults to `worker` when unset.
>
> **`class`** is `active` (agent expects work) or `observer` (read-only watcher; spies). Defaults to `active`. Coordination subjects (see `docs/proposals/2026-05-20-sesh-parallel-coordination-subjects.md`) target by class — `workers.*` reaches active agents, `spies.*` reaches observers.
>
> Both fields are set via the `SESH_ROLE` and `SESH_CLASS` environment variables read by adapters (e.g. `claude-nats-channel`) at boot. Agents that don't set the metadata appear with the default values.

- [ ] **Step 3: Checkpoint your progress**

Commit: `docs(swarm-workflow): document role/class fields in session manifest`.

---

## Task 7: Replicate local CI

**Files:** (no edits — verification only)

- [ ] **Step 1: Run the full test suite**

Run: `cd /Users/dmestas/projects/sesh && go test ./...`

Expected: all PASS, including any e2e tests gated on `SESH_E2E_*` env vars that happen to be set.

- [ ] **Step 2: Run go vet and any project linter**

Run: `go vet ./... && go build ./...`

Expected: no warnings, build succeeds.

- [ ] **Step 3: Check `.github/workflows/` for any additional CI steps**

Run: `ls .github/workflows/ && cat .github/workflows/*.yml | head -100`

Replicate any extra `go test`/`golangci-lint`/build matrix step CI runs.

- [ ] **Step 4: Final checkpoint**

If anything failed in Steps 1-3, fix it before opening the PR. Otherwise commit any final cleanup and push the branch.

---

## Acceptance (from proposal)

- [x] `Config` gains `Role` / `Class` with env-var population and validation. → Task 1
- [x] `agent_watcher.go` parses `metadata.role` / `metadata.class` from `$SRV.INFO.agents` with safe defaults. → Task 4
- [x] `AgentRef` JSON gains the two fields (`omitempty`). → Task 3
- [x] Session manifest schema doc updated to show the new fields. → Task 6
- [ ] At least one adapter (claude-nats-channel) updated to read the env and set the metadata. → **Phase 2 plan (separate)**
- [x] Integration test: spawn a session with `SESH_ROLE=implementer SESH_CLASS=active` and assert the values appear in `agents[]` within `~1s`. → Task 5
- [x] Integration test: spawn a second agent with `SESH_CLASS=observer` and assert it appears alongside but with `class=observer`. → Task 4 (`TestAgentWatcher_PopulatesRoleAndClassFromMetadata` covers this)
- [x] Backward-compat test: spawn an adapter that does NOT set the env / metadata and assert it appears with `role=worker class=active` defaults, no errors. → Task 4 (`TestAgentWatcher_DefaultsForMissingRoleClassMetadata`)

---

## Out of Scope (other phases)

- **Phase 2:** Adapter wiring (`claude-nats-channel` and siblings) — see `2026-05-22-agent-role-class-phase2-adapters.md`.
- **Phase 3:** `orch-spawn` exporting `SESH_ROLE` / `SESH_CLASS` — see `2026-05-22-agent-role-class-phase3-orch-spawn.md`.
- **Phase 4:** `sesh up --exec` flag plumbing — gated on sesh#89; not planned here.
