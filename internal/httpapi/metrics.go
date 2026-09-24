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

	usageRows := s.usage.Snapshot()
	fmt.Fprintln(w, "# HELP nexaroute_usage_exact_requests_total Routed upstream responses with exact token usage reported.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_exact_requests_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_usage_unknown_requests_total Routed upstream responses without complete exact token usage.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_unknown_requests_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_usage_input_tokens_total Exact upstream input/prompt tokens reported.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_input_tokens_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_usage_output_tokens_total Exact upstream output/completion tokens reported.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_output_tokens_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_usage_cache_read_input_tokens_total Exact cache-read input tokens reported by upstreams.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_cache_read_input_tokens_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_usage_cache_creation_input_tokens_total Exact cache-creation input tokens reported by upstreams.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_cache_creation_input_tokens_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_usage_reasoning_tokens_total Exact reasoning tokens reported by OpenAI-compatible upstreams.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_reasoning_tokens_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_estimated_cost_usd_total Cost estimate from exact usage and configured base input/output prices.")
	fmt.Fprintln(w, "# TYPE nexaroute_estimated_cost_usd_total counter")
	for _, st := range usageRows {
		id := sanitizeMetricLabel(st.Deployment)
		provider := sanitizeMetricLabel(st.Provider)
		fmt.Fprintf(w, "nexaroute_usage_exact_requests_total{deployment=%q,provider=%q} %d\n", id, provider, st.ExactRequests)
		fmt.Fprintf(w, "nexaroute_usage_unknown_requests_total{deployment=%q,provider=%q} %d\n", id, provider, st.UnknownRequests)
		fmt.Fprintf(w, "nexaroute_usage_input_tokens_total{deployment=%q,provider=%q} %d\n", id, provider, st.InputTokens)
		fmt.Fprintf(w, "nexaroute_usage_output_tokens_total{deployment=%q,provider=%q} %d\n", id, provider, st.OutputTokens)
		fmt.Fprintf(w, "nexaroute_usage_cache_read_input_tokens_total{deployment=%q,provider=%q} %d\n", id, provider, st.CacheReadInputTokens)
		fmt.Fprintf(w, "nexaroute_usage_cache_creation_input_tokens_total{deployment=%q,provider=%q} %d\n", id, provider, st.CacheCreationInputTokens)
		fmt.Fprintf(w, "nexaroute_usage_reasoning_tokens_total{deployment=%q,provider=%q} %d\n", id, provider, st.ReasoningTokens)
		fmt.Fprintf(w, "nexaroute_estimated_cost_usd_total{deployment=%q,provider=%q} %.9f\n", id, provider, st.EstimatedCostUSD)
	}
	usageTotal := s.usage.Total()
	coverageDenom := usageTotal.ExactRequests + usageTotal.UnknownRequests
	coverage := 0.0
	if coverageDenom > 0 {
		coverage = float64(usageTotal.ExactRequests) / float64(coverageDenom)
	}
	pricedCoverage := 0.0
	if usageTotal.ExactRequests > 0 {
		pricedCoverage = float64(usageTotal.PricedRequests) / float64(usageTotal.ExactRequests)
	}
	fmt.Fprintln(w, "# HELP nexaroute_usage_exact_coverage_ratio Fraction of observed routed upstream responses carrying exact usage.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_exact_coverage_ratio gauge")
	fmt.Fprintf(w, "nexaroute_usage_exact_coverage_ratio %.6f\n", coverage)
	fmt.Fprintln(w, "# HELP nexaroute_usage_priced_coverage_ratio Fraction of exact-usage responses with configured model pricing.")
	fmt.Fprintln(w, "# TYPE nexaroute_usage_priced_coverage_ratio gauge")
	fmt.Fprintf(w, "nexaroute_usage_priced_coverage_ratio %.6f\n", pricedCoverage)

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
	fmt.Fprintln(w, "# HELP nexaroute_provider_remaining_requests Latest upstream request quota remaining; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_remaining_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_remaining_tokens Latest upstream token quota remaining; -1 means unknown.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_remaining_tokens gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_rate_limit_reset_unix Latest known upstream quota reset time as Unix seconds.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_rate_limit_reset_unix gauge")
	stats := s.reg.Stats()
	sort.Slice(stats, func(i, j int) bool { return stats[i].ID < stats[j].ID })
	for _, st := range stats {
		id := sanitizeMetricLabel(st.ID)
		fmt.Fprintf(w, "nexaroute_provider_active_requests{provider=%q} %d\n", id, st.ActiveRequests)
		fmt.Fprintf(w, "nexaroute_provider_waiting_requests{provider=%q} %d\n", id, st.WaitingRequests)
		fmt.Fprintf(w, "nexaroute_provider_concurrency_limit{provider=%q} %d\n", id, st.MaxConcurrency)
		fmt.Fprintf(w, "nexaroute_provider_credentials_cooling{provider=%q} %d\n", id, st.CredentialsCooling)
		fmt.Fprintf(w, "nexaroute_provider_remaining_requests{provider=%q} %d\n", id, st.RemainingRequests)
		fmt.Fprintf(w, "nexaroute_provider_remaining_tokens{provider=%q} %d\n", id, st.RemainingTokens)
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
