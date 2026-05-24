package card

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Cache memoizes signed AgentCard bytes by AgentKey. TTL is per-entry;
// LRU eviction at cap. Single-flight is deliberately NOT done in Slice 1 —
// a stale read blocks on Compose + Sign and serializes with concurrent
// readers via the cache mutex. Slice 5 may revisit.
type Cache struct {
	composer *Composer
	signer   *Signer
	ttl      time.Duration
	cap      int

	mu      sync.Mutex
	entries map[AgentKey]*list.Element
	lru     *list.List
}

type cacheEntry struct {
	key       AgentKey
	bytes     []byte
	expiresAt time.Time
}

// NewCache wraps a Composer + Signer with an LRU TTL cache. cap of 64
// fits Slice 1 (single agent per shim); operator can raise later.
func NewCache(composer *Composer, signer *Signer, ttl time.Duration, cap int) *Cache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if cap <= 0 {
		cap = 64
	}
	return &Cache{
		composer: composer,
		signer:   signer,
		ttl:      ttl,
		cap:      cap,
		entries:  make(map[AgentKey]*list.Element),
		lru:      list.New(),
	}
}

// GetOrCompose returns signed card bytes for key, composing + signing on
// miss or expiry.
func (c *Cache) GetOrCompose(ctx context.Context, key AgentKey) ([]byte, error) {
	if c == nil {
		return nil, errors.New("nil cache")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[key]; ok {
		ent := el.Value.(*cacheEntry)
		if time.Now().Before(ent.expiresAt) {
			c.lru.MoveToFront(el)
			return ent.bytes, nil
		}
		c.lru.Remove(el)
		delete(c.entries, key)
	}

	card, err := c.composer.Compose(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	signed, err := c.signer.SignCard(card)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	c.insertLocked(key, signed)
	return signed, nil
}

// HasFresh reports whether key has a non-expired entry in the cache. Used
// by observability surfaces (e.g. /metrics) to distinguish hits from
// misses without forcing a compose+sign.
func (c *Cache) HasFresh(key AgentKey) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return false
	}
	return time.Now().Before(el.Value.(*cacheEntry).expiresAt)
}

// Invalidate drops the cached entry for key. Used in Slice 5+ when the
// source adapter goes away.
func (c *Cache) Invalidate(key AgentKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.lru.Remove(el)
		delete(c.entries, key)
	}
}

func (c *Cache) insertLocked(key AgentKey, bytes []byte) {
	for c.lru.Len() >= c.cap {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
	ent := &cacheEntry{key: key, bytes: bytes, expiresAt: time.Now().Add(c.ttl)}
	el := c.lru.PushFront(ent)
	c.entries[key] = el
}
