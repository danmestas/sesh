// Package subject builds NATS subject strings for the v0.4 A2A
// surfaces. It MUST stay byte-identical with the TypeScript SDK at
// sesh-channels/sdk/src/subjects.ts. Drift breaks cross-stack wire
// compatibility — verify with the canonical examples pinned in
// subjects_test.go and reconcile both sides on every protocol change.
//
// Canonical subject shape (clean v0.4 scheme, 2026-05-26 cutover):
//
//	agents.<verb>.<machine>.<project>.<session>[.<role>[.<inst>]]
//
// The verb is ALWAYS a single segment. Compound verbs like `card.get`
// break the 5/6/7-token addressing tier because the tier counts dots —
// extra verb segments shift the tier boundary. So `card.get` → `card`,
// `card.extended` → `cardx`. The auth boundary stays at a distinct
// subject (`cardx`) so NATS subject-based authorization can enforce it
// at the account level rather than trusting the adapter to gate.
//
// Token-count tiers (applies to Prompt only — every other verb is
// always 5-token, session-scoped):
//
//	5 tokens: session orch (reachable; broadcast across roles)
//	6 tokens: role pool   (queue group on role across replicas)
//	7 tokens: direct instance (single worker, no fanout)
//
// Convention: lowercase tokens, dot-separated, no trailing dot, no
// wildcards. Callers MUST pre-sanitize input tokens — builders do not
// escape; they return *InvalidTokenError when validation fails.
package subject

import (
	"fmt"
	"unicode"
)

// PromptQueueGroup is the NATS micro queue group for prompt
// handlers across replicas of the same role. MUST match the value used
// on the publisher side. Mirrors PROMPT_QUEUE_GROUP in subjects.ts.
//
// The string literal "agents-prompt-v2" is retained for queue-group
// continuity across the v0.3→v0.4 cutover: queue groups are NATS
// identifiers tracked independently of subject paths, so the value
// can stay stable while the SUBJECTS drop the .v2. infix. Only the
// Go symbol name was renamed (PromptV2QueueGroup → PromptQueueGroup).
const PromptQueueGroup = "agents-prompt-v2"

// Coord identifies a destination on the clean subject scheme. Mirrors
// the TS `Coord` interface. Role and Inst are optional; empty strings
// mean "omit the token" and select a coarser addressing tier.
//
// Heartbeat / Status / Card / Cardx ignore Role and Inst — those verbs
// are always 5-token session-scoped. Only Prompt consults Role and
// Inst to select the 5/6/7-token tier.
type Coord struct {
	Machine string
	Project string
	Session string
	Role    string
	Inst    string
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
// core of a Coord. Used by every verb that is always session-scoped
// (Heartbeat, Status, Card, Cardx). Role and Inst are NOT validated
// here — those verbs ignore them.
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

// validatePromptCoord validates the full Coord for Prompt, including
// optional Role + Inst. Role is required when Inst is set (can't have
// a 7-token subject that skips the role slot).
func validatePromptCoord(c Coord) error {
	if err := validateSessionCoord(c); err != nil {
		return err
	}
	if c.Role == "" {
		if c.Inst != "" {
			return &InvalidTokenError{
				Token:  c.Inst,
				Reason: "inst requires role (cannot skip the role token in a 7-token subject)",
			}
		}
		return nil
	}
	if err := validateToken(c.Role); err != nil {
		return err
	}
	if c.Inst != "" {
		if err := validateToken(c.Inst); err != nil {
			return err
		}
	}
	return nil
}

// Prompt returns the canonical prompt subject for c:
//
//	5-token (session orch):    agents.prompt.<machine>.<project>.<session>
//	6-token (role pool):       agents.prompt.<machine>.<project>.<session>.<role>
//	7-token (direct instance): agents.prompt.<machine>.<project>.<session>.<role>.<inst>
//
// Tier selection follows Role and Inst presence: empty Role → 5-token,
// Role present + empty Inst → 6-token, both present → 7-token. Mirrors
// prompt(c) in subjects.ts.
func Prompt(c Coord) (string, error) {
	if err := validatePromptCoord(c); err != nil {
		return "", err
	}
	base := "agents.prompt." + c.Machine + "." + c.Project + "." + c.Session
	if c.Role == "" {
		return base, nil
	}
	base += "." + c.Role
	if c.Inst == "" {
		return base, nil
	}
	return base + "." + c.Inst, nil
}

// Heartbeat returns agents.hb.<machine>.<project>.<session>. Always
// 5-token, session-scoped — Role and Inst on c are ignored. Mirrors
// heartbeat(c) in subjects.ts.
func Heartbeat(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.hb." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Status returns agents.status.<machine>.<project>.<session>. Always
// 5-token, session-scoped — Role and Inst on c are ignored. Mirrors
// status(c) in subjects.ts.
func Status(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.status." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Card returns agents.card.<machine>.<project>.<session>. Always
// 5-token, session-scoped — Role and Inst on c are ignored. The
// public L3 AgentCard endpoint; pairs with Cardx for the auth-gated
// extended variant. Mirrors card(c) in subjects.ts.
func Card(c Coord) (string, error) {
	if err := validateSessionCoord(c); err != nil {
		return "", err
	}
	return "agents.card." + c.Machine + "." + c.Project + "." + c.Session, nil
}

// Cardx returns agents.cardx.<machine>.<project>.<session>. Always
// 5-token, session-scoped — Role and Inst on c are ignored. The
// extended (auth-gated) L3 AgentCard endpoint; sits on a distinct
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
