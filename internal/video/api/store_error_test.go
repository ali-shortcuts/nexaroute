package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// failingJobStore simulates a backend outage: every Get fails with a
// generic (non-ErrNotFound) error, the way the PostgreSQL store wraps a
// connection failure. It embeds JobStore so only Get needs overriding.
type failingJobStore struct {
	queue.JobStore
}

func (failingJobStore) Get(context.Context, string) (video.VideoJob, error) {
	return video.VideoJob{}, errors.New("video job store: get: dial tcp: connection refused")
}

func TestHandlerGetStoreFailureIs503Not404(t *testing.T) {
	h := &Handler{Store: failingJobStore{}, RequireAuth: false}
	for _, path := range []string{
		"/v1/video/jobs/video-missing",
		"/v1/video/jobs/video-missing/assets/asset-1",
	} {
		w := performJSON(h, http.MethodGet, path, nil, "")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s: status=%d, want %d (body=%s)", path, w.Code, http.StatusServiceUnavailable, w.Body.String())
		}
		body := errorBody(t, w)
		if body["code"] != "video_store_unavailable" || body["retryable"] != true {
			t.Fatalf("GET %s: error body=%v, want code=video_store_unavailable retryable=true", path, body)
		}
		if strings.Contains(w.Body.String(), "dial tcp") {
			t.Fatalf("GET %s: backend detail leaked into response: %s", path, w.Body.String())
		}
	}
}

func TestHandlerGetMissingJobStill404(t *testing.T) {
	h := newTestHandler(false)
	w := performJSON(h, http.MethodGet, "/v1/video/jobs/no-such-job", nil, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing job: status=%d, want %d (body=%s)", w.Code, http.StatusNotFound, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "not_found" || body["retryable"] != false {
		t.Fatalf("missing job: error body=%v", body)
	}
}

// failingCreateStore fails the store operations the create path touches
// before any provider submission, simulating a backend outage during
// job creation.
type failingCreateStore struct {
	queue.JobStore
}

func (failingCreateStore) GetByIdempotency(context.Context, string) (video.VideoJob, error) {
	return video.VideoJob{}, errors.New("video job store: get by idempotency: dial tcp: connection refused")
}

func TestHandlerCreateStoreFailureIs503Not400(t *testing.T) {
	orch := &orchestrator.Orchestrator{
		Store:     failingCreateStore{JobStore: queue.NewMemoryStore()},
		Queue:     queue.New(4),
		Providers: orchestrator.Registry{},
	}
	h := &Handler{Orch: orch, RequireAuth: false}
	req := validAPIRequest()
	req.IdempotencyKey = "idem-1"
	w := performJSON(h, http.MethodPost, "/v1/video/jobs", req, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST create with failing store: status=%d, want %d (body=%s)", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "video_store_unavailable" || body["retryable"] != true {
		t.Fatalf("POST create with failing store: error body=%v", body)
	}
	if strings.Contains(w.Body.String(), "dial tcp") {
		t.Fatalf("POST create with failing store: backend detail leaked: %s", w.Body.String())
	}
}

func TestHandlerCancelMissingJobIs404Not400(t *testing.T) {
	h := newTestHandler(false)
	w := performJSON(h, http.MethodPost, "/v1/video/jobs/no-such-job/cancel", nil, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("cancel missing job: status=%d, want %d (body=%s)", w.Code, http.StatusNotFound, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "not_found" {
		t.Fatalf("cancel missing job: error body=%v", body)
	}
}

func TestHandlerCancelStoreFailureIs503Not400(t *testing.T) {
	orch := &orchestrator.Orchestrator{
		Store:     failingJobStore{},
		Queue:     queue.New(4),
		Providers: orchestrator.Registry{},
	}
	h := &Handler{Orch: orch, RequireAuth: false}
	w := performJSON(h, http.MethodPost, "/v1/video/jobs/video-missing/cancel", nil, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancel with failing store: status=%d, want %d (body=%s)", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "video_store_unavailable" || body["retryable"] != true {
		t.Fatalf("cancel with failing store: error body=%v", body)
	}
	if strings.Contains(w.Body.String(), "dial tcp") {
		t.Fatalf("cancel with failing store: backend detail leaked: %s", w.Body.String())
	}
}

// fakeAssetOpener implements both video.AssetSink (for the Handler field)
// and the Open interface getAsset asserts, so asset-open failures can be
// injected without touching disk.
type fakeAssetOpener struct{ err error }

func (fakeAssetOpener) Put(context.Context, video.Asset, []byte) (video.Asset, error) {
	return video.Asset{}, nil
}

func (f fakeAssetOpener) Open(video.Asset) (*os.File, error) { return nil, f.err }

func assetTestHandler(openErr error) *Handler {
	store := queue.NewMemoryStore()
	job := video.VideoJob{JobID: "video-1", OutputAssets: []video.Asset{{ID: "asset-1", URI: "file:///tmp/asset.bin"}}}
	if err := store.Create(context.Background(), job); err != nil {
		panic(err)
	}
	return &Handler{Store: store, Assets: fakeAssetOpener{err: openErr}, RequireAuth: false}
}

func TestHandlerGetAssetStorageFailureIs503Not404(t *testing.T) {
	h := assetTestHandler(errors.New("read /tmp/asset.bin: input/output error"))
	w := performJSON(h, http.MethodGet, "/v1/video/jobs/video-1/assets/asset-1", nil, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("asset open failure: status=%d, want %d (body=%s)", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "asset_unavailable" || body["retryable"] != true {
		t.Fatalf("asset open failure: error body=%v", body)
	}
	if strings.Contains(w.Body.String(), "input/output") {
		t.Fatalf("asset open failure: backend detail leaked: %s", w.Body.String())
	}
}

func TestHandlerGetAssetMissingFileStill404(t *testing.T) {
	h := assetTestHandler(os.ErrNotExist)
	w := performJSON(h, http.MethodGet, "/v1/video/jobs/video-1/assets/asset-1", nil, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("asset missing file: status=%d, want %d (body=%s)", w.Code, http.StatusNotFound, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "asset_not_found" {
		t.Fatalf("asset missing file: error body=%v", body)
	}
}
