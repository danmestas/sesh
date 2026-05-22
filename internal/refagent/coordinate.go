package refagent

import (
	"context"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/sesh/internal/agentmeta"
	"github.com/danmestas/sesh/internal/coord"
)

// coordinateLoop subscribes to the sesh.* coordination subjects
// appropriate for the agent's role/class and logs every received
// message. This is the reference implementation that future adapters
// (claude-nats-channel, pi-nats-channel, etc.) should model.
//
// Subscription policy:
//
//   - class=observer:       ObserverFilters(<machine>, <projectID>)
//   - class=active, role=*: ProjectTaskFilter(<machine>, <projectID>, "workers", role)
//                             + ProjectBlackboardFilter(<machine>, <projectID>)
//
// projectID may be empty (the agent is not running inside a sesh
// project). In that case the loop registers no subscriptions and
// returns immediately — the agent is still useful for direct
// agents.prompt.* traffic, just absent from coordination broadcasts.
//
// Queue groups are sourced from coord.Subject.QueueGroup() so the
// per-verb work-stealing vs fan-out policy is honored without the
// caller having to remember it.
//
// Returns when ctx is cancelled. All subscriptions are unsubscribed
// before the function returns; the caller may rely on this for
// deterministic shutdown.
func coordinateLoop(ctx context.Context, nc *nats.Conn, cfg Config, projectID string) error {
	if projectID == "" {
		slog.Info("coordinate: no project-id pinned; skipping coordination subscriptions",
			"agent", cfg.Agent, "role", cfg.Role, "class", cfg.Class)
		<-ctx.Done()
		return nil
	}

	machine := coord.Machine()
	subs, err := subscribeForClass(nc, cfg, machine, projectID)
	if err != nil {
		return err
	}
	defer func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	}()

	slog.Info("coordinate: subscribed to coordination subjects",
		"agent", cfg.Agent, "role", cfg.Role, "class", cfg.Class,
		"machine", machine, "project_id", projectID, "subjects", len(subs))

	<-ctx.Done()
	return nil
}

// subscribeForClass installs the per-class subscriptions and returns
// the underlying nats.Subscription handles so coordinateLoop can
// unsubscribe at shutdown. Each handler logs the received message at
// info level; future adapters replace the handler with their own
// dispatch logic.
//
// The handler parameter allows tests to inject custom message handlers
// in place of the default slog-only handler. Pass nil to use the
// default logging handler. This makes the function testable without
// log scraping (audit patch #6/#7).
func subscribeForClass(nc *nats.Conn, cfg Config, machine, projectID string) ([]*nats.Subscription, error) {
	return subscribeForClassWithHandler(nc, cfg, machine, projectID, nil)
}

// subscribeForClassWithHandler is the injectable form of subscribeForClass.
// handler is a factory that takes a Verb and returns a nats.MsgHandler;
// when nil, the default slog handler is used.
func subscribeForClassWithHandler(nc *nats.Conn, cfg Config, machine, projectID string, handler func(coord.Verb) nats.MsgHandler) ([]*nats.Subscription, error) {
	if handler == nil {
		handler = func(verb coord.Verb) nats.MsgHandler {
			return func(msg *nats.Msg) {
				slog.Info("coordinate: received message",
					"verb", string(verb),
					"subject", msg.Subject,
					"reply", msg.Reply,
					"size", len(msg.Data))
			}
		}
	}

	var subs []*nats.Subscription

	switch cfg.Class {
	case agentmeta.ClassObserver:
		// Observers subscribe only to report.* and blackboard.* —
		// never task.* or control.* (spy-exclusion contract).
		for _, f := range coord.ObserverFilters(machine, projectID) {
			verb := coord.Verb(f.Verb)
			sub, err := nc.Subscribe(f.String(), handler(verb))
			if err != nil {
				return subs, err
			}
			subs = append(subs, sub)
		}

	case agentmeta.ClassActive:
		// Active workers subscribe to:
		//   1. task.* targeting their role (work-stealing via queue group)
		//   2. blackboard.* under the project (fan-out)
		taskFilter := coord.ProjectTaskFilter(machine, projectID, "workers", cfg.Role)
		taskSubject := coord.ProjectTaskSubject(machine, projectID, "workers", cfg.Role)
		sub, err := nc.QueueSubscribe(taskFilter.String(), taskSubject.QueueGroup(), handler(coord.VerbTask))
		if err != nil {
			return subs, err
		}
		subs = append(subs, sub)

		bbFilter := coord.ProjectBlackboardFilter(machine, projectID)
		sub2, err := nc.Subscribe(bbFilter.String(), handler(coord.VerbBlackboard))
		if err != nil {
			return subs, err
		}
		subs = append(subs, sub2)

	default:
		// Unknown class: don't subscribe (role-reg Phase 1's
		// Config validation should have rejected this at boot, but
		// belt-and-braces guard).
		slog.Warn("coordinate: unknown class; skipping subscriptions", "class", cfg.Class)
	}

	return subs, nil
}
