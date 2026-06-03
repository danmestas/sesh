package refagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/sesh/internal/coord"
)

// TestRun_ReadsInjectedProjectID is the load-bearing C1 test: the agent
// sources its pinned 40-hex projectID from the injected SESH_PROJECT_ID env
// and brings up coordination subscriptions at the CORRECT subject whose 4th
// token equals the injected id — WITHOUT walking the filesystem for a
// .sesh/project-id pin.
//
// The cwd deliberately has NO .sesh/project-id, so any residual filesystem
// derivation would yield the wrong (or empty) id and the publish below would
// not be acked. A round-trip ack at the injected-id subject proves the
// injected value is what the loop subscribed under.
func TestRun_ReadsInjectedProjectID(t *testing.T) {
	url := startBroker(t)

	// Temp cwd with NO .sesh/project-id pin — injection must be the only
	// identity source.
	t.Chdir(t.TempDir())

	const injectedID = "abc123abc123abc123abc123abc123abc1230000"
	t.Setenv("NATS_URL", url)
	t.Setenv("SESH_PROJECT_ID", injectedID)
	t.Setenv("SESH_MACHINE", coord.MachineLocal)
	t.Setenv("SESH_SESSION", "s1")
	t.Setenv("SESH_ROLE", "implementer")
	t.Setenv("SESH_CLASS", "active")

	cancel, _ := runAgent(t, Config{
		Agent: "echo", Owner: "alice",
		Interval: 200 * time.Millisecond,
	})
	defer cancel()

	nc := testConn(t, url)

	// The 6-token role pool subject the active implementer subscribes to.
	// Its 4th token (index 3) is the projectID. We request/reply to it and
	// expect an ack — only possible if the loop subscribed under injectedID.
	poolSubj := fmt.Sprintf("agents.prompt.%s.%s.s1.implementer",
		coord.MachineLocal, injectedID)

	// Assert the 4th token equals the injected id (defensive: catches a
	// builder regression that reorders subject tokens).
	if tok := strings.Split(poolSubj, ".")[3]; tok != injectedID {
		t.Fatalf("4th subject token = %q, want injected id %q", tok, injectedID)
	}

	var lastErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := nc.Request(poolSubj, []byte("dispatch"), 200*time.Millisecond)
		if err == nil {
			if !strings.Contains(string(msg.Data), `"role":"implementer"`) {
				t.Fatalf("ack body = %s, want role=implementer", msg.Data)
			}
			return // success — injected id round-tripped to the subscription
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no ack on injected-id subject %s within 3s (last err: %v)", poolSubj, lastErr)
}

// TestRun_MissingProjectIDFailsLoud asserts that an absent identity env
// (no SESH_PROJECT_ID, no .sesh/project-id pin) yields a CLEAR boot error
// rather than a silent skip / filesystem walk.
func TestRun_MissingProjectIDFailsLoud(t *testing.T) {
	// Temp cwd with NO .sesh/project-id pin.
	t.Chdir(t.TempDir())

	t.Setenv("SESH_PROJECT_ID", "")
	t.Setenv("SESH_ROLE", "worker")
	t.Setenv("SESH_CLASS", "active")
	t.Setenv("NATS_URL", "nats://127.0.0.1:1") // unreachable; must never be dialed

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Run(ctx, Config{Agent: "echo", Owner: "alice"})
	if err == nil || !strings.Contains(err.Error(), "project-id") {
		t.Fatalf("Run with no SESH_PROJECT_ID: err = %v, want a clear project-id boot error", err)
	}
}

// TestApplyDefaults_ProjectIDFromEnv pins the env-read path for the new
// injected identity field.
func TestApplyDefaults_ProjectIDFromEnv(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef01234567"
	t.Setenv("SESH_PROJECT_ID", id)

	var c Config
	c.applyDefaults()

	if c.ProjectID != id {
		t.Errorf("ProjectID = %q, want %q", c.ProjectID, id)
	}
}

// TestApplyDefaults_ExplicitProjectIDWins asserts an explicitly-set Config
// field is not clobbered by the env read (flag/explicit > env).
func TestApplyDefaults_ExplicitProjectIDWins(t *testing.T) {
	t.Setenv("SESH_PROJECT_ID", "envvalueenvvalueenvvalueenvvalueenvvalue")

	c := Config{ProjectID: "explicitexplicitexplicitexplicitexplicit"}
	c.applyDefaults()

	if c.ProjectID != "explicitexplicitexplicitexplicitexplicit" {
		t.Errorf("ProjectID = %q, want explicit value preserved", c.ProjectID)
	}
}
