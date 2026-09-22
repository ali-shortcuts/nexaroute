package health

import (
	"sync"
	"time"
)

type Status string

const (
	Unknown  Status = "unknown"
	Healthy  Status = "healthy"
	Degraded Status = "degraded"
	HalfOpen Status = "half_open"
	Cooldown Status = "cooldown"
)

type State struct {
	Deployment          string    `json:"deployment"`
	Status              Status    `json:"status"`
	Successes           int64     `json:"successes"`
	Failures            int64     `json:"failures"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	EWMALatencyMS       float64   `json:"ewma_latency_ms"`
	LastChecked         time.Time `json:"last_checked"`
	LastSuccess         time.Time `json:"last_success"`
	LastFailure         time.Time `json:"last_failure"`
	LastError           string    `json:"last_error,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
}

type Manager struct {
	mu        sync.RWMutex
	threshold int
	cooldown  time.Duration
	states    map[string]State
}

func New(threshold int, cooldown time.Duration) *Manager {
	return &Manager{threshold: threshold, cooldown: cooldown, states: map[string]State{}}
}

func (m *Manager) Get(id string) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[id]
	if !ok {
		s = State{Deployment: id, Status: Unknown}
		m.states[id] = s
	}
	if s.Status == Cooldown && !s.CooldownUntil.IsZero() && time.Now().After(s.CooldownUntil) {
		s.Status = HalfOpen
		s.ConsecutiveFailures = 0
		s.LastError = ""
		s.CooldownUntil = time.Time{}
		m.states[id] = s
	}
	return s
}

func (m *Manager) RecordSuccess(id string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Successes++
	s.ConsecutiveFailures = 0
	s.Status = Healthy
	s.LastChecked = time.Now()
	s.LastSuccess = s.LastChecked
	s.LastError = ""
	s.CooldownUntil = time.Time{}
	ms := float64(latency.Milliseconds())
	if s.EWMALatencyMS == 0 {
		s.EWMALatencyMS = ms
	} else {
		s.EWMALatencyMS = s.EWMALatencyMS*0.75 + ms*0.25
	}
	m.states[id] = s
}

func (m *Manager) RecordFailure(id string, errMsg string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Failures++
	s.ConsecutiveFailures++
	s.LastChecked = time.Now()
	s.LastFailure = s.LastChecked
	s.LastError = errMsg
	ms := float64(latency.Milliseconds())
	if ms > 0 {
		if s.EWMALatencyMS == 0 {
			s.EWMALatencyMS = ms
		} else {
			s.EWMALatencyMS = s.EWMALatencyMS*0.75 + ms*0.25
		}
	}
	if s.Status == HalfOpen || s.ConsecutiveFailures >= m.threshold {
		s.Status = Cooldown
		s.CooldownUntil = time.Now().Add(m.cooldown)
	} else {
		s.Status = Degraded
	}
	m.states[id] = s
}

func (m *Manager) Snapshot() []State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.states))
	now := time.Now()
	for id, s := range m.states {
		if s.Status == Cooldown && now.After(s.CooldownUntil) {
			s.Status = HalfOpen
			s.ConsecutiveFailures = 0
			s.CooldownUntil = time.Time{}
			m.states[id] = s
		}
		out = append(out, s)
	}
	return out
}

// ForceCooldown immediately removes a deployment from normal routing until the
// supplied duration expires. It is used for explicit upstream signals such as
// authentication, quota, and rate-limit failures where retrying the same
// deployment immediately would only waste latency and quota.
func (m *Manager) ForceCooldown(id, reason string, d time.Duration) {
	if d <= 0 {
		d = m.cooldown
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Failures++
	s.ConsecutiveFailures++
	s.Status = Cooldown
	s.LastChecked = time.Now()
	s.LastFailure = s.LastChecked
	s.LastError = reason
	s.CooldownUntil = time.Now().Add(d)
	m.states[id] = s
}

// Configure updates circuit-breaker thresholds for future health transitions.
// Existing counters are preserved across hot reloads.
func (m *Manager) Configure(threshold int, cooldown time.Duration) {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	m.mu.Lock()
	m.threshold = threshold
	m.cooldown = cooldown
	m.mu.Unlock()
}
