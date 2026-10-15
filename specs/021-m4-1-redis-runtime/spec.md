# M4-1 (Redis) — real runtime wiring for the control-plane Redis store

## Context

`internal/controlplane/redis.go` previously held only lease and
token-bucket primitives over a small client interface — not a `Store`.
The gateway runtime refused `backend=redis` at startup. This unit
completes the Redis half of M4-1 (PostgreSQL landed in specs/020).

## Design

- `controlplane.RedisStore` implements the `Store` contract on a
  minimal `RedisHashClient` interface (HGetAll + Eval), keeping the
  package client-agnostic exactly like `SQLStore`/`database/sql`. One
  Redis hash per namespace under `nexaroute:control:snapshot:<ns>`
  with fields revision/schema/payload/updated_at.
- Writes and deletes are Lua compare-and-set scripts (atomic in Redis):
  the stored revision must equal the caller's expected revision, else
  `ErrConflict` — identical semantics to the SQL/file stores, including
  Delete-on-missing = ErrConflict. An existing record's schema is
  preserved; the script returns the effective schema so `Put`'s
  returned snapshot is exact.
- Corrupt or foreign data at a snapshot key (wrong type, unparseable
  fields) produces a deterministic error — never a false success and
  never `ErrNotFound`. Redis/client errors are wrapped as failures,
  never converted into conflicts or successes.
- The lease primitives in redis.go are unchanged and are NOT used by
  the store; no quota/lease behavior is enabled on any production path
  by this change.

## Runtime wiring (cmd/gateway)

go-redis v9.17.0 (pinned; the graph stays on the Go 1.25 floor). The
URL is resolved only from the env var named by
`control_plane.redis_url_env`; parse failures report only the failure
class (URLs can embed credentials). Bounded pool (10), dial/IO
timeouts (5s), bounded retries (2), 10s startup ping; the client is
closed at shutdown via the runtime's closer. `migration_dry_run`
performs the readiness ping but writes nothing.

## Durability boundary (operator responsibility)

Redis durability is a property of the operator's Redis, not of this
adapter: durable control-plane state requires persistence (AOF
`appendonly yes` and/or RDB snapshots) and, for high availability,
replica/failover management (Sentinel/Cluster) — a default ephemeral
Redis loses state on restart. This is documented in CONFIGURATION.md;
the adapter never implies durability it cannot provide.

## Evidence model

- Unit: a fake client emulating both scripts covers Go-side conformance
  (CRUD/conflict/not-found) plus corrupt/unavailable error mapping.
- Real service: `redis_integration_test.go` (build tag `integration`,
  `NEXAROUTE_TEST_REDIS_URL`) — conformance against the real server,
  two independent clients, corruption behavior, and a 16-writer
  first-write race (exactly one success). CI runs it in the
  `integration-redis` job against a disposable `redis:7-alpine`.
