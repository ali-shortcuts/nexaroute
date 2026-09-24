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

type ProviderState struct {
	Provider      string    `json:"provider"`
	Status        Status    `json:"status"`
	Evidence      int       `json:"evidence"`
	LastChecked   time.Time `json:"last_checked"`
	LastSuccess   time.Time `json:"last_success"`
	LastFailure   time.Time `json:"last_failure"`
	LastError     string    `json:"last_error,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
}

type State struct {
	Deployment          string                `json:"deployment"`
	Status              Status                `json:"status"`
	Successes           int64                 `json:"successes"`
	Failures            int64                 `json:"failures"`
	ConsecutiveFailures int                   `json:"consecutive_failures"`
	EWMALatencyMS       float64               `json:"ewma_latency_ms"`
	EWMATTFTMS          float64               `json:"ewma_ttft_ms"`
	EWMAFailureRate     float64               `json:"ewma_failure_rate"`
	LastChecked         time.Time             `json:"last_checked"`
	LastSuccess         time.Time             `json:"last_success"`
	LastFailure         time.Time             `json:"last_failure"`
	LastError           string                `json:"last_error,omitempty"`
	CooldownUntil       time.Time             `json:"cooldown_until,omitempty"`
	RecoveryFailures    int                   `json:"recovery_failures,omitempty"`
	Scopes              map[string]ScopeState `json:"scopes,omitempty"`
}

type Manager struct {
	mu                sync.RWMutex
	threshold         int
	cooldown          time.Duration
	scopeThreshold    int
	scopeCooldown     time.Duration
	providerThreshold int
	providerWindow    time.Duration
	providerCooldown  time.Duration
	states            map[string]State
	providers         map[string]ProviderState
	providerEvidence  map[string]map[string]time.Time
}

func New(threshold int, cooldown time.Duration) *Manager {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	return &Manager{
		threshold:         threshold,
		cooldown:          cooldown,
		scopeThreshold:    2,
		scopeCooldown:     5 * time.Minute,
		providerThreshold: 3,
		providerWindow:    20 * time.Second,
		providerCooldown:  30 * time.Second,
		states:            map[string]State{},
		providers:         map[string]ProviderState{},
		providerEvidence:  map[string]map[string]time.Time{},
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

func (m *Manager) RecordTTFT(id string, ttft time.Duration) {
	ms := float64(ttft.Milliseconds())
	if ms <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.states[id]
	st.Deployment = id
	if st.EWMATTFTMS == 0 {
		st.EWMATTFTMS = ms
	} else {
		st.EWMATTFTMS = st.EWMATTFTMS*0.75 + ms*0.25
	}
	m.states[id] = st
}

func (m *Manager) RecordSuccess(id string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.Deployment = id
	s.Successes++
	s.LastChecked = time.Now()
	s.LastSuccess = s.LastChecked
	updateEWMA(&s, latency)
	updateFailureEWMA(&s, false)
	if s.Status == Cooldown && !s.CooldownUntil.IsZero() && s.LastChecked.Before(s.CooldownUntil) {
		// A request that started before a hard cooldown (e.g. a 429
		// retry-after) completed successfully. That stale observation
		// must not re-admit a deployment that is still cooling down;
		// only observations arriving after the deadline may recover it.
		m.states[id] = s
		return
	}
	s.ConsecutiveFailures = 0
	s.RecoveryFailures = 0
	s.Status = Healthy
	s.LastError = ""
	s.CooldownUntil = time.Time{}
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
	if s.Status == Cooldown && !s.CooldownUntil.IsZero() && s.LastChecked.Before(s.CooldownUntil) {
		// Active hard cooldown: an in-flight failure observation must
		// never downgrade the deployment back to Degraded (which would
		// immediately re-admit it and defeat the breaker during the
		// half-open stampede). Keep the existing deadline; counters
		// above still record the evidence.
		m.states[id] = s
		return
	}
	if s.Status == HalfOpen || s.ConsecutiveFailures >= m.threshold {
		s.Status = Cooldown
		s.CooldownUntil = time.Now().Add(m.cooldown)
	} else {
		s.Status = Degraded
		// A degraded state carries no cooldown; drop any stale deadline
		// so the snapshot never reports a bogus cooldown_until.
		s.CooldownUntil = time.Time{}
	}
	m.states[id] = s
}

func normalizeProviderState(st ProviderState, now time.Time) ProviderState {
	if st.Status == Cooldown && !st.CooldownUntil.IsZero() && now.After(st.CooldownUntil) {
		st.Status = HalfOpen
		st.Evidence = 0
		st.LastError = ""
		st.CooldownUntil = time.Time{}
	}
	return st
}

func (m *Manager) ProviderAvailable(id string) bool {
	if id == "" {
		return true
	}
	now := time.Now()
	m.mu.RLock()
	st, ok := m.providers[id]
	if !ok {
		m.mu.RUnlock()
		return true
	}
	if st.Status != Cooldown || st.CooldownUntil.IsZero() || now.Before(st.CooldownUntil) {
		available := st.Status != Cooldown
		m.mu.RUnlock()
		return available
	}
	m.mu.RUnlock()

	m.mu.Lock()
	st, ok = m.providers[id]
	if !ok {
		m.mu.Unlock()
		return true
	}
	st = normalizeProviderState(st, now)
	m.providers[id] = st
	available := st.Status != Cooldown
	m.mu.Unlock()
	return available
}

func (m *Manager) RecordProviderFailure(providerID, deploymentID, reason string) {
	if providerID == "" {
		return
	}
	if deploymentID == "" {
		deploymentID = providerID
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	st := normalizeProviderState(m.providers[providerID], now)
	st.Provider = providerID
	st.LastChecked = now
	st.LastFailure = now
	st.LastError = reason

	// A failure from a request already in flight must never downgrade an active
	// provider cooldown back to Degraded. Keep the hard deadline intact.
	if st.Status == Cooldown && !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
		m.providers[providerID] = st
		return
	}
	if st.Status == HalfOpen {
		st.Status = Cooldown
		st.Evidence = 1
		st.CooldownUntil = now.Add(m.providerCooldown)
		m.providers[providerID] = st
		m.providerEvidence[providerID] = map[string]time.Time{deploymentID: now}
		return
	}

	evidence := m.providerEvidence[providerID]
	if evidence == nil {
		evidence = map[string]time.Time{}
	}
	for id, at := range evidence {
		if now.Sub(at) > m.providerWindow {
			delete(evidence, id)
		}
	}
	evidence[deploymentID] = now
	st.Evidence = len(evidence)
	if st.Evidence >= m.providerThreshold {
		st.Status = Cooldown
		st.CooldownUntil = now.Add(m.providerCooldown)
	} else {
		st.Status = Degraded
		st.CooldownUntil = time.Time{}
	}
	m.providerEvidence[providerID] = evidence
	m.providers[providerID] = st
}

func (m *Manager) RecordProviderSuccess(providerID string) {
	if providerID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.providers[providerID]
	st.Provider = providerID
	st.LastChecked = now
	st.LastSuccess = now
	if st.Status == Cooldown && !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
		// A successful request that started before another request opened the
		// provider circuit is stale evidence; do not re-admit the provider early.
		m.providers[providerID] = st
		return
	}
	st.Status = Healthy
	st.Evidence = 0
	st.LastError = ""
	st.CooldownUntil = time.Time{}
	m.providers[providerID] = st
	delete(m.providerEvidence, providerID)
}

func (m *Manager) ProviderSnapshot() []ProviderState {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ProviderState, 0, len(m.providers))
	for id, st := range m.providers {
		st = normalizeProviderState(st, now)
		m.providers[id] = st
		out = append(out, st)
	}
	return out
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

func (m *Manager) RetainProviders(valid map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.providers {
		if _, ok := valid[id]; !ok {
			delete(m.providers, id)
			delete(m.providerEvidence, id)
		}
	}
}

func (m *Manager) InvalidateProvider(id string) {
	if id == "" {
		return
	}
	m.mu.Lock()
	delete(m.providers, id)
	delete(m.providerEvidence, id)
	m.mu.Unlock()
}

func (m *Manager) Configure(threshold int, cooldown time.Duration) {
	m.mu.RLock()
	scopeThreshold := m.scopeThreshold
	scopeCooldown := m.scopeCooldown
	m.mu.RUnlock()
	m.ConfigureAdvanced(threshold, cooldown, scopeThreshold, scopeCooldown)
}

func (m *Manager) ConfigureProviderIncidents(threshold int, window, cooldown time.Duration) {
	if threshold < 2 {
		threshold = 2
	}
	if window <= 0 {
		window = 20 * time.Second
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	m.mu.Lock()
	m.providerThreshold = threshold
	m.providerWindow = window
	m.providerCooldown = cooldown
	m.mu.Unlock()
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
