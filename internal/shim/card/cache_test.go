package card

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// newTestCache wires a real signer with a real composer (NATS-backed)
// using the embedded test NATS server. Returns the cache and the connection
// so callers can register stub agents before invoking GetOrCompose.
func newTestCache(t *testing.T, ttl time.Duration, cap int) (*Cache, *nats.Conn) {
	t.Helper()
	url := startTestNATS(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	signer, err := NewDevSigner()
	if err != nil {
		t.Fatal(err)
	}
	composer := NewComposer(nc, defaultL1(), 250*time.Millisecond, nil)
	return NewCache(composer, signer, ttl, cap), nc
}

func TestCache_HitMarksFreshAndReturnsEqualBytes(t *testing.T) {
	cache, nc := newTestCache(t, time.Minute, 8)
	registerStubAgent(t, nc, "echo", "alice", "r", "c")

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	if cache.HasFresh(key) {
		t.Fatal("HasFresh should be false before first GetOrCompose")
	}
	a, err := cache.GetOrCompose(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !cache.HasFresh(key) {
		t.Fatal("HasFresh should be true after compose")
	}
	b, err := cache.GetOrCompose(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("cached bytes differ across hits")
	}
}

func TestCache_Expiry(t *testing.T) {
	cache, nc := newTestCache(t, 50*time.Millisecond, 8)
	registerStubAgent(t, nc, "echo", "alice", "r", "c")

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	if _, err := cache.GetOrCompose(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if !cache.HasFresh(key) {
		t.Fatal("HasFresh should be true immediately after compose")
	}
	time.Sleep(75 * time.Millisecond)
	if cache.HasFresh(key) {
		t.Fatal("HasFresh should be false after TTL")
	}
}

func TestCache_Invalidate(t *testing.T) {
	cache, nc := newTestCache(t, time.Minute, 8)
	registerStubAgent(t, nc, "echo", "alice", "r", "c")

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	if _, err := cache.GetOrCompose(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if !cache.HasFresh(key) {
		t.Fatal("should be fresh after compose")
	}
	cache.Invalidate(key)
	if cache.HasFresh(key) {
		t.Fatal("Invalidate did not drop the entry")
	}
}

func TestCache_LRUEviction(t *testing.T) {
	cache, nc := newTestCache(t, time.Minute, 2)
	registerStubAgent(t, nc, "a1", "alice", "r", "c")
	registerStubAgent(t, nc, "a2", "alice", "r", "c")
	registerStubAgent(t, nc, "a3", "alice", "r", "c")

	ctx := context.Background()
	if _, err := cache.GetOrCompose(ctx, AgentKey{Agent: "a1", Owner: "alice", Name: "a1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.GetOrCompose(ctx, AgentKey{Agent: "a2", Owner: "alice", Name: "a2"}); err != nil {
		t.Fatal(err)
	}
	// Insert third — a1 should be evicted (LRU at cap=2).
	if _, err := cache.GetOrCompose(ctx, AgentKey{Agent: "a3", Owner: "alice", Name: "a3"}); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, ok := cache.entries[AgentKey{Agent: "a1", Owner: "alice", Name: "a1"}]; ok {
		t.Errorf("a1 should have been evicted")
	}
	if cache.lru.Len() != 2 {
		t.Errorf("lru len = %d, want 2", cache.lru.Len())
	}
}

func TestCache_ComposerAccessor(t *testing.T) {
	cache, _ := newTestCache(t, time.Minute, 8)
	if cache.Composer() == nil {
		t.Fatal("Composer() returned nil for live cache")
	}
	var nilCache *Cache
	if nilCache.Composer() != nil {
		t.Fatal("nil-cache Composer() should be nil")
	}
}

func TestCache_Concurrent(t *testing.T) {
	cache, nc := newTestCache(t, time.Minute, 8)
	registerStubAgent(t, nc, "echo", "alice", "r", "c")

	key := AgentKey{Agent: "echo", Owner: "alice", Name: "echo"}
	var wg sync.WaitGroup
	var ok int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.GetOrCompose(context.Background(), key); err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 20 {
		t.Errorf("expected 20 successful reads, got %d", ok)
	}
}
