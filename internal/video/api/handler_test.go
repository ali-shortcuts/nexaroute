package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

func newTestHandler(requireAuth bool) *Handler {
	store := queue.NewMemoryStore()
	q := queue.New(4)
	provider := providers.NewFake(1)
	orch := &orchestrator.Orchestrator{
		Store:     store,
		Queue:     q,
		Providers: orchestrator.Registry{"fake": provider},
	}
	return &Handler{
		Orch:        orch,
		Store:       store,
		Providers:   orchestrator.Registry{"fake": provider},
		RequireAuth: requireAuth,
		BearerToken: "test-token",
	}
}

func validAPIRequest() video.VideoRequest {
	return video.VideoRequest{
		ProjectID:          "project-1",
		Prompt:             "A sunrise over a quiet ocean",
		Mode:               video.ModeTextToVideo,
		ProviderPreference: "fake",
		ModelPreference:    "fake-video-1",
		DurationSeconds:    4,
		AspectRatio:        video.Aspect16x9,
	}
}

func performJSON(h http.Handler, method, path string, body any, authorization string) *httptest.ResponseRecorder {
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		payload = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, payload)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response %q: %v", w.Body.String(), err)
	}
	return body
}

func TestHandlerAuthenticationRoutingAndProviders(t *testing.T) {
	h := newTestHandler(true)

	for _, auth := range []string{"", "Token test-token", "Bearer wrong"} {
		w := performJSON(h, http.MethodGet, "/v1/video/providers", nil, auth)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("authorization %q: status=%d, want %d", auth, w.Code, http.StatusUnauthorized)
		}
		body := errorBody(t, w)
		if body["code"] != "unauthorized" || body["retryable"] != false {
			t.Errorf("authorization %q: error body=%v", auth, body)
		}
	}

	w := performJSON(h, http.MethodGet, "/v1/video/providers", nil, "Bearer test-token")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("providers response: status=%d content-type=%q body=%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	var providersBody struct {
		Providers []struct {
			ID string `json:"id"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &providersBody); err != nil {
		t.Fatal(err)
	}
	if len(providersBody.Providers) != 1 || providersBody.Providers[0].ID != "fake" {
		t.Fatalf("providers=%v, want one fake provider", providersBody.Providers)
	}

	unknown := performJSON(h, http.MethodGet, "/v1/video/unknown", nil, "Bearer test-token")
	if unknown.Code != http.StatusNotFound || errorBody(t, unknown)["code"] != "not_found" {
		t.Fatalf("unknown route: status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	// A non-Registry implementation is still a valid ProviderRegistry; the
	// endpoint must return an empty list rather than assuming a concrete type.
	h.Providers = providerRegistryStub{}
	empty := performJSON(h, http.MethodGet, "/v1/video/providers", nil, "Bearer test-token")
	if empty.Code != http.StatusOK {
		t.Fatalf("non-registry providers status=%d", empty.Code)
	}
	var emptyBody map[string][]any
	if err := json.Unmarshal(empty.Body.Bytes(), &emptyBody); err != nil {
		t.Fatal(err)
	}
	if len(emptyBody["providers"]) != 0 {
		t.Fatalf("non-registry providers=%v, want empty", emptyBody["providers"])
	}
}

type providerRegistryStub struct{}

func (providerRegistryStub) Get(string) (video.VideoProvider, bool) { return nil, false }

func TestHandlerCreateGetAndJSONFailures(t *testing.T) {
	h := newTestHandler(false)

	created := performJSON(h, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	if created.Code != http.StatusAccepted || !strings.HasPrefix(created.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("create: status=%d content-type=%q body=%s", created.Code, created.Header().Get("Content-Type"), created.Body.String())
	}
	var job video.VideoJob
	if err := json.Unmarshal(created.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.JobID == "" || job.State != video.StateQueued || job.Provider != "fake" || job.Model != "fake-video-1" {
		t.Fatalf("created job=%+v", job)
	}

	got := performJSON(h, http.MethodGet, "/v1/video/jobs/"+job.JobID, nil, "")
	if got.Code != http.StatusOK {
		t.Fatalf("get existing job: status=%d body=%s", got.Code, got.Body.String())
	}
	var fetched video.VideoJob
	if err := json.Unmarshal(got.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.JobID != job.JobID || fetched.State != video.StateQueued {
		t.Fatalf("fetched job=%+v", fetched)
	}

	missing := performJSON(h, http.MethodGet, "/v1/video/jobs/missing", nil, "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing job: status=%d", missing.Code)
	}
	if body := errorBody(t, missing); body["code"] != "not_found" || body["message"] != "job not found" {
		t.Fatalf("missing job body=%v", body)
	}

	badJSON := httptest.NewRequest(http.MethodPost, "/v1/video/jobs", strings.NewReader("{"))
	badJSONW := httptest.NewRecorder()
	h.ServeHTTP(badJSONW, badJSON)
	if badJSONW.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON: status=%d", badJSONW.Code)
	}
	if body := errorBody(t, badJSONW); body["code"] != "invalid_json" {
		t.Fatalf("bad JSON body=%v", body)
	}

	invalid := validAPIRequest()
	invalid.ProjectID = ""
	badRequest := performJSON(h, http.MethodPost, "/v1/video/jobs", invalid, "")
	if badRequest.Code != http.StatusBadRequest {
		t.Fatalf("invalid request: status=%d body=%s", badRequest.Code, badRequest.Body.String())
	}
	body := errorBody(t, badRequest)
	if body["code"] != "video_create_failed" || !strings.Contains(body["message"].(string), "project_id is required") {
		t.Fatalf("invalid request body=%v", body)
	}

	// The route parser deliberately rejects incomplete job paths.
	incomplete := performJSON(h, http.MethodGet, "/v1/video/jobs/", nil, "")
	if incomplete.Code != http.StatusNotFound || errorBody(t, incomplete)["code"] != "not_found" {
		t.Fatalf("incomplete job path: status=%d body=%s", incomplete.Code, incomplete.Body.String())
	}
}

func TestHandlerCancelSuccessAndFailures(t *testing.T) {
	h := newTestHandler(false)
	created := performJSON(h, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	var job video.VideoJob
	if err := json.Unmarshal(created.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}

	cancelled := performJSON(h, http.MethodPost, "/v1/video/jobs/"+job.JobID+"/cancel", nil, "")
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel: status=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	var cancelledJob video.VideoJob
	if err := json.Unmarshal(cancelled.Body.Bytes(), &cancelledJob); err != nil {
		t.Fatal(err)
	}
	if cancelledJob.JobID != job.JobID || cancelledJob.State != video.StateCancelled {
		t.Fatalf("cancelled job=%+v", cancelledJob)
	}

	repeated := performJSON(h, http.MethodPost, "/v1/video/jobs/"+job.JobID+"/cancel", nil, "")
	if repeated.Code != http.StatusBadRequest {
		t.Fatalf("repeated cancel: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	if body := errorBody(t, repeated); body["code"] != "cancel_failed" {
		t.Fatalf("repeated cancel body=%v", body)
	}

	missing := performJSON(h, http.MethodPost, "/v1/video/jobs/no-such-job/cancel", nil, "")
	if missing.Code != http.StatusBadRequest || errorBody(t, missing)["code"] != "cancel_failed" {
		t.Fatalf("missing cancel: status=%d body=%s", missing.Code, missing.Body.String())
	}

	// RequireAuth=false is an explicit public-mode behavior.
	public := newTestHandler(false)
	if response := performJSON(public, http.MethodGet, "/v1/video/providers", nil, ""); response.Code != http.StatusOK {
		t.Fatalf("public providers status=%d", response.Code)
	}
}

func TestHandlerDownloadsOnlyStoredJobAssetsAndRedactsFilesystemPaths(t *testing.T) {
	h := newTestHandler(true)
	local, err := storage.NewLocal(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	h.Assets = local
	ctx := context.Background()
	asset, err := local.Put(ctx, video.Asset{
		ID: "episode-output", Kind: "video", Filename: "episode.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "project-1", "episode_id": "episode-1", "job_id": "job-download"},
	}, []byte("video bytes"))
	if err != nil {
		t.Fatal(err)
	}
	job := video.NewJob("job-download", validAPIRequest())
	job.State = video.StateCompleted
	job.OutputAssets = []video.Asset{asset}
	if err := h.Store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}

	download := performJSON(h, http.MethodGet, "/v1/video/jobs/job-download/assets/episode-output", nil, "Bearer test-token")
	if download.Code != http.StatusOK || download.Body.String() != "video bytes" {
		t.Fatalf("download status=%d body=%q", download.Code, download.Body.String())
	}
	if got := download.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type=%q, want video/mp4", got)
	}
	if got := download.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options=%q", got)
	}
	if !strings.Contains(download.Header().Get("Content-Disposition"), "episode.mp4") {
		t.Fatalf("Content-Disposition=%q", download.Header().Get("Content-Disposition"))
	}
	rangeRequest := httptest.NewRequest(http.MethodGet, "/v1/video/jobs/job-download/assets/episode-output", nil)
	rangeRequest.Header.Set("Authorization", "Bearer test-token")
	rangeRequest.Header.Set("Range", "bytes=0-3")
	rangeResponse := httptest.NewRecorder()
	h.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "vide" {
		t.Fatalf("range download status=%d body=%q", rangeResponse.Code, rangeResponse.Body.String())
	}

	public := performJSON(h, http.MethodGet, "/v1/video/jobs/job-download", nil, "Bearer test-token")
	if public.Code != http.StatusOK {
		t.Fatalf("public job status=%d body=%s", public.Code, public.Body.String())
	}
	var payload struct {
		OutputAssets []video.Asset `json:"output_assets"`
	}
	if err := json.Unmarshal(public.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.OutputAssets) != 1 {
		t.Fatalf("output assets=%+v", payload.OutputAssets)
	}
	wantURI := "/v1/video/jobs/job-download/assets/episode-output"
	if payload.OutputAssets[0].URI != wantURI || strings.Contains(payload.OutputAssets[0].URI, local.Root) {
		t.Fatalf("public asset URI=%q, want route %q without local filesystem path", payload.OutputAssets[0].URI, wantURI)
	}
	stored, err := h.Store.Get(ctx, "job-download")
	if err != nil {
		t.Fatal(err)
	}
	if stored.OutputAssets[0].URI != asset.URI {
		t.Fatalf("public response mutated stored asset URI: stored=%q original=%q", stored.OutputAssets[0].URI, asset.URI)
	}

	unknown := performJSON(h, http.MethodGet, "/v1/video/jobs/job-download/assets/not-an-output", nil, "Bearer test-token")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown asset status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	unauthorized := performJSON(h, http.MethodGet, wantURI, nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized download status=%d", unauthorized.Code)
	}
}

func TestPublicJobSanitizesNarrationAndSubtitleAssetURIs(t *testing.T) {
	job := video.VideoJob{
		JobID: "job-episode-1",
		NarrationAsset: &video.Asset{ID: "job-episode-1-narration", URI: "/private/assets/narration.wav"},
		SubtitleAsset: &video.Asset{ID: "job-episode-1-subtitles", URI: "/private/assets/subtitles.srt"},
	}
	got := publicJob(job)
	if got.NarrationAsset == nil || got.NarrationAsset.URI != publicAssetURI(job.JobID, job.NarrationAsset.ID) {
		t.Fatalf("narration URI was not sanitized: %+v", got.NarrationAsset)
	}
	if got.SubtitleAsset == nil || got.SubtitleAsset.URI != publicAssetURI(job.JobID, job.SubtitleAsset.ID) {
		t.Fatalf("subtitle URI was not sanitized: %+v", got.SubtitleAsset)
	}
	if strings.Contains(got.NarrationAsset.URI, "/private/") || strings.Contains(got.SubtitleAsset.URI, "/private/") {
		t.Fatal("public job leaked private asset filesystem paths")
	}
}
