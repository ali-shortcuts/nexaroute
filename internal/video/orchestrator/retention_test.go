package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// #420 item 5 (retention): terminal jobs older than the window
// are pruned; recent terminal jobs stay; non-terminal jobs are
// NEVER pruned however old; a non-positive retention disables
// pruning entirely. Pruned jobs also lose their idempotency
// mapping, so the same key may be reused afterwards.
func TestPruneTerminalJobs(t *testing.T) {
	ctx := context.Background()
	store := queue.NewMemoryStore()
	now := time.Now()
	old := now.Add(-90 * 24 * time.Hour)
	recent := now.Add(-time.Hour)

	mk := func(id string, state video.JobState, created time.Time, completed *time.Time) video.VideoJob {
		return video.VideoJob{JobID: id, IdempotencyKey: "idem-" + id, State: state, CreatedAt: created, CompletedAt: completed}
	}
	jobs := []video.VideoJob{
		mk("old-done", video.StateCompleted, old, &old),
		mk("old-failed", video.StateFailed, old, &old),
		mk("old-force-failed", video.StateFailed, old, nil), // no completion stamp: ages from creation; OperatorForceFailed below
		mk("recent-done", video.StateCompleted, recent, &recent),
		mk("old-active", video.StateProcessing, old, nil),
		mk("old-queued", video.StateQueued, old, nil),
	}
	jobs[2].OperatorForceFailed = true
	for _, j := range jobs {
		if err := store.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	o := &Orchestrator{Store: store}

	// Disabled retention: nothing pruned.
	if n, err := o.PruneTerminalJobs(ctx, now, 0); err != nil || n != 0 {
		t.Fatalf("disabled prune = %d, %v; want 0, nil", n, err)
	}

	n, err := o.PruneTerminalJobs(ctx, now, 30*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("pruned = %d, %v; want 2 (old-done, old-force-failed)", n, err)
	}
	for _, id := range []string{"old-done", "old-force-failed"} {
		if _, err := store.Get(ctx, id); err == nil {
			t.Fatalf("%s survived pruning", id)
		}
	}
	// An ordinary FAILED job is retryable by design (see
	// VideoJob.OperatorForceFailed), so it is not terminal and
	// must survive retention like any live job.
	for _, id := range []string{"recent-done", "old-active", "old-queued", "old-failed"} {
		if _, err := store.Get(ctx, id); err != nil {
			t.Fatalf("%s was wrongly pruned: %v", id, err)
		}
	}
	// Idempotency mapping of a pruned job is released.
	if _, err := store.GetByIdempotency(ctx, "idem-old-done"); err == nil {
		t.Fatal("idempotency mapping of a pruned job survived")
	}
	// Pruning is idempotent.
	if n, err := o.PruneTerminalJobs(ctx, now, 30*24*time.Hour); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v; want 0, nil", n, err)
	}
}
