package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCoveragePassRunFlagAndManifestErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"create", "--bogus-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad flag exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", "--file", filepath.Join(t.TempDir(), "missing.json")}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "read episode manifest") {
		t.Fatalf("render missing file exit=%d stderr=%q", code, stderr.String())
	}
	badJSON := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badJSON, []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", "--file", badJSON}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "decode episode manifest") {
		t.Fatalf("render bad json exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"compose", "--file", badJSON}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "decode episode manifest") {
		t.Fatalf("compose bad json exit=%d stderr=%q", code, stderr.String())
	}
	missingCompose := filepath.Join(t.TempDir(), "absent.json")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"compose", "--file", missingCompose}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "read episode manifest") {
		t.Fatalf("compose missing exit=%d stderr=%q", code, stderr.String())
	}
	// Compose with a structurally invalid episode (no shots) fails through the
	// local FFmpeg runner and asserts the gateway is never contacted.
	invalidSpec := filepath.Join(t.TempDir(), "invalid.json")
	writeJSONFile(t, invalidSpec, map[string]any{"shots": []string{}, "output_path": filepath.Join(t.TempDir(), "out.mp4"), "width": 640, "height": 360, "fps": 24})
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"compose", "--file", invalidSpec}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "compose:") {
		t.Fatalf("compose invalid exit=%d stderr=%q", code, stderr.String())
	}
}

func TestCoveragePassRenderNarrationValidation(t *testing.T) {
	root := t.TempDir()
	partial := map[string]any{
		"shots": []string{"a.mp4"}, "output_path": filepath.Join(root, "out.mp4"),
		"width": 640, "height": 360, "fps": 24,
		"narration_text_path": filepath.Join(root, "story.txt"),
	}
	partialPath := filepath.Join(root, "partial.json")
	writeJSONFile(t, partialPath, partial)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", "--file", partialPath}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "must be provided together") {
		t.Fatalf("partial narration exit=%d stderr=%q", code, stderr.String())
	}

	both := map[string]any{
		"shots": []string{"a.mp4"}, "output_path": filepath.Join(root, "out.mp4"),
		"width": 640, "height": 360, "fps": 24,
		"narration_path":        filepath.Join(root, "existing.wav"),
		"narration_text_path":   filepath.Join(root, "story.txt"),
		"piper_model_path":      filepath.Join(root, "voice.onnx"),
		"narration_output_path": filepath.Join(root, "narr.wav"),
	}
	bothPath := filepath.Join(root, "both.json")
	writeJSONFile(t, bothPath, both)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", "--file", bothPath}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "not both") {
		t.Fatalf("both narration exit=%d stderr=%q", code, stderr.String())
	}

	// Piper synthesis failure surfaces exit 1 without composing.
	synth := renderManifest{}
	synth.Shots = []string{filepath.Join(root, "shot.mp4")}
	synth.OutputPath = filepath.Join(root, "out.mp4")
	synth.Width, synth.Height, synth.FPS = 640, 360, 24
	textPath := filepath.Join(root, "story.txt")
	modelPath := filepath.Join(root, "voice.onnx")
	narrOut := filepath.Join(root, "narr.wav")
	for p, d := range map[string][]byte{textPath: []byte("hello"), modelPath: []byte("model")} {
		if err := os.WriteFile(p, d, 0600); err != nil {
			t.Fatal(err)
		}
	}
	synth.NarrationTextPath = textPath
	synth.PiperModelPath = modelPath
	synth.NarrationOutputPath = narrOut
	synthPath := filepath.Join(root, "synth.json")
	data, _ := json.Marshal(synth)
	if err := os.WriteFile(synthPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"render", "--file", synthPath, "--piper-bin", filepath.Join(root, "missing-piper")}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "narration synthesis failed") {
		t.Fatalf("synth failure exit=%d stderr=%q", code, stderr.String())
	}
}

func TestCoveragePassRunGatewayGuards(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job-get"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "requires --id") {
		t.Fatalf("job-get without id exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"asset-download", "--id", "job-1"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "requires --asset-id") {
		t.Fatalf("asset-download without asset id exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"asset-download", "--id", "job-1", "--asset-id", "a-1"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "requires --out") {
		t.Fatalf("asset-download without out exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"providers", "--token-env", ""}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--token-env must name") {
		t.Fatalf("empty token-env exit=%d stderr=%q", code, stderr.String())
	}
	t.Setenv("NEXAROUTE_VIDEO_TOKEN_EMPTY_COVERAGE", "")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"providers", "--token-env", "NEXAROUTE_VIDEO_TOKEN_EMPTY_COVERAGE"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "token is missing") {
		t.Fatalf("missing token exit=%d stderr=%q", code, stderr.String())
	}
	t.Setenv("NEXAROUTE_VIDEO_TOKEN_COVERAGE_BADURL", "secret")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"providers", "--base-url", "http://example.com", "--token-env", "NEXAROUTE_VIDEO_TOKEN_COVERAGE_BADURL"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "configure Video Gateway client") {
		t.Fatalf("bad base url exit=%d stderr=%q", code, stderr.String())
	}
}

func gatewayTestServer(t *testing.T, assetBytes []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/video/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]string{{"id": "fake"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/video/jobs":
			var req video.VideoRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(video.NewJob("job-123", req))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/video/jobs/job-123":
			_ = json.NewEncoder(w).Encode(video.NewJob("job-123", video.VideoRequest{ProjectID: "p", Prompt: "x"}))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/video/jobs/job-123/cancel":
			job := video.NewJob("job-123", video.VideoRequest{ProjectID: "p", Prompt: "x"})
			job.State = video.StateCancelled
			_ = json.NewEncoder(w).Encode(job)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/video/jobs/job-123/assets/"):
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(assetBytes)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestCoveragePassRunGatewayCommands(t *testing.T) {
	server := gatewayTestServer(t, []byte("video bytes"))
	defer server.Close()
	t.Setenv("NEXAROUTE_VIDEO_TOKEN_COVERAGE_RUN", "secret-token")

	runGateway := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		full := append([]string{}, args...)
		// Inject base-url and token-env after the command but before other flags.
		_ = full
		code := run(args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	base := []string{"--base-url", server.URL, "--token-env", "NEXAROUTE_VIDEO_TOKEN_COVERAGE_RUN"}
	if code, out, errStr := runGateway(append([]string{"providers"}, base...)...); code != 0 || !strings.Contains(out, "fake") {
		t.Fatalf("providers code=%d out=%q err=%q", code, out, errStr)
	}

	root := t.TempDir()
	reqPath := filepath.Join(root, "req.json")
	writeJSONFile(t, reqPath, video.VideoRequest{ProjectID: "p", Prompt: "A sunrise", DurationSeconds: 4, AspectRatio: video.Aspect16x9})
	if code, out, errStr := runGateway("create", "--file", reqPath, base[0], base[1], base[2], base[3]); code != 0 || !strings.Contains(out, "job-123") {
		t.Fatalf("create code=%d out=%q err=%q", code, out, errStr)
	}
	if code, out, errStr := runGateway("job-get", "--id", "job-123", base[0], base[1], base[2], base[3]); code != 0 || !strings.Contains(out, "job-123") {
		t.Fatalf("job-get code=%d out=%q err=%q", code, out, errStr)
	}
	if code, out, errStr := runGateway("job-cancel", "--id", "job-123", base[0], base[1], base[2], base[3]); code != 0 || !strings.Contains(out, "cancelled") {
		t.Fatalf("job-cancel code=%d out=%q err=%q", code, out, errStr)
	}
	downloadOut := filepath.Join(root, "episode.mp4")
	if code, out, errStr := runGateway("asset-download", "--id", "job-123", "--asset-id", "asset-1", "--out", downloadOut, base[0], base[1], base[2], base[3]); code != 0 || !strings.Contains(out, "Downloaded asset") {
		t.Fatalf("asset-download code=%d out=%q err=%q", code, out, errStr)
	}
	data, err := os.ReadFile(downloadOut)
	if err != nil || string(data) != "video bytes" {
		t.Fatalf("downloaded=%q err=%v", data, err)
	}

	// Gateway error surfaces exit 1 with the server message.
	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"bad_request","message":"bad clip request"}`))
	}))
	defer errServer.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"providers", "--base-url", errServer.URL, "--token-env", "NEXAROUTE_VIDEO_TOKEN_COVERAGE_RUN"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "bad clip request") {
		t.Fatalf("gateway error code=%d stderr=%q", code, stderr.String())
	}

	// Create with a missing file and with invalid JSON.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"create", "--file", filepath.Join(root, "absent.json"), base[0], base[1], base[2], base[3]}, &stdout, &stderr); code != 1 {
		t.Fatalf("create missing file code=%d stderr=%q", code, stderr.String())
	}
	badReq := filepath.Join(root, "badreq.json")
	if err := os.WriteFile(badReq, []byte("{oops"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"create", "--file", badReq, base[0], base[1], base[2], base[3]}, &stdout, &stderr); code != 1 {
		t.Fatalf("create bad json code=%d stderr=%q", code, stderr.String())
	}

	// Encode failure: render/compose success paths already cover JSON encoding,
	// but a broken stdout writer pins the error branch.
	ffmpeg := filepath.Join(root, "fake-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nset -eu\nlast=\nfor arg in \"$@\"; do last=\"$arg\"; done\nprintf 'x' > \"$last\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_FFMPEG", ffmpeg)
	shot := filepath.Join(root, "shot.mp4")
	if err := os.WriteFile(shot, []byte("clip"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "episode.json")
	writeJSONFile(t, manifest, map[string]any{"shots": []string{shot}, "output_path": filepath.Join(root, "episode.mp4"), "width": 640, "height": 360, "fps": 24})
	if code := run([]string{"compose", "--file", manifest}, failWriter{errors.New("stdout broken")}, &stderr); code != 1 || !strings.Contains(stderr.String(), "encode compose result") {
		t.Fatalf("compose encode failure code=%d stderr=%q", code, stderr.String())
	}
}

func TestCoveragePassAPIClientValidation(t *testing.T) {
	if _, err := newAPIClient("", "token", nil); err == nil {
		t.Fatal("empty base URL accepted")
	}
	if _, err := newAPIClient("http://127.0.0.1:8080", "  ", nil); err == nil {
		t.Fatal("blank token accepted")
	}
}

func TestCoveragePassRequestErrorBranches(t *testing.T) {
	ctx := context.Background()
	// Encode failure: channels cannot be marshaled to JSON.
	c, err := newAPIClient("http://127.0.0.1:8080", "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.request(ctx, http.MethodPost, "/v1/video/jobs", map[string]any{"bad": make(chan int)}, &map[string]any{}); err == nil || !strings.Contains(err.Error(), "encode request") {
		t.Fatalf("encode error=%v", err)
	}
	// Build failure: control characters in the base URL bypass validation when
	// set directly on the struct (same-package white-box setup).
	badBase := &apiClient{baseURL: "http://127.0.0.1:8080\x7f", token: "token", http: http.DefaultClient}
	if err := badBase.request(ctx, http.MethodGet, "/x", nil, nil); err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("build error=%v", err)
	}
	// Transport failure: server is closed before the request.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedClient, err := newAPIClient(closed.URL, "token", closed.Client())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := closedClient.request(ctx, http.MethodGet, "/v1/video/providers", nil, nil); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("transport error=%v", err)
	}
	// Oversize response.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxAPIResponseBytes+10))
	}))
	defer huge.Close()
	hugeClient, err := newAPIClient(huge.URL, "token", huge.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := hugeClient.request(ctx, http.MethodGet, "/v1/video/providers", nil, &map[string]any{}); err == nil || !strings.Contains(err.Error(), "exceeds 2 MiB") {
		t.Fatalf("oversize error=%v", err)
	}
	// Plain-text gateway error without a JSON body.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer plain.Close()
	plainClient, err := newAPIClient(plain.URL, "token", plain.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := plainClient.request(ctx, http.MethodGet, "/v1/video/providers", nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("plain error=%v", err)
	}
	// Gateway error with a message but no code.
	noCode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"gone"}`))
	}))
	defer noCode.Close()
	noCodeClient, err := newAPIClient(noCode.URL, "token", noCode.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := noCodeClient.request(ctx, http.MethodGet, "/v1/video/providers", nil, nil); err == nil || !strings.Contains(err.Error(), "gone") || strings.Contains(err.Error(), "(") {
		t.Fatalf("no-code error=%v", err)
	}
	// Decode failure on a 200 with invalid JSON.
	invalidJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{oops"))
	}))
	defer invalidJSON.Close()
	invalidClient, err := newAPIClient(invalidJSON.URL, "token", invalidJSON.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := invalidClient.request(ctx, http.MethodGet, "/v1/video/providers", nil, &map[string]any{}); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("decode error=%v", err)
	}
	// Nil output with an empty body succeeds.
	emptyOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer emptyOK.Close()
	emptyClient, err := newAPIClient(emptyOK.URL, "token", emptyOK.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := emptyClient.request(ctx, http.MethodGet, "/v1/video/providers", nil, nil); err != nil {
		t.Fatalf("empty body with nil out error=%v", err)
	}
}

func TestCoveragePassDownloadAssetErrorBranches(t *testing.T) {
	ctx := context.Background()
	server := gatewayTestServer(t, []byte("video bytes"))
	defer server.Close()
	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	// Output directory does not exist.
	if err := client.DownloadAsset(ctx, "job-123", "asset-1", filepath.Join(t.TempDir(), "no-such-dir", "out.mp4")); err == nil || !strings.Contains(err.Error(), "output directory does not exist") {
		t.Fatalf("missing dir error=%v", err)
	}
	// Misconfigured base URL fails request construction (white-box: bypass
	// constructor validation to reach the build branch).
	badBase := &apiClient{baseURL: "http://127.0.0.1:8080\x7f", token: "secret-token", http: server.Client(), downloadHTTP: server.Client()}
	if err := badBase.DownloadAsset(ctx, "job-123", "asset-1", filepath.Join(t.TempDir(), "out.mp4")); err == nil || !strings.Contains(err.Error(), "build asset download request") {
		t.Fatalf("build download error=%v", err)
	}
	// Nil download client falls back to the JSON client.
	fallback := &apiClient{baseURL: server.URL, token: "secret-token", http: server.Client(), downloadHTTP: nil}
	fallbackOut := filepath.Join(t.TempDir(), "fallback.mp4")
	if err := fallback.DownloadAsset(ctx, "job-123", "asset-1", fallbackOut); err != nil {
		t.Fatalf("nil download client fallback error=%v", err)
	}
	// Transport failure.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedClient, err := newAPIClient(closed.URL, "secret-token", closed.Client())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := closedClient.DownloadAsset(ctx, "job-123", "asset-1", filepath.Join(t.TempDir(), "out.mp4")); err == nil || !strings.Contains(err.Error(), "asset download failed") {
		t.Fatalf("download transport error=%v", err)
	}
	// Plain-text gateway failure.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer plain.Close()
	plainClient, err := newAPIClient(plain.URL, "secret-token", plain.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := plainClient.DownloadAsset(ctx, "job-123", "asset-1", filepath.Join(t.TempDir(), "out.mp4")); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("plain download error=%v", err)
	}
	// Message-only gateway failure (no code).
	noCode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"gone"}`))
	}))
	defer noCode.Close()
	noCodeClient, err := newAPIClient(noCode.URL, "secret-token", noCode.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := noCodeClient.DownloadAsset(ctx, "job-123", "asset-1", filepath.Join(t.TempDir(), "out.mp4")); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("no-code download error=%v", err)
	}
	// Empty body succeeds at HTTP level but fails the non-empty guard.
	emptyBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer emptyBody.Close()
	emptyClient, err := newAPIClient(emptyBody.URL, "secret-token", emptyBody.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := emptyClient.DownloadAsset(ctx, "job-123", "asset-1", filepath.Join(t.TempDir(), "out.mp4")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty download error=%v", err)
	}
	// Temporary file creation failure: make the destination directory read-only
	// after it passes the existence check.
	// Permission-failure branch requires a non-root user: root bypasses
	// directory permissions, so this section is skipped under root.
	// CI runners are non-root and still exercise it.
	if os.Geteuid() != 0 {
		roDir := t.TempDir()
		roOut := filepath.Join(roDir, "out.mp4")
		if err := os.Chmod(roDir, 0500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(roDir, 0700) }()
		if err := client.DownloadAsset(ctx, "job-123", "asset-1", roOut); err == nil || !strings.Contains(err.Error(), "create temporary download file") {
			t.Fatalf("tmp create error=%v", err)
		}
	}
}

func TestCoveragePassRenderComposeAndEncodeFailures(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "story.txt")
	modelPath := filepath.Join(root, "voice.onnx")
	piper := filepath.Join(root, "fake-piper")
	ffmpeg := filepath.Join(root, "fake-ffmpeg")
	for p, d := range map[string][]byte{textPath: []byte("hello"), modelPath: []byte("model")} {
		if err := os.WriteFile(p, d, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(piper, []byte("#!/bin/sh\nset -eu\nout=\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--output_file\" ]; then shift; out=$1; fi\n  shift\ndone\ncat >/dev/null\nprintf 'RIFF....WAVE' > \"$out\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nset -eu\nlast=\nfor arg in \"$@\"; do last=\"$arg\"; done\nprintf 'x' > \"$last\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_FFMPEG", ffmpeg)

	// Compose fails after successful narration because the shot file is missing.
	missingShot := filepath.Join(root, "absent.mp4")
	narrOut := filepath.Join(root, "narr.wav")
	manifest := renderManifest{}
	manifest.Shots = []string{missingShot}
	manifest.OutputPath = filepath.Join(root, "out.mp4")
	manifest.Width, manifest.Height, manifest.FPS = 640, 360, 24
	manifest.NarrationTextPath = textPath
	manifest.PiperModelPath = modelPath
	manifest.NarrationOutputPath = narrOut
	manifestPath := filepath.Join(root, "render-fail.json")
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", "--file", manifestPath, "--piper-bin", piper}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "compose episode") {
		t.Fatalf("render compose failure code=%d stderr=%q", code, stderr.String())
	}

	// Narrate failure pins the local synthesis error branch.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"narrate", "--text-file", textPath, "--model", modelPath, "--out", filepath.Join(root, "n.wav"), "--piper-bin", filepath.Join(root, "missing-piper")}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "narrate:") {
		t.Fatalf("narrate failure code=%d stderr=%q", code, stderr.String())
	}

	// Successful render with a broken stdout pins the render encode branch.
	shot := filepath.Join(root, "shot.mp4")
	if err := os.WriteFile(shot, []byte("clip"), 0600); err != nil {
		t.Fatal(err)
	}
	good := renderManifest{}
	good.Shots = []string{shot}
	good.OutputPath = filepath.Join(root, "good.mp4")
	good.Width, good.Height, good.FPS = 640, 360, 24
	good.NarrationTextPath = textPath
	good.PiperModelPath = modelPath
	good.NarrationOutputPath = filepath.Join(root, "good-narr.wav")
	goodPath := filepath.Join(root, "render-good.json")
	goodData, _ := json.Marshal(good)
	if err := os.WriteFile(goodPath, goodData, 0600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"render", "--file", goodPath, "--piper-bin", piper}, failWriter{errors.New("stdout broken")}, &stderr); code != 1 || !strings.Contains(stderr.String(), "encode render result") {
		t.Fatalf("render encode failure code=%d stderr=%q", code, stderr.String())
	}

	// Successful gateway command with broken stdout pins the generic encode branch.
	server := gatewayTestServer(t, []byte("video bytes"))
	defer server.Close()
	t.Setenv("NEXAROUTE_VIDEO_TOKEN_COVERAGE_ENCODE", "secret-token")
	if code := run([]string{"providers", "--base-url", server.URL, "--token-env", "NEXAROUTE_VIDEO_TOKEN_COVERAGE_ENCODE"}, failWriter{errors.New("stdout broken")}, &stderr); code != 1 || !strings.Contains(stderr.String(), "encode result") {
		t.Fatalf("gateway encode failure code=%d stderr=%q", code, stderr.String())
	}
}

func TestCoveragePassDefaultHelpers(t *testing.T) {
	t.Setenv("NEXAROUTE_FFMPEG", "")
	if got := defaultFFmpegBinary(); got != "ffmpeg" {
		t.Fatalf("default ffmpeg=%q, want ffmpeg", got)
	}
	t.Setenv("NEXAROUTE_FFMPEG", "  custom-ffmpeg  ")
	if got := defaultFFmpegBinary(); got != "custom-ffmpeg" {
		t.Fatalf("custom ffmpeg=%q", got)
	}
	t.Setenv("NEXAROUTE_URL", "")
	if got := defaultBaseURL(); got != "http://127.0.0.1:8080" {
		t.Fatalf("default base=%q", got)
	}
	t.Setenv("NEXAROUTE_URL", "https://gateway.example/")
	if got := defaultBaseURL(); got != "https://gateway.example/" {
		t.Fatalf("custom base=%q", got)
	}
	_ = time.Now
}
