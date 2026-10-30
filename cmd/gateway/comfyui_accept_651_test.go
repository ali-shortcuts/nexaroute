package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A submission whose /prompt request never completes (harness deadline)
// must result in exactly one POST, a FAIL outcome, no retained asset,
// local cleanup PASS, and remote cleanup NOT_RUN: without a prompt ID
// there is no remote entry to target, and the harness must not claim
// otherwise — nor silently retry into a duplicate prompt.
func TestComfyUIAcceptSubmitTimeoutSubmitsExactlyOnce(t *testing.T) {
	var promptCalls atomic.Int32
	// The handler blocks until the test releases it. It must NOT
	// block on the request context alone: the client transport does
	// not reliably tear the server-side connection down on a client
	// context deadline, so server.Close would wait forever on the
	// blocked handler (observed deadlock, not a product defect).
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"os":"posix","comfyui_version":"0.3.40"}}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			promptCalls.Add(1)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() { close(release); server.Close() }()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "500ms", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if promptCalls.Load() != 1 {
		t.Fatalf("prompt calls=%d, want exactly 1", promptCalls.Load())
	}
	if report.Outcome != externalStatusFail {
		t.Fatalf("outcome=%q, want FAIL", report.Outcome)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:submit"); status != externalStatusFail {
		t.Fatalf("submit check status=%q, want FAIL", status)
	}
	if report.InferenceExecuted {
		t.Fatal("report claims inference executed although submission never completed")
	}
	if len(report.Assets) != 0 {
		t.Fatalf("assets=%+v, want none after a failed submission", report.Assets)
	}
	if report.Cleanup == nil {
		t.Fatal("report has no cleanup record")
	}
	if report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("local cleanup status=%q, want PASS", report.Cleanup.LocalStatus)
	}
	if report.Cleanup.RemoteStatus != externalStatusNotRun || report.Cleanup.RemoteAttempted {
		t.Fatalf("remote cleanup=%+v, want NOT_RUN without an attempt (no prompt ID exists)", report.Cleanup)
	}
	if report.Target.AssetDirectory != "" {
		if _, err := os.Stat(report.Target.AssetDirectory); err == nil {
			t.Fatalf("partial workspace %q was retained after a failed submission", report.Target.AssetDirectory)
		}
	}
	// The run directory itself must be gone, not just unlisted: walk the
	// asset root given to the run and require no run-* directory remains.
	assetRoot := filepath.Dir(report.Target.AssetDirectory)
	entries, err := os.ReadDir(assetRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run-") {
			t.Fatalf("partial workspace %q was retained", entry.Name())
		}
	}
}
