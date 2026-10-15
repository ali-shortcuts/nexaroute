package cache

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// SemanticEntry is one semantically-indexed cached response. The
// prompt itself is never stored — only its embedding, a hash of the
// prompt text, and PROVENANCE describing where the response came
// from, so a later hit can be gated on the entry's privacy class
// and audited after the fact.
type SemanticEntry struct {
	Body        []byte
	ContentType string
	Status      int
	CreatedAt   time.Time

	Vector []float32
	Scope  string // isolation scope; similarity never crosses scopes

	// Provenance.
	Deployment   string // deployment that produced the response
	TrainsOnData string // origin provider's declared class: yes | no | unknown
	NoTraining   bool   // the storing request required no_training
	Embedding    string // embedding deployment that produced Vector
	PromptSHA256 string // hash of the embedded prompt text (never the text)
	SourceModel  string // upstream model that produced the response
}

// SemanticStats are the semantic cache's cumulative counters.
type SemanticStats struct {
	Hits        int64 `json:"hits"`
	Misses      int64 `json:"misses"`
	Stores      int64 `json:"stores"`
	Entries     int   `json:"entries"`
	BytesStored int64 `json:"bytes_stored"`
}

const (
	maxSemanticEntriesCap = 16384
	maxVectorDim          = 8192
)

// SemanticCache is a bounded, TTL-aware, thread-safe store of
// embedding-indexed responses. It is deliberately a sibling of the
// exact-match Cache, not a mode of it: different keying (scope +
// vector), different failure modes (similarity), different privacy
// obligations (provenance gating).
type SemanticCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	threshold  float64
	maxEntries int
	maxBytes   int64
	entries    map[string]*semNode
	head       *semNode
	tail       *semNode
	length     int
	bytes      int64
	stats      SemanticStats
	seq        uint64
}

type semNode struct {
	key   string
	entry SemanticEntry
	prev  *semNode
	next  *semNode
}

// NewSemantic builds a semantic cache. Bounds clamp like the exact
// cache's: a misconfiguration can never produce an unbounded table.
func NewSemantic(ttl time.Duration, threshold float64, maxEntries int, maxTotalBytes int64) *SemanticCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if threshold <= 0 || threshold > 1 {
		threshold = 0.95
	}
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	if maxEntries > maxSemanticEntriesCap {
		maxEntries = maxSemanticEntriesCap
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = 64 << 20
	}
	if maxTotalBytes > 1<<30 {
		maxTotalBytes = 1 << 30
	}
	return &SemanticCache{
		ttl: ttl, threshold: threshold, maxEntries: maxEntries,
		maxBytes: maxTotalBytes, entries: make(map[string]*semNode, 16),
	}
}

// CosineSimilarity returns the cosine similarity of two vectors, or
// 0 when either is empty, dimensions differ, or a norm is zero.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Lookup finds the most similar live entry in the SAME scope at or
// above the threshold. When requireNoTraining is set, only entries
// stored under no_training whose origin provider declares
// trains_on_data="no" may serve — provenance is a hard gate, not a
// ranking hint.
func (c *SemanticCache) Lookup(scope string, vector []float32, requireNoTraining bool) (SemanticEntry, float64, bool) {
	if len(vector) == 0 || len(vector) > maxVectorDim {
		return SemanticEntry{}, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *semNode
	bestScore := 0.0
	now := time.Now()
	for node := c.head; node != nil; node = node.next {
		e := node.entry
		if e.Scope != scope {
			continue
		}
		if now.Sub(e.CreatedAt) > c.ttl {
			continue
		}
		if requireNoTraining && !(e.NoTraining && e.TrainsOnData == "no") {
			continue
		}
		score := CosineSimilarity(vector, e.Vector)
		if score >= c.threshold && score > bestScore {
			best, bestScore = node, score
		}
	}
	if best == nil {
		c.stats.Misses++
		return SemanticEntry{}, 0, false
	}
	c.moveToFront(best)
	c.stats.Hits++
	return best.entry, bestScore, true
}

// Store inserts an entry, evicting least-recently-used entries
// until the count and byte bounds hold. Oversized bodies and
// out-of-range vectors are rejected outright.
func (c *SemanticCache) Store(e SemanticEntry) {
	if len(e.Body) == 0 || len(e.Vector) == 0 || len(e.Vector) > maxVectorDim || e.Scope == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if int64(len(e.Body)) > c.maxBytes {
		return
	}
	for c.bytes+int64(len(e.Body)) > c.maxBytes && c.tail != nil {
		c.removeNode(c.tail)
	}
	if c.length >= c.maxEntries && c.tail != nil {
		c.removeNode(c.tail)
	}
	c.seq++
	key := fmt.Sprintf("%s\x00%d", e.Scope, c.seq)
	node := &semNode{key: key, entry: e}
	c.entries[key] = node
	c.pushFront(node)
	c.bytes += int64(len(e.Body))
	c.stats.Stores++
}

// Invalidate clears every entry (config swap, like the exact cache).
func (c *SemanticCache) Invalidate() {
	c.mu.Lock()
	c.entries = make(map[string]*semNode, 16)
	c.head, c.tail, c.length = nil, nil, 0
	c.bytes = 0
	c.mu.Unlock()
}

// Stats returns counters and current occupancy.
func (c *SemanticCache) Stats() SemanticStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stats
	out.Entries = c.length
	out.BytesStored = c.bytes
	return out
}

func (c *SemanticCache) pushFront(n *semNode) {
	n.prev = nil
	n.next = c.head
	if c.head != nil {
		c.head.prev = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
	c.length++
}

func (c *SemanticCache) unlink(n *semNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		c.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		c.tail = n.prev
	}
	n.prev, n.next = nil, nil
	c.length--
}

func (c *SemanticCache) moveToFront(n *semNode) {
	if c.head == n {
		return
	}
	c.unlink(n)
	c.pushFront(n)
}

func (c *SemanticCache) removeNode(n *semNode) {
	if _, ok := c.entries[n.key]; ok {
		delete(c.entries, n.key)
		c.bytes -= int64(len(n.entry.Body))
		if c.bytes < 0 {
			c.bytes = 0
		}
	}
	c.unlink(n)
}
