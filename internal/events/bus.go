package events

import (
	"sync"
	"time"
)

type Event struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id,omitempty"`
	Kind       string    `json:"kind"`
	Deployment string    `json:"deployment,omitempty"`
	Message    string    `json:"message"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	ErrorType  string    `json:"error_type,omitempty"`
}

const (
	maxCounterKeys       = 256
	maxEventRequestID    = 128
	maxEventKind         = 128
	maxEventDeployment   = 512
	maxEventMessage      = 4096
	maxEventErrorType    = 128
	counterOverflowKey   = "__other__"
)

func boundedString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
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
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Event, b.count)
	for i := 0; i < b.count; i++ {
		out[i] = b.items[(b.start+i)%b.max]
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
