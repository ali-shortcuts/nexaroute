# M4-2 remainder — securitystore Redis/PostgreSQL adapters

## Starting point

PR #308 defined the securitystore.Backend contract (sessions + audit
hash chain) with a bbolt implementation and a fail-closed factory:
redis/postgres selections error as "not available in this build".
This work lands the real adapters behind the same factory, with the
SAME behavioral contract (backend_contract_test.go runs unchanged
against each adapter on real services).

## Design

- `chaincore.go`: the audit chain's decision logic — sanitize/validate,
  full-chain verification, append planning (sequence allocation,
  predecessor linkage, prefix pruning with a chain anchor) — as pure
  functions over an in-memory chain state. It is a faithful port of
  the bbolt store's appendAuditTx/verifyChainTx semantics minus the
  bbolt storage-key checks (network stores keep explicit sequence
  columns instead of composite keys). The bbolt implementation is NOT
  modified; cross-backend parity is proven empirically: identical
  operation sequences must produce identical record hashes on every
  backend.
- PostgreSQL (PR A): three tables (sessions/audit/meta). Every
  mutation runs in one transaction that takes the allocator meta row
  FOR UPDATE, serializing writers across processes; verification runs
  on the pre-append state inside the same transaction, exactly like
  bbolt's append gate.
- Redis (PR B, stacked): hashes for sessions/audit/meta plus a meta
  version; mutations follow the control-plane CAS pattern — read,
  plan in Go, commit via a Lua script that applies only if the
  version is unchanged (bounded retries).
- Factory: OpenBackend constructs the adapters. The configured
  security_store_url may use the form env:VAR_NAME to read the
  connection string from the environment (aligning with the M4-1
  no-secrets-in-config rule); plain URLs keep working.

## Verification

- runBackendContract against real PostgreSQL/Redis in the existing CI
  integration jobs (disposable services; tests are build-tagged and
  env-gated like the control-plane suites).
- Depth: two independent Backend instances on one service appending
  concurrently (chain must verify, sequences unique), close/reopen
  persistence, cross-backend hash parity vs bbolt.
- Unit: chaincore verification/planning matrices (tamper, gap, head
  mismatch, allocator regression, pruning-to-anchor, emptied state).
