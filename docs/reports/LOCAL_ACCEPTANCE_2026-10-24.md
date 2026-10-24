# Local Acceptance Execution — 2026-10-24

Owner-authorized execution package to convert as much of the external
acceptance register as possible from UNVERIFIED/BLOCKED to executed
evidence, using real systems stood up locally. Everything below was
executed on 2026-10-24 against a gateway binary built from `main` at
`bc5f0d4d` (a docs-only delta from the audited code tree `79df87d5`).

**Labels stay exact:** "executed locally" means a real system of the
named kind ran on one local host. It does not claim a live external
tenant, a production cluster, or multi-host behavior. Section 8 lists
what remains UNVERIFIED/BLOCKED after this pass.

Environment: PostgreSQL 16.15 (Ubuntu package), Redis 7.0.15,
HashiCorp Vault v2.2.1 (dev mode — the real Vault binary with an
in-memory store and a dev root token; not a production cluster),
OpenTelemetry Collector v0.163.2 (official release binary),
gnome-keyring / libsecret under `dbus-run-session`, and a
purpose-built standards-conforming OIDC provider (RS256, discovery,
JWKS, authorization-code flow with mandatory PKCE S256).

## 1. DSN-gated integration suites — executed locally (previously CI-only)

Run with the exact commands and DSNs of `.github/workflows/ci.yml`
and `integration-redis.yml`, against the local servers above.
Verbose mode confirmed every test RUN (zero skips):

- PostgreSQL — PASS: `internal/controlplane` TestPostgresIntegration
  (5 tests), `internal/securitystore`
  TestPostgresSecurityStoreIntegration, `internal/sharedstate`
  TestPostgresBreakerStoreIntegration, `internal/video/queue`
  TestPostgresVideoStore (10 tests), `internal/video/cost`
  TestPostgresLedger, `internal/breakersync`
  TestBreakerSyncPostgresTwoInstances.
- Redis (with `-race`) — PASS: `internal/controlplane`
  TestRedisIntegration (6 tests incl. outage/recovery),
  `internal/sharedstate` TestRedisBreakerStoreIntegration,
  `cmd/gateway` TestControlPlaneRedisIntegration.

## 2. Two real gateway processes on one PostgreSQL (breaker sync, ADR 0003)

Two gateway processes (replica-a :18081, replica-b :18082) shared one
PostgreSQL control plane (`breaker_sync` on, 5 s cadence) and one
loopback upstream stub whose hit counter is the ground truth
(`failure_threshold=2`, `max_attempts=1`, strategy `round_robin` — the
ready strategies additionally require probe readiness, which is
documented behavior and not what this test exercises):

1. Baseline: a request via B reached the stub (hit 1) and surfaced
   the upstream 500. B started with a clean local breaker.
2. Two failing requests via A tripped A's breaker (threshold 2);
   A's next requests failed fast with `no compatible healthy
   deployment` and produced no new stub hits.
3. After 12 s (more than two sync intervals), a request via B also
   failed fast with **zero new stub hits**. B had recorded only one
   local failure — below its own threshold — so its refusal can only
   come from replica-a's cooldown marker, consumed from the shared
   store. The marker document namespace `nexaroute:breaker/v1` was
   present in `nexaroute_control_snapshots`.
4. Recovery: with the stub healed and the 25 s cooldown expired, a
   request via A succeeded (200, hit 4).

**Video on the same topology:** a job created via replica-a's
`POST /v1/video/jobs` (Postgres job store, development fake provider)
was claimed by exactly one lease owner, reached `state=completed`,
and was read back in full via replica-b's API; the jobs table holds
exactly one row for the job. Asset files remain per-node by design
(ADR 0004 / HA inventory) and are not claimed shared.

**Operational observation (not a defect claim):** when both replicas
were started simultaneously against an *empty* PostgreSQL control
plane, the second failed startup closed with `control-plane
revision conflict` — both raced to create the initial snapshot and
the store's CAS semantics rejected the loser. Starting the first
replica, letting it initialize, then starting the second works.
Operators should sequence first-time initialization the same way.

## 3. OIDC login — full flow executed (local conforming IdP)

Gateway instance with `admin.oidc` enabled against the local IdP:

- `GET /admin/auth/login` → 302 to the IdP authorize endpoint with
  `code_challenge`/`S256`, `state`, `nonce` (all present, verified
  in the redirect).
- IdP authorization → callback with code+state → gateway exchanged
  the code (the IdP log shows one token call, Basic client auth,
  PKCE verifier match), verified the ID token against the IdP's
  current JWKS, mapped the `groups` claim
  (`nexaroute-admins` → `admin`), created a session and answered
  303 with the session cookie.
- `GET /admin/auth/status` with the session: `authenticated: true`,
  subject `alice-123`, roles `["admin"]`.
- `GET /admin/api/audit` (admin-only) with the session: 200.
  Without a session: 401. A forged callback code: 401 (fail closed).

Interop note: when the IdP's token response lacked
`Content-Type: application/json`, the standard OAuth2 client retried
the exchange with the other client-auth style after failing to parse
the body — correct client behavior against a non-conforming server;
the IdP was fixed, not the gateway. A live organizational tenant
(Keycloak/Entra/Google, …) remains UNVERIFIED.

## 4. OTLP ingestion — real OpenTelemetry Collector

Gateway telemetry configured with
`telemetry.otlp_endpoint_env` pointing at the real collector
(127.0.0.1:4318, `debug` exporter, detailed verbosity). One chat
request through the gateway produced a span the collector received
and decoded: `service.name=nexaroute`, scope `nexaroute`, with
`gen_ai.operation.name=chat`, `gen_ai.system=stub`,
`gen_ai.response.model=stub-model`,
`gen_ai.usage.input_tokens=3`, `gen_ai.usage.output_tokens=2`,
`nexaroute.deployment=stub/stub-model`. Managed backends
(Jaeger/Tempo/vendor APM) remain UNVERIFIED.

## 5. HashiCorp Vault — KV round-trip and gateway key custody

- The shipped helper (`scripts/key-provider-vault-kv.sh`) against the
  real Vault: `put` then `get` of a 32-byte master key returned
  byte-identical material (KV v2 path
  `secret/data/nexaroute/master-key`).
- A gateway booted with `NEXAROUTE_KEY_PROVIDER=command` (helper as
  the command provider) encrypted its inline provider `api_key` in
  place at startup (`nxs1` envelope in the config, encrypted backup
  written) — with **no local `.key` file**, i.e. custody was the
  Vault-held key — and served requests (chat 200).
- `nexaroute secrets verify` with Vault access prints
  `OK provider:stub.api_key` (verified_fields=1); with a wrong Vault
  token it fails (`key provider command failed`), and a gateway boot
  with the wrong token fails closed at startup with the same error.

Dev-mode Vault is a real Vault binary but a single in-memory node:
a production Vault cluster, HA failover and policy design remain
UNVERIFIED. **Vault Transit envelope mode remains NOT IMPLEMENTED**
(the supported pattern loads raw key material into the gateway
process; unchanged by this pass).

## 6. OS keyring — real Secret Service round-trip

Under `dbus-run-session` with gnome-keyring as the Secret Service:
the acceptance key was stored via `secret-tool` (service
`nexaroute`, account `nexaroute/master-key`) and looked up
byte-identical; the gateway binary with
`NEXAROUTE_KEY_PROVIDER=keyring` then decrypted the §5 config field
(`secrets verify`: `OK provider:stub.api_key`, verified_fields=1).
The project's own tests stub `secret-tool`; this run used the real
one. (The gateway was already able to *load* from a keyring; a
desktop/distribution keyring in production remains operator-verified.)

**Minor portability observation:** the keyring provider's `Store`
invocation omits `--label`, which the current `secret-tool`
(libsecret-tools) requires for a store — its stub-based test does
not model that requirement. `Store` currently has no production
caller (the runtime only Loads; provisioning is operator-side), so
this is recorded as an observation, not a production defect.

## 7. What this pass did NOT execute

Unchanged boundaries (see `docs/EXTERNAL_VERIFICATION.md`):

- Live organizational OIDC tenant; live LLM provider tenants
  (zero paid calls — unchanged policy); AWS Bedrock, Google Vertex
  AI, Azure OpenAI tenants.
- Production Vault cluster / HA; production Kubernetes cluster
  install; multi-hour production soak; multi-host HA (all runs
  above were single-host, two processes).
- In-browser axe/FCP/RTL acceptance (sandbox boundary unchanged;
  the repo's browser-evidence CI remains the executed route).
- Vault Transit envelope mode: NOT IMPLEMENTED.
- ComfyUI real model inference (HTTP contract only, as before).
