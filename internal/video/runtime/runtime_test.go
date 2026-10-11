package runtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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


func TestRuntimeComfyUIProviderEndToEndPersistsPlayableVideoAsset(t *testing.T) {
	const tokenEnv = "NEXAROUTE_TEST_COMFYUI_TOKEN"
	const token = "test-comfyui-token"
	t.Setenv(tokenEnv, token)
	videoBytes := []byte("test-only mp4 bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/prompt":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"local-prompt-1","node_errors":{}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/history/local-prompt-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"local-prompt-1":{"outputs":{"10":{"videos":[{"filename":"generated.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/view":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	workflow := map[string]map[string]any{
		"1": {"class_type": "TextEncode", "inputs": map[string]any{"text": "template"}},
		"2": {"class_type": "EmptyImage", "inputs": map[string]any{"width": 512, "height": 512}},
		"3": {"class_type": "VideoSampler", "inputs": map[string]any{"length": 24}},
	}
	workflowBytes, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(dir, "comfy-workflow.json")
	if err := os.WriteFile(workflowPath, workflowBytes, 0600); err != nil {
		t.Fatal(err)
	}

	r, err := runtime.New(video.Config{
		Enabled: true, StorePath: "jobs.json", StorageRoot: "assets",
		QueueSize: 4, Workers: 1, AuthTokenEnv: tokenEnv,
		ComfyUIEnabled: true, ComfyUIBaseURL: server.URL, ComfyUIWorkflowPath: workflowPath,
		ComfyUIModel: "local-video", ComfyUIMaxDurationSeconds: 10,
		ComfyUIPromptNodeID: "1", ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2", ComfyUIFramesNodeID: "3",
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); r.Close() }()

	body := strings.NewReader(`{"project_id":"noora","episode_id":"persepolis-01","prompt":"Noora walks through Persepolis","mode":"text_to_video","duration_seconds":1,"aspect_ratio":"16:9","provider_preference":"comfyui","model_preference":"local-video"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/video/jobs", body)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created video.VideoJob
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req = httptest.NewRequest(http.MethodGet, "/v1/video/jobs/"+created.JobID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec = httptest.NewRecorder()
		r.Handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			var got video.VideoJob
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.State == video.StateCompleted {
				if len(got.OutputAssets) != 1 {
					t.Fatalf("output assets=%+v", got.OutputAssets)
				}
				asset := got.OutputAssets[0]
				if asset.Kind != "video" || asset.ContentType != "video/mp4" || asset.SizeBytes != int64(len(videoBytes)) || len(asset.SHA256) != 64 {
					t.Fatalf("invalid persisted video asset: %+v", asset)
				}
				persisted, err := os.ReadFile(asset.URI)
				if err != nil || string(persisted) != string(videoBytes) {
					t.Fatalf("persisted video content=%q err=%v", persisted, err)
				}
				return
			}
			if got.State == video.StateFailed || got.State == video.StateNeedsManualAction {
				t.Fatalf("ComfyUI job reached unexpected state %s: %s", got.State, got.LastError)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("ComfyUI job did not complete and persist the output video")
}
