package providers

// Second-wave coverage for issue #662 (video runtime, providers, gateway).
//
// This file covers only blocks that were still at zero count on the
// ba5ff2f baseline after PR #668, in the areas the issue prioritizes:
// configuration/input validation, provider timeouts/errors/rate limits,
// and cancellation/cleanup-adjacent failure paths. Every fake here is a
// loopback stand-in for the LOCAL adapter contract (request shapes,
// error mapping, single-attempt behavior); none of it claims a live
// provider passed anything.
//
// Deliberately NOT covered here (documented in the PR body instead):
//   - aggregator download request-build error: url.Parse already rejects
//     every string http.NewRequest would reject, so the branch is
//     defensive-only.
//   - aggregator download body-close error: download builds its own
//     egress client, so no injectable transport can produce a close
//     error; real transports effectively never fail Close.
//   - code-render job-directory retry failure, SRT-empty, SRT write
//     failure: the first needs a non-root read-only work root (tests
//     run as root here), the second is excluded by plan validation
//     (total duration > 0) plus the empty-narration gate, and the third
//     needs a filesystem failure inside a freshly created job dir.
//   - runtime blocks: see the PR body (live Postgres, os.Hostname
//     failure, racy worker-done branch, hardcoded hourly ticker).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// gap2PartialBodyServer serves one response that declares Content-Length
// larger than the bytes it actually sends, then closes the connection.
// It models a truncated upstream body: ReadAll must fail instead of
// silently accepting a short asset.
func gap2PartialBodyServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: video/mp4\r\n\r\nshort"))
	}()
	return "http://" + ln.Addr().String()
}

type gap2FailCloseBody struct {
	data []byte
	done bool
}

func (b *gap2FailCloseBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	return copy(p, b.data), nil
}

func (b *gap2FailCloseBody) Close() error {
	return errors.New("gap2 simulated close failure")
}

type gap2FailCloseTransport struct{}

func (gap2FailCloseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"video/mp4"}},
		Body:       &gap2FailCloseBody{data: []byte("fake-mp4-bytes")},
	}, nil
}

func gap2AggRequest() video.VideoRequest {
	return video.VideoRequest{
		ProjectID: "p", Prompt: "a red cube", DurationSeconds: 5,
		AspectRatio: video.Aspect16x9, Mode: video.ModeTextToVideo,
	}
}

func TestGap2AggregatorNonLoopbackPlainHTTPRejected(t *testing.T) {
	t.Setenv("ISSUE662_AGG_KEY", "gap2-key")
	cfg := issue662AggConfig()
	cfg.AggregatorBaseURL = "http://192.0.2.1"
	if _, err := NewAggregator(cfg, 1<<20); err == nil || !strings.Contains(err.Error(), "only allowed on loopback") {
		t.Fatalf("non-loopback plain HTTP error = %v, want loopback refusal", err)
	}
	cfg.AggregatorBaseURL = "https://queue.example.test"
	if _, err := NewAggregator(cfg, 1<<20); err != nil {
		t.Fatalf("loopback-exempt https constructor = %v, want nil", err)
	}
	// An https link-local address passes the constructor's own checks
	// but must fail the egress policy construction, before any dial.
	cfg.AggregatorBaseURL = "https://169.254.169.254"
	if _, err := NewAggregator(cfg, 1<<20); err == nil || !strings.Contains(err.Error(), "egress policy") {
		t.Fatalf("link-local https constructor error = %v, want egress refusal", err)
	}
}

func TestGap2AggregatorReferenceAssetOnlyCarriesImageField(t *testing.T) {
	var submitted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/test/model-v1" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
			t.Errorf("decode submit: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"req-ref-1"}`))
	}))
	defer srv.Close()
	cfg := issue662AggConfig()
	cfg.AggregatorBaseURL = srv.URL
	cfg.AggregatorImageField = "image_url"
	p := issue662AggProvider(t, cfg)
	req := gap2AggRequest()
	req.Mode = video.ModeImageToVideo
	req.ReferenceAssets = []video.Asset{{ID: "ref", URI: "https://example.test/ref.png"}}
	if err := p.ValidateRequest(req); err != nil {
		t.Fatalf("reference-only request must validate: %v", err)
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if job.ProviderJobID != "req-ref-1" {
		t.Fatalf("job id = %q, want req-ref-1", job.ProviderJobID)
	}
	if submitted["image_url"] != "https://example.test/ref.png" {
		t.Fatalf("submit body image_url = %v, want the reference asset URI", submitted["image_url"])
	}
}

func TestGap2AggregatorCreateJobErrorPaths(t *testing.T) {
	newProvider := func(handler http.HandlerFunc) *Aggregator {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		cfg := issue662AggConfig()
		cfg.AggregatorBaseURL = srv.URL
		return issue662AggProvider(t, cfg)
	}
	t.Run("upstream 500 surfaces as submit error", func(t *testing.T) {
		p := newProvider(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
		})
		if _, err := p.CreateJob(context.Background(), gap2AggRequest()); err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("CreateJob error = %v, want HTTP 500", err)
		}
	})
	t.Run("empty request_id rejected", func(t *testing.T) {
		p := newProvider(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		})
		if _, err := p.CreateJob(context.Background(), gap2AggRequest()); err == nil || !strings.Contains(err.Error(), "carried no request_id") {
			t.Fatalf("CreateJob error = %v, want request_id complaint", err)
		}
	})
}

func TestGap2AggregatorCompletedWithoutVideoURLFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	cfg := issue662AggConfig()
	cfg.AggregatorBaseURL = srv.URL
	p := issue662AggProvider(t, cfg)
	status, err := p.GetJob(context.Background(), "req-done")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != video.StateFailed || !strings.Contains(status.Error, "completed without a video output URL") {
		t.Fatalf("completed-without-URL status = %+v, want failed with the URL complaint", status)
	}
}

func TestGap2AggregatorCompletedResultFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = w.Write([]byte(`{"status":"COMPLETED"}`))
			return
		}
		http.Error(w, "result exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := issue662AggConfig()
	cfg.AggregatorBaseURL = srv.URL
	p := issue662AggProvider(t, cfg)
	if _, err := p.GetJob(context.Background(), "req-done"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("completed result-fetch error = %v, want HTTP 500", err)
	}
}

func TestGap2ExtractAggregatorVideoFallbacks(t *testing.T) {
	if u, _ := extractAggregatorVideo(map[string]any{"video_url": "https://cdn.example.test/a.mp4"}); u != "https://cdn.example.test/a.mp4" {
		t.Fatalf("video_url fallback = %q", u)
	}
	if u, _ := extractAggregatorVideo(map[string]any{"url": "https://cdn.example.test/b.mp4"}); u != "https://cdn.example.test/b.mp4" {
		t.Fatalf("url fallback = %q", u)
	}
	// A non-string, non-object "video" value must fall through to the
	// flat fallbacks instead of being mistaken for a URL.
	if u, _ := extractAggregatorVideo(map[string]any{"video": float64(123), "video_url": "https://cdn.example.test/c.mp4"}); u != "https://cdn.example.test/c.mp4" {
		t.Fatalf("non-string video fall-through = %q", u)
	}
	if u, _ := extractAggregatorVideo(map[string]any{}); u != "" {
		t.Fatalf("empty result video = %q, want empty", u)
	}
}

func TestGap2AggregatorCancelJobRequiresID(t *testing.T) {
	p := issue662AggProvider(t, issue662AggConfig())
	if err := p.CancelJob(context.Background(), "   "); err == nil || !strings.Contains(err.Error(), "job ID is required") {
		t.Fatalf("CancelJob(empty) error = %v, want job ID complaint", err)
	}
}

func TestGap2AggregatorDownloadFailurePaths(t *testing.T) {
	p := issue662AggProvider(t, issue662AggConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Run("unparseable URL", func(t *testing.T) {
		if _, _, err := p.download(ctx, "://bad"); err == nil || !strings.Contains(err.Error(), "parse aggregator output URL") {
			t.Fatalf("download error = %v, want parse complaint", err)
		}
		if _, _, err := p.download(ctx, "http:///no-host"); err == nil {
			t.Fatal("hostless URL must fail parsing")
		}
	})
	t.Run("egress refuses link-local target before any dial", func(t *testing.T) {
		if _, _, err := p.download(ctx, "https://169.254.169.254/clip.mp4"); err == nil || !strings.Contains(err.Error(), "egress policy") {
			t.Fatalf("download error = %v, want egress refusal (no dial attempted)", err)
		}
	})
	t.Run("dead loopback port surfaces dial failure", func(t *testing.T) {
		if _, _, err := p.download(ctx, "http://127.0.0.1:9/clip.mp4"); err == nil || !strings.Contains(err.Error(), "download aggregator output") {
			t.Fatalf("download error = %v, want dial failure", err)
		}
	})
	t.Run("truncated body surfaces read failure", func(t *testing.T) {
		if _, _, err := p.download(ctx, gap2PartialBodyServer(t)+"/clip.mp4"); err == nil || !strings.Contains(err.Error(), "read aggregator output") {
			t.Fatalf("download error = %v, want read failure", err)
		}
	})
}

func TestGap2AggregatorDoJSONReadError(t *testing.T) {
	p := issue662AggProvider(t, issue662AggConfig())
	var out map[string]any
	if err := p.doJSON(context.Background(), http.MethodGet, gap2PartialBodyServer(t)+"/status", nil, &out); err == nil || !strings.Contains(err.Error(), "read aggregator response") {
		t.Fatalf("doJSON error = %v, want read failure", err)
	}
}

func TestGap2AggregatorRateLimitSurfacesWithoutRetry(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer srv.Close()
	cfg := issue662AggConfig()
	cfg.AggregatorBaseURL = srv.URL
	p := issue662AggProvider(t, cfg)
	if _, err := p.CreateJob(context.Background(), gap2AggRequest()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("CreateJob error = %v, want HTTP 429", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("429 produced %d upstream calls, want exactly 1 (no silent retry)", calls.Load())
	}
}

// NOTE: comfyui.go's empty-prompt branch (ValidateRequest, "ComfyUI
// requires a non-empty text prompt") is deliberately NOT tested here:
// it is unreachable through ValidateRequest. The shared validation
// rejects every input that would satisfy its condition (empty prompt
// with zero shots, or with an empty-prompt shot), a single valid shot
// makes the condition false, and multi-shot inputs fail earlier at the
// adapter's one-workflow check. The branch is defensive-only; the
// shared-validation rejections are the live behavior.

func TestGap2ComfyUIOutputDimensions(t *testing.T) {
	if w, h := outputDimensions(video.VideoRequest{AspectRatio: video.Aspect1x1}); w != 1024 || h != 1024 {
		t.Fatalf("1:1 dimensions = %dx%d, want 1024x1024", w, h)
	}
	if w, h := outputDimensions(video.VideoRequest{AspectRatio: video.Aspect9x16}); w != 720 || h != 1280 {
		t.Fatalf("9:16 dimensions = %dx%d, want 720x1280", w, h)
	}
}

func TestGap2ComfyUISetWorkflowInputWithoutInputsObject(t *testing.T) {
	workflow := map[string]map[string]any{
		"6": {"class_type": "CLIPTextEncode"},
	}
	if err := setWorkflowInput(workflow, "6", "text", "hello"); err == nil || !strings.Contains(err.Error(), "has no inputs object") {
		t.Fatalf("setWorkflowInput error = %v, want inputs-object complaint", err)
	}
	if err := setWorkflowInput(workflow, "missing", "text", "hello"); err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("setWorkflowInput missing node error = %v, want disappeared complaint", err)
	}
}

func TestGap2ComfyUIGetJobProcessingWithOutputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/history/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"prompt-1":{"status":{"status_str":"success","completed":false},"outputs":{"9":{"videos":[{"filename":"out.mp4","subfolder":"","type":"output"}]}}}}`))
	}))
	defer srv.Close()
	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), srv.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status, err := p.GetJob(context.Background(), "prompt-1")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != video.StateProcessing || status.Progress != 0.75 {
		t.Fatalf("incomplete-with-outputs status = %+v, want processing at 0.75", status)
	}
	if len(status.Outputs) != 1 || status.Outputs[0].Filename != "out.mp4" || status.Outputs[0].Metadata["node_id"] != "9" || status.Outputs[0].Metadata["provider"] != "comfyui" {
		t.Fatalf("outputs = %+v, want the single mp4 with node/provider metadata", status.Outputs)
	}
}

func gap2ComfyOutputStatus() video.ProviderJobStatus {
	return video.ProviderJobStatus{
		ProviderJobID: "prompt-1", State: video.StateCompleted,
		Outputs: []video.Asset{{
			ID: "prompt-1-9-clip.mp4", Kind: "video", Filename: "clip.mp4",
			Metadata: map[string]string{
				"filename": "clip.mp4", "subfolder": "", "type": "output",
				"project_id": "p", "job_id": "j",
			},
		}},
	}
}

func TestGap2ComfyUIDownloadFailurePaths(t *testing.T) {
	t.Run("dead loopback port surfaces dial failure", func(t *testing.T) {
		p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:9"), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.DownloadOutputs(context.Background(), gap2ComfyOutputStatus(), nil); err == nil || !strings.Contains(err.Error(), "sink is not configured") {
			t.Fatalf("nil sink error = %v, want sink complaint", err)
		}
		sink, err := storage.NewLocal(t.TempDir(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.DownloadOutputs(context.Background(), gap2ComfyOutputStatus(), sink); err == nil || !strings.Contains(err.Error(), "download ComfyUI output") {
			t.Fatalf("dead-port download error = %v, want dial failure", err)
		}
	})
	t.Run("truncated body surfaces read failure", func(t *testing.T) {
		p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), gap2PartialBodyServer(t)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		sink, err := storage.NewLocal(t.TempDir(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.DownloadOutputs(context.Background(), gap2ComfyOutputStatus(), sink); err == nil || !strings.Contains(err.Error(), "read ComfyUI output") {
			t.Fatalf("truncated download error = %v, want read failure", err)
		}
	})
	t.Run("close failure surfaces instead of silent success", func(t *testing.T) {
		p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:8188"), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		p.client = &http.Client{Transport: gap2FailCloseTransport{}}
		sink, err := storage.NewLocal(t.TempDir(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.DownloadOutputs(context.Background(), gap2ComfyOutputStatus(), sink); err == nil || !strings.Contains(err.Error(), "close ComfyUI output response") {
			t.Fatalf("close-failure download error = %v, want close complaint", err)
		}
	})
}

func TestGap2ComfyUIDownloadContentTypeFallbacks(t *testing.T) {
	serve := func(contentType string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			_, _ = w.Write([]byte("fake-mp4-bytes"))
		}))
	}
	// A Go test server always sends some Content-Type, so the octet-stream
	// shape exercises the same fallback branches a missing type would.
	cases := []struct {
		name     string
		ct       string
		filename string
		want     string
	}{
		{"charset suffix stripped", "video/mp4; charset=binary", "clip.mp4", "video/mp4"},
		{"octet-stream with mp4 extension guessed", "application/octet-stream", "clip.mp4", "video/mp4"},
		{"octet-stream with unknown extension falls back", "application/octet-stream", "clip.xyz", "video/mp4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.ct)
			defer srv.Close()
			p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), srv.URL), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			status := gap2ComfyOutputStatus()
			status.Outputs[0].Filename = tc.filename
			status.Outputs[0].Metadata["filename"] = tc.filename
			sink, err := storage.NewLocal(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := p.DownloadOutputs(context.Background(), status, sink)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved) != 1 || saved[0].ContentType != tc.want {
				t.Fatalf("saved content type = %v, want %q", saved, tc.want)
			}
			if saved[0].SizeBytes != int64(len("fake-mp4-bytes")) {
				t.Fatalf("saved size = %d, want %d", saved[0].SizeBytes, len("fake-mp4-bytes"))
			}
		})
	}
}

func TestGap2ComfyUIDoJSONRequestBuildAndEmptyBody(t *testing.T) {
	p := &ComfyUI{baseURL: "http://127.0.0.1:9/\x7f", client: http.DefaultClient}
	var out map[string]any
	if err := p.doJSON(context.Background(), http.MethodGet, "/history/x", nil, &out); err == nil {
		t.Fatal("control-character base URL must fail request construction")
	}
	// A nil sink fails before request construction, so pass a real sink:
	// the request-build error must surface first.
	sink, err := storage.NewLocal(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownloadOutputs(context.Background(), gap2ComfyOutputStatus(), sink); err == nil {
		t.Fatal("control-character base URL must fail download request construction")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p2, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), srv.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.doJSON(context.Background(), http.MethodGet, "/queue", nil, nil); err != nil {
		t.Fatalf("empty-body nil-out doJSON = %v, want nil", err)
	}
	// A payload-bearing call exercises the JSON content-type header
	// even when the response body is empty.
	if err := p2.doJSON(context.Background(), http.MethodPost, "/queue", map[string]any{"delete": []string{"x"}}, nil); err != nil {
		t.Fatalf("empty-body payload doJSON = %v, want nil", err)
	}
	// A truncated response body must surface as a read error, not a
	// silent empty result.
	p3, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), gap2PartialBodyServer(t)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var queueOut map[string]any
	if err := p3.doJSON(context.Background(), http.MethodGet, "/queue", nil, &queueOut); err == nil {
		t.Fatal("truncated doJSON body must fail the read")
	}
}

func TestGap2ComfyUIRateLimitSurfacesWithoutRetry(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer srv.Close()
	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), srv.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	req := video.VideoRequest{
		ProjectID: "p", Prompt: "a red cube", Mode: video.ModeTextToVideo,
		DurationSeconds: 1, AspectRatio: video.Aspect16x9,
	}
	if _, err := p.CreateJob(context.Background(), req); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("CreateJob error = %v, want HTTP 429", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("429 produced %d upstream calls, want exactly 1 (no silent retry)", calls.Load())
	}
}

// The partial-body helper must actually truncate, or the read-error
// tests above would be vacuous.
func TestGap2PartialBodyServerTruncates(t *testing.T) {
	url := gap2PartialBodyServer(t)
	resp, err := http.Get(url + "/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("truncated body must fail the read")
	}
}
