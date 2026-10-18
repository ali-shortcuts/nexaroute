package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Two-instance coherence for the file control-plane store (#434):
// two independent FileStore objects over one store file stand in
// for two gateway processes on one host. Writes from instance A must
// be visible to instance B, and a stale-revision write from B must
// fail with the typed conflict error instead of silently losing A's
// update. This exercises the real production store and its flock
// coordination, not a mock.
func TestTwoInstanceFileStoreCoherence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controlplane.json")
	a, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("instance A store: %v", err)
	}
	b, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("instance B store: %v", err)
	}
	ctx := context.Background()
	const ns = "config"

	// A writes revision 1.
	snapA, err := a.Put(ctx, ns, 0, []byte(`{"v":1}`))
	if err != nil {
		t.Fatalf("A put: %v", err)
	}
	if snapA.Revision != 1 {
		t.Fatalf("A revision = %d, want 1", snapA.Revision)
	}

	// B sees A's write.
	snapB, err := b.Get(ctx, ns)
	if err != nil {
		t.Fatalf("B get: %v", err)
	}
	if snapB.Revision != 1 || string(snapB.Payload) != `{"v":1}` {
		t.Fatalf("B sees revision=%d payload=%s, want 1 / {\"v\":1}", snapB.Revision, snapB.Payload)
	}

	// B writes against the stale revision 0: typed conflict, no
	// silent lost update.
	if _, err := b.Put(ctx, ns, 0, []byte(`{"v":2}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("B stale put err = %v, want ErrConflict", err)
	}

	// B writes against the current revision: succeeds as revision 2,
	// and A observes it.
	snapB2, err := b.Put(ctx, ns, 1, []byte(`{"v":2}`))
	if err != nil {
		t.Fatalf("B put at current revision: %v", err)
	}
	if snapB2.Revision != 2 {
		t.Fatalf("B revision = %d, want 2", snapB2.Revision)
	}
	snapA2, err := a.Get(ctx, ns)
	if err != nil {
		t.Fatalf("A get after B write: %v", err)
	}
	if snapA2.Revision != 2 || string(snapA2.Payload) != `{"v":2}` {
		t.Fatalf("A sees revision=%d payload=%s, want 2 / {\"v\":2}", snapA2.Revision, snapA2.Payload)
	}

	// Delete is revision-checked too: A's stale delete conflicts,
	// B's current delete succeeds and A observes the absence.
	if err := a.Delete(ctx, ns, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("A stale delete err = %v, want ErrConflict", err)
	}
	if err := b.Delete(ctx, ns, 2); err != nil {
		t.Fatalf("B delete: %v", err)
	}
	if _, err := a.Get(ctx, ns); err == nil {
		t.Fatal("A still sees the namespace after B deleted it")
	}
}
