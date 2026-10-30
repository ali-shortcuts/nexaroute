package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// The harness timeout matrix covers the submit phase (no prompt id,
// remote cleanup NOT_RUN) and the poll phase (pending prompt deleted).
// The download phase had no deadline test: a /view that never answers
// must fail the run at the harness deadline with no retained asset,
// and cleanup must still report local PASS plus remote BLOCKED — the
// prompt already completed, so there is no pending entry to delete
// and no global interrupt may be attempted. Fixture only: no real
// ComfyUI server is involved.
func TestComfyUIAcceptDownloadDeadlineFailsClosedWithCleanup(t *testing.T) {
	release := make(chan struct{})
	var promptCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			promptCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-download-deadline"}`))
		case r.URL.Path == "/history/prompt-download-deadline":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt-download-deadline":{"outputs":{"10":{"videos":[{"filename":"slow.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/view":
			// Block until the harness deadline cancels the request
			// (or the test releases the handler at teardown, so
			// server.Close cannot hang on a stuck handler).
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case r.URL.Path == "/queue" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { close(release); server.Close() }()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "1s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if promptCalls.Load() != 1 {
		t.Fatalf("prompt calls=%d, want exactly 1 (no resubmit after an ambiguous deadline)", promptCalls.Load())
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:download"); status != externalStatusFail {
		t.Fatalf("download status=%q, want FAIL", status)
	}
	if len(report.Assets) != 0 {
		t.Fatalf("deadline run reported assets: %+v", report.Assets)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup=%+v, want local PASS", report.Cleanup)
	}
	if report.Cleanup.RemoteStatus != externalStatusBlocked {
		t.Fatalf("cleanup remote=%q, want BLOCKED (completed prompt has no pending entry; no global interrupt)", report.Cleanup.RemoteStatus)
	}
	if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
		t.Fatalf("partial asset workspace survived download deadline: %v", err)
	}
}
