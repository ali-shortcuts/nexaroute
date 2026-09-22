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
}

type Bus struct {
	mu    sync.RWMutex
	max   int
	items []Event
}

func New(max int) *Bus {
	if max < 10 {
		max = 10
	}
	return &Bus{max: max}
}
func (b *Bus) Add(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.items = append(b.items, e)
	if len(b.items) > b.max {
		b.items = append([]Event(nil), b.items[len(b.items)-b.max:]...)
	}
}
func (b *Bus) Snapshot() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Event, len(b.items))
	copy(out, b.items)
	return out
}
