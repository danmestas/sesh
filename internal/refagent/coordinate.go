package refagent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/danmestas/sesh/internal/coord"
	"github.com/danmestas/sesh/internal/subject"
)

// coordinateLoop subscribes to the sesh coordination subjects per cfg.Class.
//
// Sesh's coordination subjects layer on top of the Synadia `agents.*`
// namespace by extending it with two sesh-owned segments (machine,
// project) plus the session segment — a single 5-token, session-scoped
// subject per verb:
//
//	agents.<verb>.<machine>.<project>.<session>    5 tokens — addresses the session
//
// There is no role-pool or direct-instance addressing tier. Work-stealing
// among the active agents in a session rides a NATS queue group
// (subject.PromptQueueGroup) on the SUBSCRIBE side, not the subject:
// every active agent QueueSubscribes the same 5-token prompt subject under
// that group, so each prompt is delivered to exactly one member. A single-
// member group degenerates to a plain subscribe; a multi-member one keeps
// the work-stealing semantics.
//
// Subscription policy by class:
//
//   - class=observer: subscribes to `agents.report.<machine>.<project>.<session>.>`
//     only. Verb-based exclusion ensures `agents.prompt.*` dispatch never
//     reaches an observer; the policy is convention, not type-checked, but
//     enforced by tests.
//
//   - class=active: QueueSubscribes the 5-token prompt subject under
//     subject.PromptQueueGroup. Role is no longer consulted for subject
//     selection — orch and worker subscribe identically.
//
// projectID is the injected, pinned 40-hex routing key (cfg.ProjectID,
// validated non-empty at boot in Run). identity is injected, never derived
// here: callers that reach coordinateLoop already passed the boot guard, so
// there is no empty-projectID degradation path.
//
// Returns when ctx is cancelled. All subscriptions are unsubscribed
// before the function returns; Run relies on this via coordDone.
func coordinateLoop(ctx context.Context, nc *nats.Conn, cfg Config, projectID, instanceID string) error {
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
			// Ack the message when the publisher set a reply inbox.
			// The ack carries the receiving agent's identity so the
			// dispatcher can confirm WHICH worker accepted the message.
			// Load-bearing for tier-routing tests (each tier asserts the
			// expected agent responded); also useful in production
			// (an orch learns which worker picked up a queue-group task).
			if msg.Reply != "" {
				ack := fmt.Sprintf(`{"instance_id":%q,"role":%q,"class":%q,"verb":%q}`,
					instanceID, cfg.Role, string(cfg.Class), verb)
				_ = msg.Respond([]byte(ack))
			}
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
		// Single 5-token prompt subject, QueueSubscribed under the fixed
		// PromptQueueGroup. Every active agent in the session joins the same
		// group, so a prompt is delivered to exactly one member (work-
		// stealing). Role is no longer part of the subject or the group.
		front := fmt.Sprintf("agents.prompt.%s.%s.%s", machine, projectID, session)
		fsub, err := nc.QueueSubscribe(front, subject.PromptQueueGroup, handler("prompt"))
		if err != nil {
			return fmt.Errorf("queue subscribe %s: %w", front, err)
		}
		subs = append(subs, fsub)

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
