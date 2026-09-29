package httpapi

import (
	"sort"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// B3 node telemetry view-model contract (additive only).
//
// Purpose: give the dashboard node popover a typed, testable contract for
// per-deployment KPIs without changing routing decision semantics and
// without fabricating metrics.
//
// Source mapping (authoritative origin of every KPI):
//
//	KPI              | JSON field      | Source
//	-----------------+-----------------+------------------------------
//	deployment id    | deployment      | router.Deployment.ID
//	provider id      | provider        | router.Deployment.ProviderID
//	provider name    | provider_name   | router.Deployment.ProviderName
//	node state       | state           | health.Manager status mapped via health.RichStateFor
//	latency (EWMA)   | latency_ms      | health.State.EWMALatencyMS
//	error rate       | error_rate      | health.State.EWMAFailureRate
//	cooldown until   | cooldown_until  | health.State.CooldownUntil
//	last success     | last_success    | health.State.LastSuccess
//	last failure     | last_failure    | health.State.LastFailure (+ LastError as detail)
//
// Freshness: every present KPI carries source plus observed_at (RFC3339 of
// the underlying observation) and age_ms (now minus observed_at, floored at
// 0). Provider identity is config-sourced: observed_at is the snapshot time
// with age_ms 0.
//
// Omission (never guessed): a KPI pointer is nil and therefore omitted from
// JSON when the underlying data is unavailable:
//   - provider/provider_name: omitted when the deployment has no known
//     provider mapping (unknown deployment id).
//   - state: always present when a health row is emitted (at minimum
//     UNKNOWN); observed_at/age_ms omitted when LastChecked is zero.
//   - latency_ms: omitted when EWMALatencyMS <= 0 (no latency observed).
//   - error_rate: omitted when Successes+Failures == 0 (no traffic
//     observed); a real 0.0 after successes is kept.
//   - cooldown_until: omitted unless Status == Cooldown with a non-zero
//     future deadline; degraded/half-open/healthy never carry one.
//   - last_success: omitted when LastSuccess is zero.
//   - last_failure: omitted when LastFailure is zero.
//
// The builder is a pure presentation derivation: it never mutates
// health.State, router.Deployment, or any decision-engine input.

// NodeTelemetryString is a string KPI with provenance.
type NodeTelemetryString struct {
	Value      string `json:"value"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at,omitempty"`
	AgeMS      *int64 `json:"age_ms,omitempty"`
}

// NodeTelemetryFloat is a numeric KPI with provenance.
type NodeTelemetryFloat struct {
	Value      float64 `json:"value"`
	Source     string  `json:"source"`
	ObservedAt string  `json:"observed_at,omitempty"`
	AgeMS      *int64  `json:"age_ms,omitempty"`
}

// NodeTelemetryTime is a timestamp KPI with provenance.
type NodeTelemetryTime struct {
	Value      string `json:"value"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at,omitempty"`
	AgeMS      *int64 `json:"age_ms,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// NodeTelemetry is the per-node popover view-model. Pointer fields use
// omitempty so unavailable data is omitted from the API, never guessed.
type NodeTelemetry struct {
	Deployment   string               `json:"deployment"`
	Provider     *NodeTelemetryString `json:"provider,omitempty"`
	ProviderName *NodeTelemetryString `json:"provider_name,omitempty"`
	State        *NodeTelemetryString `json:"state,omitempty"`
	LatencyMS    *NodeTelemetryFloat  `json:"latency_ms,omitempty"`
	ErrorRate    *NodeTelemetryFloat  `json:"error_rate,omitempty"`
	CooldownUntl *NodeTelemetryTime   `json:"cooldown_until,omitempty"`
	LastSuccess  *NodeTelemetryTime   `json:"last_success,omitempty"`
	LastFailure  *NodeTelemetryTime   `json:"last_failure,omitempty"`
}

func telemetryAgeMS(now, observed time.Time) *int64 {
	if observed.IsZero() || now.IsZero() {
		return nil
	}
	ms := now.Sub(observed).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	out := ms
	return &out
}

func telemetryObservedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// buildNodeTelemetry derives the additive popover contract from real
// router + health state. Deployments drive the row set; health rows for
// unknown deployments are appended so no observed node is dropped. now is
// the snapshot clock (injectable in tests); zero now disables age_ms.
func buildNodeTelemetry(deployments []router.Deployment, states []health.State, now time.Time) []NodeTelemetry {
	byDeployment := make(map[string]router.Deployment, len(deployments))
	for _, d := range deployments {
		if d.ID == "" {
			continue
		}
		byDeployment[d.ID] = d
	}
	byHealth := make(map[string]health.State, len(states))
	for _, st := range states {
		if st.Deployment == "" {
			continue
		}
		byHealth[st.Deployment] = st
	}
	ids := make(map[string]struct{}, len(byDeployment)+len(byHealth))
	for id := range byDeployment {
		ids[id] = struct{}{}
	}
	for id := range byHealth {
		ids[id] = struct{}{}
	}
	out := make([]NodeTelemetry, 0, len(ids))
	for id := range ids {
		dep, hasDep := byDeployment[id]
		st, hasHealth := byHealth[id]
		row := NodeTelemetry{Deployment: id}
		if hasDep {
			if dep.ProviderID != "" {
				row.Provider = &NodeTelemetryString{
					Value:      dep.ProviderID,
					Source:     "router.deployment.provider_id",
					ObservedAt: telemetryObservedAt(now),
					AgeMS:      telemetryAgeMS(now, now),
				}
			}
			if dep.ProviderName != "" {
				row.ProviderName = &NodeTelemetryString{
					Value:      dep.ProviderName,
					Source:     "router.deployment.provider_name",
					ObservedAt: telemetryObservedAt(now),
					AgeMS:      telemetryAgeMS(now, now),
				}
			}
		}
		if !hasHealth {
			out = append(out, row)
			continue
		}
		stateVal := health.RichStateFor(st)
		stateField := &NodeTelemetryString{
			Value:  stateVal,
			Source: "health.manager.status",
		}
		if !st.LastChecked.IsZero() {
			stateField.ObservedAt = telemetryObservedAt(st.LastChecked)
			stateField.AgeMS = telemetryAgeMS(now, st.LastChecked)
		}
		row.State = stateField
		if st.EWMALatencyMS > 0 {
			lat := &NodeTelemetryFloat{
				Value:  st.EWMALatencyMS,
				Source: "health.state.ewma_latency_ms",
			}
			if !st.LastChecked.IsZero() {
				lat.ObservedAt = telemetryObservedAt(st.LastChecked)
				lat.AgeMS = telemetryAgeMS(now, st.LastChecked)
			}
			row.LatencyMS = lat
		}
		if st.Successes+st.Failures > 0 {
			er := &NodeTelemetryFloat{
				Value:  st.EWMAFailureRate,
				Source: "health.state.ewma_failure_rate",
			}
			if !st.LastChecked.IsZero() {
				er.ObservedAt = telemetryObservedAt(st.LastChecked)
				er.AgeMS = telemetryAgeMS(now, st.LastChecked)
			}
			row.ErrorRate = er
		}
		if st.Status == health.Cooldown && !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
			row.CooldownUntl = &NodeTelemetryTime{
				Value:      telemetryObservedAt(st.CooldownUntil),
				Source:     "health.state.cooldown_until",
				ObservedAt: telemetryObservedAt(st.LastChecked),
				AgeMS:      telemetryAgeMS(now, st.LastChecked),
			}
		}
		if !st.LastSuccess.IsZero() {
			row.LastSuccess = &NodeTelemetryTime{
				Value:      telemetryObservedAt(st.LastSuccess),
				Source:     "health.state.last_success",
				ObservedAt: telemetryObservedAt(st.LastSuccess),
				AgeMS:      telemetryAgeMS(now, st.LastSuccess),
			}
		}
		if !st.LastFailure.IsZero() {
			lf := &NodeTelemetryTime{
				Value:      telemetryObservedAt(st.LastFailure),
				Source:     "health.state.last_failure",
				ObservedAt: telemetryObservedAt(st.LastFailure),
				AgeMS:      telemetryAgeMS(now, st.LastFailure),
			}
			if st.LastError != "" {
				lf.Detail = st.LastError
			}
			row.LastFailure = lf
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deployment < out[j].Deployment })
	if out == nil {
		out = []NodeTelemetry{}
	}
	return out
}
