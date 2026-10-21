//go:build integration

// Real-service integration tests for the shared breaker state-store adapter.
// They run only with the `integration` build tag and disposable service
// configuration in NEXAROUTE_TEST_POSTGRES_DSN / NEXAROUTE_TEST_REDIS_URL.
// Ordinary unit runs never touch a real server.
package sharedstate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

func TestPostgresBreakerStoreIntegration(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("integration database unreachable: %v", err)
	}
	registry, err := controlplane.DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := controlplane.ApplyMigrations(ctx, db, registry); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	sqlStore, err := controlplane.NewSQLStore(db)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := NewControlPlaneStore(sqlStore)
	if err != nil {
		t.Fatal(err)
	}
	var sequence atomic.Int64
	runBreakerStoreContract(t, func(t *testing.T) Store {
		namespace := fmt.Sprintf("integration/sharedstate/%s/%d/%d", t.Name(), time.Now().UnixNano(), sequence.Add(1))
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = db.ExecContext(cleanup, `DELETE FROM nexaroute_control_snapshots WHERE namespace = $1`, namespace)
		})
		return namespaceStore{Store: backend, namespace: namespace}
	})
}

func TestRedisBreakerStoreIntegration(t *testing.T) {
	rawURL := os.Getenv("NEXAROUTE_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("NEXAROUTE_TEST_REDIS_URL not set; skipping real-Redis integration test")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("parse integration redis URL: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("integration redis unreachable: %v", err)
	}
	redisStore, err := controlplane.NewRedisStore(redisHashAdapter{client: client})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := NewControlPlaneStore(redisStore)
	if err != nil {
		t.Fatal(err)
	}
	var sequence atomic.Int64
	runBreakerStoreContract(t, func(t *testing.T) Store {
		namespace := fmt.Sprintf("integration/sharedstate/%s/%d/%d", t.Name(), time.Now().UnixNano(), sequence.Add(1))
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = client.Del(cleanup, "nexaroute:control:snapshot:"+namespace).Err()
		})
		return namespaceStore{Store: backend, namespace: namespace}
	})
}

type namespaceStore struct {
	Store
	namespace string
}

func (s namespaceStore) Load(ctx context.Context, _ string) (Snapshot, error) {
	return s.Store.Load(ctx, s.namespace)
}

func (s namespaceStore) CompareAndSwap(ctx context.Context, _ string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	return s.Store.CompareAndSwap(ctx, s.namespace, expectedRevision, payload)
}

type redisHashAdapter struct{ client *redis.Client }

func (a redisHashAdapter) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return a.client.HGetAll(ctx, key).Result()
}

func (a redisHashAdapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.client.Eval(ctx, script, keys, args...).Result()
}
