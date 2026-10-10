package runtime_test

import (
	"context"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/runtime"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeDisabled(t *testing.T) {
	r, err := runtime.New(video.Config{}, t.TempDir())
	if err != nil || r != nil {
		t.Fatalf("disabled runtime=%v err=%v", r, err)
	}
}
func TestRuntimeFakeProviderEndToEnd(t *testing.T) {
	r, err := runtime.New(video.Config{Enabled: true, StorePath: "jobs.json", QueueSize: 4, Workers: 1, DevelopmentFakeProvider: true}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	req := httptest.NewRequest("POST", "/v1/video/jobs", strings.NewReader(`{"project_id":"p","prompt":"scene","mode":"text_to_video","duration_seconds":1,"provider_preference":"fake"}`))
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	r.Close()
}
