package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/health"
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
	fmt.Fprintln(w, "# HELP nexaroute_deployment_successes_total Successful observations by deployment.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_successes_total counter")
	fmt.Fprintln(w, "# HELP nexaroute_deployment_failures_total Failed observations by deployment.")
	fmt.Fprintln(w, "# TYPE nexaroute_deployment_failures_total counter")
	for _, d := range ds {
		h := healthByID[d.ID]
		id := sanitizeMetricLabel(d.ID)
		provider := sanitizeMetricLabel(d.ProviderID)
		fmt.Fprintf(w, "nexaroute_deployment_ewma_latency_ms{deployment=%q,provider=%q} %.3f\n", id, provider, h.EWMALatencyMS)
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

	fmt.Fprintln(w, "# HELP nexaroute_provider_active_requests Requests currently holding a provider concurrency slot.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_active_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_waiting_requests Requests waiting for a provider concurrency slot.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_waiting_requests gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_concurrency_limit Configured provider concurrency limit.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_concurrency_limit gauge")
	fmt.Fprintln(w, "# HELP nexaroute_provider_credentials_cooling Credentials currently quarantined by provider.")
	fmt.Fprintln(w, "# TYPE nexaroute_provider_credentials_cooling gauge")
	stats := s.reg.Stats()
	sort.Slice(stats, func(i, j int) bool { return stats[i].ID < stats[j].ID })
	for _, st := range stats {
		id := sanitizeMetricLabel(st.ID)
		fmt.Fprintf(w, "nexaroute_provider_active_requests{provider=%q} %d\n", id, st.ActiveRequests)
		fmt.Fprintf(w, "nexaroute_provider_waiting_requests{provider=%q} %d\n", id, st.WaitingRequests)
		fmt.Fprintf(w, "nexaroute_provider_concurrency_limit{provider=%q} %d\n", id, st.MaxConcurrency)
		fmt.Fprintf(w, "nexaroute_provider_credentials_cooling{provider=%q} %d\n", id, st.CredentialsCooling)
	}
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	ds := s.rt.All()
	if len(ds) == 0 {
		errorJSON(w, 503, "no enabled model deployments")
		return
	}
	usable := 0
	for _, d := range ds {
		if s.hm.Get(d.ID).Status != health.Cooldown {
			usable++
		}
	}
	if usable == 0 {
		errorJSON(w, 503, "all model deployments are in cooldown")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "deployments": len(ds), "usable": usable})
}

func sanitizeMetricLabel(s string) string {
	r := strings.NewReplacer("\\", "_", "\"", "_", "\n", "_", "\r", "_")
	return r.Replace(s)
}
