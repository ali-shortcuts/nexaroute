package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// Regression test for a real race in pollAndFinalize: with a nanosecond
// poll interval, an already-cancelled context and the timer are BOTH
// ready when the loop's select runs, and Go's select picks randomly
// among ready cases. The loop could therefore burn through all MaxPolls
// iterations on the timer branch and report "polling deadline reached"
// instead of context.Canceled. That is exactly the flaky failure seen
// in the coverage-stack verify run
// (TestMission95RunJobSingleShotBranches/poll_cancelled_context).
// A cancelled context must always surface as context.Canceled, on
// every run, not on most runs.
func TestPollAndFinalizeCancelledContextIsCanceled(t *testing.T) {
	store := queue.NewMemoryStore()
	job := video.VideoJob{JobID: "poll-cx", State: video.StateSubmitted, Provider: "fake", Model: "model-a", ProviderJobID: "remote-1"}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatalf("store job: %v", err)
	}
	o := &Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers:    Registry{"fake": providerWithDefaults()},
		PollInterval: time.Nanosecond, MaxPolls: 5,
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.pollAndFinalize(cancelled, providerWithDefaults(), job); !errors.Is(err, context.Canceled) {
		t.Fatalf("poll cancelled error=%v, want context.Canceled", err)
	}
}
