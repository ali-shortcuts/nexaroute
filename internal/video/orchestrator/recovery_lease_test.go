package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// A job carrying a live lease from a previous process incarnation is
// re-enqueued by Recover. The first execution attempt loses the claim, so
// recovery must arrange a later retry: otherwise the only queued copy is
// consumed and the job stays stuck forever once that lease expires.
func TestRecoverRetriesJobAfterForeignLeaseExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote-recovered", Provider: "fake", Model: "model-a", Accepted: true}
	p.statuses = []video.ProviderJobStatus{{
		ProviderJobID: "remote-recovered",
		State:         video.StateCompleted,
		Progress:      1,
		Outputs:       []video.Asset{{ID: "out", Filename: "out.mp4"}},
	}}
	p.downloaded = []video.Asset{validStoredAsset()}

	st, q := queue.NewMemoryStore(), queue.New(4)
	req := requestWithDefaults()
	req.ModelPreference = "model-a"
	job := video.NewJob("recover-leased", req)
	job.Provider, job.Model = "fake", "model-a"
	leaseUntil := time.Now().Add(time.Second)
	job.LeaseOwner, job.LeaseUntil = "previous-process", leaseUntil
	if err := st.Create(ctx, job); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{
		Store: st, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: &cost.Ledger{},
		PollInterval: time.Nanosecond, MaxPolls: 3, InstanceID: "restarted-process",
	}
	if err := o.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 1 {
		t.Fatalf("recovery queue length = %d, want 1", q.Len())
	}

	// Consume the only recovered queue entry while the foreign lease is live.
	first, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunJob(ctx, first); err != nil {
		t.Fatal(err)
	}
	if p.createCalls != 0 {
		t.Fatalf("provider submitted %d times while the foreign lease was live", p.createCalls)
	}

	// The lease expires after this point; recovery must retry the job.
	deadline := leaseUntil.Add(3 * time.Second)
	for q.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if q.Len() == 0 {
		t.Fatal("job leased at restart was never retried after the lease expired")
	}

	retried, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunJob(ctx, retried); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted {
		t.Fatalf("job state after retry = %s, want completed (last error %q)", got.State, got.LastError)
	}
	if p.createCalls != 1 {
		t.Fatalf("provider submissions = %d, want exactly 1", p.createCalls)
	}
}
