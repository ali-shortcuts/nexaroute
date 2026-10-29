package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
)

func comfyRegressionProvider(t *testing.T, serverURL string) *providers.ComfyUI {
	t.Helper()
	provider, err := providers.NewComfyUI(video.Config{
		ComfyUIEnabled: true, ComfyUIBaseURL: serverURL, ComfyUIWorkflowPath: comfyAcceptWorkflow(t),
		ComfyUIModel: "local-video", ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// Interrupted polling must stop, never report completion, and never
// resubmit: the poll loop submits nothing, it only observes.
func TestComfyUIAcceptPollInterruptedByCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/history/poll-1":
			_, _ = w.Write([]byte(`{}`))
		case "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[[1,"poll-1"]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := comfyRegressionProvider(t, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	status, polls, err := pollComfyAcceptJob(ctx, provider, "poll-1", 5*time.Millisecond)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("poll error=%v, want deadline exceeded", err)
	}
	if polls < 1 {
		t.Fatalf("polls=%d, want at least 1 before interruption", polls)
	}
	if status.State == video.StateCompleted {
		t.Fatalf("interrupted poll reported completion: %+v", status)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, polls, err = pollComfyAcceptJob(cancelled, provider, "poll-1", 5*time.Millisecond)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled poll error=%v, want context canceled", err)
	}
	if polls != 0 {
		t.Fatalf("pre-cancelled poll made %d poll(s), want 0", polls)
	}
}

// A stalled job (in neither history nor queue) must time out at the
// harness deadline with exactly one submission, fail-closed cleanup,
// and no surviving workspace.
func TestComfyUIAcceptStalledJobTimesOutWithoutResubmit(t *testing.T) {
	var promptCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			promptCalls.Add(1)
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-stalled"}`))
		case r.URL.Path == "/history/prompt-stalled":
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
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
	if promptCalls.Load() != 1 {
		t.Fatalf("prompt submissions=%d, want exactly 1 for a stalled job", promptCalls.Load())
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:poll"); status != externalStatusFail || !strings.Contains(reason, "timed out") {
		t.Fatalf("poll status=%q reason=%q", status, reason)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup=%+v", report.Cleanup)
	}
	if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
		t.Fatalf("stalled-job workspace survived: %v", err)
	}
}

// Cleanup of a running prompt must record BLOCKED, never send a global
// interrupt, and still remove the local partial workspace.
func TestComfyUIAcceptRunningPromptCleanupIsBlockedNotInterrupt(t *testing.T) {
	var deletes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/queue" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"queue_running":[[1,"running-1"]],"queue_pending":[]}`))
		case r.URL.Path == "/queue" && r.Method == http.MethodPost:
			deletes.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := comfyRegressionProvider(t, server.URL)
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := &comfyAcceptReport{Job: &comfyAcceptJob{PromptID: "running-1"}}
	comfyAcceptFailClosed(report, runDir, provider)
	if report.Cleanup == nil || !report.Cleanup.RemoteAttempted || report.Cleanup.RemoteStatus != externalStatusBlocked {
		t.Fatalf("cleanup=%+v, want remote attempted + BLOCKED", report.Cleanup)
	}
	if report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup local=%q, want PASS", report.Cleanup.LocalStatus)
	}
	if deletes.Load() != 0 {
		t.Fatalf("cleanup issued %d delete(s) for a running prompt", deletes.Load())
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("partial workspace survived running-prompt cleanup: %v", err)
	}
}
