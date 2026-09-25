package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/jev"
	"github.com/ali-shortcuts/nexaroute/internal/decision/local"
	"github.com/ali-shortcuts/nexaroute/internal/decision/policy"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// ExternalProviderStatus is the safe admin-snapshot row for one external
// DecisionProvider. It never contains secrets, payloads, or credentialed
// URLs — only identity, availability, and configuration shape.
type ExternalProviderStatus struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Enabled       bool   `json:"enabled"`
	Health        string `json:"health"` // healthy | unavailable | disabled
	KeyConfigured bool   `json:"key_configured"`
	PrivacyMode   string `json:"privacy_mode"`
}

// decisionMetrics holds bounded external-decision counters. Labels are
// provider type (bounded adapter set) and outcome class (bounded enum) only:
// never provider config IDs, deployment IDs, endpoints, request IDs, or
// session IDs.
type decisionMetrics struct {
	mu           sync.Mutex
	requests     map[string]uint64
	latencySum   map[string]float64
	latencyCount map[string]uint64
}

func newDecisionMetrics() *decisionMetrics {
	return &decisionMetrics{
		requests:     map[string]uint64{},
		latencySum:   map[string]float64{},
		latencyCount: map[string]uint64{},
	}
}

func (m *decisionMetrics) observe(providerType, outcome string, latency time.Duration) {
	if m == nil {
		return
	}
	// Defensive re-bounding: production values are already enums, but the
	// metrics map must never grow without bound regardless of caller.
	if len(providerType) > 64 {
		providerType = providerType[:64]
	}
	if len(outcome) > 64 {
		outcome = outcome[:64]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[providerType+"\x00"+outcome]++
	if latency < 0 {
		latency = 0
	}
	m.latencySum[providerType] += latency.Seconds()
	m.latencyCount[providerType]++
}

type decisionMetricRow struct {
	Type    string
	Outcome string
	Count   uint64
}

type decisionLatencyRow struct {
	Type  string
	Count uint64
	Sum   float64
}

func (m *decisionMetrics) snapshot() ([]decisionMetricRow, []decisionLatencyRow) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]decisionMetricRow, 0, len(m.requests))
	for k, v := range m.requests {
		parts := strings.SplitN(k, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		rows = append(rows, decisionMetricRow{Type: parts[0], Outcome: parts[1], Count: v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		return rows[i].Outcome < rows[j].Outcome
	})
	lat := make([]decisionLatencyRow, 0, len(m.latencyCount))
	for t, c := range m.latencyCount {
		lat = append(lat, decisionLatencyRow{Type: t, Count: c, Sum: m.latencySum[t]})
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i].Type < lat[j].Type })
	return rows, lat
}

// DecisionRuntime is an immutable per-config-generation snapshot of the
// decision plane: registry, selected provider, orchestrator, and admin rows.
// Hot reload builds a fresh runtime and swaps it atomically; in-flight
// requests keep using the old coherent snapshot (including its resolved
// credential snapshot) until they finish.
type DecisionRuntime struct {
	mode         string
	providerID   string
	timeout      time.Duration
	registry     *decision.Registry
	selected     decision.DecisionProvider
	orchestrator *decision.Orchestrator
	metrics      *decisionMetrics
	status       []ExternalProviderStatus
}

// decisionBuildOptions carries test-only overrides. Production builds always
// use the zero value (official Jev endpoint, default transport).
type decisionBuildOptions struct {
	jevEndpoint  string
	jevTransport http.RoundTripper
}

func disabledDecisionRuntime() *DecisionRuntime {
	reg := decision.NewRegistry()
	_ = reg.Register(local.New())
	_ = reg.Register(policy.New())
	return &DecisionRuntime{
		mode:         decision.ModeOff,
		providerID:   decision.BuiltinLocalID,
		timeout:      decision.DefaultDecisionTimeout,
		registry:     reg,
		metrics:      newDecisionMetrics(),
		orchestrator: decision.NewOrchestrator(decision.ModeOff, nil, decision.DefaultDecisionTimeout),
	}
}

// buildDecisionRuntime builds the production decision plane for cfg, which
// must already be validated. Configuration errors are returned (and must
// reject the reload); runtime unavailability (missing key) is NOT an error —
// the provider reports unavailable and decisions fail open.
func buildDecisionRuntime(cfg config.Config) (*DecisionRuntime, error) {
	return buildDecisionRuntimeWithOptions(cfg, decisionBuildOptions{}, nil)
}

func buildDecisionRuntimeWithOptions(cfg config.Config, opts decisionBuildOptions, carry *decisionMetrics) (*DecisionRuntime, error) {
	reg := decision.NewRegistry()
	if err := reg.Register(local.New()); err != nil {
		return nil, err
	}
	if err := reg.Register(policy.New()); err != nil {
		return nil, err
	}
	status := make([]ExternalProviderStatus, 0, len(cfg.DecisionProviders))
	for _, p := range cfg.DecisionProviders {
		key := p.ResolvedAPIKey()
		var (
			dp  decision.DecisionProvider
			err error
		)
		switch p.Type {
		case jev.ProviderType:
			if opts.jevEndpoint != "" {
				dp, err = jev.NewWithTransport(p.ID, key, p.PrivacyMode, opts.jevEndpoint, opts.jevTransport)
			} else {
				dp, err = jev.New(p.ID, key, p.PrivacyMode)
			}
		default:
			return nil, errDecisionUnsupportedType(p.ID, p.Type)
		}
		if err != nil {
			return nil, err
		}
		if err := reg.Register(dp); err != nil {
			return nil, err
		}
		health := "unavailable"
		h := dp.Health()
		switch {
		case !p.Enabled:
			health = "disabled"
		case h.Available:
			health = "healthy"
		}
		status = append(status, ExternalProviderStatus{
			ID:            p.ID,
			Type:          p.Type,
			Enabled:       p.Enabled,
			Health:        health,
			KeyConfigured: h.KeyConfigured,
			PrivacyMode:   p.PrivacyMode,
		})
	}
	mode := cfg.Decision.Mode
	providerID := cfg.Decision.Provider
	var selected decision.DecisionProvider
	if mode != decision.ModeOff {
		sel, ok := reg.Get(providerID)
		if !ok {
			return nil, errDecisionUnknownProvider(providerID)
		}
		selected = sel
	}
	timeout := time.Duration(cfg.Decision.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = decision.DefaultDecisionTimeout
	}
	metrics := carry
	if metrics == nil {
		metrics = newDecisionMetrics()
	}
	return &DecisionRuntime{
		mode:         mode,
		providerID:   providerID,
		timeout:      timeout,
		registry:     reg,
		selected:     selected,
		orchestrator: decision.NewOrchestrator(mode, selected, timeout),
		metrics:      metrics,
		status:       status,
	}, nil
}

func errDecisionUnsupportedType(id, typ string) error {
	return fmt.Errorf("decision provider %q has unsupported type %q", id, typ)
}

func errDecisionUnknownProvider(id string) error {
	return fmt.Errorf("decision.provider %q does not match any configured decision provider", id)
}

// decisionCandidatesFromScored adapts the router's eligible set for the
// decision plane. PoolOrdinal maps from the priority tier: NexaRoute core has
// no separate pool construct, and priority tiers are the authoritative
// primary/fallback bands.
func decisionCandidatesFromScored(scored []router.Scored) []decision.Candidate {
	out := make([]decision.Candidate, 0, len(scored))
	for _, s := range scored {
		d := s.Deployment
		out = append(out, decision.Candidate{
			ID:                d.ID,
			PoolOrdinal:       d.Priority,
			Priority:          d.Priority,
			ContextWindow:     d.ContextWindow,
			Tools:             d.Capabilities.Tools,
			Vision:            d.Capabilities.Vision,
			Reasoning:         d.Capabilities.Reasoning,
			Streaming:         d.Capabilities.Streaming,
			EWMALatencyMS:     s.Health.EWMALatencyMS,
			EWMAFailureRate:   s.Health.EWMAFailureRate,
			Observations:      s.Health.Successes + s.Health.Failures,
			HasCost:           d.InputCostPerMTok > 0 || d.OutputCostPerMTok > 0,
			InputCostPerMTok:  d.InputCostPerMTok,
			OutputCostPerMTok: d.OutputCostPerMTok,
		})
	}
	return out
}

// decisionFeaturesFromRequirement derives metadata-only features from the
// routing requirement. Raw prompts, session keys, and credentials never enter
// the decision plane: only capability flags, the structured-output signal,
// and bounded token estimates.
func decisionFeaturesFromRequirement(req router.Requirement, structuredOut bool) decision.RequestFeatures {
	inTokens := req.EstimatedInputTokens
	if inTokens < 0 {
		inTokens = 0
	}
	maxOut := req.MaxOutputTokens
	if maxOut < 0 {
		maxOut = 0
	}
	return decision.RequestFeatures{
		Tools:                  req.Tools,
		Vision:                 req.Vision,
		Reasoning:              req.Reasoning,
		Streaming:              req.Streaming,
		StructuredOut:          structuredOut,
		EstimatedContextTokens: inTokens + maxOut,
		EstimatedInputTokens:   inTokens,
		MaxOutputTokens:        maxOut,
	}
}

// applyDecision runs the single-provider decision flow over the router's
// ordered eligible set and returns the possibly-reordered candidates. It is a
// no-op unless a decision provider is configured; on any failure it returns
// the input order verbatim (fail open). No locks are held during provider
// I/O: the runtime is an immutable atomic snapshot.
func (s *Server) applyDecision(ctx context.Context, requestID string, req router.Requirement, candidates []router.Scored, features decision.RequestFeatures) []router.Scored {
	rt := s.decision.Load()
	if rt == nil || rt.selected == nil || rt.orchestrator == nil || len(candidates) < 2 {
		return candidates
	}
	pin := s.rt.SessionPin(req)
	outcome := rt.orchestrator.Decide(ctx, decisionCandidatesFromScored(candidates), pin, features, requestID)
	selType := rt.selected.Type()
	isExternal := selType != decision.BuiltinLocalID && selType != decision.BuiltinPolicyID
	// The nexaroute_external_decision_* series count external providers only;
	// built-in selections stay in events (kind=local_decision).
	if isExternal {
		rt.metrics.observe(selType, outcome.ExternalOutcome, time.Duration(outcome.ExternalLatencyMS)*time.Millisecond)
	}
	// The local no-op abstention is the configured equivalent of "off"; spare
	// the event bus the noise.
	if !(selType == decision.BuiltinLocalID && outcome.ExternalOutcome == decision.OutcomeAbstained) {
		s.emitDecisionEvent(requestID, outcome)
	}
	if outcome.SelectedID == "" {
		return candidates
	}
	// Map the validated final order back onto scored candidates. Any
	// inconsistency (impossible by construction) fails open.
	if len(outcome.FinalOrder) != len(candidates) {
		return candidates
	}
	byID := make(map[string]router.Scored, len(candidates))
	for _, c := range candidates {
		byID[c.Deployment.ID] = c
	}
	reordered := make([]router.Scored, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, id := range outcome.FinalOrder {
		c, ok := byID[id]
		if !ok {
			return candidates
		}
		if _, dup := seen[id]; dup {
			return candidates
		}
		seen[id] = struct{}{}
		reordered = append(reordered, c)
	}
	return reordered
}

// emitDecisionEvent records a safe, bounded decision event: provider
// identity (type + configured ID), outcome class, local selected ID,
// latency, confidence, and canonical reason codes. Never: keys, payloads,
// responses, the opaque map, task text, or remote guidance.
func (s *Server) emitDecisionEvent(requestID string, outcome decision.Outcome) {
	kind := "external_decision"
	if outcome.ProviderType == decision.BuiltinLocalID || outcome.ProviderType == decision.BuiltinPolicyID {
		kind = "local_decision"
	}
	reasons := strings.Join(decision.Strings(outcome.ReasonCodes), ",")
	if len(reasons) > 256 {
		reasons = reasons[:256]
	}
	providerID := outcome.ProviderID
	if len(providerID) > 256 {
		providerID = providerID[:256]
	}
	var b strings.Builder
	b.Grow(160)
	b.WriteString("provider=")
	b.WriteString(providerID)
	b.WriteString(" type=")
	b.WriteString(outcome.ProviderType)
	b.WriteString(" outcome=")
	b.WriteString(outcome.ExternalOutcome)
	b.WriteString(" latency_ms=")
	b.WriteString(strconv.FormatInt(outcome.ExternalLatencyMS, 10))
	b.WriteString(" confidence=")
	b.WriteString(strconv.FormatFloat(outcome.Confidence, 'f', 3, 64))
	b.WriteString(" reasons=")
	b.WriteString(reasons)
	errType := ""
	switch outcome.ExternalOutcome {
	case decision.OutcomeError, decision.OutcomeTimeout, decision.OutcomeInvalid, decision.OutcomeUnavailable:
		if len(outcome.ReasonCodes) > 0 && outcome.ReasonCodes[0].Valid() {
			errType = string(outcome.ReasonCodes[0])
		} else {
			errType = string(decision.ReasonProviderError)
		}
	}
	s.bus.Add(events.Event{
		RequestID:  requestID,
		Kind:       kind,
		Deployment: outcome.SelectedID,
		Message:    b.String(),
		LatencyMS:  outcome.ExternalLatencyMS,
		ErrorType:  errType,
	})
}

// decisionAdminSnapshot returns the safe decision-plane admin view.
func (s *Server) decisionAdminSnapshot() (mode, provider string, status []ExternalProviderStatus) {
	rt := s.decision.Load()
	if rt == nil {
		return decision.ModeOff, decision.BuiltinLocalID, nil
	}
	return rt.mode, rt.providerID, append([]ExternalProviderStatus(nil), rt.status...)
}

// decisionMetricsSnapshot returns bounded metric rows for Prometheus
// rendering (nil-safe).
func (s *Server) decisionMetricsSnapshot() ([]decisionMetricRow, []decisionLatencyRow) {
	rt := s.decision.Load()
	if rt == nil {
		return nil, nil
	}
	return rt.metrics.snapshot()
}
