package cache

import (
	"strings"
	"testing"
	"time"
)

func TestContinue95CacheConstructorClamps(t *testing.T) {
	c := New(0, 0, 0)
	if c.ttl != 5*time.Minute || c.maxEntries != 256 || c.maxBytes != 64<<20 {
		t.Errorf("New(0,0,0) = ttl %v entries %d bytes %d, want 5m/256/64MiB", c.ttl, c.maxEntries, c.maxBytes)
	}
	c = New(time.Minute, 1<<20, 1<<31)
	if c.maxEntries != maxEntriesCap || c.maxBytes != 1<<30 {
		t.Errorf("New(huge) = entries %d bytes %d, want cap %d / 1GiB", c.maxEntries, c.maxBytes, maxEntriesCap)
	}
}

func TestContinue95SemanticConstructorClamps(t *testing.T) {
	c := NewSemantic(0, 0, 0, 0)
	if c.ttl != 5*time.Minute || c.threshold != 0.95 || c.maxEntries != 1024 || c.maxBytes != 64<<20 {
		t.Errorf("NewSemantic(0,0,0,0) = ttl %v thr %v entries %d bytes %d, want 5m/0.95/1024/64MiB", c.ttl, c.threshold, c.maxEntries, c.maxBytes)
	}
	c = NewSemantic(time.Minute, 2.0, 1<<20, 1<<31)
	if c.threshold != 0.95 || c.maxEntries != maxSemanticEntriesCap || c.maxBytes != 1<<30 {
		t.Errorf("NewSemantic(huge) = thr %v entries %d bytes %d, want 0.95/cap/1GiB", c.threshold, c.maxEntries, c.maxBytes)
	}
}

func TestContinue95CacheOverwriteAndByteClamp(t *testing.T) {
	c := New(time.Minute, 16, 1<<20)
	now := time.Now()
	c.Store("k", Entry{Body: []byte("aaa"), Status: 200, CreatedAt: now})
	c.Store("k", Entry{Body: []byte("bbbb"), Status: 200, CreatedAt: now})
	if e, ok := c.Lookup("k"); !ok || string(e.Body) != "bbbb" {
		t.Errorf("overwrite Lookup = %q %v, want bbbb true", e.Body, ok)
	}
	if got := c.Stats().Entries; got != 1 {
		t.Errorf("overwrite Entries = %d, want 1", got)
	}
	// Force removeNode's negative-byte clamp: shrink the counter below
	// the stored body's size, then overwrite the key.
	c.Store("big", Entry{Body: []byte(strings.Repeat("x", 100)), Status: 200, CreatedAt: now})
	c.bytes = 10
	c.Store("big", Entry{Body: []byte("zz"), Status: 200, CreatedAt: now})
	if c.bytes != 2 {
		t.Errorf("bytes after negative clamp = %d, want 2", c.bytes)
	}
}

func TestContinue95SemanticLookupEdges(t *testing.T) {
	c := NewSemantic(time.Minute, 0.95, 16, 1<<20)
	if _, _, ok := c.Lookup("s", nil, false); ok {
		t.Error("Lookup with an empty vector should miss")
	}
	if _, _, ok := c.Lookup("s", make([]float32, maxVectorDim+1), false); ok {
		t.Error("Lookup with an over-dimension vector should miss")
	}
	if got := CosineSimilarity([]float32{0, 0}, []float32{1, 2}); got != 0 {
		t.Errorf("CosineSimilarity zero-norm = %v, want 0", got)
	}
	// Rejected stores: empty body, empty vector, empty scope.
	c.Store(SemanticEntry{Body: nil, Vector: []float32{1, 0}, Scope: "s"})
	c.Store(SemanticEntry{Body: []byte("x"), Vector: nil, Scope: "s"})
	c.Store(SemanticEntry{Body: []byte("x"), Vector: []float32{1, 0}, Scope: ""})
	// Prove the oversized rejection on a tiny-budget cache.
	small := NewSemantic(time.Minute, 0.95, 16, 10)
	small.Store(SemanticEntry{Body: []byte(strings.Repeat("y", 200)), Vector: []float32{1, 0}, Scope: "s"})
	if got := small.Stats().Stores; got != 0 {
		t.Errorf("oversized semantic store Stores = %d, want 0", got)
	}
	if got := c.Stats().Stores; got != 0 {
		t.Errorf("rejected semantic stores Stores = %d, want 0", got)
	}
}

func TestContinue95SemanticEvictionAndUnlink(t *testing.T) {
	now := time.Now()
	// Byte eviction of a single-node list exercises unlink's head branch.
	c := NewSemantic(time.Minute, 0.95, 16, 150)
	c.Store(SemanticEntry{Body: []byte(strings.Repeat("a", 100)), Vector: []float32{1, 0}, Scope: "s", CreatedAt: now})
	c.Store(SemanticEntry{Body: []byte(strings.Repeat("b", 100)), Vector: []float32{0, 1}, Scope: "s", CreatedAt: now})
	if got := c.Stats().Entries; got != 1 {
		t.Fatalf("byte-evicted Entries = %d, want 1", got)
	}
	if e, _, ok := c.Lookup("s", []float32{0, 1}, false); !ok || string(e.Body) != strings.Repeat("b", 100) {
		t.Errorf("surviving entry wrong: ok=%v body-len=%d", ok, len(e.Body))
	}
	// Count eviction at maxEntries.
	c2 := NewSemantic(time.Minute, 0.95, 2, 1<<20)
	c2.Store(SemanticEntry{Body: []byte("one"), Vector: []float32{1, 0}, Scope: "s", CreatedAt: now})
	c2.Store(SemanticEntry{Body: []byte("two"), Vector: []float32{0, 1}, Scope: "s", CreatedAt: now})
	c2.Store(SemanticEntry{Body: []byte("three"), Vector: []float32{0.7, 0.7}, Scope: "s", CreatedAt: now})
	if got := c2.Stats().Entries; got != 2 {
		t.Errorf("count-evicted Entries = %d, want 2", got)
	}
	// Middle-node unlink: three entries, look up the middle one so
	// moveToFront unlinks it (prev and next both set).
	c3 := NewSemantic(time.Minute, 0.95, 16, 1<<20)
	c3.Store(SemanticEntry{Body: []byte("A"), Vector: []float32{1, 0}, Scope: "s", CreatedAt: now})
	c3.Store(SemanticEntry{Body: []byte("B"), Vector: []float32{0, 1}, Scope: "s", CreatedAt: now})
	c3.Store(SemanticEntry{Body: []byte("C"), Vector: []float32{0.7, 0.7}, Scope: "s", CreatedAt: now})
	if e, score, ok := c3.Lookup("s", []float32{0, 1}, false); !ok || string(e.Body) != "B" || score < 0.99 {
		t.Fatalf("middle lookup = %q score %v ok %v, want B ~1.0 true", e.Body, score, ok)
	}
	if c3.head == nil || string(c3.head.entry.Body) != "B" {
		t.Error("middle lookup should move B to the head")
	}
	// Negative-byte clamp on semantic removeNode, same trick as exact cache:
	// shrink the counter below the stored bodies, then store a body
	// large enough that the byte loop evicts every node, including the
	// 100-byte one (7-100 would go negative without the clamp).
	c3.Store(SemanticEntry{Body: []byte(strings.Repeat("z", 100)), Vector: []float32{1, 0}, Scope: "s", CreatedAt: now})
	c3.bytes = 10
	c3.maxBytes = 50
	c3.Store(SemanticEntry{Body: []byte(strings.Repeat("q", 45)), Vector: []float32{1, 0}, Scope: "s", CreatedAt: now})
	if c3.bytes != 45 || c3.Stats().Entries != 1 {
		t.Errorf("semantic bytes/entries after clamped eviction = %d/%d, want 45/1", c3.bytes, c3.Stats().Entries)
	}
}
