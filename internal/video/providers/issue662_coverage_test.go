package providers

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

// issue662AggConfig is a valid aggregator configuration pointing at an
// unreachable loopback port: constructor and validation tests never dial.
func issue662AggConfig() video.Config {
	return video.Config{
		AggregatorBaseURL:   "http://127.0.0.1:9",
		AggregatorModel:     "test/model-v1",
		AggregatorAPIKeyEnv: "ISSUE662_AGG_KEY",
	}
}

func issue662AggProvider(t *testing.T, cfg video.Config) *Aggregator {
	t.Helper()
	t.Setenv("ISSUE662_AGG_KEY", "issue662-key")
	p, err := NewAggregator(cfg, 1<<20)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	return p
}

func TestIssue662AggregatorConstructorValidation(t *testing.T) {
	t.Setenv("ISSUE662_AGG_KEY", "k")
	base := issue662AggConfig()
	cases := []struct {
		name   string
		mutate func(*video.Config)
		want   string
	}{
		{"credentials in URL", func(c *video.Config) { c.AggregatorBaseURL = "https://user:pass@queue.example.test" }, "without credentials"},
		{"query in URL", func(c *video.Config) { c.AggregatorBaseURL = "https://queue.example.test/?x=1" }, "without credentials"},
		{"fragment in URL", func(c *video.Config) { c.AggregatorBaseURL = "https://queue.example.test/#frag" }, "without credentials"},
		{"missing scheme", func(c *video.Config) { c.AggregatorBaseURL = "queue.example.test" }, "absolute HTTP(S) URL"},
		{"missing model", func(c *video.Config) { c.AggregatorModel = "  " }, "aggregator_model is required"},
		{"missing key env name", func(c *video.Config) { c.AggregatorAPIKeyEnv = "  " }, "aggregator_api_key_env is required"},
		{"unsupported auth scheme", func(c *video.Config) { c.AggregatorAuthScheme = "Basic" }, `must be "Key" or "Bearer"`},
		{"duration above ceiling", func(c *video.Config) { c.AggregatorMaxDurationSeconds = 3601 }, "between 1 and 3600"},
		{"duration below floor", func(c *video.Config) { c.AggregatorMaxDurationSeconds = 0.5 }, "between 1 and 3600"},
		{"duration NaN", func(c *video.Config) { c.AggregatorMaxDurationSeconds = math.NaN() }, "between 1 and 3600"},
		{"negative cost", func(c *video.Config) { c.AggregatorCostPerSecondUSD = -0.01 }, "finite non-negative"},
		{"infinite cost", func(c *video.Config) { c.AggregatorCostPerSecondUSD = math.Inf(1) }, "finite non-negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if _, err := NewAggregator(cfg, 1<<20); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewAggregator error = %v, want %q", err, tc.want)
			}
		})
	}

	// Defaults are explicit: Key auth, five-minute duration budget, and
	// the shared 512 MiB output ceiling when the caller passes zero.
	p, err := NewAggregator(base, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.authScheme != "Key" || p.maxDuration != 300 || p.maxOutputBytes != aggregatorDefaultMaxOutput {
		t.Fatalf("defaults: scheme=%q duration=%v output=%d", p.authScheme, p.maxDuration, p.maxOutputBytes)
	}
	caps, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.SupportsReferenceFrames || len(caps.Modes) != 1 {
		t.Fatalf("unconfigured image field must stay unclaimed: %+v", caps)
	}
}

func TestIssue662AggregatorImageCapabilityFollowsConfiguredField(t *testing.T) {
	cfg := issue662AggConfig()
	cfg.AggregatorImageField = "image_url"
	cfg.AggregatorEndImageField = "end_image_url"
	p := issue662AggProvider(t, cfg)
	caps, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.SupportsReferenceFrames || len(caps.Modes) != 2 || caps.Modes[1] != video.ModeImageToVideo {
		t.Fatalf("image capability not advertised with a configured field: %+v", caps)
	}
}

func TestIssue662AggregatorValidateRequestMatrix(t *testing.T) {
	plain := issue662AggProvider(t, issue662AggConfig())
	base := video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo}
	cases := []struct {
		name   string
		mutate func(*video.VideoRequest)
		want   string
	}{
		{"unsupported mode", func(r *video.VideoRequest) { r.Mode = video.ModeImageToVideo }, "does not advertise mode"},
		{"duration above provider ceiling", func(r *video.VideoRequest) { r.DurationSeconds = 301 }, "exceeds configured aggregator maximum"},
		{"wrong model preference", func(r *video.VideoRequest) { r.ModelPreference = "other" }, `must be "test/model-v1"`},
		{"callback URL", func(r *video.VideoRequest) { r.CallbackURL = "https://example.test/hook" }, "poll-only"},
		{"audio requested", func(r *video.VideoRequest) { r.AudioRequested = true }, "does not generate audio"},
		{"dialogue requested", func(r *video.VideoRequest) { r.DialogueRequested = true }, "does not generate audio"},
		{"narration text", func(r *video.VideoRequest) { r.NarrationText = "speak" }, "does not generate audio"},
		{"subtitle language", func(r *video.VideoRequest) { r.SubtitleLanguage = "fa" }, "does not generate audio"},
		{"sound requirements", func(r *video.VideoRequest) { r.SoundRequirements = []string{"rain"} }, "does not generate audio"},
		{"multi-shot", func(r *video.VideoRequest) {
			r.ShotPlan = []video.Shot{{ShotID: "a", Prompt: "a", DurationSeconds: 1}, {ShotID: "b", Prompt: "b", DurationSeconds: 1}}
		}, "one generation per job"},
		{"character references", func(r *video.VideoRequest) { r.CharacterReferences = []video.Asset{{ID: "c"}} }, "cannot consume"},
		{"continuity context", func(r *video.VideoRequest) { r.ContinuityContext = &video.ContinuityContext{} }, "cannot consume"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			if err := plain.ValidateRequest(req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRequest error = %v, want %q", err, tc.want)
			}
		})
	}

	cfg := issue662AggConfig()
	cfg.AggregatorImageField = "image_url"
	cfg.AggregatorEndImageField = "end_image_url"
	withImage := issue662AggProvider(t, cfg)
	imageOnlyCfg := issue662AggConfig()
	imageOnlyCfg.AggregatorImageField = "image_url"
	imageOnly := issue662AggProvider(t, imageOnlyCfg)
	lastFrameNoEnd := video.VideoRequest{
		ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeImageToVideo,
		FirstFrameAsset: &video.Asset{ID: "ff", URI: "https://example.test/first.png"},
		LastFrameAsset:  &video.Asset{ID: "lf", URI: "https://example.test/last.png"},
	}
	if err := imageOnly.ValidateRequest(lastFrameNoEnd); err == nil || !strings.Contains(err.Error(), "no end-image input") {
		t.Fatalf("last frame without end field error = %v", err)
	}
	imageMode := video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeImageToVideo}
	if err := withImage.ValidateRequest(imageMode); err == nil || !strings.Contains(err.Error(), "first-frame or reference asset") {
		t.Fatalf("image mode without an asset error = %v", err)
	}
	nonHTTP := imageMode
	nonHTTP.FirstFrameAsset = &video.Asset{ID: "ff", URI: "file:///tmp/first.png"}
	if err := withImage.ValidateRequest(nonHTTP); err == nil || !strings.Contains(err.Error(), "http(s) URI") {
		t.Fatalf("non-HTTP image error = %v", err)
	}
	tooMany := imageMode
	tooMany.ReferenceAssets = []video.Asset{{ID: "a", URI: "https://example.test/a.png"}, {ID: "b", URI: "https://example.test/b.png"}}
	if err := withImage.ValidateRequest(tooMany); err == nil || !strings.Contains(err.Error(), "at most one image") {
		t.Fatalf("multiple image error = %v", err)
	}
	lastNonHTTP := imageMode
	lastNonHTTP.FirstFrameAsset = &video.Asset{ID: "ff", URI: "https://example.test/first.png"}
	lastNonHTTP.LastFrameAsset = &video.Asset{ID: "lf", URI: "file:///tmp/last.png"}
	if err := withImage.ValidateRequest(lastNonHTTP); err == nil || !strings.Contains(err.Error(), "last-frame asset") {
		t.Fatalf("non-HTTP last frame error = %v", err)
	}
	valid := imageMode
	valid.FirstFrameAsset = &video.Asset{ID: "ff", URI: "https://example.test/first.png"}
	valid.LastFrameAsset = &video.Asset{ID: "lf", URI: "https://example.test/last.png"}
	if err := withImage.ValidateRequest(valid); err != nil {
		t.Fatalf("valid first/last frame request rejected: %v", err)
	}
}

func TestIssue662AggregatorSubmitBodyCarriesOptionalFields(t *testing.T) {
	cfg := issue662AggConfig()
	cfg.AggregatorImageField = "image_url"
	cfg.AggregatorEndImageField = "end_image_url"
	p := issue662AggProvider(t, cfg)
	seed := int64(42)
	req := video.VideoRequest{
		ProjectID: "p", Prompt: "x", NegativePrompt: "blurry", DurationSeconds: 5,
		AspectRatio: video.Aspect9x16, Seed: &seed, Mode: video.ModeImageToVideo,
		FirstFrameAsset: &video.Asset{ID: "ff", URI: "https://example.test/first.png"},
		LastFrameAsset:  &video.Asset{ID: "lf", URI: "https://example.test/last.png"},
	}
	body := p.submitBody(req)
	if body["prompt"] != "x" || body["negative_prompt"] != "blurry" || body["aspect_ratio"] != "9:16" {
		t.Fatalf("submit body lost prompt/negative/aspect: %v", body)
	}
	if body["seed"] != int64(42) || body["image_url"] != "https://example.test/first.png" || body["end_image_url"] != "https://example.test/last.png" {
		t.Fatalf("submit body lost seed/image/end frame: %v", body)
	}
	if body["duration_seconds"] != float64(5) {
		t.Fatalf("submit body duration = %v", body["duration_seconds"])
	}
}

func TestIssue662AggregatorGetJobVocabularyAndResultShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/test/model-v1/requests/"), "/status")
			switch id {
			case "queued":
				_, _ = w.Write([]byte(`{"status":"QUEUED"}`))
			case "progress":
				_, _ = w.Write([]byte(`{"status":"RUNNING","progress":0.42}`))
			case "bad-progress":
				_, _ = w.Write([]byte(`{"status":"RUNNING","progress":2}`))
			case "failed":
				_, _ = w.Write([]byte(`{"status":"ERROR"}`))
			case "weird":
				_, _ = w.Write([]byte(`{"status":"SOMETHING_NEW"}`))
			case "empty-result", "completed":
				_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
			default:
				http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
			}
		case strings.Contains(r.URL.Path, "/requests/empty-result"):
			_, _ = w.Write([]byte(`{"output":{}}`))
		case strings.Contains(r.URL.Path, "/requests/"):
			_, _ = w.Write([]byte(`{"video_url":"https://cdn.example.test/out.mp4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := issue662AggProvider(t, issue662AggConfig())
	p.baseURL = srv.URL

	ctx := context.Background()
	if _, err := p.GetJob(ctx, "  "); err == nil || !strings.Contains(err.Error(), "job ID is required") {
		t.Fatalf("empty id error = %v", err)
	}
	if st, err := p.GetJob(ctx, "queued"); err != nil || st.State != video.StateQueued {
		t.Fatalf("queued = %+v, %v", st, err)
	}
	if st, err := p.GetJob(ctx, "progress"); err != nil || st.State != video.StateProcessing || st.Progress != 0.42 {
		t.Fatalf("progress = %+v, %v", st, err)
	}
	if st, err := p.GetJob(ctx, "bad-progress"); err != nil || st.Progress != 0 {
		t.Fatalf("out-of-range progress must be dropped, got %+v, %v", st, err)
	}
	if st, err := p.GetJob(ctx, "failed"); err != nil || st.State != video.StateFailed || !strings.Contains(st.Error, "failed generation") {
		t.Fatalf("failed = %+v, %v", st, err)
	}
	if st, err := p.GetJob(ctx, "weird"); err != nil || st.State != video.StateProcessing {
		t.Fatalf("unknown vocabulary must stay processing, got %+v, %v", st, err)
	}
	if st, err := p.GetJob(ctx, "empty-result"); err != nil || st.State != video.StateFailed || !strings.Contains(st.Error, "without a video output URL") {
		t.Fatalf("completed without output = %+v, %v", st, err)
	}
	if st, err := p.GetJob(ctx, "completed"); err != nil || st.State != video.StateCompleted || len(st.Outputs) != 1 || st.Outputs[0].URI != "https://cdn.example.test/out.mp4" {
		t.Fatalf("completed = %+v, %v", st, err)
	}
}

func TestIssue662ExtractAggregatorVideoPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		payload  map[string]any
		wantURL  string
		wantType string
	}{
		{"video object", map[string]any{"video": map[string]any{"url": "https://x.test/v", "content_type": "video/mp4"}}, "https://x.test/v", "video/mp4"},
		{"output string", map[string]any{"output": "https://x.test/o"}, "https://x.test/o", ""},
		{"result object", map[string]any{"result": map[string]any{"url": "https://x.test/r"}}, "https://x.test/r", ""},
		{"video_url", map[string]any{"video_url": "https://x.test/vu"}, "https://x.test/vu", ""},
		{"url", map[string]any{"url": "https://x.test/u"}, "https://x.test/u", ""},
		{"video object wins over url", map[string]any{"video": map[string]any{"url": "https://x.test/v"}, "url": "https://x.test/u"}, "https://x.test/v", ""},
		{"unrecognized", map[string]any{"other": "x"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotType := extractAggregatorVideo(tc.payload)
			if gotURL != tc.wantURL || gotType != tc.wantType {
				t.Fatalf("extractAggregatorVideo = %q, %q; want %q, %q", gotURL, gotType, tc.wantURL, tc.wantType)
			}
		})
	}
}

func TestIssue662LooksLikeVideoContainerSignatures(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"mp4", []byte("\x00\x00\x00\x18ftypmp42payload"), true},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x00}, true},
		{"avi", []byte("RIFF\x00\x00\x00\x00AVI payload"), true},
		{"flv", []byte("FLV\x01payload"), true},
		{"mpeg-ps", []byte{0x00, 0x00, 0x01, 0xBA, 0x00}, true},
		{"mpeg-ts", append([]byte{0x47}, append(make([]byte, 187), 0x47)...), true},
		{"html", []byte("<html><body>no</body></html>"), false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeVideoContainer(tc.data); got != tc.want {
				t.Fatalf("looksLikeVideoContainer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIssue662AggregatorDownloadOutputsGuards(t *testing.T) {
	ctx := context.Background()
	videoBytes := []byte("\x00\x00\x00\x18ftypmp42-real-video")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/empty":
			w.Header().Set("Content-Type", "video/mp4")
		case "/http-error":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("not a video"))
		case "/spoof":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("<html>spoof</html>"))
		case "/ok":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := issue662AggProvider(t, issue662AggConfig())

	status := func(uri, ct string) video.ProviderJobStatus {
		return video.ProviderJobStatus{ProviderJobID: "id", Outputs: []video.Asset{{ID: "id-video", Kind: "video", URI: uri, ContentType: ct}}}
	}
	if _, err := p.DownloadOutputs(ctx, status("https://x.test/v", "video/mp4"), nil); err == nil || !strings.Contains(err.Error(), "sink is not configured") {
		t.Fatalf("nil sink error = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, video.ProviderJobStatus{}, &memSink{}); err == nil || !strings.Contains(err.Error(), "no video output assets") {
		t.Fatalf("empty outputs error = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, status("file:///tmp/v.mp4", "video/mp4"), &memSink{}); err == nil || !strings.Contains(err.Error(), "not an http(s) URL") {
		t.Fatalf("non-HTTP URI error = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, status("https://x.test/v", "text/plain"), &memSink{}); err == nil || !strings.Contains(err.Error(), "non-video output content type") {
		t.Fatalf("declared non-video error = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, status("http://example.com/v.mp4", "video/mp4"), &memSink{}); err == nil || !strings.Contains(err.Error(), "only allowed on loopback") {
		t.Fatalf("remote cleartext output error = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, status(srv.URL+"/http-error", "video/mp4"), &memSink{}); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("HTTP error output = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, status(srv.URL+"/empty", "video/mp4"), &memSink{}); err == nil || !strings.Contains(err.Error(), "empty video output") {
		t.Fatalf("empty output error = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, status(srv.URL+"/text", ""), &memSink{}); err == nil || !strings.Contains(err.Error(), "non-video content type") {
		t.Fatalf("text output error = %v", err)
	}
	if _, err := p.DownloadOutputs(ctx, status(srv.URL+"/spoof", "video/mp4"), &memSink{}); err == nil || !strings.Contains(err.Error(), "do not look like a video container") {
		t.Fatalf("spoofed output error = %v", err)
	}
	sink := &issue662CaptureSink{}
	saved, err := p.DownloadOutputs(ctx, status(srv.URL+"/ok", "video/mp4"), sink)
	if err != nil || len(saved) != 1 || string(sink.data) != string(videoBytes) || saved[0].ContentType != "video/mp4" {
		t.Fatalf("valid output: saved=%+v data=%q err=%v", saved, sink.data, err)
	}
	if _, err := p.DownloadOutputs(ctx, status(srv.URL+"/ok", "video/mp4"), &memSink{fail: true}); err == nil || !strings.Contains(err.Error(), "persist aggregator video output") {
		t.Fatalf("sink failure error = %v", err)
	}

	// The configured output ceiling is enforced before persistence.
	small, err := NewAggregator(issue662AggConfig(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := small.DownloadOutputs(ctx, status(srv.URL+"/ok", "video/mp4"), &memSink{}); err == nil || !strings.Contains(err.Error(), "exceeds configured asset limit") {
		t.Fatalf("oversize output error = %v", err)
	}
}

func TestIssue662AggregatorDoJSONErrorPaths(t *testing.T) {
	t.Setenv("ISSUE662_AGG_KEY", "k")
	p, err := NewAggregator(issue662AggConfig(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.doJSON(ctx, http.MethodGet, "://bad", nil, nil); err == nil {
		t.Fatal("invalid request URL accepted")
	}
	if err := p.doJSON(ctx, http.MethodPost, p.baseURL, func() {}, nil); err == nil {
		t.Fatal("unencodable payload accepted")
	}

	long := strings.Repeat("x", 300)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/long-error":
			http.Error(w, long, http.StatusBadGateway)
		case "/bad-json":
			_, _ = w.Write([]byte(`{not json`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p.baseURL = srv.URL
	var out map[string]any
	err = p.doJSON(ctx, http.MethodGet, srv.URL+"/long-error", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") || strings.Contains(err.Error(), strings.Repeat("x", 201)) {
		t.Fatalf("long error snippet not truncated: %v", err)
	}
	if err := p.doJSON(ctx, http.MethodGet, srv.URL+"/bad-json", nil, &out); err == nil || !strings.Contains(err.Error(), "decode aggregator response") {
		t.Fatalf("bad JSON error = %v", err)
	}
	if err := p.VerifyWebhook(nil); err == nil || !strings.Contains(err.Error(), "does not support webhooks") {
		t.Fatalf("VerifyWebhook error = %v", err)
	}
}

func issue662WorkflowFile(t *testing.T, workflow map[string]map[string]any) string {
	t.Helper()
	data, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIssue662ComfyUIValidateInputMissingInputsObject(t *testing.T) {
	path := issue662WorkflowFile(t, map[string]map[string]any{
		"1": {"class_type": "Broken", "inputs": "not-an-object"},
	})
	cfg := comfyTestConfig(path, "http://127.0.0.1:8188")
	if _, err := NewComfyUI(cfg, 1024); err == nil || !strings.Contains(err.Error(), "has no inputs object") {
		t.Fatalf("missing inputs object error = %v", err)
	}
}

func TestIssue662ComfyUICreateJobUsesShotPromptAndClampsFrames(t *testing.T) {
	var submitted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prompt" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
			t.Errorf("decode prompt: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"prompt_id":"shot-1"}`))
	}))
	defer srv.Close()
	cfg := comfyTestConfig(comfyWorkflowFile(t), srv.URL)
	p, err := NewComfyUI(cfg, 1024)
	if err != nil {
		t.Fatal(err)
	}
	req := video.VideoRequest{
		ProjectID: "p", Mode: video.ModeTextToVideo, DurationSeconds: 0.01, FPS: 1,
		AspectRatio: video.Aspect16x9,
		ShotPlan:    []video.Shot{{ShotID: "s1", Prompt: "  shot prompt  ", NegativePrompt: "shot negative", DurationSeconds: 0.01}},
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if job.ProviderJobID != "shot-1" {
		t.Fatalf("job = %+v", job)
	}
	workflow, ok := submitted["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("submitted prompt type = %T", submitted["prompt"])
	}
	nodeInputs := func(id string) map[string]any {
		t.Helper()
		node, ok := workflow[id].(map[string]any)
		if !ok {
			t.Fatalf("node %s = %T", id, workflow[id])
		}
		inputs, ok := node["inputs"].(map[string]any)
		if !ok {
			t.Fatalf("node %s inputs = %T", id, node["inputs"])
		}
		return inputs
	}
	if nodeInputs("1")["text"] != "shot prompt" || nodeInputs("4")["text"] != "shot negative" {
		t.Fatalf("shot prompt not injected: positive=%v negative=%v", nodeInputs("1")["text"], nodeInputs("4")["text"])
	}
	if nodeInputs("3")["length"] != float64(1) {
		t.Fatalf("frames = %v, want clamped 1", nodeInputs("3")["length"])
	}
}

func TestIssue662ComfyUIGetJobEmptyID(t *testing.T) {
	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:8188"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetJob(context.Background(), "  "); err == nil || !strings.Contains(err.Error(), "prompt ID is required") {
		t.Fatalf("empty prompt id error = %v", err)
	}
}

func TestIssue662ComfyUICollectOutputsAndQueueParsing(t *testing.T) {
	outputs := map[string]map[string]json.RawMessage{
		"10": {
			"videos": json.RawMessage(`[{"filename":"clip.mp4","subfolder":"renders","type":"output"},{"filename":"ignored.webm","subfolder":"","type":"output"}]`),
			"gifs":   json.RawMessage(`[{"filename":"anim.gif"}]`),
			"images": json.RawMessage(`[{"filename":"frame.mp4","subfolder":"frames","type":"temp"}]`),
		},
		"11": {
			"videos": json.RawMessage(`not-json`),
		},
	}
	assets := collectComfyVideoOutputs("prompt-1", outputs)
	if len(assets) != 2 {
		t.Fatalf("assets = %+v, want two MP4 outputs", assets)
	}
	for _, asset := range assets {
		if asset.Kind != "video" || !strings.HasSuffix(asset.Filename, ".mp4") {
			t.Fatalf("non-video output collected: %+v", asset)
		}
		if asset.Metadata["provider"] != "comfyui" || asset.Metadata["node_id"] == "" {
			t.Fatalf("missing provenance metadata: %+v", asset)
		}
	}

	items := [][]json.RawMessage{
		{},
		{json.RawMessage(`1`)},
		{json.RawMessage(`0`), json.RawMessage(`"target"`)},
		{json.RawMessage(`0`), json.RawMessage(`not-a-string`)},
	}
	if !queueContains(items, "target") {
		t.Fatal("valid queue entry not found")
	}
	if queueContains(items, "other") {
		t.Fatal("malformed queue entries must not match")
	}
}

func TestIssue662ComfyUIDownloadContentTypeHandling(t *testing.T) {
	var withParams bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if withParams {
			w.Header().Set("Content-Type", "video/mp4; charset=binary")
		}
		_, _ = w.Write([]byte("video-bytes"))
	}))
	defer srv.Close()
	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), srv.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	status := video.ProviderJobStatus{Outputs: []video.Asset{{ID: "a", Filename: "clip.mp4", Metadata: map[string]string{"filename": "clip.mp4", "type": "output"}}}}
	withParams = true
	sink := &issue662CaptureSink{}
	saved, err := p.DownloadOutputs(context.Background(), status, sink)
	if err != nil || len(saved) != 1 || saved[0].ContentType != "video/mp4" {
		t.Fatalf("content type params not normalized: saved=%+v err=%v", saved, err)
	}
}

func TestIssue662ComfyUIDoJSONErrorPaths(t *testing.T) {
	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:9"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.doJSON(context.Background(), http.MethodPost, "/prompt", func() {}, nil); err == nil {
		t.Fatal("unencodable payload accepted")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	p.baseURL = srv.URL
	var out map[string]any
	if err := p.doJSON(context.Background(), http.MethodGet, "/history/x", nil, &out); err == nil || !strings.Contains(err.Error(), "decode ComfyUI response") {
		t.Fatalf("bad JSON error = %v", err)
	}
}

func issue662ValidPlan() video.Plan {
	return video.Plan{
		SchemaVersion: video.PlanSchemaVersion, Title: "issue662", DurationSeconds: 1,
		Width: 320, Height: 240, FPS: 12,
		Scenes: []video.PlanScene{{ID: "a", DurationSeconds: 1, Background: "#101828", Caption: "hello"}},
	}
}

type issue662Planner struct {
	out string
	err error
}

type fakeNarrator struct{}

func (p issue662Planner) Complete(context.Context, string, string, string) (string, error) {
	return p.out, p.err
}

func (fakeNarrator) Synthesize(context.Context, string, string) error { return nil }

type issue662CaptureSink struct {
	asset video.Asset
	data  []byte
}

func (s *issue662CaptureSink) Put(_ context.Context, asset video.Asset, content []byte) (video.Asset, error) {
	s.asset = asset
	s.data = append([]byte(nil), content...)
	asset.SizeBytes = int64(len(content))
	return asset, nil
}

func TestIssue662CodeRenderNarrationAndHelpers(t *testing.T) {
	plain := NewCodeRender(nil, &render.Renderer{}, nil, t.TempDir(), []string{"m"}, 0)
	if plain.HandlesNarration() {
		t.Fatal("provider without a narrator must not claim narration")
	}
	if plain.MaxDurationSeconds != 120 {
		t.Fatalf("default max duration = %v, want 120", plain.MaxDurationSeconds)
	}
	narrated := NewCodeRender(nil, &render.Renderer{}, fakeNarrator{}, t.TempDir(), []string{"m"}, 60)
	if !narrated.HandlesNarration() {
		t.Fatal("provider with a narrator must report narration handling")
	}
	caps, err := narrated.Capabilities(context.Background())
	if err != nil || !caps.SupportsNarration || !caps.SupportsSubtitles || !caps.SupportsAudio {
		t.Fatalf("narrated capabilities = %+v, %v", caps, err)
	}

	if got := modeOrDefault("  "); got != "auto" {
		t.Fatalf("modeOrDefault(empty) = %q", got)
	}
	if got := modeOrDefault("native"); got != "native" {
		t.Fatalf("modeOrDefault(native) = %q", got)
	}
	if got := planLanguage(video.VideoRequest{SubtitleLanguage: " fa "}); got != " fa " {
		t.Fatalf("planLanguage(subtitle) = %q", got)
	}
	if got := planLanguage(video.VideoRequest{}); got != "" {
		t.Fatalf("planLanguage(empty) = %q", got)
	}
	width, height := requestDimensions(video.VideoRequest{Width: 640, Height: 360})
	if width != 640 || height != 360 {
		t.Fatalf("explicit dimensions = %dx%d", width, height)
	}
	width, height = requestDimensions(video.VideoRequest{AspectRatio: video.Aspect9x16})
	if width != 720 || height != 1280 {
		t.Fatalf("9:16 dimensions = %dx%d", width, height)
	}
	width, height = requestDimensions(video.VideoRequest{AspectRatio: video.Aspect1x1})
	if width != 960 || height != 960 {
		t.Fatalf("1:1 dimensions = %dx%d", width, height)
	}
	width, height = requestDimensions(video.VideoRequest{})
	if width != 1280 || height != 720 {
		t.Fatalf("default dimensions = %dx%d", width, height)
	}
	if firstNonEmpty(" ", "fallback") != "fallback" || firstNonEmpty("value", "fallback") != "value" {
		t.Fatal("firstNonEmpty precedence broken")
	}
	t.Setenv("NEXAROUTE_FFMPEG", "custom-ffmpeg")
	if got := ffmpegBinary(); got != "custom-ffmpeg" {
		t.Fatalf("ffmpegBinary(env) = %q", got)
	}
	t.Setenv("NEXAROUTE_FFMPEG", " ")
	if got := ffmpegBinary(); got != "ffmpeg" {
		t.Fatalf("ffmpegBinary(default) = %q", got)
	}

	if got := copyUsage(nil); got != nil {
		t.Fatalf("copyUsage(nil) = %v", got)
	}
	if got := copyUsage(map[string]string{}); got != nil {
		t.Fatalf("copyUsage(empty) = %v", got)
	}
	original := map[string]string{"a": "b"}
	copied := copyUsage(original)
	original["a"] = "changed"
	if copied["a"] != "b" {
		t.Fatalf("copyUsage aliased the source map: %v", copied)
	}
}

func TestIssue662CodeRenderValidateRequestMatrix(t *testing.T) {
	p := NewCodeRender(nil, &render.Renderer{}, nil, t.TempDir(), []string{"m"}, 120)
	base := video.VideoRequest{ProjectID: "p", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3, AspectRatio: video.Aspect16x9, ModelPreference: "m"}
	cases := []struct {
		name   string
		mutate func(*video.VideoRequest)
		want   string
	}{
		{"unsupported creation mode", func(r *video.VideoRequest) { r.CreationMode = "hologram" }, "unsupported creation_mode"},
		{"native creation mode", func(r *video.VideoRequest) { r.CreationMode = "native" }, "requires a native generative"},
		{"non text mode", func(r *video.VideoRequest) { r.Mode = video.ModeImageToVideo }, "supports text_to_video only"},
		{"shot plan", func(r *video.VideoRequest) { r.ShotPlan = []video.Shot{{ShotID: "a", Prompt: "a", DurationSeconds: 1}} }, "does not accept shot_plan"},
		{"reference asset", func(r *video.VideoRequest) { r.ReferenceAssets = []video.Asset{{ID: "r"}} }, "reference/first/last-frame"},
		{"dialogue", func(r *video.VideoRequest) { r.DialogueRequested = true }, "does not generate dialogue"},
		{"sound", func(r *video.VideoRequest) { r.SoundRequirements = []string{"wind"} }, "does not generate music"},
		{"audio without narrator", func(r *video.VideoRequest) { r.AudioRequested = true }, "requires a configured local Piper narrator"},
		{"narration without narrator", func(r *video.VideoRequest) { r.NarrationText = "hello" }, "requires a configured local Piper narrator"},
		{"callback", func(r *video.VideoRequest) { r.CallbackURL = "https://example.test/hook" }, "does not support provider callbacks"},
		{"invalid plan", func(r *video.VideoRequest) {
			plan := issue662ValidPlan()
			plan.Title = ""
			r.VideoPlan = &plan
		}, "video_plan is invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			if err := p.ValidateRequest(req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRequest error = %v, want %q", err, tc.want)
			}
		})
	}
	validPlan := issue662ValidPlan()
	valid := base
	valid.VideoPlan = &validPlan
	if err := p.ValidateRequest(valid); err != nil {
		t.Fatalf("valid explicit plan rejected: %v", err)
	}
	if _, err := p.EstimateCost(video.VideoRequest{}); err == nil {
		t.Fatal("EstimateCost accepted an invalid request")
	}
}

func TestIssue662CodeRenderResolvePlanBranches(t *testing.T) {
	ctx := context.Background()
	req := video.VideoRequest{ProjectID: "p", Prompt: "brief", Mode: video.ModeTextToVideo, DurationSeconds: 3, ModelPreference: "m", SubtitleLanguage: "fa", AspectRatio: video.Aspect9x16}

	noAgent := NewCodeRender(nil, &render.Renderer{}, nil, t.TempDir(), []string{"m"}, 120)
	if _, _, err := noAgent.resolvePlan(ctx, req); err == nil || !strings.Contains(err.Error(), "no planning model") {
		t.Fatalf("no agent error = %v", err)
	}

	plan := issue662ValidPlan()
	explicit := req
	explicit.VideoPlan = &plan
	got, attempts, err := noAgent.resolvePlan(ctx, explicit)
	if err != nil || attempts != 0 || got.Title != plan.Title {
		t.Fatalf("explicit plan = %+v attempts=%d err=%v", got, attempts, err)
	}

	plannerErr := NewCodeRender(&agent.Agent{Planner: issue662Planner{err: errors.New("planner down")}, MaxAttempts: 1}, &render.Renderer{}, nil, t.TempDir(), []string{"m"}, 120)
	if _, _, err := plannerErr.resolvePlan(ctx, req); err == nil || !strings.Contains(err.Error(), "planner down") {
		t.Fatalf("planner failure error = %v", err)
	}
}

func TestIssue662CodeRenderCreateJobFailsBeforeRendering(t *testing.T) {
	base := video.VideoRequest{ProjectID: "p", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3, AspectRatio: video.Aspect16x9, ModelPreference: "m"}
	noRenderer := NewCodeRender(nil, nil, nil, t.TempDir(), []string{"m"}, 120)
	if _, err := noRenderer.CreateJob(context.Background(), base); err == nil || !strings.Contains(err.Error(), "renderer is not configured") {
		t.Fatalf("nil renderer error = %v", err)
	}
	missingFFmpeg := NewCodeRender(nil, &render.Renderer{FFmpeg: filepath.Join(t.TempDir(), "missing-ffmpeg")}, nil, t.TempDir(), []string{"m"}, 120)
	if _, err := missingFFmpeg.CreateJob(context.Background(), base); err == nil || !strings.Contains(err.Error(), "code-render preflight") {
		t.Fatalf("preflight error = %v", err)
	}
}

func TestIssue662CodeRenderRunFailsClosedAndHonorsCancellation(t *testing.T) {
	base := video.VideoRequest{ProjectID: "p", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3, AspectRatio: video.Aspect16x9, ModelPreference: "m"}

	newJob := func() *codeRenderJob {
		ctx, cancel := context.WithCancel(context.Background())
		_ = ctx
		return &codeRenderJob{
			ctx: ctx, cancel: cancel, done: make(chan struct{}), state: video.StateProcessing,
			plan: issue662ValidPlan(), usage: map[string]string{"tools_used": "test"},
		}
	}

	noRoot := NewCodeRender(nil, &render.Renderer{}, nil, "", []string{"m"}, 120)
	job := newJob()
	noRoot.run(context.Background(), job, base)
	job.mu.Lock()
	state, msg, finished := job.state, job.err, job.finished
	job.mu.Unlock()
	if state != video.StateFailed || !strings.Contains(msg, "work root is not configured") || finished.IsZero() {
		t.Fatalf("missing work root: state=%s err=%q finished=%v", state, msg, finished)
	}

	fileRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badRoot := NewCodeRender(nil, &render.Renderer{}, nil, fileRoot, []string{"m"}, 120)
	job = newJob()
	badRoot.run(context.Background(), job, base)
	job.mu.Lock()
	state, msg = job.state, job.err
	job.mu.Unlock()
	if state != video.StateFailed || !strings.Contains(msg, "work root") {
		t.Fatalf("unusable work root: state=%s err=%q", state, msg)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := NewCodeRender(nil, &render.Renderer{FFmpeg: filepath.Join(t.TempDir(), "missing-ffmpeg")}, nil, t.TempDir(), []string{"m"}, 120)
	job = newJob()
	cancelled.run(cancelCtx, job, base)
	job.mu.Lock()
	state = job.state
	job.mu.Unlock()
	if state != video.StateCancelled {
		t.Fatalf("canceled context produced state %s, want cancelled", state)
	}
}

func TestIssue662CodeRenderJobStatusCancellationAndDownload(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "episode.mp4")
	if err := os.WriteFile(output, []byte("\x00\x00\x00\x18ftypmp42payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	completed := &codeRenderJob{
		done: done, cancel: func() {}, state: video.StateCompleted, progress: 1, output: output,
		usage: map[string]string{"planning_model": "m"},
	}
	processingDone := make(chan struct{})
	close(processingDone)
	processing := &codeRenderJob{done: processingDone, state: video.StateProcessing, cancel: func() {}, usage: map[string]string{}}
	p := &CodeRender{jobs: map[string]*codeRenderJob{"done": completed, "processing": processing}, MaxAssetBytes: 1 << 20}

	st, err := p.GetJob(context.Background(), "done")
	if err != nil || st.State != video.StateCompleted || len(st.Outputs) != 1 || st.Usage["planning_model"] != "m" {
		t.Fatalf("completed status = %+v, %v", st, err)
	}
	if err := p.CancelJob(context.Background(), "done"); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.GetJob(context.Background(), "done"); st.State != video.StateCompleted {
		t.Fatalf("cancel downgraded a completed job: %+v", st)
	}
	if err := p.CancelJob(context.Background(), "processing"); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.GetJob(context.Background(), "processing"); st.State != video.StateCancelled {
		t.Fatalf("processing job not cancelled: %+v", st)
	}

	sink := &issue662CaptureSink{}
	saved, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{
		ProviderJobID: "done",
		Outputs:       []video.Asset{{ID: "done-output", Kind: "video", Metadata: map[string]string{"project_id": "p"}}},
	}, sink)
	if err != nil || len(saved) != 1 || sink.asset.Metadata["project_id"] != "p" {
		t.Fatalf("download = %+v, %v; metadata=%v", saved, err, sink.asset.Metadata)
	}

	if _, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{ProviderJobID: "missing"}, sink); err == nil || !strings.Contains(err.Error(), "unknown code-render job") {
		t.Fatalf("unknown job error = %v", err)
	}
	if _, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{ProviderJobID: "done"}, nil); err == nil || !strings.Contains(err.Error(), "sink is not configured") {
		t.Fatalf("nil sink error = %v", err)
	}
	empty := &codeRenderJob{done: done, state: video.StateCompleted, usage: map[string]string{}}
	p.jobs["empty"] = empty
	if _, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{ProviderJobID: "empty"}, sink); err == nil || !strings.Contains(err.Error(), "no validated output") {
		t.Fatalf("no output error = %v", err)
	}
	missingFile := &codeRenderJob{done: done, state: video.StateCompleted, output: filepath.Join(root, "missing.mp4"), usage: map[string]string{}}
	p.jobs["missing-file"] = missingFile
	if _, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{ProviderJobID: "missing-file"}, sink); err == nil || !strings.Contains(err.Error(), "read rendered output") {
		t.Fatalf("unreadable output error = %v", err)
	}
	emptyFile := filepath.Join(root, "empty.mp4")
	if err := os.WriteFile(emptyFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.jobs["empty-file"] = &codeRenderJob{done: done, state: video.StateCompleted, output: emptyFile, usage: map[string]string{}}
	if _, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{ProviderJobID: "empty-file"}, sink); err == nil || !strings.Contains(err.Error(), "rendered output is empty") {
		t.Fatalf("empty output file error = %v", err)
	}
	p.MaxAssetBytes = 4
	if _, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{ProviderJobID: "done"}, sink); err == nil || !strings.Contains(err.Error(), "exceeds the configured asset size limit") {
		t.Fatalf("oversize output error = %v", err)
	}
	p.MaxAssetBytes = 1 << 20
	if _, err := p.DownloadOutputs(context.Background(), video.ProviderJobStatus{ProviderJobID: "done"}, &memSink{fail: true}); err == nil || !strings.Contains(err.Error(), "sink exploded") {
		t.Fatalf("sink failure error = %v", err)
	}
	if err := p.VerifyWebhook(nil); err == nil || !strings.Contains(err.Error(), "does not support webhooks") {
		t.Fatalf("VerifyWebhook error = %v", err)
	}
}

func TestIssue662CodeRenderPruneKeepsTerminalWithoutFinishTime(t *testing.T) {
	now := time.Now()
	dir := mustWorkDir(t, t.TempDir(), "no-finish")
	p := &CodeRender{
		Retention: time.Hour,
		jobs: map[string]*codeRenderJob{
			"no-finish": newRetentionJob(video.StateCompleted, time.Time{}, dir),
		},
	}
	if n := p.PruneTerminal(now); n != 0 {
		t.Fatalf("pruned = %d, want 0 for a terminal job without a finish time", n)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("work directory removed despite no finish time: %v", err)
	}
}
