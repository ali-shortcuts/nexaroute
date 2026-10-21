# Architecture slice 4 — durable PostgreSQL video job store (issue #609)

Parent audit: #593. Builds on the lease-expiry and idempotency-race
fixes; does not redo them. One focused PR; no new commercial provider;
v0.23.0 untouched; no multi-replica/asset-durability claims.

## What was built

A third `queue.JobStore` implementation, `PostgresStore`
(`internal/video/queue/pg_store.go`), selected by the new
`video.job_store_backend` (`file` default | `postgres`) +
`video.job_store_url` (literal DSN or `env:VARIABLE_NAME`) config keys,
patterned on `admin.security_store_backend`. `runtime.New` branches on
the backend; unreachable/misconfigured PostgreSQL is a hard startup
error, never a silent fallback. The file store remains the default —
existing deployments are untouched.

## Schema and migrations

One table, inline DDL (repo convention — no migration tool), applied
in a single transaction serialized across processes with
`pg_advisory_xact_lock`:

```sql
CREATE TABLE IF NOT EXISTS nexaroute_video_jobs (
  job_id text PRIMARY KEY,
  idempotency_key text NOT NULL DEFAULT '',
  payload jsonb NOT NULL,          -- whole VideoJob record
  lease_owner text NOT NULL DEFAULT '',
  lease_until timestamptz NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS nexaroute_video_jobs_idem_uniq
  ON nexaroute_video_jobs (idempotency_key) WHERE idempotency_key <> '';
```

Columns are authoritative for `idempotency_key`, `lease_owner`,
`lease_until`; the JSONB payload carries everything else. Every read
overlays the columns onto the unmarshaled struct.

## Transaction boundaries

- **Create:** single `INSERT`. PK violation → "job already exists";
  idempotency-index violation → `ErrIdempotencyConflict` (a follow-up
  lookup replicates the memory store's job_id-first check order).
  Concurrent duplicates resolve to exactly one job at the DB
  serialization point.
- **TryClaim:** single conditional `UPDATE … WHERE (lease_owner = ''
  OR lease_until IS NULL OR lease_until <= clock_timestamp() OR
  lease_owner = $owner)` with `RETURNING`. Lease comparisons and the
  new `lease_until` use the **server clock**, so skewed replica clocks
  cannot resurrect a live foreign lease. Same-owner renews; live
  foreign lease refuses `(false, nil)`; missing job is `ErrNotFound`.
- **Update:** `SELECT … FOR UPDATE` + `checkTerminalUpdate` in one
  transaction — two processes cannot both pass the terminal guard.
  Byte-identical replay of a terminal job is allowed (idempotent
  completion); anything else over a terminal job is `ErrTerminalJob`.
- **Delete:** removes the row; the idempotency mapping (a column) goes
  with it, so pruning frees the key (memory-store semantic preserved).
- **GetByIdempotency:** empty keys are never indexed → `ErrNotFound`.

## Tests

- Unit (no service): `OpenPostgresStore` fail-closed paths — empty
  DSN, unreachable server, garbage DSN; DSN never leaks into errors.
- Config: `job_store_backend`/`job_store_url` validation matrix
  (unknown backend rejected, postgres requires URL, file rejects URL).
- Integration (`-tags integration`, real PG in CI):
  `TestPostgresVideoStoreDuplicateCreate` (16 racers, exactly one
  winner, 15 `ErrIdempotencyConflict`),
  `TestPostgresVideoStoreDuplicateJobID`,
  `TestPostgresVideoStoreConcurrentClaim` (16 racers, one winner;
  live-foreign refused; same-owner renews),
  `TestPostgresVideoStoreExpiredLeaseClaimable`,
  `TestPostgresVideoStoreRestartRecovery` (close/reopen: state,
  lease, idempotency mapping and duplicate-conflict persist),
  `TestPostgresVideoStoreTerminalGuard` (identical replay allowed,
  divergent overwrite blocked),
  `TestPostgresVideoStoreEmptyIdempotencyKey`,
  `TestPostgresVideoStoreDeleteFreesIdempotencyKey`.
- CI: new step in the `integration-postgres` job runs
  `go test -tags integration ./internal/video/queue/ -run
  'TestPostgresVideoStore'`.

## Remaining limits (not claimed)

- Job **metadata** is durable and shared; video **assets** are not —
  no shared asset storage is claimed.
- The cost ledger (`internal/video/cost.Ledger`) stays process-local
  and resets on restart; the job↔ledger atomic transaction is the
  next dependent slice, deliberately not faked here.
- `storeDurability` now reports `"postgres"` for the new backend;
  Studio copy must not imply more than job-metadata durability.
- Cross-process `Update` races are serialized by row locking, but
  callers still do read-modify-write at the orchestrator level;
  last-writer-wins applies to non-terminal fields.
