package video_test

import (
	"context"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
	"os"
	"testing"
	"time"
)

func TestOrchestratorIdempotencyAndPolling(t *testing.T) {
	ctx := context.Background()
	st := queue.NewMemoryStore()
	q := queue.New(2)
	p := providers.NewFake(1)
	sink, err := storage.NewLocal(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}, Ledger: &cost.Ledger{}, Assets: sink, PollInterval: time.Millisecond, MaxPolls: 3}
	r := video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 1, Mode: video.ModeTextToVideo, ProviderPreference: "fake", IdempotencyKey: "same"}
	j, err := o.Create(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := o.Create(ctx, r)
	if err != nil || j2.JobID != j.JobID {
		t.Fatalf("idempotency: %#v %v", j2, err)
	}
	queued, _ := q.Next(ctx)
	if err = o.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	done, _ := st.Get(ctx, j.JobID)
	if done.State != video.StateCompleted {
		t.Fatalf("state=%s", done.State)
	}
}
func TestBudgetGate(t *testing.T) {
	b := cost.NewBook()
	b.Set("p", "m", cost.Price{PerSecondUSD: 2, Known: true})
	r := video.VideoRequest{DurationSeconds: 2}
	e, _ := b.Estimate("p", "m", r)
	l := &cost.Ledger{}
	if err := l.Reserve(e, 1); err != cost.ErrBudgetExceeded {
		t.Fatalf("err=%v", err)
	}
}
func TestLocalStorageRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	s, _ := storage.NewLocal(dir, 100)
	a := video.Asset{Filename: "x.bin", Metadata: map[string]string{"project_id": "p", "episode_id": "e", "job_id": "j"}}
	got, err := s.Put(context.Background(), a, []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(got.URI); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(video.Asset{URI: dir + "/../escape"}); err == nil {
		t.Fatal("expected traversal rejection")
	}
}
