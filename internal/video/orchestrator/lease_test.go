package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// #434 item 6: two orchestrators over ONE store must never
// execute the same job twice — the second one's RunJob is a
// quiet skip while the first holds the lease, and exactly one
// provider submission happens in total. After the lease
// expires, the other instance may take over.
func TestTwoInstancesNeverExecuteSameJob(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote-42", Provider: "fake", Model: "model-a", Accepted: true}
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-42", State: video.StateCompleted, Progress: 1, Usage: map[string]string{"actual_cost_usd": "1.25"}, Outputs: []video.Asset{{ID: "out", Filename: "out.mp4"}}}}
	p.downloaded = []video.Asset{validStoredAsset()}
	st := queue.NewMemoryStore()
	mk := func(id string) *Orchestrator {
		return &Orchestrator{Store: st, Queue: queue.New(1), Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: &cost.Ledger{}, PollInterval: time.Nanosecond, MaxPolls: 3, InstanceID: id}
	}
	a, b := mk("instance-a"), mk("instance-b")

	created, err := a.Create(ctx, requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	queued, err := st.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Instance A holds a live lease: B's RunJob must skip
	// without touching the provider.
	if _, ok, err := st.TryClaim(ctx, created.JobID, "instance-a", 5*time.Minute, time.Now()); err != nil || !ok {
		t.Fatalf("seed lease: ok=%v err=%v", ok, err)
	}
	if err := b.RunJob(ctx, queued); err != nil {
		t.Fatalf("leased RunJob should skip quietly, got %v", err)
	}
	if p.createCalls != 0 {
		t.Fatalf("provider was submitted to %d times for a leased job", p.createCalls)
	}

	// The lease expires: B takes over and executes — exactly
	// one submission in total.
	stored, err := st.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	stored.LeaseUntil = time.Now().Add(-time.Minute)
	if err := st.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := b.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if p.createCalls != 1 {
		t.Fatalf("provider submissions = %d, want exactly 1", p.createCalls)
	}
	got, err := st.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted {
		t.Fatalf("final state = %s, want completed", got.State)
	}
	if got.LeaseOwner != "instance-b" {
		t.Fatalf("final lease owner = %q, want instance-b (the executor)", got.LeaseOwner)
	}
}
