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

type ScopeState struct {
	Status              Status    `json:"status"`
	Successes           int64     `json:"successes"`
	Failures            int64     `json:"failures"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastChecked         time.Time `json:"last_checked"`
	LastSuccess         time.Time `json:"last_success"`
	LastFailure         time.Time `json:"last_failure"`
	LastError           string    `json:"last_error,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
}

type State struct {
	Deployment          string                `json:"deployment"`
	Status              Status                `json:"status"`
	Successes           int64                 `json:"successes"`
	Failures            int64                 `json:"failures"`
	ConsecutiveFailures int                   `json:"consecutive_failures"`
	EWMALatencyMS       float64               `json:"ewma_latency_ms"`
	EWMAFailureRate      float64               `json:"ewma_failure_rate"`
	LastChecked         time.Time             `json:"last_checked"`
	LastSuccess         time.Time             `json:"last_success"`
	LastFailure         time.Time             `json:"last_failure"`
	LastError           string                `json:"last_error,omitempty"`
	CooldownUntil       time.Time             `json:"cooldown_until,omitempty"`
	RecoveryFailures    int                   `json:"recovery_failures,omitempty"`
	Scopes              map[string]ScopeState `json:"scopes,omitempty"`
}

type Manager struct {
	mu             sync.RWMutex
	threshold      int
	cooldown       time.Duration
	scopeThreshold int
	scopeCooldown  time.Duration
	states         map[string]State
}

func New(threshold int, cooldown time.Duration) *Manager {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	return &Manager{
		threshold:      threshold,
		cooldown:       cooldown,
		scopeThreshold: 2,
		scopeCooldown:  5 * time.Minute,
		states:         map[string]State{},
	}
}

func cloneState(s State) State {
	if s.Scopes != nil {
		m := make(map[string]ScopeState, len(s.Scopes))
		for k, v := range s.Scopes {
			m[k] = v
		}
		s.Scopes = m
	}
	return s
}

func normalizeGlobal(s State, now time.Time) State {
	if s.Status == Cooldown && !s.CooldownUntil.IsZero() && now.After(s.CooldownUntil) {
		s.Status = HalfOpen
		s.ConsecutiveFailures = 0
		s.RecoveryFailures = 0
		s.LastError = ""
		s.CooldownUntil = time.Time{}
	}
	return s
}

func normalizeScopes(s State, now time.Time) State {
	for name, st := range s.Scopes {
		if st.Status == Cooldown && !st.CooldownUntil.IsZero() && now.After(st.CooldownUntil) {
			st.Status = Unknown
			st.ConsecutiveFailures = 0
			st.LastError = ""
			st.CooldownUntil = time.Time{}
			s.Scopes[name] = st
		}
	}
	return s
}

func stateNeedsNormalization(s State, now time.Time) bool {
	if s.Status == Cooldown && !s.CooldownUntil.IsZero() && now.After(s.CooldownUntil) {
		return true
	}
	for _, st := range s.Scopes {
		if st.Status == Cooldown && !st.CooldownUntil.IsZero() && now.After(st.CooldownUntil) {
			return true
		}
	}
	return false
}

func scopesReadyState(s State, scopes []string) bool {
	for _, scope := range scopes {
		if st, ok := s.Scopes[scope]; ok && st.Status == Cooldown {
			return false
		}
	}
	return true
}

func (m *Manager) GetWithScopes(id string, scopes []string) (State, bool) {
	now := time.Now()
	m.mu.RLock()
	s, ok := m.states[id]
	if !ok {
		m.mu.RUnlock()
		s = State{Deployment: id, Status: Unknown}
		return s, true
	}
	if !stateNeedsNormalization(s, now) {
		out := cloneState(s)
		ready := scopesReadyState(s, scopes)
		m.mu.RUnlock()
		return out, ready
	}
	m.mu.RUnlock()

	m.mu.Lock()
	s, ok = m.states[id]
	if !ok {
		m.mu.Unlock()
		s = State{Deployment: id, Status: Unknown}
		return s, true
	}
	s = normalizeScopes(normalizeGlobal(s, now), now)
	m.states[id] = s
	out := cloneState(s)
	ready := scopesReadyState(s, scopes)
	m.mu.Unlock()
	return out, ready
}

func (m *Manager) Get(id string) State {
	s, _ := m.GetWithScopes(id, nil)
	return s
}

func updateEWMA(s *State, latency time.Duration) {
	ms := float64(latency.Milliseconds())
	if ms <= 0 {
		return
	}
	if s.EWMALatencyMS == 0 {
		s.EWMALatencyMS = ms
	} else {
		s.EWMALatencyMS = s.EWMALatencyMS*0.75 + ms*0.25
	}
}

func updateFailureEWMA(s *State, failed bool) {
	sample := 0.0
	if failed {
		sample = 1
	}
	if s.Successes+s.Failures <= 1 {
		s.EWMAFailureRate = sample
		return
	}
	// Keep recent reliability meaningful without permanently penalizing a
	// deployment for failures that happened far in the past. The same 0.25
	// observation weight used for latency keeps the signal stable but adaptive.
	s.EWMAFailureRate = s.EWMAFailureRate*0.75 + sample*0.25
}

func (m *Manager) RecordSuccess(id string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Successes++
	updateFailureEWMA(&s, false)
	s.ConsecutiveFailures = 0
	s.RecoveryFailures = 0
	s.Status = Healthy
	s.LastChecked = time.Now()
	s.LastSuccess = s.LastChecked
	s.LastError = ""
	s.CooldownUntil = time.Time{}
	updateEWMA(&s, latency)
	m.states[id] = s
}

func (m *Manager) RecordFailure(id, errMsg string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Failures++
	updateFailureEWMA(&s, true)
	s.ConsecutiveFailures++
	s.LastChecked = time.Now()
	s.LastFailure = s.LastChecked
	s.LastError = errMsg
	updateEWMA(&s, latency)
	if s.Status == HalfOpen || s.ConsecutiveFailures >= m.threshold {
		s.Status = Cooldown
		s.CooldownUntil = time.Now().Add(m.cooldown)
	} else {
		s.Status = Degraded
	}
	m.states[id] = s
}

func (m *Manager) Snapshot() []State {
	now := time.Now()
	m.mu.RLock()
	needsWrite := false
	for _, s := range m.states {
		if stateNeedsNormalization(s, now) {
			needsWrite = true
			break
		}
	}
	if !needsWrite {
		out := make([]State, 0, len(m.states))
		for _, s := range m.states {
			out = append(out, cloneState(s))
		}
		m.mu.RUnlock()
		return out
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.states))
	now = time.Now()
	for id, s := range m.states {
		s = normalizeScopes(normalizeGlobal(s, now), now)
		m.states[id] = s
		out = append(out, cloneState(s))
	}
	return out
}

func (m *Manager) Quarantine(id, reason string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Failures++
	updateFailureEWMA(&s, true)
	s.ConsecutiveFailures++
	s.RecoveryFailures = 0
	s.Status = Degraded
	s.LastChecked = time.Now()
	s.LastFailure = s.LastChecked
	s.LastError = reason
	s.CooldownUntil = time.Time{}
	updateEWMA(&s, latency)
	m.states[id] = s
}

func (m *Manager) RecordRecoveryFailure(id, reason string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Failures++
	updateFailureEWMA(&s, true)
	s.ConsecutiveFailures++
	s.RecoveryFailures++
	s.Status = Degraded
	s.LastChecked = time.Now()
	s.LastFailure = s.LastChecked
	s.LastError = reason
	s.CooldownUntil = time.Time{}
	updateEWMA(&s, latency)
	m.states[id] = s
}

func (m *Manager) EnterCooldown(id, reason string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d <= 0 {
		d = m.cooldown
	}
	s := m.states[id]
	s.Deployment = id
	s.Status = Cooldown
	s.LastChecked = time.Now()
	s.LastError = reason
	s.CooldownUntil = time.Now().Add(d)
	m.states[id] = s
}

func (m *Manager) ForceCooldown(id, reason string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d <= 0 {
		d = m.cooldown
	}
	s := m.states[id]
	s.Deployment = id
	s.Failures++
	updateFailureEWMA(&s, true)
	s.ConsecutiveFailures++
	s.Status = Cooldown
	s.LastChecked = time.Now()
	s.LastFailure = s.LastChecked
	s.LastError = reason
	s.CooldownUntil = time.Now().Add(d)
	m.states[id] = s
}

func (m *Manager) RecordScopeSuccess(id string, scopes []string) {
	if len(scopes) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	if s.Scopes == nil {
		s.Scopes = map[string]ScopeState{}
	}
	now := time.Now()
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		st := s.Scopes[scope]
		st.Status = Healthy
		st.Successes++
		st.ConsecutiveFailures = 0
		st.LastChecked = now
		st.LastSuccess = now
		st.LastError = ""
		st.CooldownUntil = time.Time{}
		s.Scopes[scope] = st
	}
	m.states[id] = s
}

func (m *Manager) RecordScopeFailure(id string, scopes []string, reason string) {
	if len(scopes) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	if s.Scopes == nil {
		s.Scopes = map[string]ScopeState{}
	}
	now := time.Now()
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		st := s.Scopes[scope]
		st.Failures++
		st.ConsecutiveFailures++
		st.LastChecked = now
		st.LastFailure = now
		st.LastError = reason
		if st.ConsecutiveFailures >= m.scopeThreshold {
			st.Status = Cooldown
			st.CooldownUntil = now.Add(m.scopeCooldown)
		} else {
			st.Status = Degraded
		}
		s.Scopes[scope] = st
	}
	m.states[id] = s
}

func (m *Manager) ScopesReady(id string, scopes []string) bool {
	if len(scopes) == 0 {
		return true
	}
	_, ready := m.GetWithScopes(id, scopes)
	return ready
}

func (m *Manager) Invalidate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[id] = State{Deployment: id, Status: Unknown}
}

func (m *Manager) Retain(valid map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.states {
		if _, ok := valid[id]; !ok {
			delete(m.states, id)
		}
	}
}

func (m *Manager) Configure(threshold int, cooldown time.Duration) {
	m.mu.RLock()
	scopeThreshold := m.scopeThreshold
	scopeCooldown := m.scopeCooldown
	m.mu.RUnlock()
	m.ConfigureAdvanced(threshold, cooldown, scopeThreshold, scopeCooldown)
}

func (m *Manager) ConfigureAdvanced(threshold int, cooldown time.Duration, scopeThreshold int, scopeCooldown time.Duration) {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	if scopeThreshold < 1 {
		scopeThreshold = 2
	}
	if scopeCooldown <= 0 {
		scopeCooldown = 5 * time.Minute
	}
	m.mu.Lock()
	m.threshold = threshold
	m.cooldown = cooldown
	m.scopeThreshold = scopeThreshold
	m.scopeCooldown = scopeCooldown
	m.mu.Unlock()
}
