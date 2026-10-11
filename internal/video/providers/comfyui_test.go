package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type comfyTestSink struct {
	asset   video.Asset
	content []byte
}

func (s *comfyTestSink) Put(_ context.Context, asset video.Asset, content []byte) (video.Asset, error) {
	s.asset = asset
	s.content = append([]byte(nil), content...)
	asset.URI = "file:///tmp/" + asset.Filename
	asset.ContentType = "video/mp4"
	asset.SizeBytes = int64(len(content))
	asset.SHA256 = strings.Repeat("a", 64)
	return asset, nil
}

func comfyWorkflowFile(t *testing.T) string {
	t.Helper()
	workflow := map[string]map[string]any{
		"1": {"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "old prompt", "clip": []any{"99", 0}}},
		"2": {"class_type": "EmptyImage", "inputs": map[string]any{"width": 512, "height": 512, "batch_size": 1}},
		"3": {"class_type": "VideoSampler", "inputs": map[string]any{"length": 16, "frame_rate": 24, "seed": 1}},
		"4": {"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "old negative"}},
	}
	data, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func comfyTestConfig(workflowPath, baseURL string) video.Config {
	return video.Config{
		ComfyUIEnabled: true, ComfyUIBaseURL: baseURL, ComfyUIWorkflowPath: workflowPath,
		ComfyUIModel: "local-video", ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}
}

func TestComfyUIConfigRejectsRemotePlainHTTPAndInvalidWorkflow(t *testing.T) {
	path := comfyWorkflowFile(t)
	for _, base := range []string{"http://example.com:8188", "http://192.0.2.4:8188"} {
		if _, err := NewComfyUI(comfyTestConfig(path, base), 1024); err == nil {
			t.Errorf("NewComfyUI(%q) unexpectedly accepted remote HTTP", base)
		}
	}
	if _, err := NewComfyUI(comfyTestConfig(filepath.Join(t.TempDir(), "missing.json"), "http://127.0.0.1:8188"), 1024); err == nil {
		t.Fatal("missing workflow unexpectedly accepted")
	}
	cfg := comfyTestConfig(path, "http://127.0.0.1:8188")
	cfg.ComfyUIFramesNodeID = "missing"
	if _, err := NewComfyUI(cfg, 1024); err == nil || !strings.Contains(err.Error(), "no configured frames node") {
		t.Fatalf("missing frame node error=%v", err)
	}
	if _, err := NewComfyUI(comfyTestConfig(path, "https://comfy.example.test"), 1024); err != nil {
		t.Fatalf("HTTPS endpoint rejected: %v", err)
	}
}

func TestComfyUICreateInjectsPromptDimensionsFramesAndSeed(t *testing.T) {
	var submitted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prompt" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
			t.Errorf("decode prompt: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"prompt_id":"prompt-123","node_errors":{}}`))
	}))
	defer server.Close()

	cfg := comfyTestConfig(comfyWorkflowFile(t), server.URL)
	provider, err := NewComfyUI(cfg, 1024)
	if err != nil {
		t.Fatal(err)
	}
	seed := int64(42)
	request := video.VideoRequest{
		ProjectID: "p", Prompt: "Noora walks through Persepolis at sunset",
		NegativePrompt: "blurry", Mode: video.ModeTextToVideo, ModelPreference: "local-video",
		DurationSeconds: 2, FPS: 12, AspectRatio: video.Aspect9x16, Seed: &seed,
	}
	job, err := provider.CreateJob(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if job.ProviderJobID != "prompt-123" || job.Provider != "comfyui" || job.Model != "local-video" || !job.Accepted {
		t.Fatalf("job=%+v", job)
	}
	workflow, ok := submitted["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("submitted prompt type=%T", submitted["prompt"])
	}
	node := func(id string) map[string]any {
		t.Helper()
		raw, ok := workflow[id].(map[string]any)
		if !ok {
			t.Fatalf("node %s type=%T", id, workflow[id])
		}
		inputs, ok := raw["inputs"].(map[string]any)
		if !ok {
			t.Fatalf("node %s inputs type=%T", id, raw["inputs"])
		}
		return inputs
	}
	if node("1")["text"] != request.Prompt || node("4")["text"] != request.NegativePrompt {
		t.Fatalf("prompt injection incorrect: positive=%v negative=%v", node("1")["text"], node("4")["text"])
	}
	if node("2")["width"] != float64(720) || node("2")["height"] != float64(1280) {
		t.Fatalf("dimension injection incorrect: %+v", node("2"))
	}
	if node("3")["length"] != float64(24) || node("3")["frame_rate"] != float64(12) || node("3")["seed"] != float64(42) {
		t.Fatalf("frame/fps/seed injection incorrect: %+v", node("3"))
	}
}

func TestComfyUIHistoryAndOutputDownload(t *testing.T) {
	videoBytes := []byte("fake mp4 bytes for provider adapter test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/history/prompt-123":
			_, _ = w.Write([]byte(`{"prompt-123":{"outputs":{"10":{"videos":[{"filename":"noora.mp4","subfolder":"renders","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/view":
			if r.URL.Query().Get("filename") != "noora.mp4" || r.URL.Query().Get("subfolder") != "renders" || r.URL.Query().Get("type") != "output" {
				t.Errorf("unexpected view query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	status, err := provider.GetJob(context.Background(), "prompt-123")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != video.StateCompleted || status.Progress != 1 || len(status.Outputs) != 1 || status.Outputs[0].Filename != "noora.mp4" {
		t.Fatalf("status=%+v", status)
	}
	sink := &comfyTestSink{}
	assets, err := provider.DownloadOutputs(context.Background(), status, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].ContentType != "video/mp4" || assets[0].SizeBytes != int64(len(videoBytes)) || string(sink.content) != string(videoBytes) {
		t.Fatalf("assets=%+v sink=%+v", assets, sink)
	}
}

func TestComfyUIQueueStatusAndSafeCancellation(t *testing.T) {
	var cancelled string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/history/pending-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodGet && r.URL.Path == "/queue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[[1,"pending-1",{}, {}, []],[2,"pending-2",{}, {}, []]]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/queue":
			var body struct {
				Delete []string `json:"delete"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode delete: %v", err)
			}
			if len(body.Delete) > 0 {
				cancelled = body.Delete[0]
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	status, err := provider.GetJob(context.Background(), "pending-1")
	if err != nil || status.State != video.StateQueued {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if err := provider.CancelJob(context.Background(), "pending-1"); err != nil {
		t.Fatal(err)
	}
	if cancelled != "pending-1" {
		t.Fatalf("cancelled=%q", cancelled)
	}
	if err := provider.CancelJob(context.Background(), "running-1"); err == nil || !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("unknown job cancellation error=%v, want fail-closed pending check", err)
	}
}

func TestComfyUIOutputLimitAndCapabilities(t *testing.T) {
	data := []byte("this is too large")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/view" {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 4)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := provider.Capabilities(context.Background())
	if err != nil || len(caps.Modes) != 1 || caps.Modes[0] != video.ModeTextToVideo || caps.SupportsAudio || caps.SupportsWebhooks || caps.SupportsCancel {
		t.Fatalf("capabilities=%+v err=%v", caps, err)
	}
	_, err = provider.DownloadOutputs(context.Background(), video.ProviderJobStatus{Outputs: []video.Asset{{
		ID: "1", Filename: "noora.mp4", Metadata: map[string]string{"filename": "noora.mp4", "type": "output"},
	}}}, &comfyTestSink{})
	if err == nil || !strings.Contains(err.Error(), "exceeds configured asset limit") {
		t.Fatalf("oversized output error=%v", err)
	}
	if _, err := provider.EstimateCost(video.VideoRequest{ProjectID: "p", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 1, AspectRatio: video.Aspect16x9}); err != nil {
		t.Fatalf("local provider cost estimate: %v", err)
	}
	if err := provider.ValidateRequest(video.VideoRequest{ProjectID: "p", Prompt: "x", Mode: video.ModeVideoToVideo, DurationSeconds: 1, AspectRatio: video.Aspect16x9}); err == nil {
		t.Fatal("unsupported mode accepted")
	}
	if err := provider.RegisterWebhook(context.Background(), video.WebhookRegistration{}); err == nil {
		t.Fatal("unsupported webhook registration accepted")
	}
}
