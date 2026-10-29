package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

func TestMission95RunJobSaveBranches2(t *testing.T) {
	ctx := context.Background()
	t.Run("queued inconsistent save failure", func(t *testing.T) {
		p := providerWithDefaults()
		p.submit = video.ProviderJob{ProviderJobID: "remote-9", Provider: "fake", Model: "model-a", Accepted: false}
		// Admission #1, post-submit #2, inconsistent mark-needs-manual #3.
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 3}
		job := video.NewJob("rj-incsave", requestWithDefaults())
		job.Provider = "fake"
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, p)
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("inconsistent save error=%v", err)
		}
	})
	t.Run("admitted no id save failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := video.NewJob("rj-admsave", requestWithDefaults())
		job.State = video.StateAdmitted
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, providerWithDefaults())
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("admitted save error=%v", err)
		}
	})
}

func TestMission95ForceFailCancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := queue.NewMemoryStore()
	job := video.NewJob("ff-cx", requestWithDefaults())
	job.State = video.StateSubmitted
	mission95Stored(t, store, job)
	o := &Orchestrator{Store: store}
	if _, _, err := o.ForceFail(ctx, "ff-cx"); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("force-fail cancelled ctx error=%v", err)
	}
}

func TestMission95CancelAdmittedTransitionError(t *testing.T) {
	ctx := context.Background()
	store := queue.NewMemoryStore()
	job := video.NewJob("cx-adm", requestWithDefaults())
	job.State = video.StateAdmitted
	mission95Stored(t, store, job)
	o := &Orchestrator{Store: store}
	if err := o.Cancel(ctx, "cx-adm"); err == nil {
		t.Fatal("cancel admitted transition error accepted")
	}
}

func TestMission95ScheduleRetryCancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := queue.New(10)
	o := &Orchestrator{Queue: q}
	job := video.NewJob("sr-cx", requestWithDefaults())
	// The retry delay is one hour, so the timer cannot fire during the
	// test; cancelling right after scheduling must take the ctx.Done
	// branch and never enqueue.
	o.scheduleRetry(ctx, job, time.Hour)
	cancel()
	time.Sleep(100 * time.Millisecond)
	if q.Len() != 0 {
		t.Fatalf("cancelled retry queued=%d, want 0", q.Len())
	}
}
