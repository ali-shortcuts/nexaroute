package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for the control-plane SQL store

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

const controlPlaneNamespace = "gateway/runtime"

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
	db      *sql.DB // non-nil only for the postgres backend
}

// Close releases backend resources (the PostgreSQL pool). File and memory
// backends hold no external resources and close as a no-op.
func (r *controlPlaneRuntime) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
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
	var db *sql.DB
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
		store, db, err = openPostgresControlPlane(cfg)
		if err != nil {
			return nil, err
		}
	case "redis":
		// The Redis Store adapter is not wired yet (M4-1 follow-up). Refuse
		// startup instead of silently falling back to memory.
		return nil, errors.New("control_plane.backend=redis is configured but the Redis store adapter is not registered in this build; refusing unsafe fallback")
	default:
		return nil, errors.New("unsupported control-plane backend")
	}
	mode := controlplane.FailureMode(cfg.ConfigFailure)
	if mode == "" {
		mode = controlplane.LastKnownGood
	}
	manager, err := controlplane.NewManager(store, mode)
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		return nil, err
	}
	rt := &controlPlaneRuntime{manager: manager, db: db}
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
	_, err = manager.Load(context.Background(), controlPlaneNamespace)
	if errors.Is(err, controlplane.ErrNotFound) {
		if _, err = manager.Save(context.Background(), controlPlaneNamespace, 0, payload); err != nil {
			_ = rt.Close()
			return nil, fmt.Errorf("initialize control-plane snapshot: %w", err)
		}
	} else if err != nil {
		_ = rt.Close()
		return nil, fmt.Errorf("load control-plane snapshot: %w", err)
	}
	return rt, nil
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
