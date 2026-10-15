package cache

import (
	"testing"
	"time"
)

// M6-4: exact-cache key derivation and lookups run on eligible
// data-plane requests.

var benchCacheBody = []byte(`{"model":"m","messages":[{"role":"user","content":"a realistic prompt body that the gateway would hash for the exact-match cache key"}]}`)

func BenchmarkCacheKeyWithContext(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = KeyWithContext("/v1/chat/completions", benchCacheBody, []byte("ctx:generation=3"))
	}
}

func BenchmarkCachePutGet(b *testing.B) {
	c := New(5*time.Minute, 1024, 1<<20)
	key := KeyWithContext("/v1/chat/completions", benchCacheBody, []byte("ctx"))
	entry := Entry{Body: []byte(`{"ok":true}`), ContentType: "application/json", Status: 200, Deployment: "p/m", CreatedAt: time.Now()}
	c.Store(key, entry)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := c.Lookup(key); !ok {
			b.Fatal("cache miss")
		}
	}
}
