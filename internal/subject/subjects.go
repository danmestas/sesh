// Package subject builds NATS subject strings for the v0.4 A2A
// surfaces. It MUST stay byte-identical with the TypeScript SDK at
// sesh-channels/sdk/src/subjects.ts. Drift breaks cross-stack wire
// compatibility — verify with the canonical examples pinned in
// subjects_test.go and reconcile both sides on every protocol change.
//
// Convention: lowercase tokens, dot-separated, no trailing dot, no
// wildcards. Callers MUST pre-sanitize input tokens — builders do not
// escape; they return *InvalidTokenError when validation fails.
package subject

import (
	"fmt"
	"strings"
	"unicode"
)

// PromptV2QueueGroup is the NATS micro queue group for v2 prompt
// handlers across replicas of the same role. MUST match the value used
// on the publisher side. Mirrors PROMPT_V2_QUEUE_GROUP in subjects.ts.
const PromptV2QueueGroup = "agents-prompt-v2"

// Coord identifies a v2 prompt destination. Mirrors the TS `Coord`
// interface. Inst is optional; empty string means "omit the inst token".
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

func validateCoord(c Coord) error {
	if err := validateToken(c.Machine); err != nil {
		return err
	}
	if err := validateToken(c.Project); err != nil {
		return err
	}
	if err := validateToken(c.Session); err != nil {
		return err
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

// PromptV2 returns agents.prompt.v2.<machine>.<project>.<session>.<role>[.<inst>].
// Mirrors promptV2(c) in subjects.ts.
func PromptV2(c Coord) (string, error) {
	if err := validateCoord(c); err != nil {
		return "", err
	}
	base := "agents.prompt.v2." + c.Machine + "." + c.Project + "." + c.Session + "." + c.Role
	if c.Inst != "" {
		return base + "." + c.Inst, nil
	}
	return base, nil
}

// ParsePromptV2 reverses PromptV2. Each extracted token is re-validated
// so the parser is no more lenient than the builder. Returns an error
// if subject doesn't match the agents.prompt.v2.* shape or any token
// fails validation. Mirrors the symmetry expected of subjects.ts.
func ParsePromptV2(subject string) (Coord, error) {
	const prefix = "agents.prompt.v2."
	if !strings.HasPrefix(subject, prefix) {
		return Coord{}, fmt.Errorf("subject %q missing %q prefix", subject, prefix)
	}
	rest := subject[len(prefix):]
	tokens := strings.Split(rest, ".")
	if len(tokens) != 4 && len(tokens) != 5 {
		return Coord{}, fmt.Errorf("subject %q has %d tokens after prefix, want 4 or 5", subject, len(tokens))
	}
	for _, tok := range tokens {
		if err := validateToken(tok); err != nil {
			return Coord{}, err
		}
	}
	c := Coord{
		Machine: tokens[0],
		Project: tokens[1],
		Session: tokens[2],
		Role:    tokens[3],
	}
	if len(tokens) == 5 {
		c.Inst = tokens[4]
	}
	return c, nil
}

// TaskStream returns agents.task.stream.<scopeKind>.<scopeID>.<taskID>.
// Mirrors stream({scopeKind, scopeId, taskId}) in subjects.ts.
func TaskStream(scopeKind, scopeID, taskID string) (string, error) {
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

// CardGet returns agents.card.get.<agent>.<owner>.<name>.
// Mirrors card({agent, owner, name}) in subjects.ts.
func CardGet(agent, owner, name string) (string, error) {
	if err := validateToken(agent); err != nil {
		return "", err
	}
	if err := validateToken(owner); err != nil {
		return "", err
	}
	if err := validateToken(name); err != nil {
		return "", err
	}
	return "agents.card.get." + agent + "." + owner + "." + name, nil
}

// CardExtended returns agents.card.extended.<agent>.<owner>.<name>.
// Mirrors cardExtended({agent, owner, name}) in subjects.ts.
func CardExtended(agent, owner, name string) (string, error) {
	if err := validateToken(agent); err != nil {
		return "", err
	}
	if err := validateToken(owner); err != nil {
		return "", err
	}
	if err := validateToken(name); err != nil {
		return "", err
	}
	return "agents.card.extended." + agent + "." + owner + "." + name, nil
}
