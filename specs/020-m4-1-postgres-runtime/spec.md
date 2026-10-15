# M4-1 (PostgreSQL) — real runtime wiring for the control-plane SQL store

## Context

`internal/controlplane` already defines the `Store` contract, an
`SQLStore` over `database/sql`, a versioned migrator with checksum
fail-closed semantics, and file/memory backends wired in
`cmd/gateway/controlplane_runtime.go`. PostgreSQL (and Redis) previously
refused startup because no driver/client was registered. The DSN is
configured only as an environment-variable NAME
(`control_plane.postgres_dsn_env`, validated); the value never enters
the config file.

## This unit

- Runtime wiring: pgx v5.11.0 (`pgx/stdlib`, pinned in go.mod)
  registered by the gateway binary — the controlplane
  package itself remains driver-agnostic. DSN resolved from the named
  environment variable only; bounded pool (10 open / 5 idle / 30m
  lifetime); startup ping with a 10s bounded context; versioned
  migrations applied with a 60s bounded context unless
  `migration_dry_run` (which must not write); the pool is closed after
  HTTP shutdown in `main`.
- Fail-closed, secret-safe errors: a missing env value names the
  variable; an unreachable database reports only the failure class and
  remediation — the driver error (which can embed DSN details) is never
  propagated for the connect phase. No fallback to file/memory, ever.
- Race fix (`SQLStore.Put`): a per-namespace
  `pg_advisory_xact_lock(hashtext(ns))` at the start of the write
  transaction. `SELECT ... FOR UPDATE` cannot lock a missing row, so
  competing first writes previously both passed the revision check and
  one leaked a raw unique-violation; now the loser deterministically
  gets `ErrConflict`.
- `controlplane.DefaultMigrationRegistry()`: canonical v1 migration
  (snapshots table DDL shared with `ControlPlaneSQLSchema`).

## Evidence model

- Unit: scripted-driver contract tests assert the lock precedes
  INSERT/UPDATE and stale writes take only the lock and roll back;
  gateway runtime tests assert fail-closed, redacted startup errors
  (canary password never appears).
- Real service: `postgres_integration_test.go` (build tag
  `integration`, DSN from `NEXAROUTE_TEST_POSTGRES_DSN`) covers store
  conformance, two independent pools, the 8-writer first-write race,
  restart persistence, and migration idempotency/checksum/concurrent
  application. CI runs it in the `integration-postgres` job against a
  disposable `postgres:16-alpine` service. Without the tag/env, these
  tests do not run and nothing is claimed.

## Remaining (M4-2 / follow-ups)

Redis Store adapter (separate unit), crash/backup/two-process gateway
tests, and operator documentation for PostgreSQL persistence/failover.
