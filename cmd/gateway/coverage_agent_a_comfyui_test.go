package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// covcliComfySuccessServer mirrors the fake ComfyUI server in
// TestComfyUIAcceptSuccessPersistsVerifiedAsset: system stats, a
// prompt submission, a completed history entry with one MP4 output,
// and a /view endpoint serving the MP4 bytes.
func covcliComfySuccessServer(t *testing.T, videoBytes []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"os":"posix","comfyui_version":"0.3.40","python_version":"3.11.9","pytorch_version":"2.5.1","ram_total":1024,"ram_free":512},"devices":[{"name":"cuda:0","type":"cuda","index":0,"vram_total":2048,"vram_free":1024}]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
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
}

// TestCoverageAgentAComfyMkdirTempError: assetDir is a symlink to the
// read-only /sys filesystem, so MkdirAll is a no-op (the path exists)
// but MkdirTemp fails EROFS.
func TestCoverageAgentAComfyMkdirTempError(t *testing.T) {
	server := covcliComfySuccessServer(t, comfyAcceptMP4())
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	link := filepath.Join(t.TempDir(), "assets-link")
	if err := os.Symlink("/sys", link); err != nil {
		t.Fatal(err)
	}
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms", "--asset-dir", link)
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	status, reason := comfyAcceptCheckStatus(report, "comfyui:model_inference")
	if status != externalStatusFail || !strings.Contains(reason, "cannot create acceptance asset workspace") {
		t.Fatalf("model_inference check=%q reason=%q, want FAIL with the workspace error", status, reason)
	}
}

// TestCoverageAgentAComfyNewLocalError: the storage.NewLocal seam
// fails, so runComfyUIAcceptChecks reports the store-initialization
// failure and removes the fresh run directory.
func TestCoverageAgentAComfyNewLocalError(t *testing.T) {
	server := covcliComfySuccessServer(t, comfyAcceptMP4())
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	old := comfyAcceptNewLocal
	comfyAcceptNewLocal = func(string, int64) (*storage.Local, error) {
		return nil, errors.New("store boom")
	}
	t.Cleanup(func() { comfyAcceptNewLocal = old })
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	status, reason := comfyAcceptCheckStatus(report, "comfyui:model_inference")
	if status != externalStatusFail || !strings.Contains(reason, "cannot initialize acceptance asset store") || !strings.Contains(reason, "store boom") {
		t.Fatalf("model_inference check=%q reason=%q, want FAIL with the store error", status, reason)
	}
}

// TestCoverageAgentAComfyAssetAccessError: the asset-access seam
// fails after a successful download, so the run fails closed on the
// asset_path check.
func TestCoverageAgentAComfyAssetAccessError(t *testing.T) {
	server := covcliComfySuccessServer(t, comfyAcceptMP4())
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	old := comfyAcceptVerifyAssetAccess
	comfyAcceptVerifyAssetAccess = func(string, video.Asset) error {
		return errors.New("access boom")
	}
	t.Cleanup(func() { comfyAcceptVerifyAssetAccess = old })
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "5s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	status, reason := comfyAcceptCheckStatus(report, "comfyui:asset_path")
	if status != externalStatusFail || !strings.Contains(reason, "access boom") {
		t.Fatalf("asset_path check=%q reason=%q, want FAIL with the injected access error", status, reason)
	}
	status, _ = comfyAcceptCheckStatus(report, "comfyui:model_inference")
	if status != externalStatusFail {
		t.Fatalf("model_inference check=%q, want FAIL", status)
	}
}

// TestCoverageAgentAPollComfyTopCtxWithLastErr: the loop-top context
// check fires on the second iteration while a GetJob error is
// outstanding, so the wrapped error carries the last status error.
// The provider points at a closed loopback port so GetJob fails on
// the first poll; the seam returns nil once, then the injected error.
func TestCoverageAgentAPollComfyTopCtxWithLastErr(t *testing.T) {
	videoCfg := comfyAcceptVideoConfig(config.VideoConfig{
		Enabled: true, ComfyUIEnabled: true,
		ComfyUIBaseURL:      "http://127.0.0.1:1",
		ComfyUIWorkflowPath: comfyAcceptWorkflow(t),
		ComfyUIModel:        "local-video", ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}, filepath.Join(t.TempDir(), "config.json"))
	provider, err := providers.NewComfyUI(videoCfg, 512<<20)
	if err != nil {
		t.Fatalf("NewComfyUI: %v", err)
	}
	old := pollComfyAcceptCtxErr
	calls := 0
	pollComfyAcceptCtxErr = func(context.Context) error {
		calls++
		if calls == 1 {
			return nil
		}
		return errors.New("ctx boom")
	}
	t.Cleanup(func() { pollComfyAcceptCtxErr = old })
	_, polls, err := pollComfyAcceptJob(context.Background(), provider, "prompt-ok", 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "last status error") || !strings.Contains(err.Error(), "ctx boom") {
		t.Fatalf("pollComfyAcceptJob err=%v, want the wrapped error with the last status error", err)
	}
	if polls != 1 {
		t.Fatalf("polls=%d want 1", polls)
	}
}

// TestCoverageAgentAComfySubmitMayHaveBeenAccepted pins the
// definite-rejection strings (nil, node errors, missing prompt_id,
// 4xx) to false and an ambiguous transport failure to true.
func TestCoverageAgentAComfySubmitMayHaveBeenAccepted(t *testing.T) {
	if comfySubmitMayHaveBeenAccepted(nil) {
		t.Fatal("nil error must be false")
	}
	for _, definite := range []string{
		"ComfyUI workflow reported node errors",
		"ComfyUI did not return a prompt_id",
		"ComfyUI output download returned HTTP 404",
	} {
		if comfySubmitMayHaveBeenAccepted(errors.New(definite)) {
			t.Fatalf("definite error %q must be false", definite)
		}
	}
	if !comfySubmitMayHaveBeenAccepted(errors.New("dial tcp 127.0.0.1:8188: connect: connection refused")) {
		t.Fatal("an ambiguous transport failure must be true")
	}
}

// covcliComfySavedAsset builds a real local sink and stores one MP4
// asset in it, returning the sink, the saved asset record, and the
// raw bytes.
func covcliComfySavedAsset(t *testing.T) (*storage.Local, video.Asset, []byte) {
	t.Helper()
	dir := t.TempDir()
	sink, err := storage.NewLocal(dir, 512<<20)
	if err != nil {
		t.Fatal(err)
	}
	data := comfyAcceptMP4()
	saved, err := sink.Put(context.Background(), video.Asset{
		Filename: "accept.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{
			"project_id": "comfyui-acceptance", "job_id": "comfyui-acceptance-test",
			"provider": "comfyui", "node_id": "10", "filename": "accept.mp4",
		},
	}, data)
	if err != nil {
		t.Fatal(err)
	}
	return sink, saved, data
}

// TestCoverageAgentAVerifyComfyAssetSizeMismatch: the stored record
// claims one byte more than the bytes read back. storage.Local.Read
// enforces the size check, so the failure surfaces as a read-back
// error; the redundant size check in verifyComfyAcceptAsset was
// removed as unreachable (see the proof comment there).
func TestCoverageAgentAVerifyComfyAssetSizeMismatch(t *testing.T) {
	sink, saved, data := covcliComfySavedAsset(t)
	asset := saved
	asset.SizeBytes = int64(len(data)) + 1
	_, err := verifyComfyAcceptAsset(context.Background(), sink, &render.Renderer{FFprobe: "ffprobe"}, asset)
	if err == nil || !strings.Contains(err.Error(), "read back persisted asset") || !strings.Contains(err.Error(), "asset size mismatch") {
		t.Fatalf("verifyComfyAcceptAsset err=%v, want the read-back size-mismatch error", err)
	}
}

// TestCoverageAgentAVerifyComfyAssetSHAMismatch: the stored record
// carries a valid-hex SHA-256 that does not match the stored bytes.
// storage.Local.Read enforces the checksum, so the failure surfaces
// as a read-back error; the redundant SHA check in
// verifyComfyAcceptAsset was removed as unreachable (see the proof
// comment there).
func TestCoverageAgentAVerifyComfyAssetSHAMismatch(t *testing.T) {
	sink, saved, _ := covcliComfySavedAsset(t)
	asset := saved
	asset.SHA256 = strings.Repeat("0", 64)
	_, err := verifyComfyAcceptAsset(context.Background(), sink, &render.Renderer{FFprobe: "ffprobe"}, asset)
	if err == nil || !strings.Contains(err.Error(), "read back persisted asset") || !strings.Contains(err.Error(), "asset checksum mismatch") {
		t.Fatalf("verifyComfyAcceptAsset err=%v, want the read-back checksum-mismatch error", err)
	}
}

// TestCoverageAgentAVerifyComfyAssetAccessStatError: EvalSymlinks on
// the same path succeeds immediately before, so the Stat failure is
// injected through the seam.
func TestCoverageAgentAVerifyComfyAssetAccessStatError(t *testing.T) {
	_, saved, _ := covcliComfySavedAsset(t)
	runDir := filepath.Dir(filepath.Dir(filepath.Dir(saved.URI)))
	old := comfyAcceptAssetStat
	comfyAcceptAssetStat = func(string) (os.FileInfo, error) {
		return nil, errors.New("stat boom")
	}
	t.Cleanup(func() { comfyAcceptAssetStat = old })
	err := verifyComfyAcceptAssetAccess(runDir, saved)
	if err == nil || !strings.Contains(err.Error(), "stat persisted asset") || !strings.Contains(err.Error(), "stat boom") {
		t.Fatalf("verifyComfyAcceptAssetAccess err=%v, want the injected stat error", err)
	}
}

// TestCoverageAgentAWriteComfyAcceptReportMarshalError: a NaN in the
// report bounds fails MarshalIndent.
func TestCoverageAgentAWriteComfyAcceptReportMarshalError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report := comfyAcceptReport{Bounds: comfyAcceptBounds{JobTimeoutSeconds: math.NaN()}}
	err := writeComfyAcceptReport(path, report)
	if err == nil || !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("writeComfyAcceptReport err=%v, want the NaN marshal error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("report file must not exist, stat err=%v", statErr)
	}
}

// TestCoverageAgentAWriteComfyAcceptReportChmodError: the temp-file
// chmod seam fails, so no report is written.
func TestCoverageAgentAWriteComfyAcceptReportChmodError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	old := comfyAcceptReportTempChmod
	comfyAcceptReportTempChmod = func(*os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { comfyAcceptReportTempChmod = old })
	err := writeComfyAcceptReport(path, comfyAcceptReport{Schema: comfyAcceptSchema})
	if err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("writeComfyAcceptReport err=%v, want the injected chmod error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("report file must not exist, stat err=%v", statErr)
	}
}

// TestCoverageAgentAWriteComfyAcceptReportWriteError: the report body
// does not fit in a 10-byte file-size limit.
func TestCoverageAgentAWriteComfyAcceptReportWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	limitFileSize(t, 10)
	err := writeComfyAcceptReport(path, comfyAcceptReport{Schema: comfyAcceptSchema})
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("writeComfyAcceptReport err=%v, want the file-too-large error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("report file must not exist, stat err=%v", statErr)
	}
}

// TestCoverageAgentAWriteComfyAcceptReportSyncError: the temp-file
// sync seam fails, so no report is written.
func TestCoverageAgentAWriteComfyAcceptReportSyncError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	old := comfyAcceptReportTempSync
	comfyAcceptReportTempSync = func(*os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { comfyAcceptReportTempSync = old })
	err := writeComfyAcceptReport(path, comfyAcceptReport{Schema: comfyAcceptSchema})
	if err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("writeComfyAcceptReport err=%v, want the injected sync error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("report file must not exist, stat err=%v", statErr)
	}
}

// TestCoverageAgentAWriteComfyAcceptReportCloseError: the temp-file
// close seam fails, so no report is written.
func TestCoverageAgentAWriteComfyAcceptReportCloseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	old := comfyAcceptReportTempClose
	comfyAcceptReportTempClose = func(*os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { comfyAcceptReportTempClose = old })
	err := writeComfyAcceptReport(path, comfyAcceptReport{Schema: comfyAcceptSchema})
	if err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("writeComfyAcceptReport err=%v, want the injected close error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("report file must not exist, stat err=%v", statErr)
	}
}
