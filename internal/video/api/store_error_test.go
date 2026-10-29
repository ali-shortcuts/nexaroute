package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
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
