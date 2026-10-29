package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// Issue #663 (gateway scope): audit and ComfyUI-acceptance branches
// that the ba5ff2f coverprofile shows as uncovered. ComfyUI tests
// use httptest servers and fake ffprobe scripts only — never a real
// ComfyUI instance and never a real model download.

func TestIssue663GatewayAuditUsageAndLoadErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAudit([]string{"bogus"}, &stdout, &stderr); code != auditExitUsage || !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("bad subcommand exit=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := runAudit([]string{"verify", "--bogus-flag"}, &stdout, &stderr); code != auditExitUsage {
		t.Fatalf("bad flag exit=%d want 2", code)
	}
	configPath := setupAuditCLI(t, 1)
	stderr.Reset()
	if code := runAudit([]string{"verify", "--config", configPath, "--format", "xml"}, &stdout, &stderr); code != auditExitUsage || !strings.Contains(stderr.String(), `unknown format "xml"`) {
		t.Fatalf("bad format exit=%d stderr=%s", code, stderr.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"listen": 123}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := runAudit([]string{"verify", "--config", bad}, &stdout, &stderr); code != auditExitUsage || !strings.Contains(stderr.String(), "load config") {
		t.Fatalf("bad config exit=%d stderr=%s", code, stderr.String())
	}
	// Store path under a regular file: open fails and the hint about
	// a running gateway's lock is printed.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"admin":{"security_store_path":"`+filepath.Join(blocker, "store.db")+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := runAudit([]string{"verify", "--config", cfgPath}, &stdout, &stderr); code != auditExitUsage || !strings.Contains(stderr.String(), "may be locked by a running gateway") {
		t.Fatalf("unopenable store exit=%d stderr=%s", code, stderr.String())
	}
}

func TestIssue663GatewayAuditStorePath(t *testing.T) {
	if _, err := auditStorePath("", ""); err == nil || !strings.Contains(err.Error(), "required when no config file path is available") {
		t.Fatalf("empty/empty: %v", err)
	}
	got, err := auditStorePath("  /abs/path/../x.db  ", "/whatever/config.json")
	if err != nil || got != "/abs/x.db" {
		t.Fatalf("absolute clean: %q %v", got, err)
	}
	got, err = auditStorePath("security/db.db", "/tmp/cfg/config.json")
	if err != nil || got != "/tmp/cfg/security/db.db" {
		t.Fatalf("relative join: %q %v", got, err)
	}
	got, err = auditStorePath("  rel.db  ", "/tmp/cfg/config.json")
	if err != nil || got != "/tmp/cfg/rel.db" {
		t.Fatalf("trimmed relative: %q %v", got, err)
	}
	got, err = auditStorePath("rel.db", "")
	if err != nil || !filepath.IsAbs(got) || !strings.HasSuffix(got, "rel.db") {
		t.Fatalf("empty config path: %q %v", got, err)
	}
}

func TestIssue663GatewayComfyAcceptFlagValidation(t *testing.T) {
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), "http://127.0.0.1:9")
	cases := [][]string{
		{"--config", configPath, "--confirm-live", "--timeout", "61m"},
		{"--config", configPath, "--confirm-live", "--poll-interval", "31s"},
		{"--config", configPath, "--confirm-live", "--prompt", "   "},
		{"--config", configPath, "--confirm-live", "--duration", "0"},
		{"--config", configPath, "--confirm-live", "--fps", "0"},
		{"--config", configPath, "--confirm-live", "--bogus-flag"},
		{"--config", filepath.Join(t.TempDir(), "missing.json"), "--confirm-live"},
	}
	for i, args := range cases {
		code, _, _, _, stderr := runComfyAcceptTest(t, args...)
		if code != 2 {
			t.Fatalf("case %d: exit=%d want 2 (stderr=%s)", i, code, stderr)
		}
	}
}

func TestIssue663GatewayComfyAcceptDefaultReportPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Unprovisioned config: preflight is BLOCKED (code 3), but the
	// report is still written — to the default .artifacts path.
	configPath := writeTestConfig(t, config.Default())
	var stdout, stderr bytes.Buffer
	code := runComfyUIAccept([]string{"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t)}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("blocked run exit=%d want 3 (stderr=%s)", code, stderr.String())
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".artifacts", "comfyui-acceptance", "*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("default report path: matches=%v err=%v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var report comfyAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Schema != comfyAcceptSchema || report.Outcome != externalStatusBlocked {
		t.Fatalf("default report: %+v", report)
	}
}

// issue663GatewayComfyServer returns an httptest ComfyUI stub whose
// /system_stats, /prompt, /history/<id> and /view endpoints behave
// like the success fixture in the existing acceptance tests.
func issue663GatewayComfyServer(t *testing.T, promptID string, videoBytes []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"` + promptID + `"}`))
		case r.URL.Path == "/history/"+promptID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"` + promptID + `":{"outputs":{"10":{"videos":[{"filename":"accept.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/view":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestIssue663GatewayComfyAcceptCheckBranches(t *testing.T) {
	// Video disabled outright: preflight is BLOCKED, inference NOT_RUN.
	// (LoadBase rejects ComfyUIEnabled without Enabled, so the
	// loadable disabled shape is both flags false; the reason text
	// is what this case pins beyond the existing status-only test.)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Video = config.VideoConfig{Enabled: false, ComfyUIEnabled: false}
	disabledPath := writeTestConfig(t, cfg)
	code, report, _, _, _ := runComfyAcceptTest(t, "--config", disabledPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t))
	if code != 3 {
		t.Fatalf("disabled exit=%d want 3", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:preflight"); status != externalStatusBlocked || !strings.Contains(reason, "must both be true") {
		t.Fatalf("disabled preflight=%q reason=%q", status, reason)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusNotRun {
		t.Fatalf("disabled inference=%q", status)
	}
	// Enabled but no target at all: a different BLOCKED reason.
	cfg.Video = config.VideoConfig{Enabled: true, ComfyUIEnabled: true}
	// LoadBase rejects an empty target before checks run, so this
	// branch is reached by calling the checks function directly
	// with a hand-built config — the only loadable path to it.
	emptyReport := newComfyAcceptReport(cfg, t.TempDir(), comfyAcceptProbe(t), time.Minute, time.Second)
	code = runComfyUIAcceptChecks(&emptyReport, cfg, disabledPath, t.TempDir(), comfyAcceptProbe(t), "a test prompt", 1, 24, 0, 0, 0, time.Minute, time.Second)
	if code != 3 {
		t.Fatalf("empty target exit=%d want 3", code)
	}
	if status, reason := comfyAcceptCheckStatus(emptyReport, "comfyui:preflight"); status != externalStatusBlocked || !strings.Contains(reason, "are required") {
		t.Fatalf("empty target preflight=%q reason=%q", status, reason)
	}
	if status, _ := comfyAcceptCheckStatus(emptyReport, "comfyui:model_inference"); status != externalStatusNotRun {
		t.Fatalf("empty target inference=%q want NOT_RUN", status)
	}
	// Workflow path that cannot be read. LoadBase rejects a missing
	// workflow file at load time, so again the checks function is
	// called directly with the hand-built config.
	cfg.Video = config.VideoConfig{Enabled: true, ComfyUIEnabled: true, ComfyUIBaseURL: "http://127.0.0.1:9", ComfyUIWorkflowPath: filepath.Join(t.TempDir(), "missing.json")}
	unreadableReport := newComfyAcceptReport(cfg, t.TempDir(), comfyAcceptProbe(t), time.Minute, time.Second)
	code = runComfyUIAcceptChecks(&unreadableReport, cfg, disabledPath, t.TempDir(), comfyAcceptProbe(t), "a test prompt", 1, 24, 0, 0, 0, time.Minute, time.Second)
	if code != 3 {
		t.Fatalf("unreadable workflow exit=%d want 3", code)
	}
	if status, reason := comfyAcceptCheckStatus(unreadableReport, "comfyui:preflight"); status != externalStatusBlocked || !strings.Contains(reason, "not readable") {
		t.Fatalf("unreadable preflight=%q reason=%q", status, reason)
	}
	// ffprobe missing: the server must never be contacted.
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	ffprobePath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, _, _ = runComfyAcceptTest(t, "--config", ffprobePath, "--confirm-live", "--ffprobe", filepath.Join(t.TempDir(), "no-ffprobe"))
	if code != 3 || calls.Load() != 0 {
		t.Fatalf("missing ffprobe exit=%d calls=%d", code, calls.Load())
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:preflight"); status != externalStatusBlocked || !strings.Contains(reason, "ffprobe executable is unavailable") {
		t.Fatalf("ffprobe preflight=%q reason=%q", status, reason)
	}
	// Duration beyond the workflow maximum: request validation fails.
	server2 := issue663GatewayComfyServer(t, "prompt-ok", comfyAcceptMP4())
	defer server2.Close()
	durationPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server2.URL)
	code, report, _, _, _ = runComfyAcceptTest(t, "--config", durationPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t), "--duration", "999")
	if code != 1 {
		t.Fatalf("oversized duration exit=%d want 1", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusFail || !strings.Contains(reason, "invalid") {
		t.Fatalf("duration inference=%q reason=%q", status, reason)
	}
	// Asset root under a regular file: creation fails before any job.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, report, _, _, _ = runComfyAcceptTest(t, "--config", durationPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t), "--asset-dir", filepath.Join(blocker, "sub"))
	if code != 1 {
		t.Fatalf("asset root exit=%d want 1", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusFail || !strings.Contains(reason, "cannot create acceptance asset root") {
		t.Fatalf("asset root inference=%q reason=%q", status, reason)
	}
	// Custom aspect ratio (both dimensions given) succeeds end to end.
	code, report, _, _, stderr := runComfyAcceptTest(t, "--config", durationPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t), "--timeout", "5s", "--poll-interval", "10ms", "--width", "640", "--height", "360")
	if code != 0 {
		t.Fatalf("custom aspect exit=%d stderr=%s", code, stderr)
	}
	if len(report.Assets) != 1 || report.Assets[0].Probe == nil || report.Assets[0].Probe.Codec != "h264" {
		t.Fatalf("custom aspect assets=%+v", report.Assets)
	}
}

func issue663GatewayAudioFFprobe(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ncat <<'JSON'\n" +
		`{"streams":[{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"1.0"}}` +
		"\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// Submit failure: the server answers SystemStats but rejects the
// workflow submission, so the submit check FAILs and the run is
// cleaned up without ever polling.
func TestIssue663GatewayComfyAcceptSubmitFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"os":"linux","comfyui_version":"0.3.40","python_version":"3.11"},"devices":[]}`))
		case "/prompt":
			http.Error(w, "queue full", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, _, _ := runComfyAcceptTest(t, "--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t))
	if code != 1 {
		t.Fatalf("submit failure exit=%d want 1", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:submit"); status != externalStatusFail || !strings.Contains(reason, "workflow submission failed") {
		t.Fatalf("submit=%q reason=%q", status, reason)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass || report.Cleanup.RemoteStatus != externalStatusNotRun {
		t.Fatalf("cleanup=%+v want local PASS / remote NOT_RUN", report.Cleanup)
	}
	if report.Target.AssetDirectory != "" {
		if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
			t.Fatalf("workspace must be removed: %s", report.Target.AssetDirectory)
		}
	}
}

// Report-write failure: preflight is BLOCKED, but the report itself
// cannot be written because --out sits under a regular file.
func TestIssue663GatewayComfyAcceptReportWriteFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeTestConfig(t, config.Default())
	var stdout, stderrBuf bytes.Buffer
	code := runComfyUIAccept([]string{"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t), "--out", filepath.Join(blocker, "report.json")}, &stdout, &stderrBuf)
	if code != 2 || !strings.Contains(stderrBuf.String(), "cannot write report") {
		t.Fatalf("report write failure exit=%d stderr=%q", code, stderrBuf.String())
	}
}

func TestIssue663GatewayComfyAcceptVerifyFailureInFlow(t *testing.T) {
	server := issue663GatewayComfyServer(t, "prompt-ok", comfyAcceptMP4())
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, _, _ := runComfyAcceptTest(t, "--config", configPath, "--confirm-live", "--ffprobe", issue663GatewayAudioFFprobe(t), "--timeout", "5s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("audio-only asset exit=%d want 1", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:asset_integrity"); status != externalStatusFail || !strings.Contains(reason, "has no video stream") {
		t.Fatalf("asset_integrity=%q reason=%q", status, reason)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusFail {
		t.Fatalf("inference=%q want FAIL", status)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup=%+v", report.Cleanup)
	}
	if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
		t.Fatalf("failed asset workspace survived: %v", err)
	}
}

// issue663GatewayComfyProvider builds a ComfyUI provider against a
// stub server using the same workflow fixture as the acceptance tests.
func issue663GatewayComfyProvider(t *testing.T, serverURL string) *providers.ComfyUI {
	t.Helper()
	videoCfg := comfyAcceptVideoConfig(config.VideoConfig{
		Enabled: true, ComfyUIEnabled: true, ComfyUIBaseURL: serverURL,
		ComfyUIWorkflowPath: comfyAcceptWorkflow(t), ComfyUIModel: "local-video",
		ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID:       "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}, filepath.Join(t.TempDir(), "config.json"))
	provider, err := providers.NewComfyUI(videoCfg, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestIssue663GatewayPollComfyAcceptJob(t *testing.T) {
	// Completed on the first poll.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/history/job-done" {
			_, _ = w.Write([]byte(`{"job-done":{"outputs":{"10":{"videos":[{"filename":"a.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	provider := issue663GatewayComfyProvider(t, server.URL)
	status, polls, err := pollComfyAcceptJob(context.Background(), provider, "job-done", time.Millisecond)
	if err != nil || status.State != video.StateCompleted || polls != 1 {
		t.Fatalf("completed: state=%v polls=%d err=%v", status.State, polls, err)
	}
	// Processing (via the queue) then completed.
	var historyCalls atomic.Int64
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/history/job-slow":
			if historyCalls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = w.Write([]byte(`{"job-slow":{"outputs":{"10":{"videos":[{"filename":"a.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[[1,"job-slow"]],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server2.Close()
	provider2 := issue663GatewayComfyProvider(t, server2.URL)
	status, polls, err = pollComfyAcceptJob(context.Background(), provider2, "job-slow", time.Millisecond)
	if err != nil || status.State != video.StateCompleted || polls != 2 {
		t.Fatalf("slow: state=%v polls=%d err=%v", status.State, polls, err)
	}
	// Provider-reported workflow error.
	server3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/history/job-bad" {
			_, _ = w.Write([]byte(`{"job-bad":{"outputs":{},"status":{"status_str":"error","completed":false}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server3.Close()
	provider3 := issue663GatewayComfyProvider(t, server3.URL)
	status, polls, err = pollComfyAcceptJob(context.Background(), provider3, "job-bad", time.Millisecond)
	if err == nil || status.State != video.StateFailed || polls != 1 || !strings.Contains(err.Error(), "execution error") {
		t.Fatalf("failed: state=%v polls=%d err=%v", status.State, polls, err)
	}
	// Already-cancelled context: zero polls, context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, polls, err = pollComfyAcceptJob(ctx, provider, "job-done", time.Millisecond)
	if polls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: polls=%d err=%v", polls, err)
	}
	// Cancelled while a status request is in flight: the wrapped
	// error names the last status error.
	started := make(chan struct{})
	var once atomic.Bool
	server4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/history/job-hang" {
			if once.CompareAndSwap(false, true) {
				close(started)
			}
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer server4.Close()
	provider4 := issue663GatewayComfyProvider(t, server4.URL)
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _, err = pollComfyAcceptJob(ctx, provider4, "job-hang", time.Millisecond)
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "last status error") {
		t.Fatalf("mid-flight cancel: %v", err)
	}
}

func TestIssue663GatewayVerifyComfyAcceptAsset(t *testing.T) {
	// Stored fixture asset through the real local store.
	put := func(t *testing.T, meta map[string]string) (*storage.Local, video.Asset) {
		t.Helper()
		sink, err := storage.NewLocal(filepath.Join(t.TempDir(), "store"), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := sink.Put(context.Background(), video.Asset{
			Filename: "accept.mp4", ContentType: "video/mp4", Metadata: meta,
		}, comfyAcceptMP4())
		if err != nil {
			t.Fatal(err)
		}
		return sink, stored
	}
	baseMeta := map[string]string{"project_id": "p", "job_id": "j", "provider": "comfyui", "node_id": "10", "filename": "accept.mp4"}
	probe := &render.Renderer{FFprobe: comfyAcceptProbe(t)}
	sink, stored := put(t, baseMeta)
	verified, err := verifyComfyAcceptAsset(context.Background(), sink, probe, stored)
	if err != nil {
		t.Fatalf("verify success: %v", err)
	}
	if verified.Probe == nil || verified.Probe.Codec != "h264" || verified.Probe.Width != 1280 || verified.Probe.Height != 720 || verified.SizeBytes != int64(len(comfyAcceptMP4())) {
		t.Fatalf("verified=%+v", verified)
	}
	// Early rejections that need no file access at all.
	for _, tc := range []struct {
		name  string
		asset video.Asset
		want  string
	}{
		{"non-positive size", video.Asset{SizeBytes: 0, ContentType: "video/mp4", SHA256: strings.Repeat("a", 64)}, "non-positive size"},
		{"non-video type", video.Asset{SizeBytes: 1, ContentType: "image/png", SHA256: strings.Repeat("a", 64)}, "non-video content type"},
		{"short SHA", video.Asset{SizeBytes: 1, ContentType: "video/mp4", SHA256: "abc"}, "no valid SHA-256"},
		{"malformed SHA", video.Asset{SizeBytes: 1, ContentType: "video/mp4", SHA256: strings.Repeat("z", 64)}, "malformed SHA-256"},
	} {
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, tc.asset); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v want %q", tc.name, err, tc.want)
		}
	}
	// Read-back failure: the URI escapes the store root.
	outside := video.Asset{SizeBytes: 1, ContentType: "video/mp4", SHA256: strings.Repeat("a", 64), URI: filepath.Join(t.TempDir(), "outside.mp4")}
	if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, outside); err == nil || !strings.Contains(err.Error(), "read back persisted asset") {
		t.Fatalf("outside root: %v", err)
	}
	// Metadata rejections, with a genuinely stored asset behind them.
	sink2, stored2 := put(t, map[string]string{"project_id": "p", "job_id": "j", "provider": "other", "node_id": "10", "filename": "accept.mp4"})
	if _, err := verifyComfyAcceptAsset(context.Background(), sink2, probe, stored2); err == nil || !strings.Contains(err.Error(), "provider=comfyui") {
		t.Fatalf("provider metadata: %v", err)
	}
	sink3, stored3 := put(t, map[string]string{"project_id": "p", "job_id": "j", "provider": "comfyui", "filename": "accept.mp4"})
	if _, err := verifyComfyAcceptAsset(context.Background(), sink3, probe, stored3); err == nil || !strings.Contains(err.Error(), "node_id") {
		t.Fatalf("node_id metadata: %v", err)
	}
	sink4, stored4 := put(t, map[string]string{"project_id": "p", "job_id": "j", "provider": "comfyui", "node_id": "10"})
	if _, err := verifyComfyAcceptAsset(context.Background(), sink4, probe, stored4); err == nil || !strings.Contains(err.Error(), "filename") {
		t.Fatalf("filename metadata: %v", err)
	}
	// Probe failures: a failing ffprobe binary, and one that reports
	// no video stream at all.
	failingProbe := filepath.Join(t.TempDir(), "ffprobe-fail")
	if err := os.WriteFile(failingProbe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sink5, stored5 := put(t, baseMeta)
	if _, err := verifyComfyAcceptAsset(context.Background(), sink5, &render.Renderer{FFprobe: failingProbe}, stored5); err == nil || !strings.Contains(err.Error(), "verify playable media") {
		t.Fatalf("failing ffprobe: %v", err)
	}
	sink6, stored6 := put(t, baseMeta)
	if _, err := verifyComfyAcceptAsset(context.Background(), sink6, &render.Renderer{FFprobe: issue663GatewayAudioFFprobe(t)}, stored6); err == nil || !strings.Contains(err.Error(), "verify playable media") {
		t.Fatalf("audio-only ffprobe: %v", err)
	}
}

func TestIssue663GatewayVerifyComfyAcceptAssetAccess(t *testing.T) {
	sink, err := storage.NewLocal(filepath.Join(t.TempDir(), "store"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := sink.Put(context.Background(), video.Asset{
		Filename: "accept.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, comfyAcceptMP4())
	if err != nil {
		t.Fatal(err)
	}
	root := sink.Root
	// The stored asset itself passes: inside the root, regular, 0600.
	if err := verifyComfyAcceptAssetAccess(root, stored); err != nil {
		t.Fatalf("stored asset access: %v", err)
	}
	// Lexical escape and symlink escape.
	escaped := stored
	escaped.URI = filepath.Join(root, "..", "outside.mp4")
	if err := verifyComfyAcceptAssetAccess(root, escaped); err == nil || !strings.Contains(err.Error(), "escapes the acceptance asset root") {
		t.Fatalf("lexical escape: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, comfyAcceptMP4(), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.mp4")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	linked := stored
	linked.URI = link
	if err := verifyComfyAcceptAssetAccess(root, linked); err == nil || !strings.Contains(err.Error(), "through a symlink") {
		t.Fatalf("symlink escape: %v", err)
	}
	// Non-regular file, wrong mode, and a missing file.
	dirAsset := stored
	dirAsset.URI = root
	if err := verifyComfyAcceptAssetAccess(root, dirAsset); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory asset: %v", err)
	}
	loose := filepath.Join(root, "loose.mp4")
	if err := os.WriteFile(loose, comfyAcceptMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	looseAsset := stored
	looseAsset.URI = loose
	if err := verifyComfyAcceptAssetAccess(root, looseAsset); err == nil || !strings.Contains(err.Error(), "want owner-only 0600") {
		t.Fatalf("loose mode: %v", err)
	}
	missing := stored
	missing.URI = filepath.Join(root, "missing.mp4")
	if err := verifyComfyAcceptAssetAccess(root, missing); err == nil {
		t.Fatal("missing asset accepted")
	}
}

func TestIssue663GatewayComfyAcceptFailClosed(t *testing.T) {
	// No job and no provider: nothing remote to clean, local removed.
	report := &comfyAcceptReport{}
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "partial"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	comfyAcceptFailClosed(report, runDir, nil)
	if report.Cleanup == nil || report.Cleanup.RemoteStatus != externalStatusNotRun || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("no-job cleanup=%+v", report.Cleanup)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("partial workspace survived: %v", err)
	}
	// No run directory at all: local is NOT_RUN, not a failure.
	report2 := &comfyAcceptReport{}
	comfyAcceptFailClosed(report2, "", nil)
	if report2.Cleanup == nil || report2.Cleanup.LocalStatus != externalStatusNotRun {
		t.Fatalf("empty run dir cleanup=%+v", report2.Cleanup)
	}
	// Queue-driven remote outcomes.
	queueCases := []struct {
		name   string
		queue  string
		status int
		want   string
	}{
		{"pending prompt is deleted", `{"queue_running":[],"queue_pending":[[1,"job-1"]]}`, http.StatusOK, externalStatusPass},
		{"absent prompt is not deleted", `{"queue_running":[],"queue_pending":[]}`, http.StatusOK, externalStatusBlocked},
		{"running prompt is not interrupted", `{"queue_running":[[1,"job-1"]],"queue_pending":[]}`, http.StatusOK, externalStatusBlocked},
		{"queue read failure", ``, http.StatusInternalServerError, externalStatusFail},
	}
	for _, tc := range queueCases {
		var deleted string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/queue" {
				var body struct {
					Delete []string `json:"delete"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if len(body.Delete) > 0 {
					deleted = body.Delete[0]
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.queue))
		}))
		provider := issue663GatewayComfyProvider(t, server.URL)
		rep := &comfyAcceptReport{Job: &comfyAcceptJob{PromptID: "job-1"}}
		comfyAcceptFailClosed(rep, "", provider)
		server.Close()
		if rep.Cleanup == nil || rep.Cleanup.RemoteStatus != tc.want {
			t.Fatalf("%s: remote=%+v want %s", tc.name, rep.Cleanup, tc.want)
		}
		if tc.want == externalStatusPass && deleted != "job-1" {
			t.Fatalf("%s: deleted=%q", tc.name, deleted)
		}
	}
}

func TestIssue663GatewayComfyAcceptReportHelpers(t *testing.T) {
	cfg := config.Default()
	cfg.Video.ComfyUIEnabled = true
	cfg.Video.ComfyUIBaseURL = "http://127.0.0.1:8188"
	report := newComfyAcceptReport(cfg, "/tmp/assets", "ffprobe", 10*time.Minute, 2*time.Second)
	if report.Schema != comfyAcceptSchema || report.Mode != "live" || report.EvidenceClass != "loopback" {
		t.Fatalf("report header=%+v", report)
	}
	if report.Bounds.JobTimeoutSeconds != 600 || report.Bounds.PollIntervalMillis != 2000 {
		t.Fatalf("bounds=%+v", report.Bounds)
	}
	if len(report.Limitations) == 0 {
		t.Fatal("report must carry its NOT_RUN limitations")
	}
	// Video config: relative workflow paths resolve against the
	// config directory; absolute paths and node inputs pass through.
	videoCfg := comfyAcceptVideoConfig(config.VideoConfig{
		Enabled: true, ComfyUIEnabled: true, ComfyUIBaseURL: "http://127.0.0.1:9",
		ComfyUIWorkflowPath: "wf.json", ComfyUIModel: "m",
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
	}, "/a/b/config.json")
	if videoCfg.ComfyUIWorkflowPath != "/a/b/wf.json" || videoCfg.ComfyUIPromptNodeID != "1" || videoCfg.ComfyUIPromptInput != "text" {
		t.Fatalf("relative video config=%+v", videoCfg)
	}
	videoCfg = comfyAcceptVideoConfig(config.VideoConfig{ComfyUIWorkflowPath: "/x/wf.json"}, "/a/b/config.json")
	if videoCfg.ComfyUIWorkflowPath != "/x/wf.json" {
		t.Fatalf("absolute video config=%+v", videoCfg)
	}
	// Checks, outcome, and limitations.
	rep := &comfyAcceptReport{EvidenceClass: "loopback"}
	addComfyAcceptCheck(rep, "a", externalStatusPass, "ok", time.Time{})
	if len(rep.Checks) != 1 || rep.Checks[0].DurationMS != 0 {
		t.Fatalf("zero-start check=%+v", rep.Checks)
	}
	addComfyAcceptCheck(rep, "b", externalStatusPass, "ok", time.Now())
	finalizeComfyAcceptReport(rep)
	if rep.Outcome != externalStatusPass {
		t.Fatalf("outcome=%q want PASS", rep.Outcome)
	}
	foundLoopback := false
	for _, lim := range rep.Limitations {
		if strings.Contains(lim, "loopback endpoint") {
			foundLoopback = true
		}
	}
	if !foundLoopback {
		t.Fatalf("loopback limitation missing: %v", rep.Limitations)
	}
	blocked := &comfyAcceptReport{}
	addComfyAcceptCheck(blocked, "a", externalStatusBlocked, "nope", time.Time{})
	finalizeComfyAcceptReport(blocked)
	if blocked.Outcome != externalStatusBlocked {
		t.Fatalf("blocked outcome=%q", blocked.Outcome)
	}
	foundBlocked := false
	for _, lim := range blocked.Limitations {
		if strings.Contains(lim, "BLOCKED") {
			foundBlocked = true
		}
	}
	if !foundBlocked {
		t.Fatalf("blocked limitation missing: %v", blocked.Limitations)
	}
	empty := &comfyAcceptReport{}
	finalizeComfyAcceptReport(empty)
	if empty.Outcome != externalStatusNotRun {
		t.Fatalf("empty outcome=%q want NOT_RUN", empty.Outcome)
	}
	// Server mapping, via a real SystemStats call against a stub
	// server (the Devices element type is anonymous, so it is built
	// by decoding, not by literal construction).
	statsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"system":{"comfyui_version":"0.3.40","python_version":"3.11"},"devices":[{"name":"cuda:0","type":"cuda","index":0,"vram_total":2048,"vram_free":1024}]}`))
	}))
	defer statsServer.Close()
	statsProvider := issue663GatewayComfyProvider(t, statsServer.URL)
	stats, err := statsProvider.SystemStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mapped := comfyAcceptServerFromStats(stats)
	if mapped.ComfyUIVersion != "0.3.40" || len(mapped.Devices) != 1 || mapped.Devices[0].VRAMTotalBytes != 2048 {
		t.Fatalf("server mapping=%+v", mapped)
	}
	sum := sha256.Sum256([]byte("abc"))
	if got := comfyAcceptSHA256([]byte("abc")); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha=%q", got)
	}
	// Report writing: round trip with mode 0600, and a rename onto
	// an existing directory fails.
	dir := t.TempDir()
	out := filepath.Join(dir, "report.json")
	if err := writeComfyAcceptReport(out, *rep); err != nil {
		t.Fatalf("write report: %v", err)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode=%v err=%v want 0600", info.Mode().Perm(), err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded comfyAcceptReport
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Outcome != externalStatusPass {
		t.Fatalf("report round trip: %+v %v", decoded, err)
	}
	if err := writeComfyAcceptReport(dir, *rep); err == nil {
		t.Fatal("report onto a directory accepted")
	}
	// Summary lines reflect exactly the sections present.
	var buf bytes.Buffer
	empty2 := comfyAcceptReport{Outcome: externalStatusNotRun}
	printComfyAcceptSummary(&buf, empty2, "/tmp/r.json")
	if strings.Contains(buf.String(), "server:") || strings.Contains(buf.String(), "job:") || strings.Contains(buf.String(), "cleanup:") {
		t.Fatalf("empty summary must not invent sections: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "report: /tmp/r.json") {
		t.Fatalf("summary missing report line: %s", buf.String())
	}
	buf.Reset()
	full := comfyAcceptReport{Outcome: externalStatusPass, Server: mapped, Job: &comfyAcceptJob{PromptID: "p", State: "completed", Polls: 2}, Cleanup: &comfyAcceptCleanup{RemoteStatus: externalStatusPass, LocalStatus: externalStatusPass}}
	printComfyAcceptSummary(&buf, full, "/tmp/r.json")
	for _, want := range []string{"server: version=\"0.3.40\"", "job: prompt=p", "cleanup: remote=PASS local=PASS"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("summary missing %q: %s", want, buf.String())
		}
	}
}
