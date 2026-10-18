package queue

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// #434 item 6: TryClaim is the atomic lease primitive behind
// multi-instance-safe video execution. The matrix: unclaimed
// → claimed; live foreign lease → refused; same owner →
// renewed; expired lease → claimable by anyone; missing job
// → ErrNotFound. Exactly one of many concurrent claimants
// wins.
func TestTryClaimMatrix(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st := NewMemoryStore()
	job := video.VideoJob{JobID: "j1", State: video.StateQueued}
	if err := st.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := st.TryClaim(ctx, "j1", "owner-a", 5*time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if claimed.LeaseOwner != "owner-a" || !claimed.LeaseUntil.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("claimed record = %+v", claimed)
	}
	if _, ok, err := st.TryClaim(ctx, "j1", "owner-b", 5*time.Minute, now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("live foreign lease must refuse: ok=%v err=%v", ok, err)
	}
	renewed, ok, err := st.TryClaim(ctx, "j1", "owner-a", 5*time.Minute, now.Add(2*time.Minute))
	if err != nil || !ok || !renewed.LeaseUntil.Equal(now.Add(7*time.Minute)) {
		t.Fatalf("same-owner renewal: ok=%v until=%v err=%v", ok, renewed.LeaseUntil, err)
	}
	if _, ok, err := st.TryClaim(ctx, "j1", "owner-b", 5*time.Minute, now.Add(8*time.Minute)); err != nil || !ok {
		t.Fatalf("expired lease must be claimable: ok=%v err=%v", ok, err)
	}
	if _, _, err := st.TryClaim(ctx, "missing", "owner-a", time.Minute, now); err != ErrNotFound {
		t.Fatalf("missing job err = %v, want ErrNotFound", err)
	}
}

func TestTryClaimSingleWinnerUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := NewMemoryStore()
	if err := st.Create(ctx, video.VideoJob{JobID: "race", State: video.StateQueued}); err != nil {
		t.Fatal(err)
	}
	const claimants = 16
	var wg sync.WaitGroup
	wins := make(chan string, claimants)
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := string(rune('a' + i))
			if _, ok, err := st.TryClaim(ctx, "race", "owner-"+owner, time.Minute, now); err == nil && ok {
				wins <- owner
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	count := 0
	for range wins {
		count++
	}
	if count != 1 {
		t.Fatalf("concurrent claims produced %d winners, want exactly 1", count)
	}
}

// The FileStore persists the lease: a store reopened on the
// same path (a restarted process) sees the other owner's live
// lease and refuses to claim.
func TestFileStoreClaimPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	path := filepath.Join(t.TempDir(), "jobs.json")
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, video.VideoJob{JobID: "jf", State: video.StateQueued}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.TryClaim(ctx, "jf", "owner-a", 5*time.Minute, now); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(ctx, "jf")
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseOwner != "owner-a" || got.LeaseUntil.IsZero() {
		t.Fatalf("lease did not persist: %+v", got)
	}
	if _, ok, err := reopened.TryClaim(ctx, "jf", "owner-b", 5*time.Minute, now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("reopened store must honor the live lease: ok=%v err=%v", ok, err)
	}
}
