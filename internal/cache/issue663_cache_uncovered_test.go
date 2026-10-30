package cache

import (
	"testing"
	"time"
)

func TestIssue663CoreExactCacheBounds(t *testing.T) {
	// Zero/negative configuration clamps to safe defaults; an entry
	// stored under the defaults must be retrievable.
	c := New(0, 0, 0)
	c.Store("k", Entry{Body: []byte("v"), CreatedAt: time.Now()})
	if got, ok := c.Lookup("k"); !ok || string(got.Body) != "v" {
		t.Fatalf("default-config cache lookup = %+v, %v", got, ok)
	}
	// Oversized configuration clamps to the caps: a cache built with
	// absurd bounds still stores and serves.
	huge := New(time.Hour, 1<<30, 1<<40)
	huge.Store("k", Entry{Body: []byte("v"), CreatedAt: time.Now()})
	if _, ok := huge.Lookup("k"); !ok {
		t.Fatal("capped-config cache must still serve entries")
	}
	// Empty bodies are never stored, and oversized bodies are
	// rejected outright rather than evicting everything else.
	small := New(time.Hour, 10, 100)
	small.Store("empty", Entry{Body: nil, CreatedAt: time.Now()})
	small.Store("big", Entry{Body: make([]byte, 101), CreatedAt: time.Now()})
	if st := small.Stats(); st.Entries != 0 || st.Stores != 0 {
		t.Fatalf("rejected stores must not count: %+v", st)
	}
	// Replacing an existing key reclaims the old bytes: the byte
	// total must reflect only the new body.
	small.Store("k", Entry{Body: []byte("12345"), CreatedAt: time.Now()})
	small.Store("k", Entry{Body: []byte("12"), CreatedAt: time.Now()})
	if st := small.Stats(); st.Entries != 1 || st.BytesStored != 2 || st.Stores != 2 {
		t.Fatalf("replacement accounting wrong: %+v", st)
	}
	// Byte-bound eviction removes least-recently-used entries first.
	evict := New(time.Hour, 10, 10)
	evict.Store("a", Entry{Body: []byte("12345"), CreatedAt: time.Now()})
	evict.Store("b", Entry{Body: []byte("12345"), CreatedAt: time.Now()})
	evict.Store("c", Entry{Body: []byte("12345"), CreatedAt: time.Now()})
	if _, ok := evict.Lookup("a"); ok {
		t.Error("byte-bound eviction must drop the least-recently-used entry first")
	}
	if _, ok := evict.Lookup("c"); !ok {
		t.Error("newest entry must survive byte-bound eviction")
	}
	// Count-bound eviction: maxEntries 1 keeps only the newest key.
	one := New(time.Hour, 1, 1<<20)
	one.Store("a", Entry{Body: []byte("1"), CreatedAt: time.Now()})
	one.Store("b", Entry{Body: []byte("2"), CreatedAt: time.Now()})
	if _, ok := one.Lookup("a"); ok {
		t.Error("count-bound eviction must drop the older entry")
	}
	if got, ok := one.Lookup("b"); !ok || string(got.Body) != "2" {
		t.Fatalf("newest entry must survive count-bound eviction: %+v %v", got, ok)
	}
	// A lookup of an expired entry removes it and counts a miss.
	expired := New(time.Nanosecond, 10, 1<<20)
	expired.Store("x", Entry{Body: []byte("v"), CreatedAt: time.Now().Add(-time.Hour)})
	if _, ok := expired.Lookup("x"); ok {
		t.Error("expired entry must not be served")
	}
	if st := expired.Stats(); st.Entries != 0 || st.Misses != 1 {
		t.Fatalf("expired lookup accounting wrong: %+v", st)
	}
}

func TestIssue663CoreSemanticCacheBounds(t *testing.T) {
	// Zero/negative configuration clamps: default threshold 0.95, so
	// a merely similar (0.9) vector must not serve.
	def := NewSemantic(0, 0, 0, 0)
	def.Store(SemanticEntry{Body: []byte("v"), Vector: []float32{1, 0}, Scope: "s", CreatedAt: time.Now()})
	if _, _, ok := def.Lookup("s", []float32{0.9, 0.4359}, false); ok {
		t.Error("default 0.95 threshold must reject a 0.9-similar vector")
	}
	// Threshold above 1 also clamps to 0.95.
	over := NewSemantic(time.Hour, 2, 0, 0)
	over.Store(SemanticEntry{Body: []byte("v"), Vector: []float32{1, 0}, Scope: "s", CreatedAt: time.Now()})
	if _, _, ok := over.Lookup("s", []float32{1, 0}, false); !ok {
		t.Error("identical vector must serve under the clamped threshold")
	}
	// Rejected stores: empty body, empty vector, over-long vector,
	// empty scope, oversized body.
	s := NewSemantic(time.Hour, 0.5, 10, 100)
	s.Store(SemanticEntry{Body: nil, Vector: []float32{1}, Scope: "s", CreatedAt: time.Now()})
	s.Store(SemanticEntry{Body: []byte("v"), Vector: nil, Scope: "s", CreatedAt: time.Now()})
	s.Store(SemanticEntry{Body: []byte("v"), Vector: make([]float32, maxVectorDim+1), Scope: "s", CreatedAt: time.Now()})
	s.Store(SemanticEntry{Body: []byte("v"), Vector: []float32{1}, Scope: "", CreatedAt: time.Now()})
	s.Store(SemanticEntry{Body: make([]byte, 101), Vector: []float32{1}, Scope: "s", CreatedAt: time.Now()})
	if st := s.Stats(); st.Entries != 0 || st.Stores != 0 {
		t.Fatalf("rejected semantic stores must not count: %+v", st)
	}
	// Count eviction drops the least-recently-used semantic entry.
	one := NewSemantic(time.Hour, 0.5, 1, 1<<20)
	one.Store(SemanticEntry{Body: []byte("a"), Vector: []float32{1, 0}, Scope: "s", CreatedAt: time.Now()})
	one.Store(SemanticEntry{Body: []byte("b"), Vector: []float32{0, 1}, Scope: "s", CreatedAt: time.Now()})
	if got, _, ok := one.Lookup("s", []float32{0, 1}, false); !ok || string(got.Body) != "b" {
		t.Fatalf("semantic count eviction must keep the newest entry: %+v %v", got, ok)
	}
	if _, _, ok := one.Lookup("s", []float32{1, 0}, false); ok {
		t.Error("evicted semantic entry must not serve")
	}
	// Byte eviction drops the oldest semantic entry.
	bytes := NewSemantic(time.Hour, 0.5, 10, 10)
	bytes.Store(SemanticEntry{Body: []byte("12345"), Vector: []float32{1, 0}, Scope: "s", CreatedAt: time.Now()})
	bytes.Store(SemanticEntry{Body: []byte("12345"), Vector: []float32{0, 1}, Scope: "s", CreatedAt: time.Now()})
	bytes.Store(SemanticEntry{Body: []byte("12345"), Vector: []float32{1, 1}, Scope: "s", CreatedAt: time.Now()})
	if st := bytes.Stats(); st.Entries != 2 || st.BytesStored != 10 {
		t.Fatalf("semantic byte eviction accounting wrong: %+v", st)
	}
	// Lookup rejects empty and over-long query vectors.
	if _, _, ok := s.Lookup("s", nil, false); ok {
		t.Error("empty query vector must not match")
	}
	if _, _, ok := s.Lookup("s", make([]float32, maxVectorDim+1), false); ok {
		t.Error("over-long query vector must not match")
	}
}
