package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func comfyAcceptWorkflow(t *testing.T) string {
	t.Helper()
	workflow := map[string]map[string]any{
		"1": {"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "old prompt"}},
		"2": {"class_type": "EmptyImage", "inputs": map[string]any{"width": 512, "height": 512, "batch_size": 1}},
		"3": {"class_type": "VideoSampler", "inputs": map[string]any{"length": 16, "frame_rate": 24, "seed": 1}},
		"4": {"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "old negative"}},
	}
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

func comfyAcceptConfig(t *testing.T, workflowPath, baseURL string) string {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Video = config.VideoConfig{
		Enabled: true, AuthTokenEnv: "NEXAROUTE_TEST_VIDEO_TOKEN",
		ComfyUIEnabled: true, ComfyUIBaseURL: baseURL, ComfyUIWorkflowPath: workflowPath,
		ComfyUIModel: "local-video", ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}
	return writeTestConfig(t, cfg)
}

func comfyAcceptProbe(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ncat <<'JSON'\n" +
		`{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720}],"format":{"duration":"1.0"}}` +
		"\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func comfyAcceptMP4() []byte {
	return []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\x00\x00\x08free")
}

func runComfyAcceptTest(t *testing.T, args ...string) (int, comfyAcceptReport, string, string, string) {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "report.json")
	assetDir := filepath.Join(t.TempDir(), "assets")
	full := append([]string{"--out", reportPath, "--asset-dir", assetDir}, args...)
	var stdout, stderr bytes.Buffer
	code := runComfyUIAccept(full, &stdout, &stderr)
	report := comfyAcceptReport{}
	if raw, err := os.ReadFile(reportPath); err == nil {
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Fatalf("decode report: %v\n%s", err, raw)
		}
	}
	return code, report, reportPath, stdout.String(), stderr.String()
}

func comfyAcceptCheckStatus(report comfyAcceptReport, id string) (string, string) {
	for _, check := range report.Checks {
		if check.ID == id {
			return check.Status, check.Reason
		}
	}
	return "", ""
}

func TestComfyUIAcceptRequiresConfirmLiveAndTouchesNothing(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runComfyUIAccept([]string{"--config", configPath, "--out", reportPath}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d want 2\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("unconfirmed run made %d upstream call(s)", calls.Load())
	}
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed run wrote a report: %v", err)
	}
	if !strings.Contains(stderr.String(), "--confirm-live") {
		t.Fatalf("missing opt-in diagnostic: %s", stderr.String())
	}
}

func TestComfyUIAcceptMarksUnprovisionedInferenceBlocked(t *testing.T) {
	configPath := writeTestConfig(t, config.Default())
	code, report, reportPath, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t))
	if code != 3 {
		t.Fatalf("exit=%d want 3\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Schema != comfyAcceptSchema || report.Outcome != externalStatusBlocked || report.InferenceExecuted {
		t.Fatalf("report=%+v", report)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:preflight"); status != externalStatusBlocked {
		t.Fatalf("preflight status=%q", status)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusNotRun {
		t.Fatalf("inference status=%q, want NOT_RUN", status)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("blocked run did not write an honest report: %v", err)
	}
}

func TestComfyUIAcceptUnavailableServerFailsClosed(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := closed.URL
	closed.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), base)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t), "--timeout", "2s")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Outcome != externalStatusFail || report.InferenceExecuted {
		t.Fatalf("report outcome=%s inference=%t", report.Outcome, report.InferenceExecuted)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:system_stats"); status != externalStatusFail {
		t.Fatalf("system stats status=%q", status)
	}
	if report.Target.AssetDirectory != "" {
		if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
			t.Fatalf("no asset workspace should survive, stat err=%v", err)
		}
	}
}

func TestComfyUIAcceptInvalidNodeMappingNeverContactsServer(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Video = config.VideoConfig{
		Enabled: true, AuthTokenEnv: "NEXAROUTE_TEST_VIDEO_TOKEN",
		ComfyUIEnabled: true, ComfyUIBaseURL: server.URL, ComfyUIWorkflowPath: comfyAcceptWorkflow(t),
		ComfyUIModel: "local-video", ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID: "1", ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "99",
	}
	configPath := writeTestConfig(t, cfg)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t))
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid mapping still contacted the server %d time(s)", calls.Load())
	}
	status, reason := comfyAcceptCheckStatus(report, "comfyui:preflight")
	if status != externalStatusFail || !strings.Contains(reason, "no configured frames node") {
		t.Fatalf("preflight status=%q reason=%q", status, reason)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusNotRun {
		t.Fatalf("inference status=%q, want NOT_RUN", status)
	}
}

func TestComfyUIAcceptTimeoutCleansUpPendingPrompt(t *testing.T) {
	var deleted atomic.Value
	var promptCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			promptCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-timeout"}`))
		case r.URL.Path == "/history/prompt-timeout":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/queue" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[[1,"prompt-timeout"]]}`))
		case r.URL.Path == "/queue" && r.Method == http.MethodPost:
			var body struct {
				Delete []string `json:"delete"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Delete) > 0 {
				deleted.Store(body.Delete[0])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "300ms", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if promptCalls.Load() != 1 || !report.InferenceExecuted {
		t.Fatalf("prompt calls=%d inference=%t", promptCalls.Load(), report.InferenceExecuted)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:poll"); status != externalStatusFail || !strings.Contains(reason, "timed out") {
		t.Fatalf("poll status=%q reason=%q", status, reason)
	}
	if report.Cleanup == nil || report.Cleanup.RemoteStatus != externalStatusPass ||
		report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup=%+v", report.Cleanup)
	}
	if got, _ := deleted.Load().(string); got != "prompt-timeout" {
		t.Fatalf("remote prompt deleted=%q", got)
	}
	if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
		t.Fatalf("partial asset workspace survived timeout: %v", err)
	}
}

func TestComfyUIAcceptOversizedOutputLeavesNoPartialAsset(t *testing.T) {
	videoBytes := bytes.Repeat([]byte("x"), 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-large"}`))
		case r.URL.Path == "/history/prompt-large":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt-large":{"outputs":{"10":{"videos":[{"filename":"large.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/view":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms", "--max-output-bytes", "16")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:download"); status != externalStatusFail ||
		!strings.Contains(reason, "exceeds configured asset limit") {
		t.Fatalf("download status=%q reason=%q", status, reason)
	}
	if len(report.Assets) != 0 {
		t.Fatalf("oversized run reported assets: %+v", report.Assets)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup=%+v", report.Cleanup)
	}
	if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
		t.Fatalf("partial oversized asset workspace survived: %v", err)
	}
}

func TestComfyUIAcceptCompletedWithoutMP4FailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-gif"}`))
		case r.URL.Path == "/history/prompt-gif":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt-gif":{"outputs":{"10":{"gifs":[{"filename":"preview.gif","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/queue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:poll"); status != externalStatusFail ||
		!strings.Contains(reason, "without an MP4 output") {
		t.Fatalf("poll status=%q reason=%q", status, reason)
	}
	if len(report.Assets) != 0 {
		t.Fatalf("fail-closed job reported assets: %+v", report.Assets)
	}
}

func TestComfyUIAcceptFailedInferenceFailsClosedWithoutResubmit(t *testing.T) {
	var promptCalls atomic.Int64
	var deleted atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			promptCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-fail"}`))
		case r.URL.Path == "/history/prompt-fail":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt-fail":{"outputs":{},"status":{"status_str":"error","completed":false}}}`))
		case r.URL.Path == "/queue" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[[1,"prompt-fail"]]}`))
		case r.URL.Path == "/queue" && r.Method == http.MethodPost:
			var body struct {
				Delete []string `json:"delete"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Delete) > 0 {
				deleted.Store(body.Delete[0])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Outcome != externalStatusFail || !report.InferenceExecuted {
		t.Fatalf("report outcome=%s inference=%t", report.Outcome, report.InferenceExecuted)
	}
	if report.Job == nil || report.Job.State != "failed" {
		t.Fatalf("job=%+v, want state failed", report.Job)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusFail {
		t.Fatalf("inference status=%q, want FAIL (a failed job must never read as complete)", status)
	}
	if len(report.Assets) != 0 {
		t.Fatalf("failed job reported assets: %+v", report.Assets)
	}
	if promptCalls.Load() != 1 {
		t.Fatalf("prompt submissions=%d, want exactly 1 (polling must never resubmit)", promptCalls.Load())
	}
	if got, _ := deleted.Load().(string); got != "prompt-fail" {
		t.Fatalf("remote prompt deleted=%q", got)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup=%+v", report.Cleanup)
	}
	if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
		t.Fatalf("partial asset workspace survived failed inference: %v", err)
	}
}

func TestComfyUIAcceptTransientPollErrorDoesNotResubmit(t *testing.T) {
	var promptCalls atomic.Int64
	var historyCalls atomic.Int64
	videoBytes := comfyAcceptMP4()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			promptCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-transient"}`))
		case r.URL.Path == "/history/prompt-transient":
			if historyCalls.Add(1) == 1 {
				// First poll observation fails transiently. The harness
				// must fall back to the queue, not submit a second prompt.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt-transient":{"outputs":{"10":{"videos":[{"filename":"accept.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/queue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[[1,"prompt-transient"]],"queue_pending":[]}`))
		case r.URL.Path == "/view":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms")
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if promptCalls.Load() != 1 {
		t.Fatalf("prompt submissions=%d, want exactly 1 despite the transient poll error", promptCalls.Load())
	}
	if historyCalls.Load() < 2 {
		t.Fatalf("history polls=%d, want at least 2", historyCalls.Load())
	}
	if len(report.Assets) != 1 {
		t.Fatalf("assets=%+v", report.Assets)
	}
}

func TestComfyUIAcceptSuccessPersistsVerifiedAsset(t *testing.T) {
	videoBytes := comfyAcceptMP4()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"os":"posix","comfyui_version":"0.3.40","python_version":"3.11.9","pytorch_version":"2.5.1","ram_total":1024,"ram_free":512},"devices":[{"name":"cuda:0","type":"cuda","index":0,"vram_total":2048,"vram_free":1024}]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			var submitted map[string]any
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("decode prompt: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-ok"}`))
		case r.URL.Path == "/history/prompt-ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt-ok":{"outputs":{"10":{"videos":[{"filename":"accept.mp4","subfolder":"renders","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/view":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, reportPath, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms")
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Schema != comfyAcceptSchema || report.Outcome != externalStatusPass ||
		!report.InferenceExecuted || report.EvidenceClass != "loopback" {
		t.Fatalf("report=%+v", report)
	}
	// Fixture labeling pin: a loopback success must carry, in the
	// report itself, the limitation that its output is harness
	// evidence only and not real model inference. Without this, a
	// PASS + inference_executed=true report could be misread as a
	// live acceptance result.
	pinned := false
	for _, lim := range report.Limitations {
		if strings.Contains(lim, "loopback") && strings.Contains(lim, "harness evidence only") {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("fixture success report lacks the loopback-not-real-inference limitation: %v", report.Limitations)
	}
	if report.Server == nil || report.Server.ComfyUIVersion != "0.3.40" || len(report.Server.Devices) != 1 {
		t.Fatalf("server metadata=%+v", report.Server)
	}
	if len(report.Assets) != 1 {
		t.Fatalf("assets=%+v", report.Assets)
	}
	asset := report.Assets[0]
	sum := sha256.Sum256(videoBytes)
	if asset.SizeBytes != int64(len(videoBytes)) || !strings.EqualFold(asset.SHA256, hex.EncodeToString(sum[:])) {
		t.Fatalf("asset integrity=%+v", asset)
	}
	if asset.Metadata["provider"] != "comfyui" || asset.Metadata["node_id"] != "10" || asset.Metadata["filename"] != "accept.mp4" {
		t.Fatalf("asset metadata=%+v", asset.Metadata)
	}
	if asset.Probe == nil || asset.Probe.Codec != "h264" || asset.Probe.Width != 1280 || asset.Probe.Height != 720 {
		t.Fatalf("probe=%+v", asset.Probe)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:asset_path"); status != externalStatusPass {
		t.Fatalf("asset_path status=%q, want PASS", status)
	}
	if !strings.HasPrefix(asset.URI, report.Target.AssetDirectory+string(filepath.Separator)) {
		t.Fatalf("asset URI %q is outside the acceptance asset root %q", asset.URI, report.Target.AssetDirectory)
	}
	if info, err := os.Stat(asset.URI); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("retained asset mode=%v err=%v, want owner-only 0600", info.Mode().Perm(), err)
	}
	reportRaw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if strings.Contains(string(reportRaw), "red cube") {
		t.Fatal("report leaks prompt text; only its length and SHA-256 may be recorded")
	}
	raw, err := os.ReadFile(asset.URI)
	if err != nil {
		t.Fatalf("read retained asset: %v", err)
	}
	if !bytes.Equal(raw, videoBytes) {
		t.Fatal("retained asset does not match the downloaded bytes")
	}
	if info, err := os.Stat(reportPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusPass {
		t.Fatalf("inference check status=%q", status)
	}
}

func TestComfyUIAcceptDispatchRoute(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatchSubcommand([]string{"comfyui-accept", "--help"}, nil, &stdout, &stderr)
	if !handled {
		t.Fatal("comfyui-accept is not dispatched")
	}
	if code != 2 {
		t.Fatalf("--help exit=%d, want flag usage code 2", code)
	}
	if !strings.Contains(stderr.String(), "confirm-live") {
		t.Fatalf("help does not mention the explicit live opt-in: %q", stderr.String())
	}
}
