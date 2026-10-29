package breakersync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/sharedstate"
)

// issue663CoreStore is an in-memory breakersync Store with scripted
// failures. It never touches a real control-plane backend.
type issue663CoreStore struct {
	view    sharedstate.View
	loadErr error
	pubErr  error
	pub     map[string]sharedstate.Entry
}

func (s *issue663CoreStore) Load(context.Context) (sharedstate.View, error) {
	if s.loadErr != nil {
		return sharedstate.View{}, s.loadErr
	}
	return s.view, nil
}

func (s *issue663CoreStore) Publish(_ context.Context, local map[string]sharedstate.Entry) (sharedstate.View, error) {
	if s.pubErr != nil {
		return sharedstate.View{}, s.pubErr
	}
	s.pub = local
	return s.view, nil
}

// issue663CoreManager records ApplySharedCooldown calls.
type issue663CoreManager struct {
	states []health.State
	calls  []issue663CoreCooldownCall
}

type issue663CoreCooldownCall struct {
	id     string
	until  time.Time
	reason string
}

func (m *issue663CoreManager) Snapshot() []health.State { return m.states }

func (m *issue663CoreManager) ApplySharedCooldown(id string, until time.Time, reason string) bool {
	m.calls = append(m.calls, issue663CoreCooldownCall{id: id, until: until, reason: reason})
	return true
}

func TestIssue663CoreNewValidationAndDefaults(t *testing.T) {
	store := &issue663CoreStore{}
	mgr := &issue663CoreManager{}
	if _, err := New(nil, mgr, Options{ReplicaID: "r1"}); err == nil {
		t.Error("nil store must be rejected")
	}
	if _, err := New(store, nil, Options{ReplicaID: "r1"}); err == nil {
		t.Error("nil manager must be rejected")
	}
	if _, err := New(store, mgr, Options{}); err == nil {
		t.Error("empty replica ID must be rejected")
	}
	// Zero MaxCooldown/Interval/Now all select their defaults without
	// error; the default max cooldown is observable through a retired
	// marker, which must demote to a bounded cooldown of exactly the
	// sharedstate default, not to a permanent retirement.
	fixed := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	mgr2 := &issue663CoreManager{}
	syncer, err := New(store, mgr2, Options{ReplicaID: "r1", Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	store.view = sharedstate.View{Entries: map[string]sharedstate.Entry{
		"dep-retired": {Status: sharedstate.StatusRetired, ReasonClass: "auth", OriginReplica: "r2"},
	}}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr2.calls) != 1 {
		t.Fatalf("retired marker must produce exactly one bounded cooldown, got %+v", mgr2.calls)
	}
	want := fixed.UTC().Add(sharedstate.DefaultMaxCooldown)
	if !mgr2.calls[0].until.Equal(want) || mgr2.calls[0].reason != "shared retired (bounded) (auth)" {
		t.Fatalf("bounded retired cooldown wrong: %+v want until=%v", mgr2.calls[0], want)
	}
}

func TestIssue663CoreApplySkipsOwnAndUnknown(t *testing.T) {
	fixed := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	mgr := &issue663CoreManager{}
	store := &issue663CoreStore{view: sharedstate.View{Entries: map[string]sharedstate.Entry{
		"dep-own":      {Status: sharedstate.StatusCooldown, CooldownUntil: fixed.Add(time.Hour), OriginReplica: "r1"},
		"dep-unknown":  {Status: sharedstate.EntryStatus("weird"), OriginReplica: "r2"},
		"dep-cool":     {Status: sharedstate.StatusCooldown, CooldownUntil: fixed.Add(time.Hour), ReasonClass: "5xx", OriginReplica: "r2"},
		"dep-noreason": {Status: sharedstate.StatusCooldown, CooldownUntil: fixed.Add(time.Hour), OriginReplica: "r2"},
	}}}
	syncer, err := New(store, mgr, Options{ReplicaID: "r1", Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.calls) != 2 {
		t.Fatalf("only foreign cooldowns must be applied, got %+v", mgr.calls)
	}
	byID := map[string]issue663CoreCooldownCall{}
	for _, c := range mgr.calls {
		byID[c.id] = c
	}
	if byID["dep-cool"].reason != "shared cooldown (5xx)" {
		t.Errorf("cooldown reason = %q", byID["dep-cool"].reason)
	}
	if byID["dep-noreason"].reason != "shared cooldown" {
		t.Errorf("reasonless cooldown reason = %q", byID["dep-noreason"].reason)
	}
}

func TestIssue663CoreSyncOnceFailures(t *testing.T) {
	mgr := &issue663CoreManager{}
	boom := errors.New("store down")
	syncer, err := New(&issue663CoreStore{loadErr: boom}, mgr, Options{ReplicaID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err == nil || !errors.Is(err, boom) {
		t.Fatalf("load failure must abort the round, got %v", err)
	}
	syncer, err = New(&issue663CoreStore{pubErr: boom}, mgr, Options{ReplicaID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncOnce(context.Background()); err == nil || !errors.Is(err, boom) {
		t.Fatalf("publish failure must be reported, got %v", err)
	}
}

func TestIssue663CoreRun(t *testing.T) {
	// A pre-cancelled context makes Run deterministic: it performs
	// exactly one immediate round, then returns without waiting for
	// the ticker. No sleep is involved.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr := &issue663CoreManager{}
	store := &issue663CoreStore{}
	var logged []string
	syncer, err := New(store, mgr, Options{ReplicaID: "r1", Interval: time.Hour, Logf: func(format string, args ...any) {
		logged = append(logged, format)
	}})
	if err != nil {
		t.Fatal(err)
	}
	syncer.Run(ctx)
	if store.pub == nil {
		t.Fatal("Run must perform one immediate publish round")
	}
	if len(logged) != 0 {
		t.Fatalf("successful round must not log, got %v", logged)
	}

	// A failing round logs (when a logger is set) and still returns on
	// cancellation instead of stopping or spinning.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	boom := errors.New("store down")
	logged = nil
	syncer2, err := New(&issue663CoreStore{loadErr: boom}, mgr, Options{ReplicaID: "r1", Interval: time.Hour, Logf: func(format string, args ...any) {
		logged = append(logged, format)
	}})
	if err != nil {
		t.Fatal(err)
	}
	syncer2.Run(ctx2)
	if len(logged) != 1 {
		t.Fatalf("failing round must log exactly once, got %v", logged)
	}

	// With no logger, a failing round must not panic.
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	syncer3, err := New(&issue663CoreStore{loadErr: boom}, mgr, Options{ReplicaID: "r1", Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	syncer3.Run(ctx3)
}
