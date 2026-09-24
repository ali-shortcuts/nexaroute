package httpapi

import (
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
)

// adminCompatMatrix serves GET /admin/api/compat: the full deployment
// compatibility matrix (spec section 20/23) - health, protocol, capability
// contract with evidence, Claude Code scorecard, last repair and last issue.
func (s *Server) adminCompatMatrix(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	type row struct {
		Deployment   string           `json:"deployment"`
		ProviderID   string           `json:"provider_id"`
		Model        string           `json:"model"`
		Health       string           `json:"health"`
		Scorecard    compat.Scorecard `json:"scorecard"`
		Contract     compat.Contract  `json:"contract"`
		LastIssue    string           `json:"last_compatibility_issue,omitempty"`
		LastRepair   string           `json:"last_repair,omitempty"`
		ProviderType string           `json:"provider_type,omitempty"`
	}
	contracts := s.capStore.Snapshot()
	rows := make([]row, 0, 64)
	for _, d := range s.rt.All() {
		contract := contracts[d.ID]
		healthState := s.hm.Get(d.ID)
		sc := contract.Scorecard(string(healthState.Status))
		if contract.Capabilities.NativeProtocol == "" {
			sc.Protocol = s.dialectFor(d.ProviderID, d.ProviderType, "").Protocol
		}
		rows = append(rows, row{
			Deployment:   d.ID,
			ProviderID:   d.ProviderID,
			Model:        d.Model,
			Health:       string(healthState.Status),
			Scorecard:    sc,
			Contract:     contract,
			LastIssue:    contract.LastCompatibilityIssue,
			LastRepair:   contract.LastRepair,
			ProviderType: d.ProviderType,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": rows, "count": len(rows)})
}

// adminCompatReset serves POST /admin/api/compat/reset: drops the stored
// capability contract for one deployment (all deployments when deployment is
// "all"), forcing fresh probes on the next sweep.
func (s *Server) adminCompatReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in struct {
		Deployment string `json:"deployment"`
	}
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	deployment := strings.TrimSpace(in.Deployment)
	if deployment == "" || deployment == "all" {
		s.capStore.Reset()
		s.syncCapabilityContracts(s.currentConfig())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reset": "all"})
		return
	}
	s.capStore.Drop(deployment)
	s.syncCapabilityContracts(s.currentConfig())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reset": deployment})
}
