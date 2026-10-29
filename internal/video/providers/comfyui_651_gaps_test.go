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
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

func comfy651Request() video.VideoRequest {
	seed := int64(42)
	return video.VideoRequest{
		ProjectID: "p", Prompt: "a single red cube rotating", Mode: video.ModeTextToVideo,
		ModelPreference: "local-video", DurationSeconds: 2, FPS: 12,
		AspectRatio: video.Aspect9x16, Seed: &seed,
	}
}

// Server-supplied output filenames are untrusted input: a history entry
// may name "../escape.mp4", an absolute path, or a backslash traversal.
// DownloadOutputs must still land every byte inside the asset-store root
// (the sink basenames + sanitizes), and must never create a file outside
// it. The /view request itself still carries the server-supplied name:
// containment is a local-storage property, not a rewrite of what the
// server was asked for.
func TestComfyUIDownloadKeepsTraversalFilenamesInsideStoreRoot(t *testing.T) {
	root := t.TempDir()
	sink, err := storage.NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var viewQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/view" {
			http.NotFound(w, r)
			return
		}
		viewQueries = append(viewQueries, r.URL.Query().Get("filename"))
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("fixture-video-bytes"))
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status := video.ProviderJobStatus{Outputs: []video.Asset{
		{ID: "p-10-escape", Filename: "../escape.mp4", Metadata: map[string]string{"filename": "../escape.mp4", "subfolder": "", "type": "output"}},
		{ID: "p-11-abs", Filename: "abs.mp4", Metadata: map[string]string{"filename": "/tmp/abs.mp4", "subfolder": "", "type": "output"}},
		{ID: "p-12-win", Filename: `..\..\win.mp4`, Metadata: map[string]string{"filename": `..\..\win.mp4`, "subfolder": "", "type": "output"}},
	}}
	// DownloadOutputs needs project/job metadata for the sink layout.
	for i := range status.Outputs {
		if status.Outputs[i].Metadata == nil {
			status.Outputs[i].Metadata = map[string]string{}
		}
		status.Outputs[i].Metadata["project_id"] = "p"
		status.Outputs[i].Metadata["job_id"] = "job-651"
	}
	assets, err := provider.DownloadOutputs(context.Background(), status, sink)
	if err != nil {
		t.Fatalf("DownloadOutputs error=%v", err)
	}
	if len(assets) != 3 || len(viewQueries) != 3 {
		t.Fatalf("assets=%d viewQueries=%v", len(assets), viewQueries)
	}
	for _, asset := range assets {
		rel, err := filepath.Rel(root, asset.URI)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("asset URI %q escapes store root %q", asset.URI, root)
		}
		if strings.Contains(asset.Filename, "/") || strings.Contains(asset.Filename, "\\") {
			t.Fatalf("stored filename %q still contains a separator", asset.Filename)
		}
	}
	// Nothing may have been created beside the store root.
	parent := filepath.Dir(root)
	for _, escaped := range []string{"escape.mp4", "abs.mp4", "win.mp4"} {
		if _, err := os.Stat(filepath.Join(parent, escaped)); err == nil {
			t.Fatalf("traversal file %q was created outside the store root", escaped)
		}
	}
}

// Two outputs that sanitize to the same stored filename must not
// overwrite each other: the whole download fails closed before any
// /view request, so no partial, self-inconsistent set is persisted.
func TestComfyUIDownloadRejectsDuplicateOutputFilenames(t *testing.T) {
	var viewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewCalls.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("x"))
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status := video.ProviderJobStatus{Outputs: []video.Asset{
		{ID: "p-10-same", Filename: "same.mp4", Metadata: map[string]string{"filename": "same.mp4", "type": "output"}},
		{ID: "p-11-same", Filename: "same.mp4", Metadata: map[string]string{"filename": "same.mp4", "type": "output"}},
	}}
	if _, err := provider.DownloadOutputs(context.Background(), status, &comfyTestSink{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("DownloadOutputs error=%v, want duplicate rejection", err)
	}
	if viewCalls.Load() != 0 {
		t.Fatalf("view calls=%d, want 0 (rejection must precede any download)", viewCalls.Load())
	}
}

// The duplicate check must compare stored names, not raw filenames:
// the sink sanitizes basenames (unsafe runs become "_") and renames
// silently over an existing file, so distinct supplied names can
// collide after sanitization. A ".." name likewise maps to asset.bin.
func TestComfyUIDownloadRejectsSanitizedDuplicateOutputFilenames(t *testing.T) {
	cases := map[string][2]string{
		"sanitizedCollision": {"a b.mp4", "a_b.mp4"},
		"dotNameCollision":   {"..", "asset.bin"},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			var viewCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				viewCalls.Add(1)
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write([]byte("x"))
			}))
			defer server.Close()
			provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			status := video.ProviderJobStatus{Outputs: []video.Asset{
				{ID: "p-20-a", Filename: pair[0], Metadata: map[string]string{"filename": pair[0], "type": "output"}},
				{ID: "p-21-b", Filename: pair[1], Metadata: map[string]string{"filename": pair[1], "type": "output"}},
			}}
			if _, err := provider.DownloadOutputs(context.Background(), status, &comfyTestSink{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("DownloadOutputs error=%v, want duplicate rejection", err)
			}
			if viewCalls.Load() != 0 {
				t.Fatalf("view calls=%d, want 0 (rejection must precede any download)", viewCalls.Load())
			}
		})
	}
}

// A submission whose /prompt request never completes (client context
// deadline) must result in exactly one POST: no retry, no second prompt,
// and an error — never a silent success or a duplicate submission.
func TestComfyUICreateJobSubmitTimeoutSubmitsExactlyOnce(t *testing.T) {
	var promptCalls atomic.Int32
	// The handler blocks until the test releases it. It must NOT
	// block on the request context alone: the client transport does
	// not reliably tear the server-side connection down on a client
	// context deadline, so server.Close would wait forever on the
	// blocked handler (observed deadlock, not a product defect: the
	// product call itself errors promptly on the deadline).
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prompt" {
			http.NotFound(w, r)
			return
		}
		promptCalls.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := provider.CreateJob(ctx, comfy651Request()); err == nil {
		t.Fatal("CreateJob with an expired submit context unexpectedly succeeded")
	}
	if promptCalls.Load() != 1 {
		t.Fatalf("prompt calls=%d, want exactly 1", promptCalls.Load())
	}
}

// A download whose /view response never arrives must fail closed on the
// caller's context deadline: an error, no asset, and nothing persisted.
func TestComfyUIDownloadTimeoutFailsClosed(t *testing.T) {
	root := t.TempDir()
	sink, err := storage.NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/view" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// See the submit-timeout test: the handler must be released by
	// the test, not by connection teardown, or server.Close hangs.
	defer func() { close(release); server.Close() }()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status := video.ProviderJobStatus{Outputs: []video.Asset{{
		ID: "p-10-slow", Filename: "slow.mp4",
		Metadata: map[string]string{"filename": "slow.mp4", "type": "output", "project_id": "p", "job_id": "job-651"},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := provider.DownloadOutputs(ctx, status, sink); err == nil {
		t.Fatal("DownloadOutputs with an expired download context unexpectedly succeeded")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("store root holds %d entries after a timed-out download, want none", len(entries))
	}
}
