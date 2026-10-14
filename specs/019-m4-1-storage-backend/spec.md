# M4-1A — Runtime-selectable security-store backend (interface + selection)

## Problem

The admin security store (sessions + tamper-evident audit chain) is a
concrete bbolt implementation (`internal/securitystore.Store`) constructed
directly by the HTTP server. The roadmap (M4-1/M4-2) requires Redis and
PostgreSQL backends to be selectable at runtime without breaking the
zero-service default, with real-service contract/crash/migration tests
(M4-2). There is currently no interface, no selection setting, and no
contract suite an alternative backend could be verified against.

## Scope of this unit (M4-1A)

- A `securitystore.Backend` interface capturing exactly the store contract
  the server consumes (session lifecycle, audit append/read, chain
  verification, close). The bbolt `Store` satisfies it (compile-time
  assertion); its behavior is unchanged.
- Runtime selection via `admin.security_store_backend`:
  `""`/`"bbolt"` (default, zero-service), `"redis"`, `"postgres"`, with
  `admin.security_store_url` carrying the future adapter address.
  Selecting `redis`/`postgres` in this build FAILS CLOSED at startup with
  an explicit error: the adapters land in M4-2, and silently falling back
  to a local or in-memory store when the operator required a durable
  shared backend would be an unsafe downgrade (mission rule B1).
- A reusable backend contract test suite that runs the full
  session/audit/chain contract against any `Backend` implementation;
  executed here against bbolt. M4-2 adapters must pass the same suite
  against real services (opt-in), plus crash/migration/two-replica tests.

## State classification (what the backend covers)

- **Durable, backend-owned:** admin sessions and the audit chain
  (`securitystore`). This is the only state with cross-restart integrity
  requirements today, so it is the M4-1/M4-2 scope.
- **Process-local by design (not backend scope):** rate-limit buckets,
  provider health/circuit state, response cache, in-flight quota
  reservations, usage counters. Replicas do not share these today;
  M4-2's two-replica tests must document the resulting semantics for
  sessions/audit only.

## Failure semantics

- Unknown backend or `redis`/`postgres` without a URL: configuration
  validation error (fail closed before startup).
- `redis`/`postgres` selected in this build: store open error; the server
  keeps its existing posture — protected admin requests fail closed and
  the data plane is unaffected.
- bbolt open failure behavior is unchanged.

## Out of scope (M4-2)

The Redis/PostgreSQL adapters themselves, their live contract/crash/
migration/backup/two-replica verification (UNVERIFIED until real services
are available), and any change to audit-chain cryptography or session
semantics.
