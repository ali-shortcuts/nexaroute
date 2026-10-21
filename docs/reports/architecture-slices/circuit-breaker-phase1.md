# Architecture slice 3: circuit-breaker phase 1 evidence

- Issue: #608 (parent audit #593, which stays open)
- Base main at execution start: `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d`
- Branch: `opencode/issue608-20261021154045`
- Date: 2026-10-21
- Scope: first safe shared circuit-breaker state-store slice only

## Current-state inventory (before this change)

| Area | Evidence | State |
|---|---|---|
| Breaker/cooldown source of truth | `internal/health/manager.go` (`Manager`, `State`, `ScopeState`, `ProviderState`) | process-local, mutex-guarded map |
| Routing gate | `internal/router/router.go` (`eligibleDeployment`, `healthRank`) | reads local `health.Manager` only |
| Probe/recovery writers | `internal/probe/engine.go` and HTTP protocol paths | per-instance writes |
| Existing shared store | `internal/controlplane` file/memory/PostgreSQL/Redis revision stores | atomic CAS, no breaker namespace |
| Existing design | `docs/reports/shared-state-design.md` | design only, no breaker code |
| Current HA statement | `docs/HA_STATE_INVENTORY.md`, `docs/KNOWN_GAPS.md` | breaker/cooldown explicitly per-instance |

No breaker synchronization was implemented before this PR. The new package is
not wired into `health.Manager`, the router or gateway startup, so that remains
true for runtime behavior.

## Implemented in this slice

1. `internal/sharedstate/breaker.go`
   - `Entry` / `EntryStatus` with only `cooldown` and `retired`.
   - Versioned schema `1`, namespace `nexaroute:breaker/v1`.
   - Entry validation, cooldown clock cap, monotonic merge, expiry pruning,
     entry-wise malformed-entry tolerance, document size/entry limits.
2. `internal/sharedstate/store.go`
   - Explicit `Store` interface: `Load` + `CompareAndSwap`.
   - Typed errors `ErrNotFound`, `ErrConflict`, `ErrUnavailable`,
     `ErrMalformed`, `ErrInvalidEntry`.
   - `ControlPlaneStore` adapter over the existing `controlplane.Store`.
     This reuses the existing file/memory/PostgreSQL/Redis backends without a
     new table, migration, driver or consistency protocol.
3. `internal/sharedstate/breaker_store.go`
   - High-level `BreakerStore` with revision-guarded read/merge/write.
   - Bounded conflict retries (default 3 after the initial attempt).
   - Idempotent no-write when the merged document is unchanged.
   - Store outages surface as `ErrUnavailable`; no empty fallback and no
     implicit circuit clearing.
4. Tests
   - `internal/sharedstate/breaker_test.go`: merge rules, expiry, clock cap,
     malformed-entry skip, codec/schema/limit checks.
   - `internal/sharedstate/store_contract_test.go`: deterministic concurrent
     publishers, idempotency, expiration, restart over the real file store,
     outage preservation, malformed documents, retry bound, adapter error
     mapping. The same contract runs against the existing memory and file
     control-plane stores.
   - `internal/sharedstate/integration_test.go` (build tag `integration`):
     the same contract against real PostgreSQL and Redis via the existing
     control-plane adapters and disposable namespaces.
5. CI wiring
   - `.github/workflows/ci.yml` runs the new PostgreSQL and Redis integration
     tests in the existing service-container jobs.
   - `.github/workflows/integration-redis.yml` also runs the Redis case with
     `-race`.

## Documented but not implemented (designed/deferred)

These are intentionally not claimed as done:

- Gateway construction and refresh loop (`internal/health.Manager` apply,
  background publish/consume cadence, jitter/backoff-to-standalone).
- Remote `retired` application. The store can carry a sticky retired marker,
  but no production consumer applies it. The ADR requires an owner decision
  before it can affect local routing.
- Provider-level and capability/scope-level circuit sharing.
- Session-affinity sharing.
- Metrics/admin surfaces, backup/restore integration, config validation knobs.
- Any multi-instance gateway acceptance test.

## Contract summary

The ADR (`docs/adr/0001-circuit-breaker-state-consistency.md`) is authoritative.
In short:

- atomic unit = one versioned document; writers use compare-and-swap;
- merge is monotonic: retired sticky, later cooldown wins, absence never
  deletes, expired entries pruned;
- cooldown TTL is the `cooldown_until` deadline (store does not do TTL);
- malformed document = reject, never treat as empty; malformed entry = skip;
- outage = keep local state, return typed error, never admit traffic or clear
  an open circuit;
- restart = read committed document; missing namespace is the only empty view;
- default behavior is unchanged because nothing is wired and no config/API is
  added;
- every persisted cooldown is capped by `now + MaxCooldown` (default 30m).

## Test evidence (local)

Environment: Linux/amd64, Go `go1.25.0`, disposable containers
`postgres:16-alpine` and `redis:7-alpine`.

```text
$ go test ./internal/sharedstate -count=1 -v
PASS
ok  	github.com/ali-shortcuts/nexaroute/internal/sharedstate

$ go test -race ./internal/sharedstate -count=1
ok  	github.com/ali-shortcuts/nexaroute/internal/sharedstate

$ go vet ./internal/sharedstate
(no output, exit 0)

$ go test -tags integration ./internal/sharedstate -run 'Test(Postgres|Redis)BreakerStoreIntegration' -count=1 -v
PASS
ok  	github.com/ali-shortcuts/nexaroute/internal/sharedstate
```

The integration run used:
`NEXAROUTE_TEST_POSTGRES_DSN=postgres://...@127.0.0.1:55432/nexaroute_test?sslmode=disable`
and `NEXAROUTE_TEST_REDIS_URL=redis://127.0.0.1:56379/0`. Both suites passed all
contract subtests against the real services.

Full repository gates run for this branch are recorded below in
"Verification commands" once executed.

## Verification commands and outcomes

To be filled from the final local gate run on the committed branch. No
CI/Security claim is made from local results; exact-SHA CI and Security
conclusions must come from the pull request runs.

## Remaining blockers and risks

- **BLOCKED (owner consistency decision):** production wiring and remote
  application semantics. Specifically: which local transitions publish, how
  frequently, how remote `retired` is bounded/demoted, and the opt-in config
  surface. Implementing these without an owner decision would silently choose
  cross-replica semantics.
- **BLOCKED (acceptance evidence):** genuine multi-instance gateway service
  test (two gateway processes + shared store, incident, partition, heal). The
  unit and real-service adapter tests here are not distributed acceptance.
- **UNVERIFIED:** deployment-level PostgreSQL/Redis durability, replication,
  failover, clock-skew behavior at fleet scale and performance impact. Those
  remain operator-owned, as before.
- **Risk:** un-wired library code can drift from future `health.Manager`
  changes. The contract tests pin the store/merge layer; wiring work must add
  routing-level tests before claiming runtime behavior.
