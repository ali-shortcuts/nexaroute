package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// newRetentionJob builds a code-render job record directly so retention
// behavior can be asserted deterministically, without FFmpeg or a
// planning model.
func newRetentionJob(state video.JobState, finished time.Time, dir string) *codeRenderJob {
	return &codeRenderJob{
		done:     make(chan struct{}),
		state:    state,
		finished: finished,
		workDir:  dir,
		usage:    map[string]string{},
	}
}

func mustWorkDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCodeRenderPruneTerminalReclaimsExpiredWorkDirs is the retention
// contract proof: terminal jobs older than the window are removed with
// their work directories, a fresh terminal job is kept so its output
// can still be downloaded, and an old non-terminal job is never touched
// because a render may still be writing into its directory.
func TestCodeRenderPruneTerminalReclaimsExpiredWorkDirs(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	oldDone := mustWorkDir(t, root, "old-completed")
	oldFailed := mustWorkDir(t, root, "old-failed")
	oldCancelled := mustWorkDir(t, root, "old-cancelled")
	freshDone := mustWorkDir(t, root, "fresh-completed")
	oldActive := mustWorkDir(t, root, "old-processing")

	p := &CodeRender{
		WorkRoot:  root,
		Retention: time.Hour,
		jobs: map[string]*codeRenderJob{
			"old-completed": newRetentionJob(video.StateCompleted, now.Add(-3*time.Hour), oldDone),
			"old-failed":    newRetentionJob(video.StateFailed, now.Add(-3*time.Hour), oldFailed),
			"old-cancelled": newRetentionJob(video.StateCancelled, now.Add(-3*time.Hour), oldCancelled),
			"fresh-done":    newRetentionJob(video.StateCompleted, now.Add(-time.Minute), freshDone),
			"old-active":    newRetentionJob(video.StateProcessing, time.Time{}, oldActive),
		},
	}

	if n := p.PruneTerminal(now); n != 3 {
		t.Fatalf("pruned = %d, want 3", n)
	}
	for _, id := range []string{"old-completed", "old-failed", "old-cancelled"} {
		if _, ok := p.jobs[id]; ok {
			t.Errorf("expired terminal job %q survived pruning", id)
		}
	}
	for _, id := range []string{"fresh-done", "old-active"} {
		if _, ok := p.jobs[id]; !ok {
			t.Errorf("job %q must be retained", id)
		}
	}
	for _, dir := range []string{oldDone, oldFailed, oldCancelled} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("expired work directory %q was not removed (err=%v)", dir, err)
		}
	}
	for _, dir := range []string{freshDone, oldActive} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("retained work directory %q was removed: %v", dir, err)
		}
	}
}

// TestCodeRenderPruneTerminalIdempotent proves a second sweep is a
// no-op: retention must never delete a job twice or error on absence.
func TestCodeRenderPruneTerminalIdempotent(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	dir := mustWorkDir(t, root, "expired")
	p := &CodeRender{
		WorkRoot:  root,
		Retention: time.Hour,
		jobs: map[string]*codeRenderJob{
			"expired": newRetentionJob(video.StateCompleted, now.Add(-2*time.Hour), dir),
		},
	}
	if n := p.PruneTerminal(now); n != 1 {
		t.Fatalf("first prune = %d, want 1", n)
	}
	if n := p.PruneTerminal(now); n != 0 {
		t.Fatalf("second prune = %d, want 0", n)
	}
}

// TestCodeRenderPruneTerminalDisabledKeepsEverything proves a
// non-positive retention is an explicit "keep forever", matching the
// historical behavior and the job-store retention contract.
func TestCodeRenderPruneTerminalDisabledKeepsEverything(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	dir := mustWorkDir(t, root, "kept")
	p := &CodeRender{
		WorkRoot:  root,
		Retention: 0,
		jobs: map[string]*codeRenderJob{
			"kept": newRetentionJob(video.StateCompleted, now.Add(-1000*time.Hour), dir),
		},
	}
	if n := p.PruneTerminal(now); n != 0 {
		t.Fatalf("pruned = %d, want 0 when retention is disabled", n)
	}
	if _, ok := p.jobs["kept"]; !ok {
		t.Fatal("job pruned despite disabled retention")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("work directory removed despite disabled retention: %v", err)
	}
}

// TestNewCodeRenderDefaultsRetention proves the constructor installs a
// bounded default so the historical unbounded work-directory leak is
// closed unless an operator opts out.
func TestNewCodeRenderDefaultsRetention(t *testing.T) {
	p := NewCodeRender(nil, nil, nil, t.TempDir(), []string{"m"}, 120)
	if p.Retention != DefaultCodeRenderRetention || p.Retention <= 0 {
		t.Fatalf("default retention = %v, want %v", p.Retention, DefaultCodeRenderRetention)
	}
}

// TestCodeRenderFailedJobPublishesNoAsset is the QC failure-path proof:
// a job that failed its deterministic quality gate has no validated
// output, so DownloadOutputs refuses and the asset sink stays empty.
func TestCodeRenderFailedJobPublishesNoAsset(t *testing.T) {
	root := t.TempDir()
	sink, err := storage.NewLocal(root, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	p := &CodeRender{
		jobs: map[string]*codeRenderJob{
			"failed": {
				done:  make(chan struct{}),
				state: video.StateFailed,
				err:   "QC: scene \"a\" frame at 1.00s is blank (luminance variance 0.000)",
				usage: map[string]string{},
			},
		},
	}
	_, err = p.DownloadOutputs(context.Background(), video.ProviderJobStatus{
		ProviderJobID: "failed", State: video.StateFailed,
		Outputs: []video.Asset{{ID: "failed-output", Kind: "video", Filename: "episode.mp4", ContentType: "video/mp4"}},
	}, sink)
	if err == nil {
		t.Fatal("a failed job must not publish an asset")
	}
	entries, walkErr := os.ReadDir(root)
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed job published %d entries into the asset sink", len(entries))
	}
}
