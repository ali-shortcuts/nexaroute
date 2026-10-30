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

// The same filename under two different ComfyUI subfolders still maps to
// one stored basename: the sink keys assets by name inside the job
// directory, so accepting both would overwrite one set of bytes. The
// whole download must be refused before any /view request.
func TestComfyUIDownloadRejectsSameFilenameAcrossSubfolders(t *testing.T) {
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
		{ID: "p-30-a", Filename: "same.mp4", Metadata: map[string]string{"filename": "same.mp4", "subfolder": "a", "type": "output"}},
		{ID: "p-31-b", Filename: "same.mp4", Metadata: map[string]string{"filename": "same.mp4", "subfolder": "b", "type": "output"}},
	}}
	if _, err := provider.DownloadOutputs(context.Background(), status, &comfyTestSink{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("DownloadOutputs error=%v, want duplicate rejection", err)
	}
	if viewCalls.Load() != 0 {
		t.Fatalf("view calls=%d, want 0 (rejection must precede any download)", viewCalls.Load())
	}
}

// A later asset that fails while being persisted after its bytes were
// renamed into place must not survive the rollback. The first asset is
// deleted by DownloadOutputs and the failing asset is removed by the
// sink itself, so the failed job's directory holds no orphaned file.
func TestComfyUIDownloadRollsBackWhenLaterPersistenceFails(t *testing.T) {
	root := t.TempDir()
	sink, err := storage.NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(root, "p", "job-651")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Make the second asset's metadata sidecar path a directory so the
	// sink's atomic metadata rename fails after the video bytes land.
	if err := os.Mkdir(filepath.Join(jobDir, "second.mp4.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/view" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("bytes-for-" + r.URL.Query().Get("filename")))
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status := video.ProviderJobStatus{Outputs: []video.Asset{
		{ID: "p-40-first", Filename: "first.mp4", Metadata: map[string]string{"filename": "first.mp4", "type": "output", "project_id": "p", "job_id": "job-651"}},
		{ID: "p-41-second", Filename: "second.mp4", Metadata: map[string]string{"filename": "second.mp4", "type": "output", "project_id": "p", "job_id": "job-651"}},
	}}
	assets, err := provider.DownloadOutputs(context.Background(), status, sink)
	if err == nil {
		t.Fatal("DownloadOutputs unexpectedly succeeded although persistence failed")
	}
	if assets != nil {
		t.Fatalf("DownloadOutputs returned %d assets alongside an error, want none", len(assets))
	}
	var files []string
	if err := filepath.Walk(jobDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("failed persistence left files behind: %v", files)
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

// A multi-output download that fails on a later asset must not leave
// the earlier asset behind as an orphaned partial set, and must not
// touch an unrelated job's asset in the same store. The caller gets an
// error and nothing it can mistake for a completed job; the other
// job's bytes and checksum record stay byte-identical. Both failure
// shapes are pinned: a later asset over the size limit, and a later
// asset whose /view request fails.
func TestComfyUIDownloadPartialFailureCleansUpAndLeavesOtherJobIntact(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"oversizedLaterAsset": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(make([]byte, 64))
		},
		"failedLaterAsset": func(w http.ResponseWriter) {
			http.Error(w, "upstream gone", http.StatusBadGateway)
		},
	}
	for name, later := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			sink, err := storage.NewLocal(root, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			// An unrelated, already-completed job's asset in the
			// same store: it must survive this job's failed
			// download byte-for-byte, metadata included.
			other, err := sink.Put(context.Background(), video.Asset{
				ID: "other-1", Kind: "video", Filename: "other.mp4", ContentType: "video/mp4",
				Metadata: map[string]string{"project_id": "p", "job_id": "other-job"},
			}, []byte("other-job-bytes"))
			if err != nil {
				t.Fatal(err)
			}
			otherBefore, err := os.ReadFile(other.URI)
			if err != nil {
				t.Fatal(err)
			}
			otherMetaBefore, err := os.ReadFile(other.URI + ".json")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/view" {
					http.NotFound(w, r)
					return
				}
				switch r.URL.Query().Get("filename") {
				case "first.mp4":
					w.Header().Set("Content-Type", "video/mp4")
					_, _ = w.Write([]byte("first-bytes"))
				case "second.mp4":
					later(w)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			// 16-byte cap: the first asset fits, the 64-byte
			// oversized asset does not; the HTTP-500 asset fails
			// on status regardless of size.
			provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 16)
			if err != nil {
				t.Fatal(err)
			}
			status := video.ProviderJobStatus{Outputs: []video.Asset{
				{ID: "p-10-first", Filename: "first.mp4", Metadata: map[string]string{"filename": "first.mp4", "type": "output", "project_id": "p", "job_id": "job-651"}},
				{ID: "p-11-second", Filename: "second.mp4", Metadata: map[string]string{"filename": "second.mp4", "type": "output", "project_id": "p", "job_id": "job-651"}},
			}}
			assets, err := provider.DownloadOutputs(context.Background(), status, sink)
			if err == nil {
				t.Fatal("DownloadOutputs unexpectedly succeeded with a failing later asset")
			}
			if assets != nil {
				t.Fatalf("DownloadOutputs returned %d assets alongside an error, want none", len(assets))
			}
			// No file of the failed job may remain anywhere under
			// its store directory.
			var leftovers []string
			_ = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !info.IsDir() && strings.Contains(path, "job-651") {
					leftovers = append(leftovers, path)
				}
				return nil
			})
			if len(leftovers) != 0 {
				t.Fatalf("failed download left files behind: %v", leftovers)
			}
			otherAfter, err := os.ReadFile(other.URI)
			if err != nil {
				t.Fatal(err)
			}
			if string(otherAfter) != string(otherBefore) {
				t.Fatalf("other job asset changed: before %q after %q", otherBefore, otherAfter)
			}
			otherMetaAfter, err := os.ReadFile(other.URI + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if string(otherMetaAfter) != string(otherMetaBefore) {
				t.Fatal("other job asset metadata changed during a failed download")
			}
			if read, err := sink.Read(other); err != nil || string(read) != "other-job-bytes" {
				t.Fatalf("other job asset no longer reads back intact: read=%q err=%v", read, err)
			}
		})
	}
}
