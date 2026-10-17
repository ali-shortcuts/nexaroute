package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type Handler struct {
	Orch        *orchestrator.Orchestrator
	Store       queue.JobStore
	Providers   orchestrator.ProviderRegistry
	Assets      video.AssetSink
	RequireAuth bool
	BearerToken string
	// Universal, when set, enables POST /v1/video/universal/plan: plan
	// generation with any configured text/coding model for review-first
	// workflows in Video Studio. Nil disables the endpoint (404 via the
	// generic route handling, reported as video_unavailable).
	Universal            *agent.Agent
	UniversalMaxDuration float64
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
	if r.Method == http.MethodPost && r.URL.Path == "/v1/video/jobs" {
		h.create(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/video/providers" {
		h.providers(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/video/universal/plan" {
		h.universalPlan(w, r)
		return
	}
	parts, ok := requestPathParts(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_path", "request path is invalid", false)
		return
	}
	if len(parts) >= 4 && parts[0] == "v1" && parts[1] == "video" && parts[2] == "jobs" {
		id := parts[3]
		if len(parts) == 4 && r.Method == http.MethodGet {
			h.get(w, r, id)
			return
		}
		if len(parts) == 5 && parts[4] == "cancel" && r.Method == http.MethodPost {
			h.cancel(w, r, id)
			return
		}
		if len(parts) == 6 && parts[4] == "assets" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			h.getAsset(w, r, id, parts[5])
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "route not found", false)
}

func requestPathParts(r *http.Request) ([]string, bool) {
	raw := strings.Trim(r.URL.EscapedPath(), "/")
	if raw == "" {
		return nil, true
	}
	encoded := strings.Split(raw, "/")
	parts := make([]string, len(encoded))
	for i, part := range encoded {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, false
		}
		parts[i] = decoded
	}
	return parts, true
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	if h.Orch == nil {
		writeError(w, http.StatusServiceUnavailable, "video_unavailable", "video orchestration is not configured", true)
		return
	}
	defer r.Body.Close()
	var req video.VideoRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON", false)
		return
	}
	j, err := h.Orch.Create(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "video_create_failed", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusAccepted, publicJob(j))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, id string) {
	if h.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "video_unavailable", "video job store is not configured", true)
		return
	}
	j, err := h.Store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "job not found", false)
		return
	}
	writeJSON(w, http.StatusOK, publicJob(j))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request, id string) {
	if h.Orch == nil {
		writeError(w, http.StatusServiceUnavailable, "video_unavailable", "video orchestration is not configured", true)
		return
	}
	if err := h.Orch.Cancel(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, "cancel_failed", err.Error(), false)
		return
	}
	h.get(w, r, id)
}

// ForceFail is intentionally not routed by ServeHTTP. The gateway's admin
// control plane invokes it only after admin authorization, CSRF validation,
// durable audit intent recording, and explicit operator confirmation.
func (h *Handler) ForceFail(ctx context.Context, id string) (attempted, confirmed bool, err error) {
	if h == nil || h.Orch == nil {
		return false, false, fmt.Errorf("video orchestration is not configured")
	}
	return h.Orch.ForceFail(ctx, id)
}

func (h *Handler) getAsset(w http.ResponseWriter, r *http.Request, jobID, assetID string) {
	if h.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "video_unavailable", "video job store is not configured", true)
		return
	}
	job, err := h.Store.Get(r.Context(), jobID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "job not found", false)
		return
	}
	var asset video.Asset
	found := false
	for _, candidate := range job.OutputAssets {
		if candidate.ID == assetID {
			asset, found = candidate, true
			break
		}
	}
	if !found {
		for _, candidate := range job.SourceAssets {
			if candidate.ID == assetID {
				asset, found = candidate, true
				break
			}
		}
	}
	if !found || strings.TrimSpace(asset.URI) == "" {
		writeError(w, http.StatusNotFound, "asset_not_found", "asset not found", false)
		return
	}
	opener, ok := h.Assets.(interface {
		Open(video.Asset) (*os.File, error)
	})
	if !ok {
		writeError(w, http.StatusNotImplemented, "asset_download_unavailable", "asset downloads are not configured", false)
		return
	}
	file, err := opener.Open(asset)
	if err != nil {
		writeError(w, http.StatusNotFound, "asset_not_found", "asset not found", false)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || asset.SizeBytes <= 0 || info.Size() != asset.SizeBytes {
		writeError(w, http.StatusInternalServerError, "asset_integrity_error", "stored asset metadata does not match the file", false)
		return
	}
	contentType := "application/octet-stream"
	if parsed, _, parseErr := mime.ParseMediaType(strings.TrimSpace(asset.ContentType)); parseErr == nil {
		contentType = parsed
	}
	filename := strings.ReplaceAll(strings.TrimSpace(asset.Filename), "\\", "/")
	filename = filepath.Base(filename)
	if filename == "" || filename == "." || filename == ".." {
		filename = asset.ID
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = `attachment; filename="video-asset.bin"`
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filename, time.Time{}, file)
}

func (h *Handler) providers(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID           string              `json:"id"`
		Capabilities *video.Capabilities `json:"capabilities,omitempty"`
	}
	out := []item{}
	if reg, ok := h.Providers.(orchestrator.Registry); ok {
		ids := make([]string, 0, len(reg))
		for id := range reg {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			entry := item{ID: id}
			if caps, err := reg[id].Capabilities(r.Context()); err == nil {
				c := caps
				entry.Capabilities = &c
			}
			out = append(out, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// universalPlanRequest is the body of POST /v1/video/universal/plan.
type universalPlanRequest struct {
	Prompt          string  `json:"prompt"`
	Model           string  `json:"model"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	AspectRatio     string  `json:"aspect_ratio,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	FPS             int     `json:"fps,omitempty"`
	Language        string  `json:"language,omitempty"`
}

// universalPlan generates (but does not execute) a validated video plan
// with the requested configured text/coding model, so Video Studio users
// can review and edit the plan before submitting it as a job.
func (h *Handler) universalPlan(w http.ResponseWriter, r *http.Request) {
	if h.Universal == nil {
		writeError(w, http.StatusServiceUnavailable, "video_unavailable", "universal video planning is not configured", true)
		return
	}
	defer r.Body.Close()
	var req universalPlanRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON", false)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" || strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_plan_request", "prompt and model are required", false)
		return
	}
	width, height := req.Width, req.Height
	if width == 0 && height == 0 {
		switch req.AspectRatio {
		case "9:16":
			width, height = 720, 1280
		case "1:1":
			width, height = 960, 960
		default:
			width, height = 1280, 720
		}
	}
	res, err := h.Universal.GeneratePlan(r.Context(), agent.Request{
		Brief: req.Prompt, Model: req.Model, DurationSeconds: req.DurationSeconds,
		Width: width, Height: height, FPS: req.FPS, AspectRatio: req.AspectRatio,
		Language: req.Language, MaxDuration: h.UniversalMaxDuration,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "plan_failed", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plan": res.Plan, "model": res.Model, "attempts": res.Attempts,
		"creation_mode": "programmatic",
		"notice":        "This plan was authored by a text/coding model and will be executed by NexaRoute's deterministic renderer. It is not native generative video.",
	})
}

func publicJob(job video.VideoJob) video.VideoJob {
	job.OutputAssets = publicAssets(job.JobID, job.OutputAssets)
	job.SourceAssets = publicAssets(job.JobID, job.SourceAssets)
	if job.NarrationAsset != nil {
		asset := *job.NarrationAsset
		asset.URI = publicAssetURI(job.JobID, asset.ID)
		job.NarrationAsset = &asset
	}
	if job.SubtitleAsset != nil {
		asset := *job.SubtitleAsset
		asset.URI = publicAssetURI(job.JobID, asset.ID)
		job.SubtitleAsset = &asset
	}
	job.Request.ShotPlan = append([]video.Shot(nil), job.Request.ShotPlan...)
	for i := range job.Request.ShotPlan {
		if job.Request.ShotPlan[i].OutputAsset != nil {
			asset := *job.Request.ShotPlan[i].OutputAsset
			asset.URI = publicAssetURI(job.JobID, asset.ID)
			job.Request.ShotPlan[i].OutputAsset = &asset
		}
	}
	return job
}

func publicAssets(jobID string, assets []video.Asset) []video.Asset {
	if len(assets) == 0 {
		return assets
	}
	out := append([]video.Asset(nil), assets...)
	for i := range out {
		out[i].URI = publicAssetURI(jobID, out[i].ID)
	}
	return out
}

func publicAssetURI(jobID, assetID string) string {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(assetID) == "" {
		return ""
	}
	return fmt.Sprintf("/v1/video/jobs/%s/assets/%s", url.PathEscape(jobID), url.PathEscape(assetID))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	writeJSON(w, status, map[string]any{"code": code, "message": msg, "retryable": retryable})
}
