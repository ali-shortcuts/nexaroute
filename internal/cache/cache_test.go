package cache

import (
	"testing"
	"time"
)

func TestCacheLookupMissAndHit(t *testing.T) {
	c := New(time.Minute, 8, 1<<20)
	if _, ok := c.Lookup("k"); ok {
		t.Fatal("empty cache must miss")
	}
	if c.Stats().Misses != 1 {
		t.Fatalf("expected 1 miss, got %d", c.Stats().Misses)
	}
	c.Store("k", Entry{Body: []byte("hello"), ContentType: "application/json", Status: 200, CreatedAt: time.Now()})
	e, ok := c.Lookup("k")
	if !ok || string(e.Body) != "hello" || e.Status != 200 {
		t.Fatalf("expected hit with body, got ok=%v entry=%+v", ok, e)
	}
	if c.Stats().Hits != 1 {
		t.Fatalf("expected 1 hit, got %d", c.Stats().Hits)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	c := New(5*time.Millisecond, 8, 1<<20)
	c.Store("k", Entry{Body: []byte("x"), CreatedAt: time.Now()})
	time.Sleep(10 * time.Millisecond)
	if _, ok := c.Lookup("k"); ok {
		t.Fatal("expired entry must miss")
	}
	if c.Stats().Entries != 0 {
		t.Fatalf("expired entry must be evicted, entries=%d", c.Stats().Entries)
	}
}

func TestCacheEntryBoundEvictsLRU(t *testing.T) {
	c := New(time.Minute, 2, 1<<20)
	c.Store("a", Entry{Body: []byte("a"), CreatedAt: time.Now()})
	c.Store("b", Entry{Body: []byte("b"), CreatedAt: time.Now()})
	// Touch "a" so "b" becomes the LRU victim.
	if _, ok := c.Lookup("a"); !ok {
		t.Fatal("expected a to be live")
	}
	c.Store("c", Entry{Body: []byte("c"), CreatedAt: time.Now()})
	if _, ok := c.Lookup("b"); ok {
		t.Fatal("LRU entry b should have been evicted")
	}
	if _, ok := c.Lookup("a"); !ok {
		t.Fatal("recently used a must survive")
	}
	if _, ok := c.Lookup("c"); !ok {
		t.Fatal("freshly stored c must survive")
	}
}

func TestCacheByteBound(t *testing.T) {
	c := New(time.Minute, 100, 3) // total budget of 3 bytes
	c.Store("big1", Entry{Body: []byte("aaaa"), CreatedAt: time.Now()})
	if c.Stats().BytesStored > 3 {
		t.Fatalf("byte bound violated: %d", c.Stats().BytesStored)
	}
	c.Store("k", Entry{Body: []byte("xy"), CreatedAt: time.Now()})
	if c.Stats().BytesStored != 2 {
		t.Fatalf("expected only the new 2-byte entry, got %d", c.Stats().BytesStored)
	}
	if _, ok := c.Lookup("k"); !ok {
		t.Fatal("new entry must be resident")
	}
}

func TestCacheInvalidateAndReset(t *testing.T) {
	c := New(time.Minute, 8, 1<<20)
	c.Store("k", Entry{Body: []byte("v"), CreatedAt: time.Now()})
	c.Invalidate()
	if _, ok := c.Lookup("k"); ok {
		t.Fatal("invalidate must clear entries")
	}
	if c.Stats().Entries != 0 || c.Stats().BytesStored != 0 {
		t.Fatalf("invalidate must clear accounting: %+v", c.Stats())
	}
	c.Store("k", Entry{Body: []byte("v"), CreatedAt: time.Now()})
	c.Reset()
	if c.Stats().Hits != 0 || c.Stats().Misses != 0 || c.Stats().Entries != 0 {
		t.Fatalf("reset must clear stats: %+v", c.Stats())
	}
}

func TestCacheEmptyStoreIgnored(t *testing.T) {
	c := New(time.Minute, 8, 1<<20)
	c.Store("k", Entry{Body: nil, CreatedAt: time.Now()})
	if c.Stats().Stores != 0 || c.Stats().Entries != 0 {
		t.Fatalf("empty body must not be stored: %+v", c.Stats())
	}
}

func TestCacheKeyDistinctPerPathAndBody(t *testing.T) {
	k1 := Key("/v1/messages", []byte(`{"a":1}`))
	k2 := Key("/v1/chat/completions", []byte(`{"a":1}`))
	k3 := Key("/v1/messages", []byte(`{"a":2}`))
	if k1 == k2 || k1 == k3 || k2 == k3 {
		t.Fatal("keys must be distinct per path/body")
	}
	if len(k1) != 64 {
		t.Fatalf("expected sha256 hex key length 64, got %d", len(k1))
	}
}

func TestCacheGenerationRejectsInFlightOldResponse(t *testing.T) {
	c := New(time.Minute, 8, 1<<20)
	key := KeyScoped("/v1/messages", []byte(`{"model":"m"}`), []byte("client-a"))
	generation := c.Generation()
	c.Invalidate()
	c.StoreForGeneration(key, generation, Entry{Body: []byte("stale"), CreatedAt: time.Now()})
	if _, ok := c.Lookup(key); ok {
		t.Fatal("old request repopulated cache after invalidation")
	}
	fresh := c.Generation()
	c.StoreForGeneration(key, fresh, Entry{Body: []byte("fresh"), CreatedAt: time.Now()})
	if entry, ok := c.LookupForGeneration(key, fresh); !ok || string(entry.Body) != "fresh" {
		t.Fatalf("fresh response was not stored: ok=%v entry=%+v", ok, entry)
	}
	if _, ok := c.LookupForGeneration(key, generation); ok {
		t.Fatal("old-generation request hit new-generation cache")
	}
}

func TestCacheReconfigureAppliesNewLimits(t *testing.T) {
	c := New(time.Minute, 4, 100)
	old := c.Generation()
	c.Store("old", Entry{Body: []byte("old"), CreatedAt: time.Now()})
	c.Reconfigure(time.Millisecond, 1, 4)
	if c.Generation() == old || c.Stats().Entries != 0 {
		t.Fatal("reconfiguration did not invalidate old entries")
	}
	c.Store("too-large", Entry{Body: []byte("12345"), CreatedAt: time.Now()})
	if c.Stats().Entries != 0 {
		t.Fatal("reconfigured byte limit was not enforced")
	}
	c.Store("one", Entry{Body: []byte("a"), CreatedAt: time.Now()})
	c.Store("two", Entry{Body: []byte("b"), CreatedAt: time.Now()})
	if _, ok := c.Lookup("one"); ok {
		t.Fatal("reconfigured entry limit was not enforced")
	}
	time.Sleep(3 * time.Millisecond)
	if _, ok := c.Lookup("two"); ok {
		t.Fatal("reconfigured TTL was not enforced")
	}
}

func TestCacheKeyScopedByClientInputs(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	if KeyScoped("/v1/messages", body, []byte("client-a")) == KeyScoped("/v1/messages", body, []byte("client-b")) {
		t.Fatal("request scope must distinguish otherwise identical bodies")
	}
}
