package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for the control-plane SQL store
	"github.com/redis/go-redis/v9"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/sharedstate"
)

const controlPlaneNamespace = "gateway/runtime"

// controlPlaneBudgetAlertNamespace is the durable namespace holding
// fired budget-threshold webhook marks for exactly-once-per-period
// delivery across restarts.
const controlPlaneBudgetAlertNamespace = "budgets/alerts"

// Bounded PostgreSQL settings for the control-plane store. The control
// plane is low-volume metadata traffic; small pools keep a fleet of
// gateways from exhausting database connections.
const (
	controlPlanePostgresMaxOpenConns    = 10
	controlPlanePostgresMaxIdleConns    = 5
	controlPlanePostgresConnMaxLifetime = 30 * time.Minute
	controlPlanePostgresPingTimeout     = 10 * time.Second
	controlPlanePostgresMigrateTimeout  = 60 * time.Second
)

type controlPlaneRuntime struct {
	manager *controlplane.Manager
	store   controlplane.Store // raw store, retained for namespace consumers (breaker sync)
	closer  io.Closer          // non-nil for the postgres/redis backends
}

// Close releases backend resources (the PostgreSQL pool or the Redis
// client). File and memory backends hold no external resources and close
// as a no-op.
func (r *controlPlaneRuntime) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

func openControlPlaneRuntime(cfg config.ControlPlaneConfig, configPath string) (*controlPlaneRuntime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	backend := cfg.Backend
	if backend == "" {
		backend = "file"
	}
	var store controlplane.Store
	var closer io.Closer
	switch backend {
	case "file":
		path := cfg.StatePath
		if path == "" {
			path = configPath + ".controlplane.json"
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(configPath), path)
		}
		var err error
		store, err = controlplane.NewFileStore(path)
		if err != nil {
			return nil, err
		}
	case "memory":
		store = controlplane.NewMemoryStore()
	case "postgres":
		var err error
		var db *sql.DB
		store, db, err = openPostgresControlPlane(cfg)
		if err != nil {
			return nil, err
		}
		closer = db
	case "redis":
		var err error
		store, closer, err = openRedisControlPlane(cfg)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unsupported control-plane backend")
	}
	mode := controlplane.FailureMode(cfg.ConfigFailure)
	if mode == "" {
		mode = controlplane.LastKnownGood
	}
	manager, err := controlplane.NewManager(store, mode)
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, err
	}
	rt := &controlPlaneRuntime{manager: manager, store: store, closer: closer}
	if cfg.MigrationDryRun {
		return rt, nil
	}
	payload, err := json.Marshal(struct {
		Schema  int    `json:"schema"`
		Backend string `json:"backend"`
	}{Schema: 1, Backend: backend})
	if err != nil {
		_ = rt.Close()
		return nil, err
	}
	if err := ensureControlPlaneSnapshot(context.Background(), manager, payload); err != nil {
		_ = rt.Close()
		return nil, err
	}
	return rt, nil
}

// ensureControlPlaneSnapshot initializes the reserved runtime marker once.
// Two replicas can both observe ErrNotFound on an empty shared store; if one
// wins the revision-0 compare-and-swap, the loser re-reads and accepts only
// the exact same bootstrap marker. Conflicts against any other value remain
// fail-closed rather than being mistaken for a successful initialization.
func ensureControlPlaneSnapshot(ctx context.Context, manager *controlplane.Manager, payload []byte) error {
	_, err := manager.Load(ctx, controlPlaneNamespace)
	if err == nil {
		return nil
	}
	if !errors.Is(err, controlplane.ErrNotFound) {
		return fmt.Errorf("load control-plane snapshot: %w", err)
	}
	if _, err := manager.Save(ctx, controlPlaneNamespace, 0, payload); err == nil {
		return nil
	} else if !errors.Is(err, controlplane.ErrConflict) {
		return fmt.Errorf("initialize control-plane snapshot: %w", err)
	}

	current, reloadErr := manager.Load(ctx, controlPlaneNamespace)
	if reloadErr != nil {
		return fmt.Errorf("initialize control-plane snapshot: revision conflict and reload failed: %w", reloadErr)
	}
	if !bytes.Equal(current.Payload, payload) {
		return errors.New("initialize control-plane snapshot: competing initializer committed an unexpected bootstrap marker")
	}
	return nil
}

// BreakerStore builds the shared circuit-breaker state store over the
// same control-plane backend this runtime already opened — no second
// connection, credential, or migration. Used only when the operator
// opts in via control_plane.breaker_sync (ADR 0003).
func (r *controlPlaneRuntime) BreakerStore(replicaID string) (*sharedstate.BreakerStore, error) {
	adapter, err := sharedstate.NewControlPlaneStore(r.store)
	if err != nil {
		return nil, err
	}
	return sharedstate.NewBreakerStore(adapter, sharedstate.Options{ReplicaID: replicaID})
}

// controlPlaneBudgetAlertIO adapts the durable control-plane manager
// to httpapi.BudgetAlertSnapshotIO so budget webhook alerts
// deduplicate across restarts on every control-plane backend (file,
// postgres, redis). Control-plane errors map to the alerter's
// sentinel errors; every failure is best-effort and never fails a
// data-plane request.
type controlPlaneBudgetAlertIO struct {
	runtime *controlPlaneRuntime
}

func (a *controlPlaneBudgetAlertIO) Read(ctx context.Context) ([]byte, uint64, error) {
	snap, err := a.runtime.manager.Load(ctx, controlPlaneBudgetAlertNamespace)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			return nil, 0, httpapi.ErrBudgetAlertStoreNotFound
		}
		if errors.Is(err, controlplane.ErrConflict) {
			return nil, 0, httpapi.ErrBudgetAlertStoreConflict
		}
		return nil, 0, err
	}
	return append([]byte(nil), snap.Payload...), snap.Revision, nil
}

func (a *controlPlaneBudgetAlertIO) Write(ctx context.Context, payload []byte, expectedRevision uint64) (uint64, error) {
	snap, err := a.runtime.manager.Save(ctx, controlPlaneBudgetAlertNamespace, expectedRevision, payload)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			return 0, httpapi.ErrBudgetAlertStoreNotFound
		}
		if errors.Is(err, controlplane.ErrConflict) {
			return 0, httpapi.ErrBudgetAlertStoreConflict
		}
		return 0, err
	}
	return snap.Revision, nil
}

// openPostgresControlPlane wires the real PostgreSQL backend: the DSN is
// resolved only from the environment variable named by the configuration
// (never persisted or logged), the pool is bounded, startup pings with a
// bounded context, and the versioned migrations are applied unless this
// is a migration dry run (which must not write). Every failure is
// fail-closed with a secret-safe, actionable error.
func openPostgresControlPlane(cfg config.ControlPlaneConfig) (controlplane.Store, *sql.DB, error) {
	envName := cfg.PostgresDSNEnv
	if envName == "" {
		return nil, nil, errors.New("control_plane.postgres_dsn_env must name the environment variable holding the PostgreSQL DSN")
	}
	dsn := os.Getenv(envName)
	if dsn == "" {
		return nil, nil, fmt.Errorf("environment variable %s named by control_plane.postgres_dsn_env is not set or is empty", envName)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, errors.New("control-plane PostgreSQL driver is unavailable")
	}
	db.SetMaxOpenConns(controlPlanePostgresMaxOpenConns)
	db.SetMaxIdleConns(controlPlanePostgresMaxIdleConns)
	db.SetConnMaxLifetime(controlPlanePostgresConnMaxLifetime)
	pingCtx, cancel := context.WithTimeout(context.Background(), controlPlanePostgresPingTimeout)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		_ = db.Close()
		// The driver error can embed host/user details from the DSN; report
		// only the failure class and the remediation.
		return nil, nil, fmt.Errorf("control-plane PostgreSQL is unreachable via the DSN in %s; check network reachability, credentials and that the database exists", envName)
	}
	if !cfg.MigrationDryRun {
		registry, err := controlplane.DefaultMigrationRegistry()
		if err != nil {
			_ = db.Close()
			return nil, nil, err
		}
		migrateCtx, cancel := context.WithTimeout(context.Background(), controlPlanePostgresMigrateTimeout)
		err = controlplane.ApplyMigrations(migrateCtx, db, registry)
		cancel()
		if err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("apply control-plane migrations: %w", err)
		}
	}
	store, err := controlplane.NewSQLStore(db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return store, db, nil
}

// Bounded Redis settings for the control-plane store, mirroring the
// PostgreSQL posture: small pool, bounded dial/read/write timeouts and
// bounded retries so a sick Redis fails operations instead of hanging
// the gateway.
const (
	controlPlaneRedisPoolSize    = 10
	controlPlaneRedisDialTimeout = 5 * time.Second
	controlPlaneRedisIOTimeout   = 5 * time.Second
	controlPlaneRedisMaxRetries  = 2
	controlPlaneRedisPingTimeout = 10 * time.Second
)

// redisHashAdapter adapts a go-redis client to the control plane's
// minimal RedisHashClient contract.
type redisHashAdapter struct{ client *redis.Client }

func (a redisHashAdapter) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return a.client.HGetAll(ctx, key).Result()
}

func (a redisHashAdapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.client.Eval(ctx, script, keys, args...).Result()
}

// openRedisControlPlane wires the real Redis backend: the URL is
// resolved only from the environment variable named by the configuration
// (never persisted or logged), timeouts and retries are bounded, and
// startup pings with a bounded context. Failures are fail-closed with
// secret-safe errors; there is no fallback to another backend.
func openRedisControlPlane(cfg config.ControlPlaneConfig) (controlplane.Store, io.Closer, error) {
	envName := cfg.RedisURLEnv
	if envName == "" {
		return nil, nil, errors.New("control_plane.redis_url_env must name the environment variable holding the Redis URL")
	}
	rawURL := os.Getenv(envName)
	if rawURL == "" {
		return nil, nil, fmt.Errorf("environment variable %s named by control_plane.redis_url_env is not set or is empty", envName)
	}
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		// Parse errors can quote parts of the URL; report only the class.
		return nil, nil, fmt.Errorf("the Redis URL in %s is invalid; expected a redis:// or rediss:// URL", envName)
	}
	opts.PoolSize = controlPlaneRedisPoolSize
	opts.DialTimeout = controlPlaneRedisDialTimeout
	opts.ReadTimeout = controlPlaneRedisIOTimeout
	opts.WriteTimeout = controlPlaneRedisIOTimeout
	opts.MaxRetries = controlPlaneRedisMaxRetries
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(context.Background(), controlPlaneRedisPingTimeout)
	err = client.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("control-plane Redis is unreachable via the URL in %s; check network reachability and credentials", envName)
	}
	store, err := controlplane.NewRedisStore(redisHashAdapter{client: client})
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return store, client, nil
}
