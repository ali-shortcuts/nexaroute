package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issue663CoreStore is a scripted Store for Manager branch coverage.
// It never touches Postgres/Redis; every failure is in-memory.
type issue663CoreStore struct {
	getErr error
	putErr error
	value  Snapshot
}

func (s *issue663CoreStore) Get(context.Context, string) (Snapshot, error) {
	if s.getErr != nil {
		return Snapshot{}, s.getErr
	}
	return s.value, nil
}

func (s *issue663CoreStore) Put(context.Context, string, uint64, []byte) (Snapshot, error) {
	if s.putErr != nil {
		return Snapshot{}, s.putErr
	}
	return s.value, nil
}

func (s *issue663CoreStore) Delete(context.Context, string, uint64) error { return nil }

func TestIssue663CoreManagerLoadSaveBranches(t *testing.T) {
	ctx := context.Background()
	if _, err := NewManager(nil, FailClosed); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil store must fail with ErrUnavailable, got %v", err)
	}
	if _, err := NewManager(NewMemoryStore(), FailureMode("bogus")); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("invalid failure mode must fail, got %v", err)
	}

	// Load: conflict is counted and returned, never served from cache.
	conflictStore := &issue663CoreStore{getErr: ErrConflict, value: Snapshot{Revision: 1, Payload: []byte("v")}}
	mgr, err := NewManager(conflictStore, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Load(ctx, "ns"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Load conflict must surface, got %v", err)
	}
	if got := mgr.Health(); got.Conflicts != 1 || got.Loads != 1 {
		t.Fatalf("conflict health = %+v", got)
	}

	// Load: a non-unavailable error is returned verbatim in every mode.
	boom := errors.New("store exploded")
	for _, mode := range []FailureMode{FailClosed, FailOpen, LastKnownGood} {
		mgr, err := NewManager(&issue663CoreStore{getErr: boom}, mode)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.Load(ctx, "ns"); !errors.Is(err, boom) {
			t.Fatalf("mode %s: generic Load error must surface, got %v", mode, err)
		}
	}

	// Load: unavailable with no cached snapshot in LastKnownGood mode.
	mgr, err = NewManager(&issue663CoreStore{getErr: ErrUnavailable}, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Load(ctx, "ns"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("uncached unavailable Load must fail, got %v", err)
	}
	if got := mgr.Health(); got.Unavailable != 1 {
		t.Fatalf("unavailable health = %+v", got)
	}
	// ... and the same store in FailOpen mode also fails (only
	// LastKnownGood with a cached value serves stale data).
	mgr, err = NewManager(&issue663CoreStore{getErr: ErrUnavailable}, FailOpen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Load(ctx, "ns"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("FailOpen unavailable Load must fail, got %v", err)
	}

	// Save: unavailable and conflict are counted and returned; a
	// successful save is cached and observable via LastKnown.
	mgr, err = NewManager(&issue663CoreStore{putErr: ErrUnavailable}, FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Save(ctx, "ns", 0, []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Save unavailable must surface, got %v", err)
	}
	mgr, err = NewManager(&issue663CoreStore{putErr: ErrConflict}, FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Save(ctx, "ns", 0, []byte("x")); !errors.Is(err, ErrConflict) {
		t.Fatalf("Save conflict must surface, got %v", err)
	}
	mgr, err = NewManager(&issue663CoreStore{putErr: boom}, FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Save(ctx, "ns", 0, []byte("x")); !errors.Is(err, boom) {
		t.Fatalf("generic Save error must surface, got %v", err)
	}
	okStore := &issue663CoreStore{value: Snapshot{Revision: 7, Payload: []byte("saved")}}
	mgr, err = NewManager(okStore, FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := mgr.Save(ctx, "ns", 6, []byte("saved"))
	if err != nil || saved.Revision != 7 {
		t.Fatalf("Save = %+v, %v", saved, err)
	}
	if got := mgr.Health(); got.Saves != 1 {
		t.Fatalf("saves health = %+v", got)
	}
	cached, ok := mgr.LastKnown("ns")
	if !ok || string(cached.Payload) != "saved" {
		t.Fatalf("saved snapshot must be cached: %+v %v", cached, ok)
	}
	// Mutating the returned payload must not corrupt the cache: the
	// manager clones on both store and read paths.
	cached.Payload[0] = 'X'
	cached2, _ := mgr.LastKnown("ns")
	if string(cached2.Payload) != "saved" {
		t.Fatalf("cache clone broken: %q", cached2.Payload)
	}
	mgr.Invalidate("ns")
	if _, ok := mgr.LastKnown("ns"); ok {
		t.Fatal("Invalidate must drop the cached snapshot")
	}
}

func TestIssue663CoreFileStorePersistFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// The store directory is a regular file, so persist's MkdirAll
	// fails before any write happens.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(filepath.Join(blocker, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Note: the store lock guards the same directory first, so a blocked
	// directory fails at lock acquisition before persist's own MkdirAll
	// runs; persist's MkdirAll error is unreachable through Put/Delete/
	// Restore in the current code. The fail-closed behavior is what this
	// case pins down.
	if _, err := store.Put(ctx, "ns", 0, []byte("payload")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Put under a file directory must fail closed, got %v", err)
	}
	// A store whose path is itself a directory fails at load/persist
	// time rather than silently succeeding.
	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store2.Put(ctx, "ns", 0, []byte("payload")); err == nil {
		t.Fatal("Put into a directory path must fail")
	}
	// Happy path on a real file-backed store, exercising persist +
	// load through the manager's Save/Load pair.
	real, err := NewFileStore(filepath.Join(dir, "sub", "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(real, FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := mgr.Save(ctx, "ns", 0, []byte("file-backed"))
	if err != nil || saved.Revision != 1 || saved.Schema != 1 {
		t.Fatalf("file-backed Save = %+v, %v", saved, err)
	}
	loaded, err := mgr.Load(ctx, "ns")
	if err != nil || string(loaded.Payload) != "file-backed" {
		t.Fatalf("file-backed Load = %+v, %v", loaded, err)
	}
	// Persist again through a fresh store instance over the same file
	// to prove the bytes on disk round-trip (not just the cache).
	real2, err := NewFileStore(filepath.Join(dir, "sub", "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = real2.Get(ctx, "ns")
	if err != nil || string(loaded.Payload) != "file-backed" {
		t.Fatalf("reopened store Load = %+v, %v", loaded, err)
	}
}

func TestIssue663CoreDefaultMigrationRegistry(t *testing.T) {
	reg, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	all := reg.All()
	if len(all) != 1 || all[0].Version != 1 || all[0].Name != "create control snapshots table" {
		t.Fatalf("default registry = %+v", all)
	}
	if all[0].Up != ControlPlaneSQLSchema() {
		t.Fatal("default migration Up must equal ControlPlaneSQLSchema")
	}
	if all[0].Checksum() == "" {
		t.Fatal("migration checksum must not be empty")
	}
	pending, err := reg.Pending(nil)
	if err != nil || len(pending) != 1 || pending[0].Version != 1 {
		t.Fatalf("pending with no applied migrations = %+v, %v", pending, err)
	}
	pending, err = reg.Pending(map[uint64]string{1: all[0].Checksum()})
	if err != nil || len(pending) != 0 {
		t.Fatalf("applied migration must not be pending: %+v, %v", pending, err)
	}
	if _, err := reg.Pending(map[uint64]string{1: "deadbeef"}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum mismatch must fail, got %v", err)
	}
}

func TestIssue663CoreFileLockTimeoutError(t *testing.T) {
	err := &FileLockTimeoutError{Path: "/data/store.json", Op: "put", Timeout: 5 * time.Second}
	if !strings.Contains(err.Error(), "put") || !strings.Contains(err.Error(), "/data/store.json") || !strings.Contains(err.Error(), "5s") {
		t.Fatalf("timeout error text wrong: %q", err.Error())
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("timeout error must unwrap to ErrUnavailable")
	}
	var target *FileLockTimeoutError
	if !errors.As(err, &target) || target.Op != "put" {
		t.Fatalf("errors.As must recover the typed timeout, got %+v", target)
	}
	if got := lockFilePath("/data/dir/store.json"); got != "/data/dir/.controlplane.lock" {
		t.Fatalf("lockFilePath = %q", got)
	}
}

func TestIssue663CoreWrapStoreError(t *testing.T) {
	if err := wrapStoreError("get", nil); err != nil {
		t.Fatalf("nil error must stay nil, got %v", err)
	}
	if err := wrapStoreError("get", context.Canceled); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("canceled must pass through without becoming unavailable, got %v", err)
	}
	if err := wrapStoreError("get", context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline must pass through, got %v", err)
	}
	for _, sentinel := range []error{ErrUnavailable, ErrNotFound, ErrConflict, ErrMalformedSnapshot, ErrInvalidRevision} {
		if err := wrapStoreError("get", sentinel); !errors.Is(err, sentinel) {
			t.Fatalf("sentinel %v must pass through, got %v", sentinel, err)
		}
	}
	generic := errors.New("disk exploded")
	wrapped := wrapStoreError("put", generic)
	if !errors.Is(wrapped, ErrUnavailable) || !errors.Is(wrapped, generic) || !strings.HasPrefix(wrapped.Error(), "put: ") {
		t.Fatalf("generic error must become unavailable with op prefix, got %v", wrapped)
	}
}
