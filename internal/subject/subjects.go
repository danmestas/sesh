// Package subject builds NATS subject strings for the v0.4 A2A
// surfaces. It MUST stay byte-identical with the TypeScript SDK at
// sesh-channels/sdk/src/subjects.ts. Drift breaks cross-stack wire
// compatibility — verify with the canonical examples pinned in
// subjects_test.go and reconcile both sides on every protocol change.
//
// Canonical subject shape (single 5-token tier per verb):
//
//	agents.<verb>.<machine>.<project>.<session>
//
// Every verb — Prompt, Heartbeat, Status, Card, Cardx — is a single
// 5-token, session-scoped subject. There is no role/instance addressing
// tier: a prompt addresses the session, and work-stealing among same-
// session subscribers is handled by a NATS queue group (PromptQueueGroup)
// on the SUBSCRIBE side. The queue group is invisible in the subject
// bytes; it lives only on QueueSubscribe.
//
// The verb is ALWAYS a single segment. Compound verbs like `card.get`
// would split into two segments and push the subject to 6 tokens, so
// `card.get` → `card`, `card.extended` → `cardx`. The auth boundary
// stays at a distinct subject (`cardx`) so NATS subject-based
// authorization can enforce it at the account level rather than
// trusting the adapter to gate.
//
// Convention: lowercase tokens, dot-separated, no trailing dot, no
// wildcards. Callers MUST pre-sanitize input tokens — builders do not
// escape; they return *InvalidTokenError when validation fails.
package subject

import (
	"fmt"
	"unicode"
)

// PromptQueueGroup is the NATS queue group every prompt subscriber
// joins. It is applied UNCONDITIONALLY on the subscribe side (see
// internal/refagent/coordinate.go): all active agents in a session
// QueueSubscribe the single 5-token prompt subject under this group,
// so a prompt is delivered to exactly one member (work-stealing). A
// single-member group degenerates to a plain subscribe; a multi-member
// one preserves the work-stealing semantics. MUST match the value used
// by the TS adapters (PROMPT_QUEUE_GROUP in subjects.ts).
//
// The string literal "agents-prompt-v2" is retained for queue-group
// continuity across the v0.3→v0.4 cutover: queue groups are NATS
// identifiers tracked independently of subject paths, so the value
// can stay stable while the SUBJECTS drop the .v2. infix.
const PromptQueueGroup = "agents-prompt-v2"

// Coord identifies a session-scoped destination on the subject scheme.
// Mirrors the TS `Coord` interface. Every verb (Prompt, Heartbeat,
// Status, Card, Cardx) builds the same 5-token subject from these three
// tokens — there is no role/instance addressing tier.
type Coord struct {
	Machine string
	Project string
	Session string
}

// InvalidTokenError reports a subject token that violates the
// lowercase-dot-separated convention. Mirrors InvalidSubjectTokenError
// in subjects.ts.
type InvalidTokenError struct {
	Token  string
	Reason string
}

func (e *InvalidTokenError) Error() string {
	return fmt.Sprintf("invalid subject token %q: %s", e.Token, e.Reason)
}

// validateToken rejects empty tokens or tokens containing '.', '*',
// '>', or any rune satisfying unicode.IsSpace. Reason text matches the
// TS side exactly so cross-stack grep finds both messages.
func validateToken(token string) error {
	if token == "" {
		return &InvalidTokenError{Token: token, Reason: "empty"}
	}
	for _, r := range token {
		if r == '.' || r == '*' || r == '>' || unicode.IsSpace(r) {
			return &InvalidTokenError{
				Token:  token,
				Reason: "contains reserved character (. whitespace * >)",
			}
		}
	}
	return nil
}

// validateSessionCoord validates the 5-token (machine, project, session)
// core of a Coord. Used by every verb — all are session-scoped.
func validateSessionCoord(c Coord) error {
	if err := validateToken(c.Machine); err != nil {
		return err
	}
	if err := validateToken(c.Project); err != nil {
		return err
	}
	if err := validateToken(c.Session); err != nil {
		return err
	}
	return nil
}

// Prompt returns agents.prompt.<machine>.<project>.<session>. A single
// 5-token, session-scoped subject — work-stealing among same-session
// subscribers rides PromptQueueGroup on the subscribe side, not the
// subject. Mirrors prompt(c) in subjects.ts.
func Prompt(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.prompt." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Heartbeat returns agents.hb.<machine>.<project>.<session>. Always
// 5-token, session-scoped. Mirrors heartbeat(c) in subjects.ts.
func Heartbeat(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.hb." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Status returns agents.status.<machine>.<project>.<session>. Always
// 5-token, session-scoped. Mirrors status(c) in subjects.ts.
func Status(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.status." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Card returns agents.card.<machine>.<project>.<session>. Always
// 5-token, session-scoped. The public L3 AgentCard endpoint; pairs
// with Cardx for the auth-gated extended variant. Mirrors card(c) in
// subjects.ts.
func Card(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.card." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Cardx returns agents.cardx.<machine>.<project>.<session>. Always
// 5-token, session-scoped. The extended (auth-gated) L3 AgentCard
// endpoint; sits on a distinct
// single-segment verb (NOT `card.extended`) so NATS subject-based
// authorization can gate at the account level. Mirrors cardx(c) in
// subjects.ts.
func Cardx(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.cardx." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Stream returns agents.task.stream.<scopeKind>.<scopeID>.<taskID>.
// Mirrors stream({scopeKind, scopeId, taskId}) in subjects.ts. The
// scope-keyed task stream subject is unchanged by the clean-subject
// cutover — operator subjects keyed on (kind, id, task) live on a
// distinct shape from session-scoped agent verbs.
func Stream(scopeKind, scopeID, taskID string) (string, error) {
	if err := validateToken(scopeKind); err != nil {
		return "", err
	}
	if err := validateToken(scopeID); err != nil {
		return "", err
	}
	if err := validateToken(taskID); err != nil {
		return "", err
	}
	return "agents.task.stream." + scopeKind + "." + scopeID + "." + taskID, nil
}
