//go:build integration

// Real-Redis integration tests for the control-plane Redis store. They
// run only with the `integration` build tag and a URL in
// NEXAROUTE_TEST_REDIS_URL pointing at a DISPOSABLE Redis (CI provides
// one via a service container). Ordinary unit runs never touch a real
// server.
//
// These tests are the M4-2 evidence base for the Redis adapter: store
// conformance against the real server (Lua compare-and-set semantics,
// not the unit-test fake), two independent clients, the concurrent
// first-write race, corrupt-data handling and cross-client persistence.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type goRedisHashAdapter struct{ client *redis.Client }

func (a goRedisHashAdapter) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return a.client.HGetAll(ctx, key).Result()
}

func (a goRedisHashAdapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.client.Eval(ctx, script, keys, args...).Result()
}

func integrationRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("NEXAROUTE_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("NEXAROUTE_TEST_REDIS_URL not set; skipping real-Redis integration test")
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("parse integration redis URL: %v", err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("integration redis unreachable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func integrationRedisStore(t *testing.T) (*RedisStore, *redis.Client) {
	t.Helper()
	client := integrationRedisClient(t)
	store, err := NewRedisStore(goRedisHashAdapter{client: client})
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}

func uniqueRedisNamespace(t *testing.T, client *redis.Client) string {
	t.Helper()
	ns := fmt.Sprintf("integration/%s/%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = client.Del(context.Background(), redisSnapshotPrefix+ns).Err()
	})
	return ns
}

func TestRedisIntegrationStoreConformance(t *testing.T) {
	ctx := context.Background()
	store, client := integrationRedisStore(t)
	ns := uniqueRedisNamespace(t, client)

	if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	created, err := store.Put(ctx, ns, 0, []byte(`{"v":1}`))
	if err != nil || created.Revision != 1 {
		t.Fatalf("create: %+v %v", created, err)
	}
	got, err := store.Get(ctx, ns)
	if err != nil || got.Revision != 1 || string(got.Payload) != `{"v":1}` {
		t.Fatalf("get after create: %+v %v", got, err)
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
}

// Two independent clients must observe each other's committed writes and
// never silently overwrite a newer revision; a foreign (corrupt) value
// at a snapshot key must fail deterministically, never read as success.
func TestRedisIntegrationTwoClientsAndCorruption(t *testing.T) {
	ctx := context.Background()
	a, clientA := integrationRedisStore(t)
	b, _ := integrationRedisStore(t)
	ns := uniqueRedisNamespace(t, clientA)

	if _, err := a.Put(ctx, ns, 0, []byte("from-a")); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, ns)
	if err != nil || string(got.Payload) != "from-a" {
		t.Fatalf("second client read: %+v %v", got, err)
	}
	if _, err := b.Put(ctx, ns, 1, []byte("from-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Put(ctx, ns, 1, []byte("a-overwrite")); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-client stale write: %v, want ErrConflict", err)
	}

	corruptNS := ns + "/corrupt"
	t.Cleanup(func() { _ = clientA.Del(ctx, redisSnapshotPrefix+corruptNS).Err() })
	if err := clientA.Set(ctx, redisSnapshotPrefix+corruptNS, "not-a-hash", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(ctx, corruptNS); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt key Get: %v, want a deterministic non-NotFound error", err)
	}
	if _, err := a.Put(ctx, corruptNS, 0, []byte("x")); err == nil {
		t.Fatal("Put over a corrupt non-hash key must fail, not succeed")
	}
}

// Sixteen concurrent first writes to a fresh namespace must produce
// exactly one success; the Lua CAS makes the rest deterministic
// ErrConflicts.
func TestRedisIntegrationConcurrentFirstWriteRace(t *testing.T) {
	ctx := context.Background()
	store, client := integrationRedisStore(t)
	ns := uniqueRedisNamespace(t, client)
	const writers = 16
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.Put(ctx, ns, 0, []byte(fmt.Sprintf("writer-%d", i)))
		}(i)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("writer %d: unexpected error: %v", i, err)
		}
	}
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("successes=%d conflicts=%d, want 1 and %d", successes, conflicts, writers-1)
	}
}
