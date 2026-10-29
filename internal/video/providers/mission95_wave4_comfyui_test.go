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

// Workflow with a distinct node per input so each setWorkflowInput failure
// can be triggered in isolation (the shared fixture merges width+height).
func wave4ComfyWorkflowFile(t *testing.T) string {
	t.Helper()
	workflow := map[string]map[string]any{
		"1": {"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "old prompt"}},
		"2": {"class_type": "EmptyImage", "inputs": map[string]any{"width": 512}},
		"3": {"class_type": "VideoSampler", "inputs": map[string]any{"length": 16, "frame_rate": 24, "noise_seed": 1}},
		"4": {"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "old negative"}},
		"5": {"class_type": "EmptyImage", "inputs": map[string]any{"height": 512}},
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

func wave4ComfyConfig(workflowPath, baseURL string) video.Config {
	return video.Config{
		ComfyUIEnabled: true, ComfyUIBaseURL: baseURL, ComfyUIWorkflowPath: workflowPath,
		ComfyUIModel: "local-video", ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "5",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "noise_seed",
		ComfyUINegativeNodeID: "4",
	}
}

func wave4ComfyServer(t *testing.T, status int, body string, submitted *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prompt" {
			http.NotFound(w, r)
			return
		}
		if submitted != nil {
			_ = json.NewDecoder(r.Body).Decode(submitted)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func wave4ComfyRequest() video.VideoRequest {
	seed := int64(7)
	return video.VideoRequest{
		ProjectID: "p", Prompt: "a cat in a library", NegativePrompt: "blurry",
		Mode: video.ModeTextToVideo, ModelPreference: "local-video",
		DurationSeconds: 2, FPS: 24, AspectRatio: video.Aspect16x9, Seed: &seed,
	}
}

// A workflow value that cannot be marshaled fails CreateJob before any
// HTTP call: the clone step is fail-closed, not best-effort.
func TestWave4ComfyUnmarshalableWorkflowFailsClosed(t *testing.T) {
	p := &ComfyUI{
		workflow:    map[string]map[string]any{"1": {"inputs": map[string]any{"text": func() {}}}},
		model:       "local-video",
		maxDuration: 30,
	}
	// No negative prompt and no seed: with a zero config those would fail
	// validation before the marshal step under test is reached.
	req := wave4ComfyRequest()
	req.NegativePrompt = ""
	req.Seed = nil
	if _, err := p.CreateJob(context.Background(), req); err == nil || !strings.Contains(err.Error(), "clone ComfyUI workflow") {
		t.Fatalf("err = %v, want a clone-workflow error", err)
	}
}

// An empty top-level prompt falls back to the single shot's prompt, and an
// empty negative prompt falls back to the shot's negative prompt.
func TestWave4ComfyShotPlanPromptFallback(t *testing.T) {
	var submitted map[string]any
	srv := wave4ComfyServer(t, http.StatusOK, `{"prompt_id":"p-1","node_errors":{}}`, &submitted)
	defer srv.Close()
	provider, err := NewComfyUI(wave4ComfyConfig(wave4ComfyWorkflowFile(t), srv.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	req := wave4ComfyRequest()
	req.Prompt = ""
	req.NegativePrompt = ""
	req.ShotPlan = []video.Shot{{ShotID: "s1", DurationSeconds: 2, Prompt: "shot one prompt", NegativePrompt: "shot negative"}}
	job, err := provider.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if job.ProviderJobID != "p-1" {
		t.Fatalf("job = %+v", job)
	}
	workflow, _ := submitted["prompt"].(map[string]any)
	promptNode, _ := workflow["1"].(map[string]any)
	promptInputs, _ := promptNode["inputs"].(map[string]any)
	negativeNode, _ := workflow["4"].(map[string]any)
	negativeInputs, _ := negativeNode["inputs"].(map[string]any)
	if promptInputs["text"] != "shot one prompt" {
		t.Fatalf("prompt input = %v, want the shot prompt", promptInputs["text"])
	}
	if negativeInputs["text"] != "shot negative" {
		t.Fatalf("negative input = %v, want the shot negative", negativeInputs["text"])
	}
}

// A workflow node that disappears after construction (operator edits the
// file, in-memory workflow diverges) makes CreateJob fail for that input
// instead of submitting a half-wired workflow.
func TestWave4ComfyMissingNodeFailsPerInput(t *testing.T) {
	cases := []struct {
		name   string
		delete string
		want   string
	}{
		{"prompt", "1", `"1"`},
		{"negative", "4", `"4"`},
		{"width", "2", `"2"`},
		{"height", "5", `"5"`},
		{"frames", "3", `"3"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := wave4ComfyServer(t, http.StatusOK, `{"prompt_id":"p-1","node_errors":{}}`, nil)
			defer srv.Close()
			provider, err := NewComfyUI(wave4ComfyConfig(wave4ComfyWorkflowFile(t), srv.URL), 1024)
			if err != nil {
				t.Fatal(err)
			}
			delete(provider.workflow, tc.delete)
			if _, err := provider.CreateJob(context.Background(), wave4ComfyRequest()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want an error naming node %s", err, tc.want)
			}
		})
	}
}

// Deleting the FPS/seed node fails at the FPS step first (FPS is wired
// before seed), and a seed-only failure is reachable when FPS is intact
// but the seed input is gone.
func TestWave4ComfyFPSAndSeedInputFailures(t *testing.T) {
	srv := wave4ComfyServer(t, http.StatusOK, `{"prompt_id":"p-1","node_errors":{}}`, nil)
	defer srv.Close()
	provider, err := NewComfyUI(wave4ComfyConfig(wave4ComfyWorkflowFile(t), srv.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	delete(provider.workflow["3"]["inputs"].(map[string]any), "frame_rate")
	if _, err := provider.CreateJob(context.Background(), wave4ComfyRequest()); err == nil || !strings.Contains(err.Error(), "frame_rate") {
		t.Fatalf("err = %v, want a frame_rate input error", err)
	}
	provider2, err := NewComfyUI(wave4ComfyConfig(wave4ComfyWorkflowFile(t), srv.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	delete(provider2.workflow["3"]["inputs"].(map[string]any), "noise_seed")
	if _, err := provider2.CreateJob(context.Background(), wave4ComfyRequest()); err == nil || !strings.Contains(err.Error(), "noise_seed") {
		t.Fatalf("err = %v, want a noise_seed input error", err)
	}
}

// An upstream submission failure is wrapped, and a success response without
// a prompt_id is rejected instead of producing a job with an empty id.
func TestWave4ComfySubmitFailures(t *testing.T) {
	srv := wave4ComfyServer(t, http.StatusBadGateway, `bad gateway`, nil)
	defer srv.Close()
	provider, err := NewComfyUI(wave4ComfyConfig(wave4ComfyWorkflowFile(t), srv.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateJob(context.Background(), wave4ComfyRequest()); err == nil || !strings.Contains(err.Error(), "submit ComfyUI workflow") {
		t.Fatalf("err = %v, want a wrapped submit error", err)
	}
	empty := wave4ComfyServer(t, http.StatusOK, `{"node_errors":{}}`, nil)
	defer empty.Close()
	provider2, err := NewComfyUI(wave4ComfyConfig(wave4ComfyWorkflowFile(t), empty.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider2.CreateJob(context.Background(), wave4ComfyRequest()); err == nil || !strings.Contains(err.Error(), "did not return a prompt_id") {
		t.Fatalf("err = %v, want a missing prompt_id error", err)
	}
}
