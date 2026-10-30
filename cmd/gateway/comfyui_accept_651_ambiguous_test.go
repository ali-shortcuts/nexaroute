package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// A submission whose connection drops after the server read /prompt but
// before any response is ambiguous: ComfyUI may already have queued the
// workflow, but the client has no prompt ID. The harness must submit
// exactly once, fail closed, retain no asset, remove its local workspace,
// leave remote cleanup NOT_RUN (there is no ID to target), and warn that
// a retry may duplicate a workflow that is actually queued.
func TestComfyUIAcceptAmbiguousSubmissionFailsClosedWithoutResubmit(t *testing.T) {
	var promptCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			promptCalls.Add(1)
			// Abort the connection after reading the request with no
			// response: the client sees EOF/reset, as with a proxy or
			// server crash after accepting the submission.
			panic(http.ErrAbortHandler)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "2s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if promptCalls.Load() != 1 {
		t.Fatalf("prompt submissions=%d, want exactly 1 (never retry an ambiguous submission)", promptCalls.Load())
	}
	if report.Outcome != externalStatusFail {
		t.Fatalf("outcome=%q, want FAIL", report.Outcome)
	}
	if report.InferenceExecuted {
		t.Fatal("report claims inference executed although no prompt ID was returned")
	}
	if report.Job != nil {
		t.Fatalf("report recorded a job without a prompt ID: %+v", report.Job)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:submit"); status != externalStatusFail {
		t.Fatalf("submit check status=%q, want FAIL", status)
	}
	if len(report.Assets) != 0 {
		t.Fatalf("assets=%+v, want none after an ambiguous submission", report.Assets)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass ||
		report.Cleanup.RemoteAttempted || report.Cleanup.RemoteStatus != externalStatusNotRun {
		t.Fatalf("cleanup=%+v, want local PASS, remote NOT_RUN without an attempt", report.Cleanup)
	}
	if !strings.Contains(report.Cleanup.RemoteReason, "check the server queue") {
		t.Fatalf("remote cleanup reason does not warn about a possibly queued prompt: %q", report.Cleanup.RemoteReason)
	}
	warned := false
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "idempotency") && strings.Contains(warning, "queue") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("report warnings do not disclose the ambiguous submission: %v", report.Warnings)
	}
	if report.Target.AssetDirectory != "" {
		if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
			t.Fatalf("partial workspace %q survived an ambiguous submission: %v", report.Target.AssetDirectory, err)
		}
	}
}

// A completion that lists two outputs sharing a stored filename must fail
// closed at the harness boundary too: no /view request, no retained asset,
// a FAIL download check, and the per-run workspace removed.
func TestComfyUIAcceptDuplicateOutputFilenamesFailsClosed(t *testing.T) {
	var viewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-duplicate"}`))
		case r.URL.Path == "/history/prompt-duplicate":
			_, _ = w.Write([]byte(`{"prompt-duplicate":{"outputs":{"10":{"videos":[{"filename":"same.mp4","subfolder":"a","type":"output"}]},"11":{"videos":[{"filename":"same.mp4","subfolder":"b","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/view":
			viewCalls.Add(1)
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("fixture-video-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), server.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t,
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--timeout", "2s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if viewCalls.Load() != 0 {
		t.Fatalf("view calls=%d, want 0 (collision must be refused before downloading)", viewCalls.Load())
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:download"); status != externalStatusFail ||
		!strings.Contains(reason, "duplicate") {
		t.Fatalf("download status=%q reason=%q, want FAIL duplicate rejection", status, reason)
	}
	if len(report.Assets) != 0 {
		t.Fatalf("collision run reported assets: %+v", report.Assets)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("cleanup=%+v, want local PASS", report.Cleanup)
	}
	if _, err := os.Stat(report.Target.AssetDirectory); !os.IsNotExist(err) {
		t.Fatalf("partial workspace survived a duplicate-output failure: %v", err)
	}
}
