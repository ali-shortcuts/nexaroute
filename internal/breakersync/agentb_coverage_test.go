package breakersync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/sharedstate"
)

// Agent B coverage: LocalMarkers must skip snapshot rows without a
// deployment ID (a health manager can report an anonymous row), and Run
// must keep syncing on its ticker until the context is cancelled —
// otherwise a replica would publish once at startup and then silently
// stop converging with its peers.

// LocalMarkers skips empty deployments and keeps only publishable markers.
func TestAgentBLocalMarkersSkipsEmptyDeployment(t *testing.T) {
	now := time.Now().UTC()
	states := []health.State{
		{Deployment: "", Status: health.Cooldown, CooldownUntil: now.Add(time.Minute)},
		{Deployment: "dep-1", Status: health.Cooldown, CooldownUntil: now.Add(time.Minute), LastErrorClass: "5xx"},
		{Deployment: "dep-2", Status: health.Retired, LastErrorClass: "auth"},
		{Deployment: "dep-3", Status: health.Cooldown, CooldownUntil: now.Add(-time.Minute)},
		{Deployment: "dep-4", Status: health.Healthy},
	}
	got := LocalMarkers(states, now)
	if _, ok := got[""]; ok {
		t.Fatalf("LocalMarkers published an empty deployment: %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("LocalMarkers = %v, want dep-1 (cooldown) and dep-2 (retired) only", got)
	}
	if got["dep-1"].Status != sharedstate.StatusCooldown || got["dep-1"].ReasonClass != "5xx" {
		t.Fatalf("dep-1 marker = %+v, want cooldown/5xx", got["dep-1"])
	}
	if got["dep-2"].Status != sharedstate.StatusRetired {
		t.Fatalf("dep-2 marker = %+v, want retired", got["dep-2"])
	}
}

type agentbCountingStore struct {
	mu        sync.Mutex
	loads     int
	publishes int
}

func (s *agentbCountingStore) Load(context.Context) (sharedstate.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return sharedstate.View{Entries: map[string]sharedstate.Entry{}}, nil
}

func (s *agentbCountingStore) Publish(context.Context, map[string]sharedstate.Entry) (sharedstate.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishes++
	return sharedstate.View{Entries: map[string]sharedstate.Entry{}}, nil
}

func (s *agentbCountingStore) rounds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// Run must sync on every tick, not just once at startup.
func TestAgentBRunTicksOnInterval(t *testing.T) {
	store := &agentbCountingStore{}
	manager := health.New(3, time.Minute)
	syncer, err := New(store, manager, Options{ReplicaID: "tick-test", Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncer.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.rounds() >= 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if got := store.rounds(); got < 3 {
		t.Fatalf("Run performed %d sync rounds, want at least 3 (startup + 2 ticks)", got)
	}
}
