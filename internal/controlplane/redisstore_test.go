package controlplane

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRedisHash emulates the two Lua scripts' semantics in Go so the
// adapter's client-side logic (result decoding, error mapping) is unit
// tested without a server. Atomicity itself rests on Redis executing
// each script atomically and is proven by the gated integration suite
// against a real Redis.
type fakeRedisHash struct {
	mu   sync.Mutex
	data map[string]map[string]string
	err  error
}

func newFakeRedisHash() *fakeRedisHash {
	return &fakeRedisHash{data: make(map[string]map[string]string)}
}

func (f *fakeRedisHash) HGetAll(_ context.Context, key string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]string, len(f.data[key]))
	for k, v := range f.data[key] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRedisHash) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	key := keys[0]
	str := func(i int) string { s, _ := args[i].(string); return s }
	switch script {
	case redisPutScript:
		fields := f.data[key]
		var curRev uint64
		if fields != nil {
			curRev, _ = strconv.ParseUint(fields["revision"], 10, 64)
		}
		expected, _ := strconv.ParseUint(str(0), 10, 64)
		if curRev != expected {
			return []any{int64(0), ""}, nil
		}
		if fields == nil {
			fields = make(map[string]string)
			f.data[key] = fields
		}
		schema := fields["schema"]
		if schema == "" {
			schema = str(2)
		}
		fields["revision"] = str(1)
		fields["schema"] = schema
		fields["payload"] = str(3)
		fields["updated_at"] = str(4)
		return []any{int64(1), schema}, nil
	case redisDeleteScript:
		fields := f.data[key]
		var curRev uint64
		if fields != nil {
			curRev, _ = strconv.ParseUint(fields["revision"], 10, 64)
		}
		expected, _ := strconv.ParseUint(str(0), 10, 64)
		if fields == nil || curRev != expected {
			return int64(0), nil
		}
		delete(f.data, key)
		return int64(1), nil
	}
	return nil, errors.New("unknown script")
}

func TestRedisStoreConformanceAgainstFake(t *testing.T) {
	ctx := context.Background()
	store, err := NewRedisStore(newFakeRedisHash())
	if err != nil {
		t.Fatal(err)
	}
	const ns = "gateway/runtime"
	if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	created, err := store.Put(ctx, ns, 0, []byte(`{"v":1}`))
	if err != nil || created.Revision != 1 || created.Schema != 1 {
		t.Fatalf("create: %+v %v", created, err)
	}
	got, err := store.Get(ctx, ns)
	if err != nil || got.Revision != 1 || string(got.Payload) != `{"v":1}` {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := store.Put(ctx, ns, 0, []byte("x")); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-create: %v, want ErrConflict", err)
	}
	updated, err := store.Put(ctx, ns, 1, []byte(`{"v":2}`))
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update: %+v %v", updated, err)
	}
	if _, err := store.Put(ctx, ns, 1, []byte("stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v, want ErrConflict", err)
	}
	if err := store.Delete(ctx, ns, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete: %v, want ErrConflict", err)
	}
	if err := store.Delete(ctx, ns, 2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, err := store.Put(ctx, "", 0, []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty namespace: %v", err)
	}
	if _, err := store.Put(ctx, ns, 0, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty payload: %v", err)
	}
	if _, err := NewRedisStore(nil); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestRedisStoreCorruptAndUnavailable(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedisHash()
	store, _ := NewRedisStore(fake)
	fake.data[redisSnapshotPrefix+"broken"] = map[string]string{"revision": "not-a-number"}
	if _, err := store.Get(ctx, "broken"); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt record: %v, want a corrupt-record error", err)
	}
	fake.err = errors.New("connection refused")
	if _, err := store.Get(ctx, "broken"); err == nil || !strings.Contains(err.Error(), "get control snapshot") {
		t.Fatalf("unavailable get: %v", err)
	}
	if _, err := store.Put(ctx, "broken", 0, []byte("x")); err == nil || !strings.Contains(err.Error(), "write control snapshot") {
		t.Fatalf("unavailable put: %v", err)
	}
	if err := store.Delete(ctx, "broken", 1); err == nil || !strings.Contains(err.Error(), "delete control snapshot") {
		t.Fatalf("unavailable delete: %v", err)
	}
}
