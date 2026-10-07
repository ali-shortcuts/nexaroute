package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/events"
)

func (s *Server) adminDrain(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"draining": s.draining.Load(), "inflight": s.inflight.Load()})
	case http.MethodPost:
		var in struct {
			Draining *bool `json:"draining"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Draining == nil {
			errorJSON(w, http.StatusBadRequest, "draining boolean is required")
			return
		}
		s.draining.Store(*in.Draining)
		s.bus.Add(events.Event{Kind: "gateway_draining_changed", Message: map[bool]string{true: "draining enabled", false: "draining disabled"}[*in.Draining]})
		writeJSON(w, http.StatusOK, map[string]any{"draining": *in.Draining, "inflight": s.inflight.Load()})
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
