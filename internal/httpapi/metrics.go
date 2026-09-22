package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/universal-llm-gateway/internal/health"
)

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ds := s.rt.All()
	hs := s.hm.Snapshot()
	by := map[health.Status]int{}
	latSum := 0.0
	latN := 0
	for _, h := range hs {
		by[h.Status]++
		if h.EWMALatencyMS > 0 {
			latSum += h.EWMALatencyMS
			latN++
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP ulg_deployments_total Configured enabled model deployments.")
	fmt.Fprintln(w, "# TYPE ulg_deployments_total gauge")
	fmt.Fprintf(w, "ulg_deployments_total %d\n", len(ds))
	fmt.Fprintln(w, "# HELP ulg_health_deployments Deployment count by health status.")
	fmt.Fprintln(w, "# TYPE ulg_health_deployments gauge")
	for _, st := range []health.Status{health.Unknown, health.Healthy, health.Degraded, health.HalfOpen, health.Cooldown} {
		fmt.Fprintf(w, "ulg_health_deployments{status=%q} %d\n", st, by[st])
	}
	if latN > 0 {
		fmt.Fprintln(w, "# HELP ulg_ewma_latency_ms Average EWMA upstream latency across observed deployments.")
		fmt.Fprintln(w, "# TYPE ulg_ewma_latency_ms gauge")
		fmt.Fprintf(w, "ulg_ewma_latency_ms %.3f\n", latSum/float64(latN))
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
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "_"), "\"", "_")
}
