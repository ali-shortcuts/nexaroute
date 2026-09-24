package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

type routePreviewRequest struct {
	Model      string `json:"model"`
	Tools      bool   `json:"tools"`
	Vision     bool   `json:"vision"`
	Streaming  bool   `json:"streaming"`
	Reasoning  bool   `json:"reasoning"`
	SessionKey string `json:"session_key"`
}

type routePreviewCandidate struct {
	DeploymentID string   `json:"deployment_id"`
	ProviderID   string   `json:"provider_id"`
	ProviderName string   `json:"provider_name,omitempty"`
	ProviderType string   `json:"provider_type"`
	Model        string   `json:"model"`
	Priority     int      `json:"priority"`
	Weight       float64  `json:"weight"`
	Score        float64  `json:"score"`
	Health       string   `json:"health"`
	EWMA         float64  `json:"ewma_latency_ms"`
	Pressure     float64  `json:"capacity_pressure"`
	Pinned       bool     `json:"session_pinned"`
	Reasons      []string `json:"reasons,omitempty"`
}

// adminRoutePreview explains, for one hypothetical request, which deployment
// the active routing plane would choose right now and why. Read-only: no
// health state changes, no upstream calls, no secrets in the response.
func (s *Server) adminRoutePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in routePreviewRequest
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) == "" {
		errorJSON(w, 400, "model is required")
		return
	}
	req := router.Requirement{Model: in.Model, Tools: in.Tools, Vision: in.Vision, Streaming: in.Streaming, Reasoning: in.Reasoning}
	req = s.prepareRequirement(req, r, "")
	// A preview is hypothetical, so an explicit session key in the request
	// body wins over whatever the admin client's own headers would derive.
	if sk := strings.TrimSpace(in.SessionKey); sk != "" {
		req.SessionKey = sk
	}
	cfg, candidates := s.routeSnapshot(req)

	pinned := s.rt.PinnedDeployment(req)
	out := make([]routePreviewCandidate, 0, len(candidates))
	for _, c := range candidates {
		pc := routePreviewCandidate{
			DeploymentID: c.Deployment.ID,
			ProviderID:   c.Deployment.ProviderID,
			ProviderName: c.Deployment.ProviderName,
			ProviderType: c.Deployment.ProviderType,
			Model:        c.Deployment.Model,
			Priority:     c.Deployment.Priority,
			Weight:       c.Deployment.Weight,
			Score:        round2(c.Score),
			Health:       string(c.Health.Status),
			EWMA:         round2(c.Health.EWMALatencyMS),
			Pressure:     round2(c.CapacityPressure),
			Pinned:       pinned != "" && pinned == c.Deployment.ID,
		}
		pc.Reasons = s.previewReasons(c, pinned, len(candidates))
		out = append(out, pc)
		if len(out) >= 25 {
			break
		}
	}

	notes := []string{}
	if req.Reasoning {
		notes = append(notes, "reasoning requests are restricted to deployments that advertise reasoning capability from real protocol fields")
	}
	if len(candidates) == 0 {
		if _, usable := s.rt.Readiness(cfg.Routing.Strategy); usable == 0 {
			notes = append(notes, "no verified-healthy deployments exist at all; probe or recover providers first")
		} else {
			notes = append(notes, "no deployment matches this model/capability combination; check aliases, capabilities and enabled flags")
		}
	}
	s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_preview", Message: fmt.Sprintf("model=%s candidates=%d", in.Model, len(candidates))})
	writeJSON(w, 200, map[string]any{
		"model":            in.Model,
		"strategy":         cfg.Routing.Strategy,
		"fallback_enabled": cfg.Routing.FallbackOnUnknownModel,
		"max_attempts":     cfg.Routing.MaxAttempts,
		"session_pinned":   pinned,
		"candidate_count":  len(candidates),
		"candidates":       out,
		"notes":            notes,
	})
}

func (s *Server) previewReasons(c router.Scored, pinned string, total int) []string {
	reasons := []string{fmt.Sprintf("eligible and verified healthy (eligible candidates: %d)", total)}
	if pinned != "" && pinned == c.Deployment.ID {
		reasons = append(reasons, "session affinity pins this deployment for this session/model/capability bucket")
	}
	reasons = append(reasons, fmt.Sprintf("priority tier %d", c.Deployment.Priority))
	if !c.Health.LastSuccess.IsZero() {
		reasons = append(reasons, fmt.Sprintf("health proof last refreshed %s ago (%s)", time.Since(c.Health.LastSuccess).Round(time.Second), c.Health.LastSuccess.Format("15:04:05")))
	}
	if c.Health.EWMALatencyMS > 0 {
		reasons = append(reasons, fmt.Sprintf("observed latency %.0f ms", c.Health.EWMALatencyMS))
	} else {
		reasons = append(reasons, "no latency evidence yet")
	}
	if c.CapacityPressure > 0 {
		reasons = append(reasons, fmt.Sprintf("provider capacity pressure %.0f%%", c.CapacityPressure*100))
	} else {
		reasons = append(reasons, "provider has idle capacity")
	}
	reasons = append(reasons, fmt.Sprintf("evidence score %.2f (health, weight, latency, failure history, pressure)", c.Score))
	return reasons
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
