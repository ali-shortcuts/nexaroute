package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// mission95StubStore delegates to a MemoryStore except for List, which the
// test controls: either a fixed job list (for limit/durability behavior)
// or an error (for the list-failure branch).
type mission95StubStore struct {
	*queue.MemoryStore
	listJobs []video.VideoJob
	listErr  error
}

func (s *mission95StubStore) List(context.Context) ([]video.VideoJob, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.listJobs, nil
}

// mission95PutOnlySink implements video.AssetSink but deliberately does
// NOT implement Open, so asset downloads through it must report the
// download-unavailable branch.
type mission95PutOnlySink struct{}

func (mission95PutOnlySink) Put(context.Context, video.Asset, []byte) (video.Asset, error) {
	return video.Asset{}, nil
}

func mission95StoredAsset(t *testing.T, store *storage.Local, data []byte) video.Asset {
	t.Helper()
	asset, err := store.Put(context.Background(), video.Asset{
		ID: "asset-1", Filename: "clip.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, data)
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func mission95JobWithAssets(t *testing.T, store *queue.MemoryStore, id string, outputs, sources []video.Asset) {
	t.Helper()
	job := video.VideoJob{JobID: id, State: video.StateCompleted, CreatedAt: time.Now(), OutputAssets: outputs, SourceAssets: sources}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestMission95ServeHTTPEmptyPathAndUnconfiguredHandlers(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("root path status=%d, want 404", w.Code)
	}
	if body := errorBody(t, w); body["code"] != "not_found" {
		t.Fatalf("root path body=%v", body)
	}

	// Create with no orchestrator configured.
	w = performJSON(&Handler{}, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	if w.Code != http.StatusServiceUnavailable || errorBody(t, w)["code"] != "video_unavailable" {
		t.Fatalf("unconfigured create status=%d body=%s", w.Code, w.Body.String())
	}
	// Create with an orchestrator that rejects the request.
	w = performJSON(newTestHandler(false), http.MethodPost, "/v1/video/jobs", video.VideoRequest{}, "")
	if w.Code != http.StatusBadRequest || errorBody(t, w)["code"] != "video_create_failed" {
		t.Fatalf("rejected create status=%d body=%s", w.Code, w.Body.String())
	}
	// Get with no store configured.
	w = performJSON(&Handler{}, http.MethodGet, "/v1/video/jobs/job-1", nil, "")
	if w.Code != http.StatusServiceUnavailable || errorBody(t, w)["code"] != "video_unavailable" {
		t.Fatalf("unconfigured get status=%d body=%s", w.Code, w.Body.String())
	}
	// Cancel with no orchestrator configured.
	w = performJSON(&Handler{}, http.MethodPost, "/v1/video/jobs/job-1/cancel", nil, "")
	if w.Code != http.StatusServiceUnavailable || errorBody(t, w)["code"] != "video_unavailable" {
		t.Fatalf("unconfigured cancel status=%d body=%s", w.Code, w.Body.String())
	}
	// Asset download with no store configured.
	w = performJSON(&Handler{}, http.MethodGet, "/v1/video/jobs/job-1/assets/asset-1", nil, "")
	if w.Code != http.StatusServiceUnavailable || errorBody(t, w)["code"] != "video_unavailable" {
		t.Fatalf("unconfigured asset status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMission95ListDurabilityLimitAndFailure(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	jobs := make([]video.VideoJob, 0, 250)
	for i := 0; i < 250; i++ {
		jobs = append(jobs, video.VideoJob{
			JobID: fmt.Sprintf("job-%03d", i), State: video.StateCompleted,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	stub := &mission95StubStore{MemoryStore: queue.NewMemoryStore(), listJobs: jobs}
	h := &Handler{Store: stub}

	decode := func(path string) (int, map[string]any) {
		t.Helper()
		w := performJSON(h, http.MethodGet, path, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return w.Code, body
	}
	_, all := decode("/v1/video/jobs?limit=999")
	if total, _ := all["total"].(float64); total != 200 {
		t.Fatalf("clamped total=%v, want 200", total)
	}
	// A store type outside the known classes reports the generic class.
	if durability, _ := all["durability"].(string); durability != "store" {
		t.Fatalf("durability=%q, want store", durability)
	}
	_, two := decode("/v1/video/jobs?limit=2")
	if total, _ := two["total"].(float64); total != 2 {
		t.Fatalf("limit=2 total=%v", total)
	}
	_, fifty := decode("/v1/video/jobs?limit=0")
	if total, _ := fifty["total"].(float64); total != 50 {
		t.Fatalf("limit=0 total=%v, want default 50", total)
	}
	_, fifty2 := decode("/v1/video/jobs?limit=banana")
	if total, _ := fifty2["total"].(float64); total != 50 {
		t.Fatalf("limit=banana total=%v, want default 50", total)
	}

	failing := &Handler{Store: &mission95StubStore{MemoryStore: queue.NewMemoryStore(), listErr: fmt.Errorf("store down")}}
	w := performJSON(failing, http.MethodGet, "/v1/video/jobs", nil, "")
	if w.Code != http.StatusInternalServerError || errorBody(t, w)["code"] != "video_list_failed" {
		t.Fatalf("list failure status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMission95GetAssetBranches(t *testing.T) {
	local, err := storage.NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("clip-data-1234")
	stored := mission95StoredAsset(t, local, data)

	store := queue.NewMemoryStore()
	mission95JobWithAssets(t, store, "job-src", nil, []video.Asset{stored})
	mission95JobWithAssets(t, store, "job-out", []video.Asset{stored}, nil)
	h := &Handler{Store: store, Assets: local}

	// An asset found only in SourceAssets still downloads.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/video/jobs/job-src/assets/"+stored.ID, nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != string(data) {
		t.Fatalf("source-asset download status=%d body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("source-asset content type=%q", ct)
	}

	// Unknown job: store lookup fails.
	w = performJSON(h, http.MethodGet, "/v1/video/jobs/nope/assets/"+stored.ID, nil, "")
	if w.Code != http.StatusNotFound || errorBody(t, w)["code"] != "not_found" {
		t.Fatalf("unknown-job asset status=%d body=%s", w.Code, w.Body.String())
	}
	// Unknown asset id within a known job.
	w = performJSON(h, http.MethodGet, "/v1/video/jobs/job-out/assets/nope", nil, "")
	if w.Code != http.StatusNotFound || errorBody(t, w)["code"] != "asset_not_found" {
		t.Fatalf("unknown asset status=%d body=%s", w.Code, w.Body.String())
	}
	// Sink without Open: downloads are unavailable.
	noOpen := &Handler{Store: store, Assets: mission95PutOnlySink{}}
	w = performJSON(noOpen, http.MethodGet, "/v1/video/jobs/job-out/assets/"+stored.ID, nil, "")
	if w.Code != http.StatusNotImplemented || errorBody(t, w)["code"] != "asset_download_unavailable" {
		t.Fatalf("no-opener asset status=%d body=%s", w.Code, w.Body.String())
	}
	// Opener failure (asset URI outside the root): not found.
	outside := stored
	outside.URI = filepath.Join(t.TempDir(), "outside.mp4")
	outsideStore := queue.NewMemoryStore()
	mission95JobWithAssets(t, outsideStore, "job-outside", []video.Asset{outside}, nil)
	w = performJSON(&Handler{Store: outsideStore, Assets: local}, http.MethodGet, "/v1/video/jobs/job-outside/assets/"+stored.ID, nil, "")
	if w.Code != http.StatusNotFound || errorBody(t, w)["code"] != "asset_not_found" {
		t.Fatalf("opener-failure asset status=%d body=%s", w.Code, w.Body.String())
	}
	// Symlink inside the root that resolves outside it: also a
	// deterministic rejection, not a retryable outage.
	outsideFile := filepath.Join(t.TempDir(), "real.mp4")
	if err := os.WriteFile(outsideFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(local.Root, "link.mp4")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Fatal(err)
	}
	link := stored
	link.URI = linkPath
	linkStore := queue.NewMemoryStore()
	mission95JobWithAssets(t, linkStore, "job-link", []video.Asset{link}, nil)
	w = performJSON(&Handler{Store: linkStore, Assets: local}, http.MethodGet, "/v1/video/jobs/job-link/assets/"+stored.ID, nil, "")
	if w.Code != http.StatusNotFound || errorBody(t, w)["code"] != "asset_not_found" {
		t.Fatalf("symlink-escape asset status=%d body=%s", w.Code, w.Body.String())
	}
	// Recorded size disagrees with the file: integrity error.
	wrongSize := stored
	wrongSize.SizeBytes = 999
	wrongSize.SHA256 = ""
	sizeStore := queue.NewMemoryStore()
	mission95JobWithAssets(t, sizeStore, "job-size", []video.Asset{wrongSize}, nil)
	w = performJSON(&Handler{Store: sizeStore, Assets: local}, http.MethodGet, "/v1/video/jobs/job-size/assets/"+stored.ID, nil, "")
	if w.Code != http.StatusInternalServerError || errorBody(t, w)["code"] != "asset_integrity_error" {
		t.Fatalf("size-mismatch asset status=%d body=%s", w.Code, w.Body.String())
	}
	// Recorded checksum disagrees (same size, different content hash).
	sum := sha256.Sum256([]byte("different content!"))
	wrongHash := stored
	wrongHash.SHA256 = hex.EncodeToString(sum[:])
	hashStore := queue.NewMemoryStore()
	mission95JobWithAssets(t, hashStore, "job-hash", []video.Asset{wrongHash}, nil)
	w = performJSON(&Handler{Store: hashStore, Assets: local}, http.MethodGet, "/v1/video/jobs/job-hash/assets/"+stored.ID, nil, "")
	if w.Code != http.StatusInternalServerError || errorBody(t, w)["code"] != "asset_integrity_error" {
		t.Fatalf("checksum-mismatch asset status=%d body=%s", w.Code, w.Body.String())
	}
	// A filename that cannot be used as a download name falls back to
	// the asset id in the disposition.
	fallback := stored
	fallback.Filename = ".."
	fallbackStore := queue.NewMemoryStore()
	mission95JobWithAssets(t, fallbackStore, "job-fallback", []video.Asset{fallback}, nil)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/video/jobs/job-fallback/assets/"+stored.ID, nil)
	(&Handler{Store: fallbackStore, Assets: local}).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fallback-name download status=%d body=%s", w.Code, w.Body.String())
	}
	if disp := w.Header().Get("Content-Disposition"); !strings.Contains(disp, stored.ID) {
		t.Fatalf("fallback disposition=%q, want asset id %q", disp, stored.ID)
	}
}

func TestMission95UniversalPlanBranches(t *testing.T) {
	h := &Handler{Universal: &agent.Agent{}}
	// Invalid JSON is rejected before any planning attempt.
	req := httptest.NewRequest(http.MethodPost, "/v1/video/universal/plan", strings.NewReader("{"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || errorBody(t, w)["code"] != "invalid_json" {
		t.Fatalf("universal invalid-json status=%d body=%s", w.Code, w.Body.String())
	}
	// 1:1 aspect selects 960x960, then planning fails closed (no planner
	// configured) with a 502 — the aspect branch ran before the failure.
	w = performJSON(h, http.MethodPost, "/v1/video/universal/plan",
		map[string]any{"prompt": "a scene", "model": "m", "aspect_ratio": "1:1"}, "")
	if w.Code != http.StatusBadGateway || errorBody(t, w)["code"] != "plan_failed" {
		t.Fatalf("universal 1:1 status=%d body=%s", w.Code, w.Body.String())
	}
	// Default aspect selects 1280x720, same fail-closed outcome.
	w = performJSON(h, http.MethodPost, "/v1/video/universal/plan",
		map[string]any{"prompt": "a scene", "model": "m"}, "")
	if w.Code != http.StatusBadGateway || errorBody(t, w)["code"] != "plan_failed" {
		t.Fatalf("universal default-aspect status=%d body=%s", w.Code, w.Body.String())
	}
	// Unconfigured universal planning is unavailable.
	w = performJSON(&Handler{}, http.MethodPost, "/v1/video/universal/plan",
		map[string]any{"prompt": "a scene", "model": "m"}, "")
	if w.Code != http.StatusServiceUnavailable || errorBody(t, w)["code"] != "video_unavailable" {
		t.Fatalf("universal unconfigured status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMission95PublicJobRewritesShotAssetURIs(t *testing.T) {
	store := queue.NewMemoryStore()
	shotAsset := video.Asset{ID: "shot-asset-1", URI: "/private/path/shot.mp4", Filename: "shot.mp4"}
	job := video.VideoJob{
		JobID: "job-shots", State: video.StateCompleted, CreatedAt: time.Now(),
		Request: video.VideoRequest{ShotPlan: []video.Shot{{OutputAsset: &shotAsset}}},
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	w := performJSON(&Handler{Store: store}, http.MethodGet, "/v1/video/jobs/job-shots", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get job status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	reqBody, _ := body["request"].(map[string]any)
	plan, _ := reqBody["shot_plan"].([]any)
	if len(plan) != 1 {
		t.Fatalf("shot_plan=%v", plan)
	}
	shot, _ := plan[0].(map[string]any)
	asset, _ := shot["output_asset"].(map[string]any)
	want := "/v1/video/jobs/job-shots/assets/shot-asset-1"
	if asset["uri"] != want {
		t.Fatalf("shot asset uri=%v, want %s", asset["uri"], want)
	}
	if got := publicAssetURI("", "x"); got != "" {
		t.Fatalf("publicAssetURI empty job=%q", got)
	}
	if got := publicAssetURI("j", ""); got != "" {
		t.Fatalf("publicAssetURI empty asset=%q", got)
	}
	if got := publicAssetURI("j", "a"); got != "/v1/video/jobs/j/assets/a" {
		t.Fatalf("publicAssetURI=%q", got)
	}
}
