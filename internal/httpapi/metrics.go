package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ds := s.rt.All()
	hs := s.hm.Snapshot()
	by := map[health.Status]int{}
	healthByID := map[string]health.State{}
	for _, h := range hs {
		by[h.Status]++
		healthByID[h.Deployment] = h
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP nexaroute_http_requests_total Total HTTP requests received by the gateway.")
	fmt.Fprintln(w, "# TYPE nexaroute_http_requests_total counter")
	fmt.Fprintf(w, "nexaroute_http_requests_total %d\n", s.requestTotal.Load())

	fmt.Fprintln(w, "# HELP nexaroute_inflight_requests Data-plane requests currently admitted by the gateway.")
	fmt.Fprintln(w, "# TYPE nexaroute_inflight_requests gauge")
	fmt.Fprintf(w, "nexaroute_inflight_requests %d\n", s.inflight.Load())
	fmt.Fprintln(w, "# HELP nexaroute_inflight_request_limit Global data-plane admission limit.")
	fmt.Fprintln(w, "# TYPE nexaroute_inflight_request_limit gauge")
	fmt.Fprintf(w, "nexaroute_inflight_request_limit %d\n", s.admissionLimit())
	fmt.Fprintln(w, "# HELP nexaroute_overload_rejections_total Data-plane requests rejected because the gateway was at capacity.")
	fmt.Fprintln(w, "# TYPE nexaroute_overload_rejections_total counter")
	fmt.Fprintf(w, "nexaroute_overload_rejections_total %d\n", s.overloadRejects.Load())

	ps := s.probe.Stats()
	fmt.Fprintln(w, "# HELP nexaroute_probe_active Active health/recovery probes currently holding probe capacity.")
	fmt.Fprintln(w, "# TYPE nexaroute_probe_active gauge")
	fmt.Fprintf(w, "nexaroute_probe_active %d\n", ps.ActiveProbes)
	fmt.Fprintln(w, "# HELP nexaroute_recovery_queue_depth Recovery tasks waiting in the bounded supervisor queue.")
	fmt.Fprintln(w, "# TYPE nexaroute_recovery_queue_depth gauge")
	fmt.Fprintf(w, "nexaroute_recovery_queue_depth %d\n", ps.RecoveryQueueDepth)
	fmt.Fprintln(w, "# HELP nexaroute_recovery_tracked Deployments currently queued, active, or delay-scheduled for recovery.")
	fmt.Fprintln(w, "# TYPE nexaroute_recovery_tracked gauge")
	fmt.Fprintf(w, "nexaroute_recovery_tracked %d\n", ps.RecoveryTracked)
	fmt.Fprintln(w, "# HELP nexaroute_recovery_worker_limit Fixed recovery worker-pool size.")
	fmt.Fprintln(w, "# TYPE nexaroute_recovery_worker_limit gauge")
	fmt.Fprintf(w, "nexaroute_recovery_worker_limit %d\n", ps.RecoveryWorkers)

	fmt.Fprintln(w, "# HELP nexaroute_deployments_total Configured enabled model deployments.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployments_total gauge")
	fmt.Fprintf(w, "nexaroute_deployments_total %d\n", len(ds))

	fmt.Fprintln(w, "# HELP nexaroute_health_deployments Deployment count by health status.")
	fmt.Fprintln(w, "# TYPE nexaroute_health_deployments gauge")
	for _, st := range []health.Status{health.Unknown, health.Healthy, health.Degraded, health.HalfOpen, health.Cooldown} {
		fmt.Fprintf(w, "nexaroute_health_deployments{status=%q} %d\n", st, by[st])
	}

	fmt.Fprintln(w, "# HELP nexaroute_deployment_ewma_latency_ms Response-header EWMA latency by deployment.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_ewma_latency_ms gauge")
	fmt.Fprintln(w, "# HELP nexaroute_deployment_ewma_ttft_ms Streaming time-to-first-byte EWMA by deployment.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_ewma_ttft_ms gauge")
	fmt.Fprintln(w, "# HELP nexaroute_deployment_ewma_failure_rate Recency-weighted failure rate by deployment.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_ewma_failure_rate gauge")
	fmt.Fprintln(w, "# HELP nexaroute_deployment_successes_total Successful observations by deployment.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_successes_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_deployment_failures_total Failed observations by deployment.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_failures_total counter")
	for _, d := range ds {
		h := healthByID[d.ID]
		id := sanitizeMetricLabel(d.ID)
		provider := sanitizeMetricLabel(d.ProviderID)
		fmt.Fprintf(w, "nexaroute_deployment_ewma_latency_ms{deployment=%q,provider=%q} %.3f\n", id, provider, h.EWMALatencyMS)
		fmt.Fprintf(w, "nexaroute_deployment_ewma_ttft_ms{deployment=%q,provider=%q} %.3f\n", id, provider, h.EWMATTFTMS)
		fmt.Fprintf(w, "nexaroute_deployment_ewma_failure_rate{deployment=%q,provider=%q} %.6f\n", id, provider, h.EWMAFailureRate)
		fmt.Fprintf(w, "nexaroute_deployment_successes_total{deployment=%q,provider=%q} %d\n", id, provider, h.Successes)
		fmt.Fprintf(w, "nexaroute_deployment_failures_total{deployment=%q,provider=%q} %d\n", id, provider, h.Failures)
	}

	fmt.Fprintln(w, "# HELP nexaroute_runtime_events_total Cumulative routing/probe/stream events.")
	fmt.Fprintln(w, "# TYPE nexaroute_runtime_events_total counter")
	counts := s.bus.Counts()
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(w, "nexaroute_runtime_events_total{kind=%q} %d\n", sanitizeMetricLabel(k), counts[k])
	}

	fmt.Fprintln(w, "# HELP nexaroute_errors_total Normalized runtime failures by fault type.")
	fmt.Fprintln(w, "# TYPE nexaroute_errors_total counter")
	errorCounts := s.bus.ErrorCounts()
	errorTypes := make([]string, 0, len(errorCounts))
	for k := range errorCounts {
		errorTypes = append(errorTypes, k)
	}
	sort.Strings(errorTypes)
	for _, k := range errorTypes {
		fmt.Fprintf(w, "nexaroute_errors_total{error_type=%q} %d\n", sanitizeMetricLabel(k), errorCounts[k])
	}

	fmt.Fprintln(w, "# HELP nexaroute_provider_active_requests Requests currently holding a provider concurrency slot.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_active_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_waiting_requests Requests waiting for a provider concurrency slot.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_waiting_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_concurrency_limit Configured provider concurrency limit.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_concurrency_limit gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_credentials_cooling Credentials currently quarantined by provider.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_credentials_cooling gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_request_limit Latest upstream request quota ceiling; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_request_limit gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_remaining_requests Latest raw upstream request quota remaining; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_remaining_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_reserved_requests Data-plane upstream requests currently reserved before fresh quota evidence.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_reserved_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_effective_remaining_requests Raw request quota minus in-flight local reservations; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_effective_remaining_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_token_limit Latest upstream token quota ceiling; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_token_limit gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_remaining_tokens Latest raw upstream token quota remaining; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_remaining_tokens gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_reserved_tokens Conservative token bounds reserved by in-flight data-plane upstream attempts.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_reserved_tokens gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_effective_remaining_tokens Raw token quota minus in-flight local reservations; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_effective_remaining_tokens gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_request_reset_unix Latest known upstream request-quota reset time as Unix seconds.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_request_reset_unix gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_token_reset_unix Latest known upstream token-quota reset time as Unix seconds.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_token_reset_unix gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_rate_limit_reset_unix Conservative latest known upstream quota reset time as Unix seconds.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_rate_limit_reset_unix gauge")
	stats := s.reg.Stats()
	sort.Slice(stats, func(i, j int) bool { return stats[i].ID < stats[j].ID })
	for _, st := range stats {
		id := sanitizeMetricLabel(st.ID)
		fmt.Fprintf(w, "nexaroute_provider_active_requests{provider=%q} %d\n", id, st.ActiveRequests)
		fmt.Fprintf(w, "nexaroute_provider_waiting_requests{provider=%q} %d\n", id, st.WaitingRequests)
		fmt.Fprintf(w, "nexaroute_provider_concurrency_limit{provider=%q} %d\n", id, st.MaxConcurrency)
		fmt.Fprintf(w, "nexaroute_provider_credentials_cooling{provider=%q} %d\n", id, st.CredentialsCooling)
		fmt.Fprintf(w, "nexaroute_provider_request_limit{provider=%q} %d\n", id, st.RequestLimit)
		fmt.Fprintf(w, "nexaroute_provider_remaining_requests{provider=%q} %d\n", id, st.RemainingRequests)
		fmt.Fprintf(w, "nexaroute_provider_reserved_requests{provider=%q} %d\n", id, st.ReservedRequests)
		fmt.Fprintf(w, "nexaroute_provider_effective_remaining_requests{provider=%q} %d\n", id, st.EffectiveRemainingRequests)
		fmt.Fprintf(w, "nexaroute_provider_token_limit{provider=%q} %d\n", id, st.TokenLimit)
		fmt.Fprintf(w, "nexaroute_provider_remaining_tokens{provider=%q} %d\n", id, st.RemainingTokens)
		fmt.Fprintf(w, "nexaroute_provider_reserved_tokens{provider=%q} %d\n", id, st.ReservedTokens)
		fmt.Fprintf(w, "nexaroute_provider_effective_remaining_tokens{provider=%q} %d\n", id, st.EffectiveRemainingTokens)
		fmt.Fprintf(w, "nexaroute_provider_request_reset_unix{provider=%q} %d\n", id, st.RequestResetUnix)
		fmt.Fprintf(w, "nexaroute_provider_token_reset_unix{provider=%q} %d\n", id, st.TokenResetUnix)
		fmt.Fprintf(w, "nexaroute_provider_rate_limit_reset_unix{provider=%q} %d\n", id, st.RateLimitResetUnix)
	}

	fmt.Fprintln(w, "# HELP nexaroute_provider_incident_state Provider-level circuit state (1 for current state).")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_incident_state gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_incident_evidence Distinct failing deployments observed inside the provider incident window.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_incident_evidence gauge")
	providerHealth := s.hm.ProviderSnapshot()
	sort.Slice(providerHealth, func(i, j int) bool { return providerHealth[i].Provider < providerHealth[j].Provider })
	for _, st := range providerHealth {
		id := sanitizeMetricLabel(st.Provider)
		fmt.Fprintf(w, "nexaroute_provider_incident_state{provider=%q,status=%q} 1\n", id, st.Status)
		fmt.Fprintf(w, "nexaroute_provider_incident_evidence{provider=%q} %d\n", id, st.Evidence)
	}

	// Exact-match response cache counters.
	cs := s.respCache.Stats()
	fmt.Fprintln(w, "# HELP nexaroute_cache_hits_total Exact-match response cache hits.")
	fmt.Fprintln(w, "# TYPE nexaroute_cache_hits_total counter")
	fmt.Fprintf(w, "nexaroute_cache_hits_total %d\n", cs.Hits)
	fmt.Fprintln(w, "# HELP nexaroute_cache_misses_total Exact-match response cache misses.")
	fmt.Fprintln(w, "# TYPE nexaroute_cache_misses_total counter")
	fmt.Fprintf(w, "nexaroute_cache_misses_total %d\n", cs.Misses)
	fmt.Fprintln(w, "# HELP nexaroute_cache_bypasses_total Requests ineligible for the response cache.")
	fmt.Fprintln(w, "# TYPE nexaroute_cache_bypasses_total counter")
	fmt.Fprintf(w, "nexaroute_cache_bypasses_total %d\n", cs.Bypasses)
	fmt.Fprintln(w, "# HELP nexaroute_cache_entries Currently cached exact-match responses.")
	fmt.Fprintln(w, "# TYPE nexaroute_cache_entries gauge")
	fmt.Fprintf(w, "nexaroute_cache_entries %d\n", cs.Entries)
	fmt.Fprintln(w, "# HELP nexaroute_cache_bytes Cached response bytes.")
	fmt.Fprintln(w, "# TYPE nexaroute_cache_bytes gauge")
	fmt.Fprintf(w, "nexaroute_cache_bytes %d\n", cs.BytesStored)

	// Cumulative token accounting per deployment and estimated spend.
	usageSnap := s.usageSnapshotWithPrices(s.currentConfig())
	fmt.Fprintln(w, "# HELP nexaroute_deployment_tokens_total Cumulative upstream-reported tokens by deployment and kind.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_tokens_total counter")
	for _, row := range usageSnap.ByDeployment {
		id := sanitizeMetricLabel(row.Deployment)
		fmt.Fprintf(w, "nexaroute_deployment_tokens_total{deployment=%q,kind=%q} %d\n", id, "prompt", row.PromptTokens)
		fmt.Fprintf(w, "nexaroute_deployment_tokens_total{deployment=%q,kind=%q} %d\n", id, "completion", row.CompletionTok)
	}
	fmt.Fprintln(w, "# HELP nexaroute_estimated_cost_usd_total Cumulative estimated spend in USD from configured per-model pricing.")
	fmt.Fprintln(w, "# TYPE nexaroute_estimated_cost_usd_total counter")
	fmt.Fprintf(w, "nexaroute_estimated_cost_usd_total %.6f\n", usageSnap.TotalEstimatedCostUSD)

	// Phase C — task classification metrics (bounded cardinality)
	fmt.Fprintln(w, "# HELP nexaroute_task_classifications_total Requests classified by task type and complexity.")
	fmt.Fprintln(w, "# TYPE nexaroute_task_classifications_total counter")
	taskCounts := s.taskClassificationSnapshot()
	// Sort keys for deterministic output
	keys := make([]string, 0, len(taskCounts))
	for k := range taskCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, "|", 2)
		if len(parts) != 2 {
			continue
		}
		taskType := sanitizeMetricLabel(parts[0])
		complexity := sanitizeMetricLabel(parts[1])
		fmt.Fprintf(w, "nexaroute_task_classifications_total{task_type=%q,complexity=%q} %d\n", taskType, complexity, taskCounts[k])
	}
	fmt.Fprintln(w, "# HELP nexaroute_task_analysis_total Total task analysis attempts.")
	fmt.Fprintln(w, "# TYPE nexaroute_task_analysis_total counter")
	fmt.Fprintf(w, "nexaroute_task_analysis_total %d\n", s.taskAnalysisTotal.Load())

	// Phase D — decision plane metrics (bounded cardinality)
	s.runtimeMu.RLock()
	decisionMetrics := map[string]int64{}
	if s.decisionOrchestrator != nil {
		decisionMetrics = s.decisionOrchestrator.MetricsSnapshot()
	}
	s.runtimeMu.RUnlock()
	fmt.Fprintln(w, "# HELP nexaroute_decision_total Decision plane executions by outcome.")
	fmt.Fprintln(w, "# TYPE nexaroute_decision_total counter")
	for k, v := range decisionMetrics {
		if k == "latency_avg_ms" || k == "latency_count" || strings.HasPrefix(k, "external_") {
			continue
		}
		fmt.Fprintf(w, "nexaroute_decision_total{outcome=%q} %d\n", sanitizeMetricLabel(k), v)
	}
	fmt.Fprintln(w, "# HELP nexaroute_decision_latency_avg_ms Average decision latency ms.")
	fmt.Fprintln(w, "# TYPE nexaroute_decision_latency_avg_ms gauge")
	if avg, ok := decisionMetrics["latency_avg_ms"]; ok {
		fmt.Fprintf(w, "nexaroute_decision_latency_avg_ms %d\n", avg)
	} else {
		fmt.Fprintln(w, "nexaroute_decision_latency_avg_ms 0")
	}
	fmt.Fprintln(w, "# HELP nexaroute_decision_latency_count Total decision latency samples.")
	fmt.Fprintln(w, "# TYPE nexaroute_decision_latency_count counter")
	if cnt, ok := decisionMetrics["latency_count"]; ok {
		fmt.Fprintf(w, "nexaroute_decision_latency_count %d\n", cnt)
	} else {
		fmt.Fprintln(w, "nexaroute_decision_latency_count 0")
	}

	// Phase F: external decision provider metrics (bounded labels)
	fmt.Fprintln(w, "# HELP nexaroute_external_decision_requests_total External decision requests by type and outcome.")
	fmt.Fprintln(w, "# TYPE nexaroute_external_decision_requests_total counter")
	outcomes := []struct {
		key     string
		outcome string
	}{
		{"external_selected", "selected"},
		{"external_error", "error"},
		{"external_timeout", "timeout"},
		{"external_invalid", "invalid"},
		{"external_unavailable", "unavailable"},
		{"external_request_too_large", "request_too_large"},
		{"external_response_too_large", "response_too_large"},
	}
	for _, o := range outcomes {
		if v, ok := decisionMetrics[o.key]; ok && v > 0 {
			fmt.Fprintf(w, "nexaroute_external_decision_requests_total{type=\"jev\",outcome=%q} %d\n", o.outcome, v)
		}
	}
	if total, ok := decisionMetrics["external_total"]; ok {
		fmt.Fprintf(w, "nexaroute_external_decision_requests_total{type=\"jev\",outcome=\"total\"} %d\n", total)
	}
	fmt.Fprintln(w, "# HELP nexaroute_external_decision_latency_seconds External decision latency.")
	fmt.Fprintln(w, "# TYPE nexaroute_external_decision_latency_seconds gauge")
	if avg, ok := decisionMetrics["external_latency_avg_ms"]; ok {
		fmt.Fprintf(w, "nexaroute_external_decision_latency_seconds{type=\"jev\"} %.3f\n", float64(avg)/1000.0)
	} else {
		fmt.Fprintf(w, "nexaroute_external_decision_latency_seconds{type=\"jev\"} 0\n")
	}
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	routingCfg, _ := s.runtimeSettingsSnapshot()
	total, usable := s.rt.Readiness(routingCfg.Strategy)
	if total == 0 {
		errorJSON(w, 503, "no enabled model deployments")
		return
	}
	if usable == 0 {
		if router.IsReadyStrategy(routingCfg.Strategy) {
			errorJSON(w, 503, "no verified healthy model deployments in ready queue")
		} else {
			errorJSON(w, 503, "no usable model deployments")
		}
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "deployments": total, "usable": usable})
}

func sanitizeMetricLabel(s string) string {
	r := strings.NewReplacer("\\", "_", "\"", "_", "\n", "_", "\r", "_")
	return r.Replace(s)
}
