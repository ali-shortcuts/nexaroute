package httpapi

import (
	"encoding/json"
	"net/http"
)

func (s *Server) adminConfigHistory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.applyMu.Lock()
		available := len(s.configHistory)
		s.applyMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"revision": s.currentConfigRevision(), "available_rollbacks": available})
	case http.MethodPost:
		var in struct {
			Steps            int    `json:"steps"`
			ExpectedRevision uint64 `json:"expected_revision"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if in.Steps < 1 {
			in.Steps = 1
		}
		if in.Steps > len(s.configHistory) {
			errorJSON(w, 409, "requested rollback is not available")
			return
		}
		s.applyMu.Lock()
		defer s.applyMu.Unlock()
		if in.ExpectedRevision != 0 && in.ExpectedRevision != s.currentConfigRevision() {
			errorJSON(w, http.StatusPreconditionFailed, "config revision changed; re-read history")
			return
		}
		cfg := cloneConfig(s.configHistory[len(s.configHistory)-in.Steps])
		if err := s.applyConfigLocked(cfg); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		s.configHistory = s.configHistory[:len(s.configHistory)-in.Steps]
		rev := s.configRevision.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"rolled_back": true, "revision": rev, "steps": in.Steps})
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
