package runtime_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/runtime"
)

func TestRuntimeDisabled(t *testing.T) {
	r, err := runtime.New(video.Config{}, t.TempDir())
	if err != nil || r != nil {
		t.Fatalf("disabled runtime=%v err=%v", r, err)
	}
}

func TestRuntimeEnabledFailsClosedWithoutAuthToken(t *testing.T) {
	t.Setenv("NEXAROUTE_TEST_VIDEO_TOKEN", "")
	_, err := runtime.New(video.Config{Enabled: true, AuthTokenEnv: "NEXAROUTE_TEST_VIDEO_TOKEN", DevelopmentFakeProvider: true}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "missing or empty") {
		t.Fatalf("expected missing token error, got %v", err)
	}
	_, err = runtime.New(video.Config{Enabled: true, DevelopmentFakeProvider: true}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "auth_token_env is required") {
		t.Fatalf("expected missing token environment name error, got %v", err)
	}
}

func TestRuntimeFakeProviderEndToEndAuthenticatedAndPersistsArtifact(t *testing.T) {
	const tokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN"
	const token = "test-video-token-for-runtime"
	t.Setenv(tokenEnv, token)
	dir := t.TempDir()
	r, err := runtime.New(video.Config{
		Enabled: true, StorePath: "jobs.json", StorageRoot: "assets",
		QueueSize: 4, Workers: 1, AuthTokenEnv: tokenEnv, DevelopmentFakeProvider: true,
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		r.Close()
	}()

	body := strings.NewReader("{\"project_id\":\"p\",\"prompt\":\"scene\",\"mode\":\"text_to_video\",\"duration_seconds\":1,\"provider_preference\":\"fake\"}")
	req := httptest.NewRequest("POST", "/v1/video/jobs", body)
	rec := httptest.NewRecorder()
	r.Handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated create status=%d body=%s", rec.Code, rec.Body.String())
	}

	body = strings.NewReader("{\"project_id\":\"p\",\"prompt\":\"scene\",\"mode\":\"text_to_video\",\"duration_seconds\":1,\"provider_preference\":\"fake\"}")
	req = httptest.NewRequest("POST", "/v1/video/jobs", body)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	r.Handler.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("authenticated create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created video.VideoJob
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req = httptest.NewRequest("GET", "/v1/video/jobs/"+created.JobID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec = httptest.NewRecorder()
		r.Handler.ServeHTTP(rec, req)
		if rec.Code == 200 {
			var got video.VideoJob
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.State == video.StateCompleted {
				if len(got.OutputAssets) != 1 {
					t.Fatalf("output assets=%d, want 1", len(got.OutputAssets))
				}
				asset := got.OutputAssets[0]
				if asset.Kind != "development-placeholder" || asset.ContentType != "text/plain" {
					t.Fatalf("fake provider output misrepresented as a video: %+v", asset)
				}
				if asset.URI == "" || len(asset.SHA256) != 64 || asset.SizeBytes <= 0 {
					t.Fatalf("output was not durably stored and checksummed: %+v", asset)
				}
				if !strings.HasPrefix(asset.URI, filepath.Join(dir, "assets")) {
					t.Fatalf("output escaped configured storage root: %s", asset.URI)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake video job did not complete and persist its placeholder artifact")
}
