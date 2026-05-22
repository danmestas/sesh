package refagent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/danmestas/sesh/internal/coord"
)

// coordinateLoop subscribes to the sesh coordination tiers per cfg.Class.
//
// Sesh's coordination subject hierarchy layers on top of the Synadia
// `agents.*` namespace by extending it with three sesh-owned segments
// (machine, project, role) and one identity segment (worker-id):
//
//	agents.<verb>.<machine>.<project>.<session>                       5 tokens — addresses the session's orch (one per sesh)
//	agents.<verb>.<machine>.<project>.<session>.<role>                6 tokens — addresses a role pool (queue group on role)
//	agents.<verb>.<machine>.<project>.<session>.<role>.<worker_id>    7 tokens — addresses a specific worker
//
// Each tier is a distinct NATS subject and reaches only subscribers whose
// pattern resolves to the same token count. Native NATS subject matching
// does the tier routing — no application-level dispatcher required.
//
// Subscription policy by class:
//
//   - class=observer: subscribes to `agents.report.<machine>.<project>.<session>.>`
//     only. Verb-based exclusion ensures `agents.prompt.*` dispatch never
//     reaches an observer; the policy is convention, not type-checked, but
//     enforced by tests.
//
//   - class=active, role=orch: subscribes to the 5-token session front door
//     PLUS the 7-token direct-address. The 6-token role pool is skipped
//     because the orch is the dispatcher, not a worker.
//
//   - class=active, role=<worker>: subscribes to the 6-token role pool
//     (queue group on role, work-stealing) AND the 7-token direct-address.
//
// projectID is empty when the agent is not running inside a sesh project
// (resolveProjectID returned ""); the loop registers no subscriptions
// and waits on ctx. The agent still serves Synadia direct prompts via
// the micro framework registered in register().
//
// Returns when ctx is cancelled. All subscriptions are unsubscribed
// before the function returns; Run relies on this via coordDone.
func coordinateLoop(ctx context.Context, nc *nats.Conn, cfg Config, projectID, instanceID string) error {
	if projectID == "" {
		slog.Info("coordinate: no project-id pinned; skipping coordination subscriptions",
			"agent", cfg.Agent, "role", cfg.Role, "class", cfg.Class)
		<-ctx.Done()
		return nil
	}

	machine := coord.Machine()
	session := cfg.Session

	var subs []*nats.Subscription
	defer func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	}()

	handler := func(verb string) nats.MsgHandler {
		return func(msg *nats.Msg) {
			slog.Info("coordinate: received",
				"verb", verb, "subject", msg.Subject,
				"reply", msg.Reply, "size", len(msg.Data))
		}
	}

	switch cfg.Class {
	case agentmeta.ClassObserver:
		reportFilter := fmt.Sprintf("agents.report.%s.%s.%s.>", machine, projectID, session)
		sub, err := nc.Subscribe(reportFilter, handler("report"))
		if err != nil {
			return fmt.Errorf("subscribe %s: %w", reportFilter, err)
		}
		subs = append(subs, sub)

	case agentmeta.ClassActive:
		// 7-token direct-address: every active agent (worker or orch) is
		// reachable by its instance_id for targeted dispatch.
		direct := fmt.Sprintf("agents.prompt.%s.%s.%s.%s.%s",
			machine, projectID, session, cfg.Role, instanceID)
		sub, err := nc.Subscribe(direct, handler("prompt"))
		if err != nil {
			return fmt.Errorf("subscribe %s: %w", direct, err)
		}
		subs = append(subs, sub)

		if cfg.Role == orchRole {
			// 5-token session front door: orch receives external prompts
			// addressing the session as a whole and decides internal dispatch.
			front := fmt.Sprintf("agents.prompt.%s.%s.%s", machine, projectID, session)
			fsub, err := nc.Subscribe(front, handler("prompt"))
			if err != nil {
				return fmt.Errorf("subscribe %s: %w", front, err)
			}
			subs = append(subs, fsub)
		} else {
			// 6-token role pool: workers of the same role share a queue
			// group keyed on role, so each pool message reaches exactly one.
			pool := fmt.Sprintf("agents.prompt.%s.%s.%s.%s",
				machine, projectID, session, cfg.Role)
			psub, err := nc.QueueSubscribe(pool, cfg.Role, handler("prompt"))
			if err != nil {
				return fmt.Errorf("queue subscribe %s: %w", pool, err)
			}
			subs = append(subs, psub)
		}

	default:
		// applyDefaults + agentmeta.ValidateClass at boot should make this
		// unreachable; belt-and-braces guard so a future Config extension
		// doesn't silently skip coordination.
		slog.Warn("coordinate: unknown class; skipping subscriptions", "class", cfg.Class)
	}

	slog.Info("coordinate: tiers active",
		"agent", cfg.Agent, "role", cfg.Role, "class", cfg.Class,
		"machine", machine, "project_id", projectID, "session", session,
		"subscriptions", len(subs))

	<-ctx.Done()
	return nil
}

// orchRole is the reserved role token identifying the session orchestrator
// — the agent that subscribes to the 5-token session front door. Single
// constant so a typo in coordinateLoop and a typo in operator launch
// config produce a comparison failure at compile time (after a refactor)
// rather than a silent unreachable orch.
const orchRole = "orch"
