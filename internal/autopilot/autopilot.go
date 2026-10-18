// Package autopilot implements the Phase 18 routing-policy change lifecycle:
// draft → shadow → canary → promoted / auto-rolled-back.
//
// A new policy version never earns full traffic by assumption. It is staged as
// a candidate, served a bounded fraction of real requests, and judged against
// a gate built from real signals (error-class rates, failover rate, no-eligible
// rate, budget-reservation anomalies, TTFT/latency percentiles) compared with
// the incumbent over the same window. Missing or thin evidence means HOLD —
// never an assumed success. A gate violation reverts to the previous version
// atomically and raises a visible incident.
//
// The controller changes routing policy only. It never touches credentials,
// budgets or stored data, and rollback never rewrites an in-flight lease: a
// request that started under a policy finishes under that policy.
package autopilot

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"
)

// State is a policy version's position in the canary lifecycle.
type State string

const (
	// StateDraft is a candidate that is defined but earns no traffic.
	StateDraft State = "draft"
	// StateShadow observes what a candidate would choose without serving it.
	// Shadow evidence is collected by the decision plane (#412); the autopilot
	// never routes production traffic to a shadow candidate.
	StateShadow State = "shadow"
	// StateCanary serves a bounded share of traffic to the candidate.
	StateCanary State = "canary"
	// StatePromoted means the candidate became the incumbent.
	StatePromoted State = "promoted"
	// StateRolledBack means the candidate was withdrawn after a gate trip.
	StateRolledBack State = "rolled_back"
)

func (s State) valid() bool {
	switch s {
	case StateDraft, StateShadow, StateCanary, StatePromoted, StateRolledBack:
		return true
	}
	return false
}

// PolicyRef identifies a routing policy version without exposing its
// configuration. ID is the policy id; Version is the operator label or derived
// fingerprint (#412).
type PolicyRef struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

func (p PolicyRef) String() string {
	if p.Version == "" {
		return p.ID
	}
	return p.ID + "@" + p.Version
}

// ErrorClass is a bounded classification of a terminal failure. It is never
// upstream error text.
type ErrorClass string

const (
	ClassNone           ErrorClass = ""
	ClassServerError    ErrorClass = "server_error"
	ClassRateLimit      ErrorClass = "rate_limit"
	ClassAuthError      ErrorClass = "auth_error"
	ClassTimeout        ErrorClass = "timeout"
	ClassTransportError ErrorClass = "transport_error"
	ClassNoEligible     ErrorClass = "no_eligible_candidate"
	ClassClientError    ErrorClass = "client_error"
)

// Sample is the bounded outcome of one request served under a policy version.
// It carries counters and latency only — never prompts, completions or
// credentials.
type Sample struct {
	Success       bool
	ErrorClass    ErrorClass
	Failover      bool
	NoEligible    bool
	BudgetAnomaly bool
	LatencyMS     float64
	TTFTMS        float64
	HasTTFT       bool
	At            time.Time
}

const maxLatencySamples = 4096

// window is a bounded per-policy signal accumulator. It keeps the most recent
// latency/TTFT samples so percentile computation is deterministic and cannot
// grow without bound.
type window struct {
	requests        int64
	successes       int64
	errors          int64
	serverErrors    int64
	rateLimits      int64
	authErrors      int64
	timeouts        int64
	transportErrors int64
	noEligible      int64
	failovers       int64
	budgetAnomalies int64
	latencyMS       []float64
	ttftMS          []float64
}

func (w *window) observe(s Sample) {
	w.requests++
	if s.Success {
		w.successes++
	} else {
		w.errors++
		switch s.ErrorClass {
		case ClassServerError:
			w.serverErrors++
		case ClassRateLimit:
			w.rateLimits++
		case ClassAuthError:
			w.authErrors++
		case ClassTimeout:
			w.timeouts++
		case ClassTransportError:
			w.transportErrors++
		}
	}
	if s.Failover {
		w.failovers++
	}
	if s.NoEligible {
		w.noEligible++
	}
	if s.BudgetAnomaly {
		w.budgetAnomalies++
	}
	if s.LatencyMS > 0 {
		w.latencyMS = appendBounded(w.latencyMS, s.LatencyMS)
	}
	if s.HasTTFT && s.TTFTMS > 0 {
		w.ttftMS = appendBounded(w.ttftMS, s.TTFTMS)
	}
}

func appendBounded(dst []float64, v float64) []float64 {
	if len(dst) >= maxLatencySamples {
		copy(dst, dst[1:])
		dst[len(dst)-1] = v
		return dst
	}
	return append(dst, v)
}

// Signals is an immutable, privacy-safe snapshot of the evidence a gate
// evaluates. Rates are computed against Requests and are 0 when there is no
// data — never an assumed value.
type Signals struct {
	Requests        int64   `json:"requests"`
	Successes       int64   `json:"successes"`
	Errors          int64   `json:"errors"`
	ErrorRate       float64 `json:"error_rate"`
	ServerErrors    int64   `json:"server_errors"`
	RateLimits      int64   `json:"rate_limits"`
	AuthErrors      int64   `json:"auth_errors"`
	Timeouts        int64   `json:"timeouts"`
	TransportErrors int64   `json:"transport_errors"`
	NoEligible      int64   `json:"no_eligible"`
	NoEligibleRate  float64 `json:"no_eligible_rate"`
	Failovers       int64   `json:"failovers"`
	FailoverRate    float64 `json:"failover_rate"`
	BudgetAnomalies int64   `json:"budget_anomalies"`
	LatencyP50MS    float64 `json:"latency_p50_ms"`
	LatencyP95MS    float64 `json:"latency_p95_ms"`
	TTFTCount       int64   `json:"ttft_count"`
	TTFTP95MS       float64 `json:"ttft_p95_ms"`
}

func (w *window) snapshot() Signals {
	s := Signals{
		Requests:        w.requests,
		Successes:       w.successes,
		Errors:          w.errors,
		ServerErrors:    w.serverErrors,
		RateLimits:      w.rateLimits,
		AuthErrors:      w.authErrors,
		Timeouts:        w.timeouts,
		TransportErrors: w.transportErrors,
		NoEligible:      w.noEligible,
		Failovers:       w.failovers,
		BudgetAnomalies: w.budgetAnomalies,
		LatencyP50MS:    percentile(w.latencyMS, 0.50),
		LatencyP95MS:    percentile(w.latencyMS, 0.95),
		TTFTCount:       int64(len(w.ttftMS)),
		TTFTP95MS:       percentile(w.ttftMS, 0.95),
	}
	if w.requests > 0 {
		s.ErrorRate = float64(w.errors) / float64(w.requests)
		s.NoEligibleRate = float64(w.noEligible) / float64(w.requests)
		s.FailoverRate = float64(w.failovers) / float64(w.requests)
	}
	return s
}

// percentile returns the nearest-rank percentile of a copy of the samples.
// An empty set yields 0.
func percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]float64, len(samples))
	copy(cp, samples)
	sort.Float64s(cp)
	if p <= 0 {
		return cp[0]
	}
	if p >= 1 {
		return cp[len(cp)-1]
	}
	idx := int(p*float64(len(cp)-1) + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// GateConfig bounds when a candidate may be promoted or must be rolled back.
// Every threshold is checked against real observed signals; thin data yields a
// HOLD verdict regardless of the thresholds.
type GateConfig struct {
	// MinSamples is the minimum candidate request count before any verdict
	// other than hold is allowed. This is the "no promotion on missing data"
	// guarantee.
	MinSamples int
	// MinPromoteSamples is the sample count required to PROMOTE. It is raised
	// to at least MinSamples.
	MinPromoteSamples int
	// MaxErrorRate is the candidate error rate above which it rolls back.
	MaxErrorRate float64
	// MaxErrorRateDelta is the candidate-minus-incumbent error-rate margin
	// above which the candidate rolls back. Ignored while the incumbent has
	// fewer than MinSamples samples.
	MaxErrorRateDelta float64
	// MaxFailoverRate is the candidate failover rate above which it rolls back.
	MaxFailoverRate float64
	// MaxNoEligibleRate is the candidate no-eligible-candidate rate above which
	// it rolls back.
	MaxNoEligibleRate float64
	// MaxBudgetAnomalies is a hard count of budget-reservation anomalies.
	MaxBudgetAnomalies int64
	// MaxLatencyP95MS and MaxTTFTP95MS are optional (0 disables) latency
	// ceilings applied to the candidate window.
	MaxLatencyP95MS float64
	MaxTTFTP95MS    float64
}

// DefaultGateConfig returns conservative thresholds. They favour holding over
// promoting: a candidate must clear MinPromoteSamples clean requests before it
// earns full traffic.
func DefaultGateConfig() GateConfig {
	return GateConfig{
		MinSamples:         20,
		MinPromoteSamples:  100,
		MaxErrorRate:       0.20,
		MaxErrorRateDelta:  0.15,
		MaxFailoverRate:    0.50,
		MaxNoEligibleRate:  0.10,
		MaxBudgetAnomalies: 0,
	}
}

func (g GateConfig) withDefaults() GateConfig {
	d := DefaultGateConfig()
	if g.MinSamples <= 0 {
		g.MinSamples = d.MinSamples
	}
	if g.MinPromoteSamples <= 0 {
		g.MinPromoteSamples = d.MinPromoteSamples
	}
	if g.MinPromoteSamples < g.MinSamples {
		g.MinPromoteSamples = g.MinSamples
	}
	if g.MaxErrorRate <= 0 {
		g.MaxErrorRate = d.MaxErrorRate
	}
	if g.MaxErrorRateDelta <= 0 {
		g.MaxErrorRateDelta = d.MaxErrorRateDelta
	}
	if g.MaxFailoverRate <= 0 {
		g.MaxFailoverRate = d.MaxFailoverRate
	}
	if g.MaxNoEligibleRate <= 0 {
		g.MaxNoEligibleRate = d.MaxNoEligibleRate
	}
	if g.MaxBudgetAnomalies < 0 {
		g.MaxBudgetAnomalies = 0
	}
	if g.MaxLatencyP95MS < 0 {
		g.MaxLatencyP95MS = 0
	}
	if g.MaxTTFTP95MS < 0 {
		g.MaxTTFTP95MS = 0
	}
	return g
}

// Verdict is the outcome of a gate evaluation.
type Verdict string

const (
	VerdictHold     Verdict = "hold"
	VerdictPromote  Verdict = "promote"
	VerdictRollback Verdict = "rollback"
)

// GateResult is the recorded evidence for one gate decision.
type GateResult struct {
	Verdict     Verdict   `json:"verdict"`
	Reasons     []string  `json:"reasons"`
	EvaluatedAt time.Time `json:"evaluated_at"`
	Signals     Signals   `json:"signals"`
}

// EvaluateGate is a pure function over observed signals. It never promotes or
// rolls back on missing or thin data: below MinSamples the verdict is hold.
func EvaluateGate(candidate, incumbent Signals, cfg GateConfig) GateResult {
	cfg = cfg.withDefaults()
	res := GateResult{
		Verdict: VerdictHold,
		Signals: candidate,
	}
	if candidate.Requests < int64(cfg.MinSamples) {
		res.Reasons = []string{fmt.Sprintf("insufficient_samples:%d<%d", candidate.Requests, cfg.MinSamples)}
		return res
	}

	violations := make([]string, 0, 4)
	if candidate.ErrorRate > cfg.MaxErrorRate {
		violations = append(violations, fmt.Sprintf("error_rate:%.4f>%.4f", candidate.ErrorRate, cfg.MaxErrorRate))
	}
	if incumbent.Requests >= int64(cfg.MinSamples) {
		delta := candidate.ErrorRate - incumbent.ErrorRate
		if delta > cfg.MaxErrorRateDelta {
			violations = append(violations, fmt.Sprintf("error_rate_regression:%.4f>%.4f", delta, cfg.MaxErrorRateDelta))
		}
	}
	if candidate.FailoverRate > cfg.MaxFailoverRate {
		violations = append(violations, fmt.Sprintf("failover_rate:%.4f>%.4f", candidate.FailoverRate, cfg.MaxFailoverRate))
	}
	if candidate.NoEligibleRate > cfg.MaxNoEligibleRate {
		violations = append(violations, fmt.Sprintf("no_eligible_rate:%.4f>%.4f", candidate.NoEligibleRate, cfg.MaxNoEligibleRate))
	}
	if candidate.BudgetAnomalies > cfg.MaxBudgetAnomalies {
		violations = append(violations, fmt.Sprintf("budget_anomalies:%d>%d", candidate.BudgetAnomalies, cfg.MaxBudgetAnomalies))
	}
	if cfg.MaxLatencyP95MS > 0 && candidate.LatencyP95MS > cfg.MaxLatencyP95MS {
		violations = append(violations, fmt.Sprintf("latency_p95_ms:%.1f>%.1f", candidate.LatencyP95MS, cfg.MaxLatencyP95MS))
	}
	if cfg.MaxTTFTP95MS > 0 && candidate.TTFTP95MS > cfg.MaxTTFTP95MS {
		violations = append(violations, fmt.Sprintf("ttft_p95_ms:%.1f>%.1f", candidate.TTFTP95MS, cfg.MaxTTFTP95MS))
	}
	if len(violations) > 0 {
		res.Verdict = VerdictRollback
		res.Reasons = violations
		return res
	}
	if candidate.Requests < int64(cfg.MinPromoteSamples) {
		res.Reasons = []string{fmt.Sprintf("awaiting_promotion_samples:%d<%d", candidate.Requests, cfg.MinPromoteSamples)}
		return res
	}
	res.Verdict = VerdictPromote
	res.Reasons = []string{"gates_satisfied"}
	return res
}

// Lease pins the policy version a request started under. A rollback never
// mutates an issued lease, so in-flight requests finish on the policy they
// began with.
type Lease struct {
	ID         uint64    `json:"id"`
	Policy     PolicyRef `json:"policy"`
	Generation uint64    `json:"generation"`
	Canary     bool      `json:"canary"`
	IssuedAt   time.Time `json:"issued_at"`
}

// Transition records one lifecycle state change with the evidence that caused
// it.
type Transition struct {
	At       time.Time   `json:"at"`
	From     State       `json:"from"`
	To       State       `json:"to"`
	PolicyID string      `json:"policy_id"`
	Version  string      `json:"version,omitempty"`
	Cause    string      `json:"cause"`
	Gate     *GateResult `json:"gate,omitempty"`
}

// Incident is a visible, operator-facing record of an automatic rollback.
type Incident struct {
	ID              string     `json:"id"`
	At              time.Time  `json:"at"`
	Kind            string     `json:"kind"`
	Cause           string     `json:"cause"`
	PolicyID        string     `json:"policy_id"`
	BadVersion      string     `json:"bad_version,omitempty"`
	RestoredVersion string     `json:"restored_version,omitempty"`
	Gate            GateResult `json:"gate"`
	Message         string     `json:"message"`
}

// Status is the bounded operator surface fed by real controller state.
type Status struct {
	GeneratedAt       time.Time    `json:"generated_at"`
	TargetPolicyID    string       `json:"target_policy_id"`
	State             State        `json:"state"`
	Incumbent         PolicyRef    `json:"incumbent"`
	Candidate         *PolicyRef   `json:"candidate,omitempty"`
	CanaryPercent     float64      `json:"canary_percent"`
	CanaryServed      int64        `json:"canary_served"`
	CanaryMaxRequests int64        `json:"canary_max_requests,omitempty"`
	InFlight          int          `json:"in_flight"`
	Generation        uint64       `json:"generation"`
	CandidateSignals  Signals      `json:"candidate_signals"`
	IncumbentSignals  Signals      `json:"incumbent_signals"`
	LastGateDecision  *GateResult  `json:"last_gate_decision,omitempty"`
	LastRollback      *Incident    `json:"last_rollback,omitempty"`
	Transitions       []Transition `json:"transitions"`
	Incidents         []Incident   `json:"incidents"`
}

const maxHistory = 64

// Controller owns the lifecycle of one target policy. It is safe for
// concurrent use by the data plane.
type Controller struct {
	mu       sync.Mutex
	now      func() time.Time
	targetID string
	cfg      GateConfig

	incumbent    PolicyRef
	candidate    PolicyRef
	hasCandidate bool
	state        State
	generation   uint64

	canaryPercent     float64
	canaryMaxRequests int64
	canaryServed      int64

	candidateWindow window
	incumbentWindow window

	transitions  []Transition
	incidents    []Incident
	lastGate     *GateResult
	lastRollback *Incident

	leaseSeq    uint64
	incidentSeq uint64
	active      map[uint64]Lease
}

// New builds a controller for a target policy with the given gate config. The
// incumbent is the currently-served policy version.
func New(target PolicyRef, incumbent PolicyRef, cfg GateConfig) *Controller {
	if target.ID == "" {
		target = incumbent
	}
	return &Controller{
		now:       time.Now,
		targetID:  target.ID,
		cfg:       cfg.withDefaults(),
		incumbent: incumbent,
		state:     StateDraft,
		active:    map[uint64]Lease{},
	}
}

// SetClock injects a deterministic clock (tests).
func (c *Controller) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now != nil {
		c.now = now
	}
}

// TargetPolicyID returns the policy id this controller manages.
func (c *Controller) TargetPolicyID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targetID
}

// TargetMatches reports whether the given policy id is the one under change.
func (c *Controller) TargetMatches(policyID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return policyID != "" && policyID == c.targetID
}

// BeginShadow stages a candidate for observation only. No canary traffic is
// served in this state.
func (c *Controller) BeginShadow(candidate PolicyRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := c.state
	c.candidate = candidate
	c.hasCandidate = true
	c.state = StateShadow
	c.canaryServed = 0
	c.candidateWindow = window{}
	c.recordTransitionLocked(from, StateShadow, candidate, "shadow_started", nil)
}

// StartCanary stages a candidate and begins serving a bounded share of traffic
// to it. percent is clamped to [0,100]; maxRequests <= 0 means unbounded by
// count (the percent still bounds the share).
func (c *Controller) StartCanary(candidate PolicyRef, percent float64, maxRequests int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if candidate.ID == "" {
		return fmt.Errorf("candidate policy id is required")
	}
	if candidate.ID == c.incumbent.ID && candidate.Version == c.incumbent.Version {
		return fmt.Errorf("candidate %s is identical to the incumbent", candidate)
	}
	from := c.state
	c.candidate = candidate
	c.hasCandidate = true
	c.state = StateCanary
	c.canaryPercent = clamp(percent, 0, 100)
	c.canaryMaxRequests = maxRequests
	if c.canaryMaxRequests < 0 {
		c.canaryMaxRequests = 0
	}
	c.canaryServed = 0
	c.candidateWindow = window{}
	c.recordTransitionLocked(from, StateCanary, candidate, "canary_started", nil)
	return nil
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Select issues a lease for a request. In canary state a bounded share of
// requests is assigned to the candidate; every other state serves the
// incumbent. The returned lease is immutable.
func (c *Controller) Select(requestKey string) Lease {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref := c.incumbent
	canary := false
	if c.state == StateCanary && c.hasCandidate {
		exhausted := c.canaryMaxRequests > 0 && c.canaryServed >= c.canaryMaxRequests
		if !exhausted && c.shouldCanaryLocked(requestKey) {
			ref = c.candidate
			canary = true
			c.canaryServed++
		}
	}
	c.leaseSeq++
	l := Lease{
		ID:         c.leaseSeq,
		Policy:     ref,
		Generation: c.generation,
		Canary:     canary,
		IssuedAt:   c.now(),
	}
	c.active[l.ID] = l
	return l
}

func (c *Controller) shouldCanaryLocked(requestKey string) bool {
	if c.canaryPercent >= 100 {
		return true
	}
	if c.canaryPercent <= 0 {
		return false
	}
	if requestKey == "" {
		// No stable key: fall back to a monotonic counter so the share is
		// still bounded and deterministic.
		return (c.canaryServed % int64(100/c.canaryPercent+1)) == 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestKey))
	return float64(h.Sum32()%100000) < c.canaryPercent*1000
}

// Record completes a lease with its outcome. Recording an auto-rollback is
// synchronous so a tripping gate is reflected before the next selection.
func (c *Controller) Record(l Lease, s Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, l.ID)
	if s.At.IsZero() {
		s.At = c.now()
	}
	if l.Canary {
		c.candidateWindow.observe(s)
	} else {
		c.incumbentWindow.observe(s)
	}
	if c.state == StateCanary && c.hasCandidate {
		c.maybeAutoRollbackLocked()
	}
}

// Release drops a lease without recording an outcome (for example a request
// rejected before any attempt). It never mutates routing state.
func (c *Controller) Release(l Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, l.ID)
}

func (c *Controller) maybeAutoRollbackLocked() {
	if c.candidateWindow.requests < int64(c.cfg.MinSamples) {
		return
	}
	res := c.evaluateLocked()
	if res.Verdict == VerdictRollback {
		c.rollbackLocked("auto_rollback", res)
	}
}

// Evaluate returns the current gate decision for the candidate without
// changing state.
func (c *Controller) Evaluate() GateResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evaluateLocked()
}

func (c *Controller) evaluateLocked() GateResult {
	res := EvaluateGate(c.candidateWindow.snapshot(), c.incumbentWindow.snapshot(), c.cfg)
	res.EvaluatedAt = c.now()
	c.lastGate = &res
	return res
}

// Promote promotes the candidate to incumbent when the gate permits. It
// returns false when there is no candidate, the controller is not in canary
// state, or the gate is not a promote (missing/thin data can never promote).
func (c *Controller) Promote() (Transition, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateCanary || !c.hasCandidate {
		return Transition{}, false
	}
	res := c.evaluateLocked()
	if res.Verdict != VerdictPromote {
		return Transition{}, false
	}
	from := c.state
	promoted := c.candidate
	c.incumbent = promoted
	c.incumbentWindow = c.candidateWindow
	c.candidateWindow = window{}
	c.hasCandidate = false
	c.canaryServed = 0
	c.generation++
	c.state = StatePromoted
	tr := c.recordTransitionLocked(from, StatePromoted, promoted, "gate_promoted", &res)
	return tr, true
}

// Rollback atomically withdraws the candidate and restores the incumbent. It
// changes routing policy only and leaves issued leases intact.
func (c *Controller) Rollback(cause string) (Incident, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasCandidate {
		return Incident{}, false
	}
	res := c.evaluateLocked()
	return c.rollbackLocked(cause, res), true
}

func (c *Controller) rollbackLocked(cause string, res GateResult) Incident {
	bad := c.candidate
	restored := c.incumbent
	from := c.state
	c.generation++
	c.state = StateRolledBack
	c.hasCandidate = false
	c.canaryServed = 0
	c.candidateWindow = window{}
	c.recordTransitionLocked(from, StateRolledBack, bad, cause, &res)

	c.incidentSeq++
	reason := "gate trip"
	if len(res.Reasons) > 0 {
		reason = res.Reasons[0]
	}
	inc := Incident{
		ID:              fmt.Sprintf("inc-%d", c.incidentSeq),
		At:              c.now(),
		Kind:            cause,
		Cause:           reason,
		PolicyID:        c.targetID,
		BadVersion:      bad.Version,
		RestoredVersion: restored.Version,
		Gate:            res,
		Message:         fmt.Sprintf("policy %s rolled back %s → %s: %s", c.targetID, bad.String(), restored.String(), reason),
	}
	c.incidents = appendBoundedIncident(c.incidents, inc)
	c.lastRollback = &inc
	return inc
}

func (c *Controller) recordTransitionLocked(from, to State, ref PolicyRef, cause string, gate *GateResult) Transition {
	tr := Transition{
		At:       c.now(),
		From:     from,
		To:       to,
		PolicyID: ref.ID,
		Version:  ref.Version,
		Cause:    cause,
		Gate:     gate,
	}
	c.transitions = appendBoundedTransition(c.transitions, tr)
	return tr
}

func appendBoundedTransition(dst []Transition, tr Transition) []Transition {
	if len(dst) >= maxHistory {
		copy(dst, dst[1:])
		dst[len(dst)-1] = tr
		return dst
	}
	return append(dst, tr)
}

func appendBoundedIncident(dst []Incident, inc Incident) []Incident {
	if len(dst) >= maxHistory {
		copy(dst, dst[1:])
		dst[len(dst)-1] = inc
		return dst
	}
	return append(dst, inc)
}

// Status returns the bounded operator-facing snapshot. Slices are copied so a
// caller cannot mutate controller state.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		GeneratedAt:       c.now(),
		TargetPolicyID:    c.targetID,
		State:             c.state,
		Incumbent:         c.incumbent,
		CanaryPercent:     c.canaryPercent,
		CanaryServed:      c.canaryServed,
		CanaryMaxRequests: c.canaryMaxRequests,
		InFlight:          len(c.active),
		Generation:        c.generation,
		CandidateSignals:  c.candidateWindow.snapshot(),
		IncumbentSignals:  c.incumbentWindow.snapshot(),
		Transitions:       append([]Transition(nil), c.transitions...),
		Incidents:         append([]Incident(nil), c.incidents...),
	}
	if c.hasCandidate {
		cand := c.candidate
		st.Candidate = &cand
	}
	if c.lastGate != nil {
		g := *c.lastGate
		st.LastGateDecision = &g
	}
	if c.lastRollback != nil {
		inc := *c.lastRollback
		st.LastRollback = &inc
	}
	if st.Transitions == nil {
		st.Transitions = []Transition{}
	}
	if st.Incidents == nil {
		st.Incidents = []Incident{}
	}
	return st
}
