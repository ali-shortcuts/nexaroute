//go:build integration

// Real-service two-instance acceptance tests for breaker sync. They run
// only with the `integration` build tag and disposable service
// configuration in NEXAROUTE_TEST_POSTGRES_DSN / NEXAROUTE_TEST_REDIS_URL.
// Two independent health managers + syncers share one control-plane
// store, standing in for two gateway processes.
package breakersync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/sharedstate"
)

type replica struct {
	hm     *health.Manager
	syncer *Syncer
}

func newReplica(t *testing.T, cp controlplane.Store, namespace, id string) replica {
	t.Helper()
	adapter, err := sharedstate.NewControlPlaneStore(cp)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := sharedstate.NewBreakerStore(adapter, sharedstate.Options{Namespace: namespace, ReplicaID: id})
	if err != nil {
		t.Fatal(err)
	}
	hm := health.New(3, time.Minute)
	syncer, err := New(bs, hm, Options{ReplicaID: id})
	if err != nil {
		t.Fatal(err)
	}
	return replica{hm: hm, syncer: syncer}
}

// runTwoInstanceScenario drives incident → propagation → partition →
// heal → restart against one real backing store.
func runTwoInstanceScenario(t *testing.T, cp controlplane.Store, namespace string, cleanup func()) {
	t.Helper()
	if cleanup != nil {
		defer cleanup()
	}
	ctx := context.Background()
	a := newReplica(t, cp, namespace, "instance-a")
	b := newReplica(t, cp, namespace, "instance-b")

	// Incident on A: provider failure opens the circuit there.
	a.hm.EnterCooldown("dep-x", "upstream 5xx", 5*time.Minute)
	if err := a.syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("A publish: %v", err)
	}
	if err := b.syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("B apply: %v", err)
	}
	if s := b.hm.Get("dep-x"); s.Status != health.Cooldown {
		t.Fatalf("after propagation B dep-x = %v, want cooldown", s.Status)
	}

	// Partition: A retires a second deployment and publishes, but B is
	// cut off and does not sync — B must stay ignorant, not guess.
	a.hm.Retire("dep-y", "credential rejected", "auth")
	if err := a.syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("A publish during partition: %v", err)
	}
	if s := b.hm.Get("dep-y"); s.Status == health.Retired || s.Status == health.Cooldown {
		t.Fatalf("during partition B dep-y = %v, want untouched", s.Status)
	}

	// Heal: B syncs again and receives the retired marker as a bounded
	// cooldown only — never a local retirement.
	if err := b.syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("B heal sync: %v", err)
	}
	s := b.hm.Get("dep-y")
	if s.Status == health.Retired {
		t.Fatal("remote retired marker retired the deployment on B")
	}
	if s.Status != health.Cooldown || time.Until(s.CooldownUntil) > sharedstate.DefaultMaxCooldown {
		t.Fatalf("after heal B dep-y = %+v, want bounded cooldown", s)
	}

	// Restart: a fresh instance with an empty manager converges from
	// the shared document alone.
	c := newReplica(t, cp, namespace, "instance-c")
	if err := c.syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("C restart sync: %v", err)
	}
	if s := c.hm.Get("dep-x"); s.Status != health.Cooldown {
		t.Fatalf("after restart C dep-x = %v, want cooldown", s.Status)
	}
}

func TestBreakerSyncPostgresTwoInstances(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	cp, err := controlplane.NewSQLStore(db)
	if err != nil {
		t.Fatal(err)
	}
	namespace := fmt.Sprintf("nexaroute:breaker/it/%d", time.Now().UnixNano())
	runTwoInstanceScenario(t, cp, namespace, func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		_, _ = db.ExecContext(cleanCtx, `DELETE FROM nexaroute_control_snapshots WHERE namespace = $1`, namespace)
	})
}

func TestBreakerSyncRedisTwoInstances(t *testing.T) {
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
	cp, err := controlplane.NewRedisStore(redisHashAdapter{client: client})
	if err != nil {
		t.Fatal(err)
	}
	namespace := fmt.Sprintf("nexaroute:breaker/it/%d", time.Now().UnixNano())
	runTwoInstanceScenario(t, cp, namespace, func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		_ = client.Del(cleanCtx, "nexaroute:control:snapshot:"+namespace).Err()
	})
}

type redisHashAdapter struct{ client *redis.Client }

func (a redisHashAdapter) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return a.client.HGetAll(ctx, key).Result()
}

func (a redisHashAdapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.client.Eval(ctx, script, keys, args...).Result()
}
