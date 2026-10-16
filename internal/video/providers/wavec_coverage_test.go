package providers

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Constructor edge cases beyond the existing remote-HTTP coverage:
// credential/query/fragment URLs, empty workflow path, empty and
// oversize workflow files, malformed workflow JSON, and duration bounds.
func TestComfyUIConstructorEdgeCases(t *testing.T) {
	path := comfyWorkflowFile(t)
	good := func() video.Config { return comfyTestConfig(path, "http://127.0.0.1:8188") }
	for name, mutate := range map[string]func(*video.Config){
		"user info":      func(c *video.Config) { c.ComfyUIBaseURL = "http://user@127.0.0.1:8188" },
		"query string":   func(c *video.Config) { c.ComfyUIBaseURL = "http://127.0.0.1:8188/?x=1" },
		"fragment":       func(c *video.Config) { c.ComfyUIBaseURL = "http://127.0.0.1:8188/#f" },
		"not a url":      func(c *video.Config) { c.ComfyUIBaseURL = "://bad" },
		"missing scheme": func(c *video.Config) { c.ComfyUIBaseURL = "127.0.0.1:8188" },
		"empty workflow": func(c *video.Config) { c.ComfyUIWorkflowPath = "" },
		"bad duration":   func(c *video.Config) { c.ComfyUIMaxDurationSeconds = 99999 },
		"negative dur":   func(c *video.Config) { c.ComfyUIMaxDurationSeconds = -5 },
		"http hostname":  func(c *video.Config) { c.ComfyUIBaseURL = "http://localhost:8188" },
	} {
		cfg := good()
		mutate(&cfg)
		_, err := NewComfyUI(cfg, 1024)
		switch name {
		case "http hostname":
			if err != nil {
				t.Errorf("%s rejected: %v", name, err)
			}
		default:
			if err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	}
	// Empty workflow file.
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewComfyUI(comfyTestConfig(empty, "http://127.0.0.1:8188"), 1024); err == nil {
		t.Error("empty workflow accepted")
	}
	// Malformed workflow JSON.
	junk := filepath.Join(t.TempDir(), "junk.json")
	if err := os.WriteFile(junk, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewComfyUI(comfyTestConfig(junk, "http://127.0.0.1:8188"), 1024); err == nil {
		t.Error("malformed workflow accepted")
	}
	// Oversize workflow (sparse file: no 8MiB write cost).
	big := filepath.Join(t.TempDir(), "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((8 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := NewComfyUI(comfyTestConfig(big, "http://127.0.0.1:8188"), 1024); err == nil {
		t.Error("oversize workflow accepted")
	}
	// Zero maxOutputBytes falls back to the default instead of failing.
	p, err := NewComfyUI(good(), 0)
	if err != nil || p == nil {
		t.Fatalf("zero maxOutputBytes: %v", err)
	}
	// HTTPS to a link-local/metadata address is refused by the egress
	// policy at construction (fail-closed, before any dial).
	cfg := good()
	cfg.ComfyUIBaseURL = "https://169.254.169.254/"
	if _, err := NewComfyUI(cfg, 1024); err == nil {
		t.Error("metadata address accepted")
	}
}

func validComfyRequest() video.VideoRequest {
	return video.VideoRequest{
		ProjectID: "proj-1", Mode: video.ModeTextToVideo, Prompt: "a cat", DurationSeconds: 5,
		OutputFormat: "mp4", AspectRatio: video.Aspect16x9, Width: 1280, Height: 720,
	}
}

// The request-validation matrix: every unsupported dimension is rejected
// explicitly instead of being silently ignored.
func TestComfyUIValidateRequestMatrix(t *testing.T) {
	path := comfyWorkflowFile(t)
	p, err := NewComfyUI(comfyTestConfig(path, "http://127.0.0.1:8188"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.ValidateRequest(validComfyRequest()); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	base := validComfyRequest()
	cases := map[string]func(*video.VideoRequest){
		"image mode":      func(r *video.VideoRequest) { r.Mode = video.ModeImageToVideo },
		"too long":        func(r *video.VideoRequest) { r.DurationSeconds = 600 },
		"wrong model":     func(r *video.VideoRequest) { r.ModelPreference = "other" },
		"audio requested": func(r *video.VideoRequest) { r.AudioRequested = true },
		"narration":       func(r *video.VideoRequest) { r.NarrationText = "speak" },
		"subtitles":       func(r *video.VideoRequest) { r.SubtitleLanguage = "en" },
		"multi-shot": func(r *video.VideoRequest) {
			r.ShotPlan = []video.Shot{
				{ShotID: "s1", Prompt: "a", DurationSeconds: 2},
				{ShotID: "s2", Prompt: "b", DurationSeconds: 2},
			}
		},
		"empty prompt":      func(r *video.VideoRequest) { r.Prompt = "" },
		"reference asset":   func(r *video.VideoRequest) { r.ReferenceAssets = []video.Asset{{ID: "x"}} },
		"continuity":        func(r *video.VideoRequest) { r.ContinuityContext = &video.ContinuityContext{} },
		"callback":          func(r *video.VideoRequest) { r.CallbackURL = "https://x.test/hook" },
		"webm output":       func(r *video.VideoRequest) { r.OutputFormat = "webm" },
		"custom dims unset": func(r *video.VideoRequest) { r.AspectRatio = video.AspectCustom; r.Width = 0 },
		"tiny dims":         func(r *video.VideoRequest) { r.AspectRatio = video.AspectCustom; r.Width, r.Height = 32, 32 },
		"huge dims":         func(r *video.VideoRequest) { r.AspectRatio = video.AspectCustom; r.Width, r.Height = 5000, 5000 },
	}
	for name, mutate := range cases {
		req := base
		mutate(&req)
		if err := p.ValidateRequest(req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A single valid shot supplies the prompt when the top-level prompt
	// is empty, and its negative prompt is honored.
	single := base
	single.Prompt = ""
	single.ShotPlan = []video.Shot{{ShotID: "s1", Prompt: "shot prompt", DurationSeconds: 2, NegativePrompt: "ugly"}}
	if err := p.ValidateRequest(single); err != nil {
		t.Fatalf("shot-plan prompt: %v", err)
	}
	// Constructor defaults: empty model name and zero duration budget.
	defCfg := comfyTestConfig(path, "http://127.0.0.1:8188")
	defCfg.ComfyUIModel = ""
	defCfg.ComfyUIMaxDurationSeconds = 0
	pd, err := NewComfyUI(defCfg, 1024)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	tooLong := validComfyRequest()
	tooLong.DurationSeconds = 61
	if err := pd.ValidateRequest(tooLong); err == nil || !strings.Contains(err.Error(), "60.00") {
		t.Fatalf("default duration budget: %v", err)
	}
	// A configured-but-missing optional node fails construction.
	optCfg := comfyTestConfig(path, "http://127.0.0.1:8188")
	optCfg.ComfyUINegativeNodeID = "missing"
	if _, err := NewComfyUI(optCfg, 1024); err == nil || !strings.Contains(err.Error(), "no configured negative prompt node") {
		t.Fatalf("optional node wiring: %v", err)
	}
	// EstimateCost inherits validation and prices a self-hosted run at 0.
	est, err := p.EstimateCost(validComfyRequest())
	if err != nil || !est.PriceKnown || est.EstimatedUSD != 0 || est.BillableSeconds != 5 {
		t.Fatalf("estimate: %+v %v", est, err)
	}
	if _, err := p.EstimateCost(video.VideoRequest{}); err == nil {
		t.Error("estimate on invalid request accepted")
	}
	// The webhook surface is explicitly unsupported, not half-implemented.
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/hook", nil)
	if err := p.RegisterWebhook(ctx, video.WebhookRegistration{}); err == nil {
		t.Error("RegisterWebhook accepted")
	}
	if err := p.VerifyWebhook(req); err == nil || !strings.Contains(err.Error(), "does not support webhooks") {
		t.Errorf("VerifyWebhook: %v", err)
	}
	if _, err := p.ParseWebhook(req); err == nil || !strings.Contains(err.Error(), "does not support webhooks") {
		t.Errorf("ParseWebhook: %v", err)
	}
}

// defaultInput/validateInput: workflow node wiring errors are explicit.
func TestComfyUIInputWiring(t *testing.T) {
	if defaultInput("  x  ", "fb") != "x" || defaultInput("   ", "fb") != "fb" {
		t.Fatal("defaultInput")
	}
	path := comfyWorkflowFile(t)
	p, err := NewComfyUI(comfyTestConfig(path, "http://127.0.0.1:8188"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.validateInput("1", "text", "prompt"); err != nil {
		t.Fatalf("valid wiring: %v", err)
	}
	for name, args := range map[string][3]string{
		"missing node":  {"nope", "text", "prompt"},
		"missing input": {"1", "nope", "prompt"},
	} {
		if err := p.validateInput(args[0], args[1], args[2]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The fake provider's actual-cost path reports realized spend to the caller.
func TestFakeProviderActualCost(t *testing.T) {
	cost := 0.042
	f := NewFake(1)
	f.ActualCostUSD = &cost
	ctx := context.Background()
	job, err := f.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.GetJob(ctx, job.ProviderJobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != video.StateCompleted || status.ActualCostUSD == nil || *status.ActualCostUSD != cost {
		t.Fatalf("actual cost: %+v", status)
	}
	if status.Usage["actual_cost_usd"] == "" {
		t.Fatalf("usage missing actual_cost_usd: %v", status.Usage)
	}
	// Without an actual cost, no usage annotation is synthesized.
	f2 := NewFake(1)
	job2, _ := f2.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5})
	st2, _ := f2.GetJob(ctx, job2.ProviderJobID)
	if st2.ActualCostUSD != nil || len(st2.Usage) != 0 {
		t.Fatalf("phantom cost: %+v", st2)
	}
}
