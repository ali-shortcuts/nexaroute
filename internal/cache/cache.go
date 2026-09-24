// Package cache implements the gateway's opt-in exact-match response cache.
//
// It is intentionally conservative, Cloudflare-AI-Gateway-inspired, and
// bounded: only complete non-streaming JSON responses are stored, entries
// expire by TTL, the table is LRU-bounded, and every hit is observable via
// headers, metrics, and events. Streaming responses are never cached because
// partial upstream failures could not be detected before bytes were committed.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Entry is one cached upstream response.
type Entry struct {
	Body        []byte
	ContentType string
	Status      int
	CreatedAt   time.Time
	// Deployment records which deployment produced the cached response for
	// observability in the dashboard.
	Deployment string
}

// Stats exposes cumulative counters for metrics and the dashboard.
type Stats struct {
	Hits        int64 `json:"hits"`
	Misses      int64 `json:"misses"`
	Bypasses    int64 `json:"bypasses"`
	Stores      int64 `json:"stores"`
	Entries     int   `json:"entries"`
	BytesStored int64 `json:"bytes_stored"`
}

// Cache is a bounded, TTL-aware, thread-safe response store.
type Cache struct {
	mu         sync.Mutex
	ttl        time.Duration
	entries    map[string]*lruNode
	order      *lruList
	maxEntries int
	maxBytes   int64
	bytes      int64
	generation uint64
	stats      Stats
}

// New returns a cache with the given TTL, entry bound, and total byte bound.
// Bounds are clamped to safe minima so a misconfigured cache can never grow
// without limit.
func New(ttl time.Duration, maxEntries int, maxTotalBytes int64) *Cache {
	ttl, maxEntries, maxTotalBytes = normalizeBounds(ttl, maxEntries, maxTotalBytes)
	return &Cache{
		ttl:        ttl,
		entries:    make(map[string]*lruNode, 16),
		order:      &lruList{},
		maxEntries: maxEntries,
		maxBytes:   maxTotalBytes,
	}
}

func normalizeBounds(ttl time.Duration, maxEntries int, maxTotalBytes int64) (time.Duration, int, int64) {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = 256
	}
	if maxEntries > maxEntriesCap {
		maxEntries = maxEntriesCap
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = 64 << 20
	}
	if maxTotalBytes > 1<<30 {
		maxTotalBytes = 1 << 30
	}
	return ttl, maxEntries, maxTotalBytes
}

// Key derives the cache key for an ingress path and request body.
func Key(path string, body []byte) string { return KeyScoped(path, body, nil) }

// KeyScoped includes request inputs outside the JSON body (the authenticated
// client, session headers and headers allowed to reach upstream providers).
// The scope is only ever stored as part of this digest, never in plaintext.
func KeyScoped(path string, body, scope []byte) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	h.Write([]byte{0})
	h.Write(scope)
	return hex.EncodeToString(h.Sum(nil))
}

// Generation identifies the current config's cache entries. In-flight old
// responses must not repopulate the cache after a config swap.
func (c *Cache) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// Lookup returns a live entry for the key and marks it recently used.
func (c *Cache) Lookup(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookupLocked(key)
}

// LookupForGeneration never serves an old-config request from a newer cache.
func (c *Cache) LookupForGeneration(key string, generation uint64) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		c.stats.Misses++
		return Entry{}, false
	}
	return c.lookupLocked(key)
}

func (c *Cache) lookupLocked(key string) (Entry, bool) {
	node, ok := c.entries[key]
	if !ok {
		c.stats.Misses++
		return Entry{}, false
	}
	if time.Since(node.entry.CreatedAt) > c.ttl {
		c.removeNode(node)
		c.stats.Misses++
		return Entry{}, false
	}
	c.order.moveToFront(node)
	c.stats.Hits++
	return node.entry, true
}

// Store inserts an entry, evicting the least-recently-used entries (and any
// expired entries encountered) until the byte and count bounds hold again.
// Entries larger than the total byte budget are rejected outright: they could
// never coexist with anything else and would silently break the bound.
func (c *Cache) Store(key string, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, e)
}

// StoreForGeneration rejects a response that started before the last config
// swap, even if it finished after Invalidate/Reconfigure acquired the lock.
func (c *Cache) StoreForGeneration(key string, generation uint64, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation == generation {
		c.storeLocked(key, e)
	}
}

func (c *Cache) storeLocked(key string, e Entry) {
	if len(e.Body) == 0 || int64(len(e.Body)) > c.maxBytes {
		return
	}
	if old, ok := c.entries[key]; ok {
		c.removeNode(old)
	}
	for c.bytes+int64(len(e.Body)) > c.maxBytes && c.order.len > 0 {
		c.removeNode(c.order.tail)
	}
	if c.order.len >= c.maxEntries {
		c.removeNode(c.order.tail)
	}
	node := &lruNode{key: key, entry: e}
	c.entries[key] = node
	c.order.pushFront(node)
	c.bytes += int64(len(e.Body))
	c.stats.Stores++
}

// Bypass records a request that was eligible for caching policy but skipped
// (e.g. disabled cache, oversized body, non-deterministic parameters).
func (c *Cache) Bypass() {
	c.mu.Lock()
	c.stats.Bypasses++
	c.mu.Unlock()
}

// removeNode unlinks a node from the LRU structures and reclaims byte
// accounting.
func (c *Cache) removeNode(n *lruNode) {
	if _, ok := c.entries[n.key]; ok {
		delete(c.entries, n.key)
		c.bytes -= int64(len(n.entry.Body))
		if c.bytes < 0 {
			c.bytes = 0
		}
	}
	c.order.unlink(n)
}

// Invalidate clears every entry. Called on config swap so cached responses
// can never outlive the routing topology that produced them.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateLocked()
}

func (c *Cache) invalidateLocked() {
	c.generation++
	c.entries = make(map[string]*lruNode, 16)
	c.order = &lruList{}
	c.bytes = 0
}

// Reconfigure atomically applies new cache limits and invalidates old entries.
func (c *Cache) Reconfigure(ttl time.Duration, maxEntries int, maxTotalBytes int64) {
	ttl, maxEntries, maxTotalBytes = normalizeBounds(ttl, maxEntries, maxTotalBytes)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl, c.maxEntries, c.maxBytes = ttl, maxEntries, maxTotalBytes
	c.invalidateLocked()
}

// Reset clears entries and counters (admin action).
func (c *Cache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateLocked()
	c.stats = Stats{}
}

// Stats returns a snapshot of counters and current occupancy.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stats
	out.Entries = c.order.len
	out.BytesStored = c.bytes
	return out
}

const maxEntriesCap = 65536

// lruNode/lruList form a minimal intrusive doubly-linked list; a full
// container/list dependency is unnecessary for two operations.
type lruNode struct {
	key   string
	entry Entry
	prev  *lruNode
	next  *lruNode
}

type lruList struct {
	head *lruNode
	tail *lruNode
	len  int
}

func (l *lruList) pushFront(n *lruNode) {
	n.prev = nil
	n.next = l.head
	if l.head != nil {
		l.head.prev = n
	}
	l.head = n
	if l.tail == nil {
		l.tail = n
	}
	l.len++
}

func (l *lruList) unlink(n *lruNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	n.prev, n.next = nil, nil
	l.len--
}

func (l *lruList) moveToFront(n *lruNode) {
	if l.head == n {
		return
	}
	l.unlink(n)
	l.pushFront(n)
}
