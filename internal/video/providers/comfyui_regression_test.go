package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func comfyWorkflowFileWith(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Malformed workflows must fail at construction, before any server is
// contacted. The adapter validates shape and configured node inputs
// locally; node-class support itself is server-side (ComfyUI's registry
// includes custom nodes), so an unknown class_type defers to server
// validation instead of being rejected here.
func TestComfyUIMalformedWorkflowFilesRejected(t *testing.T) {
	base := comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:8188")
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "syntax error", raw: `{"1":`, want: "API-format JSON object"},
		{name: "top-level array", raw: `[]`, want: "API-format JSON object"},
		{name: "top-level string", raw: `"workflow"`, want: "API-format JSON object"},
		{name: "node without inputs object", raw: `{"1":{"class_type":"CLIPTextEncode"},"2":{"class_type":"EmptyImage","inputs":{"width":512,"height":512}},"3":{"class_type":"VideoSampler","inputs":{"length":16,"frame_rate":24}},"4":{"class_type":"CLIPTextEncode","inputs":{"text":"neg"}}}`, want: "no inputs object"},
		{name: "node inputs null", raw: `{"1":{"class_type":"CLIPTextEncode","inputs":null},"2":{"class_type":"EmptyImage","inputs":{"width":512,"height":512}},"3":{"class_type":"VideoSampler","inputs":{"length":16,"frame_rate":24}},"4":{"class_type":"CLIPTextEncode","inputs":{"text":"neg"}}}`, want: "no inputs object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.ComfyUIWorkflowPath = comfyWorkflowFileWith(t, tc.raw)
			if _, err := NewComfyUI(cfg, 1024); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewComfyUI error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestComfyUIOversizedWorkflowFileRejected(t *testing.T) {
	cfg := comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:8188")
	cfg.ComfyUIWorkflowPath = comfyWorkflowFileWith(t, strings.Repeat("x", (8<<20)+1))
	if _, err := NewComfyUI(cfg, 1024); err == nil || !strings.Contains(err.Error(), "between 1 byte and") {
		t.Fatalf("oversized workflow error=%v", err)
	}
}

// A server-side rejection (HTTP 400 with node_errors, or HTTP 500) must
// fail the submission closed: no job, exactly one submit attempt.
func TestComfyUIRejectedJobFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "validation rejection", status: http.StatusBadRequest, body: `{"error":{"message":"prompt validation failed"},"node_errors":{"5":{"errors":[{"message":"unknown node class"}]}}}`},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":"boom"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var submits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/prompt" && r.Method == http.MethodPost {
					submits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.CreateJob(context.Background(), validComfyRequest()); err == nil || !strings.Contains(err.Error(), "submit ComfyUI workflow") {
				t.Fatalf("CreateJob error=%v", err)
			}
			if submits.Load() != 1 {
				t.Fatalf("submits=%d, want exactly 1", submits.Load())
			}
		})
	}
}

// A 200 response that still carries node_errors must not be treated as
// an accepted job: the server is reporting that the workflow did not
// validate, even though it also echoed a prompt_id.
func TestComfyUINodeErrorsWithPromptIDRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prompt" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-bad","node_errors":{"5":{"errors":[{"message":"unknown node class"}]}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateJob(context.Background(), validComfyRequest()); err == nil || !strings.Contains(err.Error(), "node errors") {
		t.Fatalf("CreateJob error=%v, want node-errors rejection", err)
	}
}

// An unknown node class in the workflow is not an adapter-side rejection:
// the adapter has no server node registry. It must reach the server, and
// the server's rejection (above) is what fails the job closed.
func TestComfyUIUnknownNodeClassDefersToServerValidation(t *testing.T) {
	workflow := `{"1":{"class_type":"CLIPTextEncode","inputs":{"text":"old"}},"2":{"class_type":"EmptyImage","inputs":{"width":512,"height":512}},"3":{"class_type":"VideoSampler","inputs":{"length":16,"frame_rate":24,"seed":1}},"4":{"class_type":"CLIPTextEncode","inputs":{"text":"neg"}},"9":{"class_type":"DefinitelyNotARealNode","inputs":{}}}`
	var contacted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prompt" {
			contacted.Store(true)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"node_errors":{"9":{"errors":[{"message":"unknown"}]}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	cfg := comfyTestConfig(comfyWorkflowFileWith(t, workflow), server.URL)
	provider, err := NewComfyUI(cfg, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateJob(context.Background(), validComfyRequest()); err == nil {
		t.Fatal("unknown-node workflow unexpectedly accepted")
	}
	if !contacted.Load() {
		t.Fatal("unknown-node workflow never reached the server for validation")
	}
}

func TestComfyUIHistoryFailureStatesAreFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      string
		history string
		want    string
	}{
		{
			name:    "execution error",
			id:      "prompt-err",
			history: `{"prompt-err":{"outputs":{},"status":{"status_str":"error","completed":false}}}`,
			want:    "execution error",
		},
		{
			name:    "completed with images only",
			id:      "prompt-img",
			history: `{"prompt-img":{"outputs":{"10":{"images":[{"filename":"frame.png","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`,
			want:    "without an MP4 output",
		},
		{
			name:    "completed with webm only",
			id:      "prompt-webm",
			history: `{"prompt-webm":{"outputs":{"10":{"videos":[{"filename":"clip.webm","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`,
			want:    "without an MP4 output",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/history/"+id {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.history))
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
			if err != nil {
				t.Fatal(err)
			}
			status, err := provider.GetJob(context.Background(), id)
			if err != nil {
				t.Fatalf("GetJob error=%v", err)
			}
			if status.State != video.StateFailed || !strings.Contains(status.Error, tc.want) {
				t.Fatalf("status=%+v, want failed containing %q", status, tc.want)
			}
		})
	}
}

// A prompt that is in neither history nor queue is stalled/unknown: the
// adapter must say so instead of inventing a state.
func TestComfyUIStalledJobAbsentFromHistoryAndQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/history/ghost-1":
			_, _ = w.Write([]byte(`{}`))
		case "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.GetJob(context.Background(), "ghost-1"); err == nil || !strings.Contains(err.Error(), "absent from history and queue") {
		t.Fatalf("stalled job error=%v", err)
	}
}

// Cancelling a running prompt must refuse without touching /queue with a
// delete: ComfyUI has no per-prompt interrupt, and a global interrupt
// would kill unrelated work.
func TestComfyUICancelRunningRefusesWithoutDelete(t *testing.T) {
	var deletes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/queue" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[[1,"running-1"]],"queue_pending":[]}`))
		case r.URL.Path == "/queue" && r.Method == http.MethodPost:
			deletes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.CancelJob(context.Background(), "running-1"); err == nil || !strings.Contains(err.Error(), "refusing global interrupt") {
		t.Fatalf("running cancel error=%v", err)
	}
	if deletes.Load() != 0 {
		t.Fatalf("running cancel issued %d delete(s)", deletes.Load())
	}
}

func TestComfyUIDownloadRejectsInvalidOutputs(t *testing.T) {
	status := video.ProviderJobStatus{Outputs: []video.Asset{{
		ID: "1", Filename: "noora.mp4", Metadata: map[string]string{"filename": "noora.mp4", "type": "output"},
	}}}
	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
		statusCode  int
		want        string
	}{
		{name: "image content type", contentType: "image/png", body: []byte("not video"), statusCode: http.StatusOK, want: "non-video content type"},
		{name: "empty body", contentType: "video/mp4", body: []byte{}, statusCode: http.StatusOK, want: "empty video output"},
		{name: "not found", contentType: "application/json", body: []byte(`{"error":"no such file"}`), statusCode: http.StatusNotFound, want: "HTTP 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/view" {
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(tc.statusCode)
					_, _ = w.Write(tc.body)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.DownloadOutputs(context.Background(), status, &comfyTestSink{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DownloadOutputs error=%v, want %q", err, tc.want)
			}
		})
	}
}

// The size limit is exclusive: exactly maxOutputBytes is accepted.
func TestComfyUIDownloadAtExactLimitPasses(t *testing.T) {
	data := []byte("12345678")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/view" {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	status := video.ProviderJobStatus{Outputs: []video.Asset{{
		ID: "1", Filename: "noora.mp4", Metadata: map[string]string{"filename": "noora.mp4", "type": "output"},
	}}}
	assets, err := provider.DownloadOutputs(context.Background(), status, &comfyTestSink{})
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
}

// A cancelled context must fail a status read, never return a state.
func TestComfyUIGetJobRespectsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.GetJob(ctx, "prompt-1"); err == nil {
		t.Fatal("cancelled GetJob unexpectedly returned a status")
	}
}

// A malformed history payload must not invent a terminal state: the
// queue fallback still applies, so a queued prompt reads as queued.
func TestComfyUIMalformedHistoryFallsBackToQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/history/pending-9":
			_, _ = w.Write([]byte(`{not json`))
		case "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[[1,"pending-9"]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	status, err := provider.GetJob(context.Background(), "pending-9")
	if err != nil || status.State != video.StateQueued {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
