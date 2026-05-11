// Package coord manages "children" — leases for the things one tier below
// the current node. The hub holds its sessions; a session leaf would hold its
// agents; an agent leaf would hold its tasks. Same code, same bucket name,
// different parent NATS URL at each tier.
//
// Each level only knows about what's directly underneath it. There's no
// cross-tier registry. State propagates upward only if explicitly mirrored.
//
// YAGNI: no Get/List today. Add when a caller actually needs them.
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
	// DefaultChildrenBucket is the JetStream KV bucket every tier uses to
	// track its direct children.
	DefaultChildrenBucket = "children"

	// DefaultChildTTL is the per-key TTL. A child must renew within this
	// window or its lease is reclaimable.
	DefaultChildTTL = time.Hour
)

// Coord is a handle to a parent node's children KV bucket.
type Coord struct {
	nc       *nats.Conn
	children jetstream.KeyValue
}

// Connect dials the parent node's client NATS URL and ensures the children
// bucket exists. The "parent" is whichever node is one tier above the caller:
// for a session leaf, the parent is the hub; for an agent leaf, the parent
// is the session leaf.
func Connect(ctx context.Context, parentNATS string) (*Coord, error) {
	nc, err := nats.Connect(parentNATS,
		nats.Name("sesh-coord"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("coord: connect %s: %w", parentNATS, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("coord: jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: DefaultChildrenBucket,
		TTL:    DefaultChildTTL,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("coord: kv bucket %q: %w", DefaultChildrenBucket, err)
	}
	return &Coord{nc: nc, children: kv}, nil
}

// Close drops the NATS connection.
func (c *Coord) Close() error {
	c.nc.Close()
	return nil
}

// ErrNotOwner is returned by Renew/Release when the caller's owner identity
// doesn't match the stored lease.
var ErrNotOwner = errors.New("coord: caller is not the owner")
