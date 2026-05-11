// Package coord implements sesh-side coordination state on top of the hub's
// JetStream. Today: session leases (claim / renew / release / list / get) via
// a single KV bucket named "sessions".
//
// Design captured during simple-brainstorm (2026-05-11):
//
//   - Coord lives in the consumer (sesh), not in EdgeSync. EdgeSync's hub is
//     a NATS+fossil substrate; "session" is a sesh concept.
//   - Atomic claim via jetstream.KeyValue.Create — fails fast if a session is
//     already held. Owner identity is verified on every Renew / Release.
//   - Per-key TTL on the bucket; a leaf renews on a ticker (~TTL/3). Crash =
//     lease auto-expires; another claimer takes over.
//   - The bucket is created idempotently on first connect (CreateOrUpdate); the
//     hub stays session-agnostic — no hub-side handlers needed.
//   - YAGNI: no generic Registry[T] yet. When agents (or any second concrete
//     consumer) show up, copy-modify; refactor only when a third reveals real
//     shared patterns.
package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// SessionsBucket is the JetStream KV bucket holding session leases.
	SessionsBucket = "sessions"

	// DefaultSessionTTL is the per-key TTL applied when the bucket is
	// created. A lease that isn't renewed within this window is reclaimable.
	DefaultSessionTTL = 30 * time.Second
)

// Coord is a handle to the hub-side coordination KV. Construct with Connect;
// remember to Close when done.
type Coord struct {
	nc       *nats.Conn
	js       jetstream.JetStream
	sessions jetstream.KeyValue
}

// Connect dials the hub's client NATS URL, attaches to JetStream, and ensures
// the sessions KV bucket exists with DefaultSessionTTL.
func Connect(ctx context.Context, natsURL string) (*Coord, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("sesh-coord"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("coord: connect %s: %w", natsURL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("coord: jetstream: %w", err)
	}

	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: SessionsBucket,
		TTL:    DefaultSessionTTL,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("coord: kv bucket %q: %w", SessionsBucket, err)
	}

	return &Coord{nc: nc, js: js, sessions: kv}, nil
}

// Close closes the underlying NATS connection.
func (c *Coord) Close() error {
	c.nc.Close()
	return nil
}

// ErrNotOwner is returned by Renew/Release when the caller's owner identity
// doesn't match the stored lease.
var ErrNotOwner = errors.New("coord: caller is not the lease owner")
