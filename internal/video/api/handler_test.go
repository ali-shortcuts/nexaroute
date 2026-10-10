package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
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
