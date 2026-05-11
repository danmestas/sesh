package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Session is a single lease record stored in the sessions KV bucket.
type Session struct {
	Project   string    `json:"project"`
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	ClaimedAt time.Time `json:"claimed_at"`
	RenewedAt time.Time `json:"renewed_at"`
}

// sessionKey returns the KV key for a (project, sessionID) pair. Format
// "<project>.<session-id>" — dotted hierarchy mirrors NATS subject
// conventions and lets List filter by project prefix.
func sessionKey(project, sessionID string) string {
	return project + "." + sessionID
}

// Claim atomically takes ownership of (project, sessionID). Returns an error
// if the lease is already held — the message names the current owner.
func (c *Coord) Claim(ctx context.Context, project, sessionID, owner string) (Session, error) {
	now := time.Now().UTC()
	s := Session{
		Project:   project,
		ID:        sessionID,
		Owner:     owner,
		ClaimedAt: now,
		RenewedAt: now,
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return s, fmt.Errorf("claim: marshal: %w", err)
	}

	if _, err := c.sessions.Create(ctx, sessionKey(project, sessionID), payload); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			cur, getErr := c.Get(ctx, project, sessionID)
			if getErr == nil {
				return s, fmt.Errorf("session %s.%s already held by %q since %s",
					project, sessionID, cur.Owner, cur.ClaimedAt.Format(time.RFC3339))
			}
			return s, fmt.Errorf("session %s.%s already held", project, sessionID)
		}
		return s, fmt.Errorf("claim: %w", err)
	}
	return s, nil
}

// Renew refreshes the lease for (project, sessionID). Fails with ErrNotOwner
// if the stored owner doesn't match.
func (c *Coord) Renew(ctx context.Context, project, sessionID, owner string) error {
	entry, err := c.sessions.Get(ctx, sessionKey(project, sessionID))
	if err != nil {
		return fmt.Errorf("renew: get: %w", err)
	}
	var s Session
	if err := json.Unmarshal(entry.Value(), &s); err != nil {
		return fmt.Errorf("renew: decode: %w", err)
	}
	if s.Owner != owner {
		return fmt.Errorf("%w: held by %q", ErrNotOwner, s.Owner)
	}

	s.RenewedAt = time.Now().UTC()
	payload, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("renew: marshal: %w", err)
	}

	if _, err := c.sessions.Update(ctx, sessionKey(project, sessionID), payload, entry.Revision()); err != nil {
		return fmt.Errorf("renew: cas update: %w", err)
	}
	return nil
}

// Release relinquishes the lease. No-op if the key has already expired or
// been deleted. Fails with ErrNotOwner if a different owner holds it.
func (c *Coord) Release(ctx context.Context, project, sessionID, owner string) error {
	entry, err := c.sessions.Get(ctx, sessionKey(project, sessionID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("release: get: %w", err)
	}
	var s Session
	if err := json.Unmarshal(entry.Value(), &s); err != nil {
		return fmt.Errorf("release: decode: %w", err)
	}
	if s.Owner != owner {
		return fmt.Errorf("%w: held by %q", ErrNotOwner, s.Owner)
	}
	return c.sessions.Delete(ctx, sessionKey(project, sessionID))
}

// Get returns the current lease for (project, sessionID), or
// jetstream.ErrKeyNotFound if absent.
func (c *Coord) Get(ctx context.Context, project, sessionID string) (Session, error) {
	entry, err := c.sessions.Get(ctx, sessionKey(project, sessionID))
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(entry.Value(), &s); err != nil {
		return s, fmt.Errorf("get: decode: %w", err)
	}
	return s, nil
}

// List returns every current lease whose key starts with "<project>.".
func (c *Coord) List(ctx context.Context, project string) ([]Session, error) {
	keyLister, err := c.sessions.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer keyLister.Stop()

	prefix := project + "."
	var out []Session
	for key := range keyLister.Keys() {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := c.sessions.Get(ctx, key)
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(entry.Value(), &s); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
