package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// Two outputs that differ only by letter case sanitize to distinct
// stored names on Linux but collide on case-insensitive filesystems
// (default macOS APFS, Windows), where the sink's rename would
// silently overwrite the first asset while its record still claims
// its own SHA-256 — the same self-inconsistency the exact-match
// duplicate check was built to prevent. The check must therefore
// fold case: a case-variant pair is refused up front, before any
// /view request, on every platform. Fixture only: no real ComfyUI
// server is involved.
func TestComfyUIDownloadRejectsCaseInsensitiveDuplicateOutputFilenames(t *testing.T) {
	var viewCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/view" {
			http.NotFound(w, r)
			return
		}
		viewCalls.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("fixture-video-bytes"))
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status := video.ProviderJobStatus{Outputs: []video.Asset{
		{ID: "p-10-Clip", Filename: "Clip.mp4", Metadata: map[string]string{"filename": "Clip.mp4", "type": "output", "project_id": "p", "job_id": "job-651"}},
		{ID: "p-11-clip", Filename: "clip.mp4", Metadata: map[string]string{"filename": "clip.mp4", "type": "output", "project_id": "p", "job_id": "job-651"}},
	}}
	root := t.TempDir()
	sink, err := storage.NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownloadOutputs(context.Background(), status, sink); err == nil || !strings.Contains(err.Error(), "duplicate video output filename") {
		t.Fatalf("DownloadOutputs error=%v, want duplicate rejection", err)
	}
	if viewCalls.Load() != 0 {
		t.Fatalf("/view calls=%d, want 0 (refusal must precede any download)", viewCalls.Load())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("store root holds %d entries after a refused download, want none", len(entries))
	}
}

// CancelJob for a prompt that is neither running nor pending must
// refuse without issuing a delete: there is no remote entry to
// cancel, and a delete for an unknown id could remove unrelated
// queue state. The harness cleanup path depends on this refusal
// (it records BLOCKED, never a global interrupt). Fixture only.
func TestComfyUICancelAbsentPromptRefusesWithoutDelete(t *testing.T) {
	var deleteCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/queue" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		case r.URL.Path == "/queue" && r.Method == http.MethodPost:
			deleteCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), server.URL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.CancelJob(context.Background(), "no-such-prompt"); err == nil || !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("CancelJob error=%v, want not-pending refusal", err)
	}
	if deleteCalls.Load() != 0 {
		t.Fatalf("delete calls=%d, want 0 (no remote entry exists to delete)", deleteCalls.Load())
	}
}
