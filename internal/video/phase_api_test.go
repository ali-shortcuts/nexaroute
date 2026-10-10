package video_test

import (
	"context"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/api"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVideoAPIJobCreate(t *testing.T) {
	st := queue.NewMemoryStore()
	q := queue.New(1)
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": providers.NewFake(1)}, PollInterval: time.Millisecond}
	h := &api.Handler{Orch: o, Store: st, Providers: o.Providers}
	r := httptest.NewRequest("POST", "/v1/video/jobs", strings.NewReader(`{"project_id":"p","prompt":"x","mode":"text_to_video","duration_seconds":1,"provider_preference":"fake"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
func TestComposerRejectsEmptyInputs(t *testing.T) {
	r := composer.New("/nonexistent", t.TempDir())
	if _, err := r.Concat(context.Background(), nil, "/tmp/out.mp4"); err == nil {
		t.Fatal("expected error")
	}
	_ = video.SchemaVersion
}
