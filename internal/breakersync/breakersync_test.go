package breakersync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/sharedstate"
)

// twoReplicas builds two health managers and two syncers over one shared
// in-memory control-plane store — the same topology as two gateway
// processes over PostgreSQL/Redis, without the network.
func twoReplicas(t *testing.T) (hmA, hmB *health.Manager, syncA, syncB *Syncer) {
	t.Helper()
	adapter, err := sharedstate.NewControlPlaneStore(controlplane.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	storeA, err := sharedstate.NewBreakerStore(adapter, sharedstate.Options{ReplicaID: "replica-a"})
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := sharedstate.NewBreakerStore(adapter, sharedstate.Options{ReplicaID: "replica-b"})
	if err != nil {
		t.Fatal(err)
	}
	hmA = health.New(3, time.Minute)
	hmB = health.New(3, time.Minute)
	syncA, err = New(storeA, hmA, Options{ReplicaID: "replica-a"})
	if err != nil {
		t.Fatal(err)
	}
	syncB, err = New(storeB, hmB, Options{ReplicaID: "replica-b"})
	if err != nil {
		t.Fatal(err)
	}
	return hmA, hmB, syncA, syncB
}

func TestSyncPropagatesCooldownAcrossReplicas(t *testing.T) {
	hmA, hmB, syncA, syncB := twoReplicas(t)
	ctx := context.Background()
	hmA.EnterCooldown("dep-1", "upstream 5xx", 5*time.Minute)
	if err := syncA.SyncOnce(ctx); err != nil {
		t.Fatalf("publish round: %v", err)
	}
	if err := syncB.SyncOnce(ctx); err != nil {
		t.Fatalf("apply round: %v", err)
	}
	s := hmB.Get("dep-1")
	if s.Status != health.Cooldown {
		t.Fatalf("replica B status = %v, want cooldown", s.Status)
	}
	if s.CooldownUntil.IsZero() || time.Until(s.CooldownUntil) > 5*time.Minute {
		t.Fatalf("replica B cooldown until = %v, want within 5m", s.CooldownUntil)
	}
	if s.Failures != 0 || s.ConsecutiveFailures != 0 {
		t.Fatalf("applied marker polluted failure accounting: %+v", s)
	}
	// Replica A's own state is untouched by the round trip.
	if got := hmA.Get("dep-1"); got.Status != health.Cooldown {
		t.Fatalf("replica A status = %v, want cooldown", got.Status)
	}
}

func TestSyncRemoteRetiredAppliesOnlyBoundedCooldown(t *testing.T) {
	hmA, hmB, syncA, syncB := twoReplicas(t)
	ctx := context.Background()
	hmA.Retire("dep-9", "credential rejected", "auth")
	if err := syncA.SyncOnce(ctx); err != nil {
		t.Fatalf("publish round: %v", err)
	}
	if err := syncB.SyncOnce(ctx); err != nil {
		t.Fatalf("apply round: %v", err)
	}
	s := hmB.Get("dep-9")
	if s.Status == health.Retired {
		t.Fatal("a remote retired marker must never retire a deployment locally")
	}
	if s.Status != health.Cooldown {
		t.Fatalf("replica B status = %v, want bounded cooldown", s.Status)
	}
	if until := time.Until(s.CooldownUntil); until <= 0 || until > sharedstate.DefaultMaxCooldown {
		t.Fatalf("bounded cooldown = %v, want within (0, %v]", until, sharedstate.DefaultMaxCooldown)
	}
}

type failingStore struct{ err error }

func (f failingStore) Load(context.Context) (sharedstate.View, error) {
	return sharedstate.View{}, f.err
}

func (f failingStore) Publish(context.Context, map[string]sharedstate.Entry) (sharedstate.View, error) {
	return sharedstate.View{}, f.err
}

func TestSyncOutageLeavesLocalStateUntouched(t *testing.T) {
	hm := health.New(3, time.Minute)
	hm.EnterCooldown("dep-1", "local incident", 10*time.Minute)
	syncer, err := New(failingStore{err: sharedstate.ErrUnavailable}, hm, Options{ReplicaID: "solo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); !errors.Is(err, sharedstate.ErrUnavailable) {
		t.Fatalf("SyncOnce error = %v, want ErrUnavailable wrapped", err)
	}
	s := hm.Get("dep-1")
	if s.Status != health.Cooldown {
		t.Fatalf("outage changed local state: %+v", s)
	}
}

func TestSyncNeverShortensLocalCooldown(t *testing.T) {
	hmA, hmB, syncA, syncB := twoReplicas(t)
	ctx := context.Background()
	hmB.EnterCooldown("dep-2", "local longer incident", 25*time.Minute)
	before := hmB.Get("dep-2").CooldownUntil
	hmA.EnterCooldown("dep-2", "peer shorter incident", 5*time.Minute)
	if err := syncA.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncB.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := hmB.Get("dep-2").CooldownUntil; !got.Equal(before) {
		t.Fatalf("local cooldown shortened: %v -> %v", before, got)
	}
}

func TestSyncExpiredPeerMarkerNotApplied(t *testing.T) {
	hmA, hmB, syncA, syncB := twoReplicas(t)
	ctx := context.Background()
	// A's marker has a one-minute deadline; by the time B syncs with a
	// clock two minutes later it must be treated as expired.
	hmA.EnterCooldown("dep-3", "short incident", time.Minute)
	if err := syncA.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Minute)
	hmB.SetNowFunc(func() time.Time { return future })
	if err := syncB.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s := hmB.Get("dep-3"); s.Status == health.Cooldown && s.CooldownUntil.After(future) {
		t.Fatalf("expired peer marker applied: %+v", s)
	}
}
