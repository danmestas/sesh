package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Child is a single lease record. Name is opaque to coord — callers compose
// it from whatever vocabulary their tier uses (e.g. "<project>-session-<id>").
type Child struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	ClaimedAt time.Time `json:"claimed_at"`
	RenewedAt time.Time `json:"renewed_at"`
}

// Claim atomically takes ownership of name. Returns a friendly error naming
// the current owner if the lease is held.
func (c *Coord) Claim(ctx context.Context, name, owner string) (Child, error) {
	now := time.Now().UTC()
	ch := Child{Name: name, Owner: owner, ClaimedAt: now, RenewedAt: now}
	payload, err := json.Marshal(ch)
	if err != nil {
		return ch, fmt.Errorf("claim: marshal: %w", err)
	}

	if _, err := c.children.Create(ctx, name, payload); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			if entry, getErr := c.children.Get(ctx, name); getErr == nil {
				var cur Child
				if json.Unmarshal(entry.Value(), &cur) == nil {
					return ch, fmt.Errorf("child %q already held by %q since %s",
						name, cur.Owner, cur.ClaimedAt.Format(time.RFC3339))
				}
			}
			return ch, fmt.Errorf("child %q already held", name)
		}
		return ch, fmt.Errorf("claim: %w", err)
	}
	return ch, nil
}

// Renew refreshes the lease. Fails with ErrNotOwner if a different owner now
// holds the key.
func (c *Coord) Renew(ctx context.Context, name, owner string) error {
	entry, err := c.children.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	var cur Child
	if err := json.Unmarshal(entry.Value(), &cur); err != nil {
		return fmt.Errorf("renew: decode: %w", err)
	}
	if cur.Owner != owner {
		return fmt.Errorf("%w: held by %q", ErrNotOwner, cur.Owner)
	}
	cur.RenewedAt = time.Now().UTC()
	payload, err := json.Marshal(cur)
	if err != nil {
		return fmt.Errorf("renew: marshal: %w", err)
	}
	if _, err := c.children.Update(ctx, name, payload, entry.Revision()); err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	return nil
}

// Release relinquishes a held lease. No-op if expired or already deleted.
// Fails with ErrNotOwner if a different owner now holds it.
func (c *Coord) Release(ctx context.Context, name, owner string) error {
	entry, err := c.children.Get(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("release: %w", err)
	}
	var cur Child
	if err := json.Unmarshal(entry.Value(), &cur); err != nil {
		return fmt.Errorf("release: decode: %w", err)
	}
	if cur.Owner != owner {
		return fmt.Errorf("%w: held by %q", ErrNotOwner, cur.Owner)
	}
	return c.children.Delete(ctx, name)
}
