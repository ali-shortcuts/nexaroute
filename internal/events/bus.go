package events

import (
	"sync"
	"time"
	"unicode/utf8"
)

type Event struct {
	Time            time.Time `json:"time"`
	RequestID       string    `json:"request_id,omitempty"`
	Kind            string    `json:"kind"`
	Deployment      string    `json:"deployment,omitempty"`
	Message         string    `json:"message"`
	LatencyMS       int64     `json:"latency_ms,omitempty"`
	StatusCode      int       `json:"status_code,omitempty"`
	ErrorType       string    `json:"error_type,omitempty"`
	VirtualEndpoint string    `json:"virtual_endpoint,omitempty"`
	PublicModel     string    `json:"public_model,omitempty"`
	RouteProfile    string    `json:"route_profile,omitempty"`
	Pool            string    `json:"pool,omitempty"`
}

const (
	maxCounterKeys        = 256
	maxEventRequestID     = 128
	maxEventKind          = 128
	maxEventDeployment    = 512
	maxEventMessage       = 4096
	maxEventErrorType     = 128
	maxEventVirtual       = 256
	maxEventPublicModel   = 256
	maxEventRouteProfile  = 256
	maxEventPool          = 256
	counterOverflowKey    = "__other__"
)

func boundedString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	// Back up to a rune boundary so multibyte text is not split into an
	// invalid UTF-8 sequence (json encoding would substitute U+FFFD).
	for i := 0; i < 3 && len(cut) > 0; i++ {
		if utf8.ValidString(cut) {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

func incrementBoundedCounter(m map[string]uint64, key string) {
	if key == "" {
		return
	}
	if _, ok := m[key]; ok {
		m[key]++
		return
	}
	// Reserve one slot for the overflow bucket so even future dynamic event
	// kinds/error types cannot grow this map without bound.
	if len(m) >= maxCounterKeys-1 {
		m[counterOverflowKey]++
		return
	}
	m[key] = 1
}

type Bus struct {
	mu          sync.RWMutex
	max         int
	items       []Event
	start       int
	count       int
	counts      map[string]uint64
	errorCounts map[string]uint64
}

func New(max int) *Bus {
	if max < 10 {
		max = 10
	}
	return &Bus{max: max, items: make([]Event, max), counts: map[string]uint64{}, errorCounts: map[string]uint64{}}
}
func (b *Bus) Add(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.RequestID = boundedString(e.RequestID, maxEventRequestID)
	e.Kind = boundedString(e.Kind, maxEventKind)
	e.Deployment = boundedString(e.Deployment, maxEventDeployment)
	e.Message = boundedString(e.Message, maxEventMessage)
	e.ErrorType = boundedString(e.ErrorType, maxEventErrorType)
	e.VirtualEndpoint = boundedString(e.VirtualEndpoint, maxEventVirtual)
	e.PublicModel = boundedString(e.PublicModel, maxEventPublicModel)
	e.RouteProfile = boundedString(e.RouteProfile, maxEventRouteProfile)
	e.Pool = boundedString(e.Pool, maxEventPool)
	if b.count < b.max {
		idx := (b.start + b.count) % b.max
		b.items[idx] = e
		b.count++
	} else {
		b.items[b.start] = e
		b.start = (b.start + 1) % b.max
	}
	incrementBoundedCounter(b.counts, e.Kind)
	incrementBoundedCounter(b.errorCounts, e.ErrorType)
}
func (b *Bus) Snapshot() []Event {
	return b.SnapshotLimit(0)
}

func (b *Bus) SnapshotLimit(limit int) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := b.count
	if limit > 0 && count > limit {
		count = limit
	}
	out := make([]Event, count)
	offset := b.count - count
	for i := 0; i < count; i++ {
		out[i] = b.items[(b.start+offset+i)%b.max]
	}
	return out
}

func (b *Bus) Counts() map[string]uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]uint64, len(b.counts))
	for k, v := range b.counts {
		out[k] = v
	}
	return out
}

func (b *Bus) ErrorCounts() map[string]uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]uint64, len(b.errorCounts))
	for k, v := range b.errorCounts {
		out[k] = v
	}
	return out
}
