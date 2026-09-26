package providerstate

import (
	"sync"
	"time"
)

// Config holds health circuit parameters for DecisionProviders.
// This is separate from model/provider health.Manager.
type Config struct {
	FailureThreshold int           // consecutive failures or failures in window to open cooldown
	FailureWindow    time.Duration // window for counting failures
	Cooldown         time.Duration // cooldown duration
}

// DefaultConfig returns conservative defaults matching spec.
func DefaultConfig() Config {
	return Config{
		FailureThreshold: 3,
		FailureWindow:    30 * time.Second,
		Cooldown:         60 * time.Second,
	}
}

// State holds bounded runtime facts per DecisionProvider ID.
type State struct {
	Status              string    // healthy, cooldown, degraded (future)
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Successes           int64     `json:"successes"`
	FailuresInWindow    int       `json:"failures_in_window"`
	LastFailure         time.Time `json:"last_failure,omitempty"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
	// internal: ring of failure times for window counting
	failTimes []time.Time
}

// Clock allows fake clock for tests.
type Clock func() time.Time

// Manager tracks per-provider decision health. Concurrency safe.
type Manager struct {
	mu     sync.RWMutex
	cfg    Config
	states map[string]*State
	clock  Clock
}

// New creates a manager with given config and clock.
func New(cfg Config, clock Clock) *Manager {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.FailureWindow <= 0 {
		cfg.FailureWindow = 30 * time.Second
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 60 * time.Second
	}
	if clock == nil {
		clock = time.Now
	}
	return &Manager{
		cfg:    cfg,
		states: make(map[string]*State),
		clock:  clock,
	}
}

// getState returns state pointer, creating if missing (caller holds lock)
func (m *Manager) getStateLocked(id string) *State {
	st, ok := m.states[id]
	if !ok {
		st = &State{Status: "healthy"}
		m.states[id] = st
	}
	return st
}

// IsCooldown reports whether provider is currently in cooldown.
func (m *Manager) IsCooldown(id string) bool {
	m.mu.RLock()
	st, ok := m.states[id]
	if !ok {
		m.mu.RUnlock()
		return false
	}
	until := st.CooldownUntil
	m.mu.RUnlock()
	if until.IsZero() {
		return false
	}
	now := m.clock()
	if now.Before(until) {
		return true
	}
	// expired: transition to healthy lazily on next check
	// But don't mutate under RLock; let caller handle expiry via maybeRecover
	m.mu.Lock()
	// re-check under write lock
	st2, ok2 := m.states[id]
	if ok2 && !st2.CooldownUntil.IsZero() && !m.clock().Before(st2.CooldownUntil) {
		st2.Status = "healthy"
		st2.CooldownUntil = time.Time{}
	}
	m.mu.Unlock()
	return false
}

// CooldownUntil returns cooldown expiry (zero if not in cooldown)
func (m *Manager) CooldownUntil(id string) time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if st, ok := m.states[id]; ok {
		return st.CooldownUntil
	}
	return time.Time{}
}

// Snapshot returns shallow copy of all states for admin/metrics.
func (m *Manager) Snapshot() map[string]State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]State, len(m.states))
	for id, st := range m.states {
		copied := *st
		// don't expose internal failTimes
		copied.failTimes = nil
		out[id] = copied
	}
	return out
}

// SnapshotOne returns copy of one provider state.
func (m *Manager) SnapshotOne(id string) (State, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if st, ok := m.states[id]; ok {
		copied := *st
		copied.failTimes = nil
		return copied, true
	}
	return State{}, false
}

// RecordSuccess records a healthy SELECT or ABSTAIN. Resets consecutive failures and clears cooldown.
func (m *Manager) RecordSuccess(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.getStateLocked(id)
	st.Successes++
	st.LastSuccess = m.clock()
	st.ConsecutiveFailures = 0
	st.failTimes = nil
	st.FailuresInWindow = 0
	// Recover from cooldown immediately on successful call (or ABSTAIN healthy)
	if st.Status == "cooldown" || !st.CooldownUntil.IsZero() {
		st.Status = "healthy"
		st.CooldownUntil = time.Time{}
	}
	if st.Status == "" {
		st.Status = "healthy"
	}
}

// RecordFailure records a failure (network error, timeout, malformed, unknown candidate, primary constraint violation, panic, invalid confidence/action).
// It increments counters and may open cooldown if threshold crossed in window.
func (m *Manager) RecordFailure(id string) {
	now := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.getStateLocked(id)
	st.ConsecutiveFailures++
	st.LastFailure = now
	// Maintain sliding window
	st.failTimes = append(st.failTimes, now)
	// Prune outside window
	cutoff := now.Add(-m.cfg.FailureWindow)
	// Keep only within window
	kept := st.failTimes[:0]
	for _, t := range st.failTimes {
		if t.After(cutoff) || t.Equal(cutoff) {
			kept = append(kept, t)
		}
	}
	st.failTimes = kept
	st.FailuresInWindow = len(kept)
	if st.Status == "" {
		st.Status = "healthy"
	}
	// Check threshold
	if st.FailuresInWindow >= m.cfg.FailureThreshold && st.Status != "cooldown" {
		st.Status = "cooldown"
		st.CooldownUntil = now.Add(m.cfg.Cooldown)
	}
}

// UpdateConfig hot-reloads health config. Existing state preserved but thresholds updated.
func (m *Manager) UpdateConfig(cfg Config) {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.FailureWindow <= 0 {
		cfg.FailureWindow = 30 * time.Second
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 60 * time.Second
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// Reset clears state for a provider (when identity materially changes: endpoint/key/type)
func (m *Manager) Reset(id string) {
	m.mu.Lock()
	delete(m.states, id)
	m.mu.Unlock()
}

// Config returns current config snapshot
func (m *Manager) Config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}
