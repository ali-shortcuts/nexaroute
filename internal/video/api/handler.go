package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type Handler struct {
	Orch        *orchestrator.Orchestrator
	Store       queue.JobStore
	Providers   orchestrator.ProviderRegistry
	RequireAuth bool
	BearerToken string
}

func (h *Handler) authorized(r *http.Request) bool {
	if !h.RequireAuth {
		return true
	}
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(raw, prefix))
	return len(got) == len(h.BearerToken) && subtle.ConstantTimeCompare([]byte(got), []byte(h.BearerToken)) == 1
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required", false)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/v1/video/jobs" {
		h.create(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/video/jobs/") {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 4 {
			id := parts[3]
			if r.Method == "GET" {
				h.get(w, r, id)
				return
			}
			if r.Method == "POST" && len(parts) == 5 && parts[4] == "cancel" {
				h.cancel(w, r, id)
				return
			}
		}
	}
	if r.Method == "GET" && r.URL.Path == "/v1/video/providers" {
		h.providers(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "route not found", false)
}
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req video.VideoRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeError(w, 400, "invalid_json", "request body is not valid JSON", false)
		return
	}
	j, err := h.Orch.Create(r.Context(), req)
	if err != nil {
		writeError(w, 400, "video_create_failed", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}
func (h *Handler) get(w http.ResponseWriter, r *http.Request, id string) {
	j, err := h.Store.Get(r.Context(), id)
	if err != nil {
		writeError(w, 404, "not_found", "job not found", false)
		return
	}
	writeJSON(w, 200, j)
}
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.Orch.Cancel(r.Context(), id); err != nil {
		writeError(w, 400, "cancel_failed", err.Error(), false)
		return
	}
	h.get(w, r, id)
}
func (h *Handler) providers(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID string `json:"id"`
	}
	out := []item{}
	if reg, ok := h.Providers.(orchestrator.Registry); ok {
		for id := range reg {
			out = append(out, item{ID: id})
		}
	}
	writeJSON(w, 200, map[string]any{"providers": out})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	writeJSON(w, status, map[string]any{"code": code, "message": msg, "retryable": retryable})
}
