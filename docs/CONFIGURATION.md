# Configuration

## Runtime environment overrides

```text
NEXAROUTE_CONFIG                  config path used by the CLI default resolver
NEXAROUTE_LISTEN                  override listen address
NEXAROUTE_ADMIN_KEY               override admin API key
NEXAROUTE_ADMIN_BIND_LOCAL_ONLY   true/false override
NEXAROUTE_OIDC_CLIENT_SECRET      example secret name; set the exact name configured by admin.oidc.client_secret_env
NEXAROUTE_LOG_FILE                override bounded log path; "off" disables the app-owned file sink
NEXAROUTE_STRICT_CONFIG           true/false opt-in strict config validation (see below)
NEXAROUTE_MASTER_KEY              base64-encoded 32-byte master key (overrides file sources)
NEXAROUTE_MASTER_KEY_FILE         path to a raw 32-byte master key with mode 0600
```

## Encrypted secrets and master-key setup

Secret-bearing provider, decision-provider, Admin, client-auth, custom-header,
and proxy values are encrypted at rest as AES-256-GCM envelopes. Environment
variable *references* stay references; environment-injected key values are not
copied into the JSON file. See the canonical [security model](SECURITY.md) for
the cryptographic format, fail-closed behavior, and crash-recovery details.

By default the first config load creates `<config-path>.key` as a raw 32-byte
key with mode `0600`; the config directory defaults to mode `0700`. For managed
deployments, prefer an explicit, separately protected key file or inject
`NEXAROUTE_MASTER_KEY` as base64 of exactly 32 bytes. Environment key values
take precedence over `NEXAROUTE_MASTER_KEY_FILE`, which takes precedence over
the automatic sibling key. Do not copy the key into the same backup location
as the encrypted config without independent access controls.

The first plaintext-to-encrypted migration creates an authenticated encrypted
`<config-path>.<UTC timestamp>.enc.bak` before atomically replacing the config.
For regular backups, save the encrypted config together with its exact matching
master key in separately protected storage; an encrypted config alone cannot
be restored. To rotate an auto-managed key, run:

```bash
nexaroute secrets status --config "$CONFIG"
nexaroute secrets verify --config "$CONFIG"
nexaroute secrets rotate --config "$CONFIG"
nexaroute secrets verify --config "$CONFIG"
```

Rotation refuses when either master-key environment override is active. It
retains the previous key at `<config-path>.key.previous` and stages the new key
at `.key.next` so startup can complete an interrupted key activation. Keep a
secure offline copy of each config/key pair before rotating again; the fixed
`.key.previous` filename is overwritten by the next rotation. Never delete or
regenerate a key to recover a config. See [the operations runbook](OPERATIONS.md)
for backup, restore, and interruption procedures.

## Optional shared control plane

The shared control plane is disabled by default. When enabled, connection
strings are read only from environment variables named by the durable config;
the connection strings themselves are never written to JSON:

```json
{
  "control_plane": {
    "enabled": true,
    "postgres_dsn_env": "NEXAROUTE_PG_DSN",
    "redis_url_env": "NEXAROUTE_REDIS_URL",
    "config_failure": "last_known_good",
    "identity_failure": "fail_closed",
    "budget_failure": "fail_closed",
    "rate_limit_failure": "fail_closed"
  }
}
```

The supported failure modes are `fail_open`, `fail_closed`, and
`last_known_good`. The PostgreSQL migration runner uses an advisory lock and
checksum verification. Runtime connection wiring is opt-in and must not be
enabled without PostgreSQL/Redis availability and integration tests.

## Implicit-default warnings and opt-in strict config

When a config value is absent or set to an invalid zero value, NexaRoute
applies a documented default so existing configurations keep booting. To keep
those silent defaults visible, every applied default and every legacy
migration emits a secret-safe warning on startup that names the config key and
the behavior chosen for it (for example `probe.max_tokens: 0 is not a valid
value; defaulted to 1`). Warnings never print secret values, credentials, or
full URLs, and they never change the applied default.

Set `NEXAROUTE_STRICT_CONFIG=true` to opt into strict config validation. In
strict mode the gateway refuses to boot when the loaded config relied on an
implicit default or on the legacy `routing.public_model` migration, failing
with an actionable message that names each offending key. Default mode
behavior is unchanged: without the variable the same configuration boots with
warnings exactly as before.

## Bounded logging and long-running stability

NexaRoute owns a rotating operational log by default. With `logging.file = "auto"`, the log is placed beside the active config file as `nexaroute.log`.

Default retention:

- `max_size_mb: 32` per file;
- `max_backups: 3`;
- current file plus three backups = approximately **128 MB maximum app-owned log storage**;
- old numeric backups beyond retention are deleted automatically on startup/rotation;
- log files are created with mode `0600`;
- `access_mode: "sampled"` logs errors and slow non-streaming requests, while ordinary successful requests are sampled every 1000 requests;
- long-lived SSE responses are not misclassified as "slow" merely because the stream stays open;
- console/journald output is rate-limited to `console_max_lines_per_minute: 30` by default.

Available `access_mode` values are `off`, `errors`, `sampled`, and `all`. Setting `max_backups: 0` keeps only the current bounded log file. Setting `console_max_lines_per_minute: 0` disables console mirroring. Logging sink/rotation changes take effect after restart; routing/probe settings remain hot-reloadable.

The in-memory event feed is independently bounded: it keeps only the newest ring of events, truncates oversized event fields, and caps dynamic event/error counter keys. It cannot grow indefinitely with uptime.

## Provider types

```text
openai_compatible
anthropic_compatible
```

A provider is generic; adding another OpenAI-compatible endpoint should normally require configuration, not a new code branch.

## Provider credentials

A provider may use a primary key plus additional credentials:

```json
{
  "api_key_env": "PROVIDER_KEY_1",
  "credentials": [
    {"name":"key-2", "api_key_env":"PROVIDER_KEY_2", "enabled":true},
    {"name":"key-3", "api_key":"literal-only-if-you-really-need-it", "enabled":true}
  ]
}
```

Usable keys rotate. Auth/quota/rate-limit failures cool an individual credential so another key can be tried.

## Provider endpoint controls

Per provider:

- `base_url`
- `chat_path`
- `messages_path`
- `models_path`
- `count_tokens_path`
- `proxy_url`
- `headers`
- `forward_headers`
- `max_concurrency`
- `stream_idle_timeout_seconds`

## Model aliases

Several deployments can share one alias:

```json
{"id":"deepseek","model":"deepseek-chat","aliases":["coding"],"enabled":true,"priority":10,"weight":1}
{"id":"qwen","model":"qwen-max","aliases":["coding"],"enabled":true,"priority":10,"weight":1}
{"id":"kimi","model":"kimi-k2","aliases":["coding"],"enabled":true,"priority":10,"weight":1}
```

A client requesting `model: "coding"` receives the best eligible deployment according to the selected strategy. `auto` and `claude-auto` intentionally match the full eligible pool.

`cost_aware` is an opt-in verified-ready strategy. It preserves configured priority tiers, requires healthy/capability-eligible candidates, and orders known-priced deployments by an estimated upper-bound request cost using the conservative prompt estimate plus the caller's explicit output-token ceiling. If the request omits an output ceiling, cost comparison is disabled for that request rather than inventing one. Deployments with unknown pricing are never treated as zero-cost. Session affinity remains valid only inside the best priority tier.

## Routing settings

Available strategies:

```text
ready_mesh
cost_aware
ready_queue
adaptive_round_robin
adaptive
priority
round_robin
least_latency
```

Important controls:

- `fallback_on_unknown_model`
- `max_attempts`
- `max_inflight_requests` — global admission limit for expensive data-plane POST requests; default `128`, range `1..10000`. Health, readiness, metrics, model listing, and Admin API remain observable when the data plane is saturated.
- `failure_threshold` (legacy/other routing strategies)
- `cooldown_seconds` (default `1800` for supervised ready-strategy recovery)
- `request_timeout_ms`
- `latency_weight`
- `failure_weight`
- `retry_backoff_ms`
- `max_retry_after_seconds`


### Ready-mesh supervisor semantics

With `routing.strategy = "ready_mesh"` (recommended/default):

- a deployment must pass a health probe before it can serve Claude traffic;
- automatic background sweeps skip a `healthy` deployment while its ready-health lease is fresh;
- successful real Claude traffic refreshes that lease, so active models are not needlessly probe-tested;
- an idle healthy deployment is micro-probed after the lease expires so cold fallbacks do not stay falsely healthy forever;
- the first eligible routed failure removes the deployment from the ready queue immediately;
- the recovery supervisor owns retry/cooldown until the deployment proves healthy again;
- changing provider Base URL, credentials, auth mode, proxy, endpoint paths, forwarded headers, or the upstream model ID invalidates the old health proof;
- display-name-only changes do not unnecessarily invalidate a working deployment;
- a temporary all-key `429` cooldown is a wait state and does not spend the five recovery attempts;
- the explicit **Probe all models** action remains available when an operator intentionally wants a full retest.

## Probe settings

Supervised ready-strategy recovery adds:

- `ready_lease_seconds` — maximum age of a healthy proof before an idle ready model is revalidated; default `300`
- `recovery_attempts` — supervisor probes after a quarantined model fails; default `5`
- `recovery_retry_ms` — delay between failed recovery probes; default `500`
- a model returns to the verified ready pool immediately on the first successful recovery probe
- after all recovery attempts fail, `routing.cooldown_seconds` is applied before the next recovery cycle



Default behavior:

- supervisor interval: 120 seconds
- ready-health lease: 300 seconds
- max output: 1 token
- timeout: 8 seconds
- concurrency: 16
- failure threshold: inherited by deployment health policy

At 100 models, the ready-health lease prevents healthy active models from being synthetic-probed on every sweep. Only new/unverified, recovery-owned, or lease-expired idle deployments need background work. Increase the lease when a provider is expensive or quota-constrained.

## Admin settings

For local-only use, the default is safest:

```json
{"bind_local_only": true, "api_key": ""}
```

The default config explicitly enables the restricted emergency path for local setup. For a deployment, set the policy intentionally. `emergency_access_enabled` controls the static Admin API key and keyless loopback compatibility identity; if the flag is absent from an older config file, loading defaults it to `false` and emits a migration warning. Explicit `false` is preserved when the config is saved. Emergency access maps only to the fixed owner identity and every use is audited. When OIDC is enabled, emergency credentials cannot bypass OIDC discovery/authentication failures.

### OIDC, sessions, and security audit store

For normal human administration, enable one trusted OIDC issuer and configure a callback URL that exactly matches the provider registration. The client secret is read from the named process environment variable, never from JSON. Example (replace host, issuer, IDs, and claim values with the provider's actual registration):

Discovery authorization, token, and JWKS endpoints must be absolute HTTPS URLs. HTTP is accepted only when the configured issuer and all affected endpoints are loopback addresses for local development; URLs with user information or fragments are rejected. A malformed discovery document or unsafe endpoint fails startup closed.

```json
{
  "admin": {
    "bind_local_only": false,
    "emergency_access_enabled": false,
    "security_store_path": "/var/lib/nexaroute/security.db",
    "session_ttl_seconds": 28800,
    "idle_timeout_seconds": 1800,
    "audit_retention_days": 365,
    "audit_max_events": 100000,
    "oidc": {
      "enabled": true,
      "issuer_url": "https://identity.example.com",
      "client_id": "nexaroute-control-plane",
      "client_secret_env": "NEXAROUTE_OIDC_CLIENT_SECRET",
      "redirect_url": "https://gateway.example.com/admin/auth/oidc/callback",
      "audience": "nexaroute-control-plane",
      "role_claim": "groups",
      "role_mappings": {
        "nexaroute-viewers": "viewer",
        "nexaroute-operators": "operator",
        "nexaroute-admins": "admin"
      }
    }
  }
}
```

When `security_store_path` is relative, it is resolved against the directory containing the config file. If omitted, the default is `security/nexaroute-security.db`, so the application creates a dedicated private `security/` child directory instead of placing the database directly beside the config. Missing parent directories are created as mode `0700`; an already existing parent that is not private is rejected and must be fixed by the operator.

Set the secret outside the config, for example with your service manager/secret injector (do not put its value in shell history):

```text
NEXAROUTE_OIDC_CLIENT_SECRET=<secret supplied by the identity provider>
```

`issuer_url`, `client_id`, `redirect_url`, `audience`, `role_claim`, and a 1–100-entry `role_mappings` allowlist are required when OIDC is enabled. HTTPS is required except HTTP loopback URLs for local development/tests. Discovery metadata's issuer must exactly match `issuer_url`. The ID token must include both the OIDC client ID and configured expected audience in `aud`, and the OIDC library validates signature, issuer, client ID, expiry, and authorized-party rules. Only a string or array of strings at `role_claim` is accepted. Every mapped value must resolve to exactly one internal role (`viewer`, `operator`, or `admin`); unknown values, ambiguous/conflicting mapped roles, malformed claims, or untrusted HTTP role headers never grant access. Registered Admin route/method pairs are checked server-side and unknown paths/methods deny by default.

The verified OIDC role permissions are:

| Role | Read | Write / privileged actions |
|---|---|---|
| `viewer` | config, providers, routing, usage | none |
| `operator` | config, providers, routing, usage | routing; evaluation; video management |
| `admin` | config, providers, routing, usage, audit | config, providers, routing; evaluation; keys; video management |

Only `admin` can read the durable audit API or manage client keys. The emergency `owner` identity is not an OIDC-mapped role and is available only through the explicit break-glass policy. These permissions are checked by the API middleware, not by frontend visibility.

The login uses Authorization Code + PKCE S256, single-use random state and nonce, and a separate HttpOnly browser-binding cookie. The fixed callback path is `/admin/auth/oidc/callback`; redirects return to `/`, not to a caller-provided `next` URL. OIDC discovery is performed at gateway startup and needs outbound access to the configured issuer; startup records an unavailable provider and all Admin access fails closed. `nexaroute config validate` checks local schema/limits but does not replace a successful runtime discovery check.

The server-side session identifier is a 32-byte random opaque value; only its SHA-256 hash and verified subject/roles/policy fingerprint are stored in the security DB. Absolute TTL and idle timeout are validated (TTL 5 minutes–30 days; idle timeout 1 minute–TTL). Session and audit writes use local bbolt transactions and survive process restart on the same host. The DB file must be mode `0600`; its parent directory must be a real, private `0700` directory. New parent directories are created with mode `0700`, but an existing unsafe parent is rejected rather than chmod'd automatically. Prepare it before service startup, for example with `install -d -m 0700 /var/lib/nexaroute`. Startup also rejects a symlink DB file. Place it on durable local storage and back it up under the same controls as other security records. Audit history is bounded by both `audit_retention_days` and `audit_max_events`; event records omit request bodies, passwords, tokens, codes, cookies, and session identifiers. If a required audit write fails, privileged actions fail with HTTP 503.

This backend is **single-host only**: do not use a network filesystem or point multiple gateway processes/hosts at the same database. Pending OIDC transactions are in memory and the callback must return to the same process that started login; use a single instance or sticky routing. Distributed sessions/audit are not implemented. Keep a separately protected recovery procedure: an OIDC outage does not silently enable break-glass. If policy permits emergency recovery, an operator with trusted filesystem/service access must explicitly disable OIDC, explicitly enable emergency access, set a strong key, and restart; restore the OIDC configuration after recovery.

For local development, the validator permits `http://localhost` or loopback IPs for the issuer and callback. Automated tests use an in-process `httptest` provider with signed RSA ID tokens and real discovery/JWKS/token endpoints; that test provider is not a runtime mode or a production identity service.

### Built-in TLS and optional mTLS

TLS is off by default. When enabled, NexaRoute serves HTTPS on the configured `listen` address and enforces TLS 1.2 or newer. Certificate, key, and CA paths may be absolute or relative to the directory containing the config file. NexaRoute validates the PEM materials at startup/config validation and reloads them for each new TLS handshake, so atomically replacing the files updates new connections without restarting. Existing connections keep their established TLS session.

```json
{
  "listen": "0.0.0.0:8443",
  "admin": { "bind_local_only": false, "api_key": "set-a-long-random-admin-key" },
  "tls": {
    "enabled": true,
    "cert_file": "tls/server.crt",
    "key_file": "tls/server.key",
    "client_ca_file": "tls/client-ca.crt",
    "require_client_cert_admin": true,
    "require_client_cert_data_plane": false
  }
}
```

`client_ca_file` is required when either client-certificate flag is enabled. Client certificates are verified against that CA during the TLS handshake; NexaRoute then requires a verified certificate for `/admin/api/*` and/or `/v1/*` according to the corresponding flag. Use a CA and certificates dedicated to the intended clients. mTLS is an additional peer check and does not replace OIDC role authorization or the explicitly configured emergency-access policy. Keep certificate/key files readable by the gateway process and protect the private key with restrictive filesystem permissions.

### Read-only configuration commands

```bash
nexaroute config validate --config /etc/nexaroute/config.json
nexaroute config diff --config /etc/nexaroute/config.json
nexaroute config diff --config /etc/nexaroute/config.json --against /etc/nexaroute/staging.json
nexaroute config dry-run --config /etc/nexaroute/config.json
```

`validate` checks the schema and configured TLS material. `diff` compares with built-in defaults unless `--against` names a second config; changed secret fields are reported only as redacted markers. `dry-run` validates and prepares provider adapters without opening a listener, probing providers, or making upstream requests. Config loading may perform a required on-disk format migration for a legacy config when such a migration is enabled; otherwise these commands do not alter runtime settings. Exit codes: **0** valid/no differences, **1** invalid config or preparation/runtime error, **2** usage error.


### Ready-mesh controls

- `session_affinity` — preserve a successful conversation/deployment relationship while it remains healthy.
- `session_ttl_seconds` — idle affinity lease; default `3600`.
- `p2c_window` — maximum number of best-priority candidates considered before the power-of-two pick; default `8`.
- `capacity_weight` — penalty for live active/waiting provider pressure; default `35`.
- The Web UI exposes all Ready Mesh controls, including session affinity/TTL, P2C window, capacity weight, capability thresholds/cooldown, global in-flight admission, and ready-health lease.
- `capability_failure_threshold` — consecutive scoped failures before a capability circuit opens; default `2`.
- `capability_cooldown_seconds` — scoped circuit cooldown; default `300`.

Cooldowns are intentionally tiered by scope:

- `routing.cooldown_seconds` — per-deployment recovery cooldown; default `1800` seconds.
- `routing.provider_cooldown_seconds` — provider-wide cooldown that blocks a whole provider; default `30` seconds.
- `decision_provider_health.cooldown_seconds` — decision-plane provider cooldown; default `60` seconds.

Each tier accepts an explicit per-configuration override within its validation
range. Provider-level values remain short because they quarantine more traffic
than a single deployment; the deployment value is longer because it controls
supervised recovery for one route target.

Provider presets are served by the gateway itself through the Admin API so the Web UI does not maintain a second hard-coded provider catalog. Custom Provider remains fully editable. `models_path` may be a normal path or an absolute URL for compatible providers whose discovery endpoint lives on a different host/path.

**Test connection** checks endpoint reachability/auth separately from **Test selected models**, which performs actual minimal model inference.

## Routing additions, cache, client auth and model fields

### routing (additions)

| Field | Default | Meaning |
|---|---|---|
| `hedging_enabled` | `false` | Race a second deployment when the first attempt is slow to response headers. First attempt only; loser abandoned with no health penalty. |
| `hedging_delay_ms` | `1500` | Delay before the hedge partner launches (50–600000). |

### cache (new object, opt-in)

| Field | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Master switch. When false, all caching behavior is skipped. |
| `ttl_seconds` | `300` | Entry lifetime (1–2592000). |
| `max_entries` | `256` | LRU entry bound (1–65536). |
| `max_body_bytes` | `1048576` | Per-response cap and total byte budget seed (1024–67108864). |

Only non-streaming requests with `temperature` absent/0 and `top_p` absent/1 are eligible. Every config swap invalidates the cache. Per-request bypass header: `x-nexaroute-no-cache: 1`.

### client_auth (new object, opt-in)

| Field | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Gate `/v1/*` endpoints with static keys. |
| `keys` | `[]` | 8–512 byte keys; presented via `Authorization: Bearer <key>` or `x-api-key`. |
| `rpm` | `0` | Per-key requests-per-minute ceiling; `0` disables the ceiling. |

### In-flight quota reservations

No new configuration switch is required. Reservations activate only for normal Chat/Messages data-plane attempts that carry NexaRoute's internal request-size estimate and only influence routing when a provider has already supplied usable quota evidence.

For each in-flight upstream leg, NexaRoute temporarily subtracts:

- one request from the latest reported request quota; and
- the conservative estimated input tokens plus an explicit output-token ceiling when available.

If a fresh `remaining-requests` or `remaining-tokens` header arrives, that resource's local reservation is released immediately because the provider has supplied newer evidence. When a provider omits a fresh remaining header, the reservation is retained until the response body is consumed or closed. This matters for long SSE streams.

The resulting **effective remaining** values feed quota pressure. This is intentionally not a hard local rate limiter: unknown or ambiguous provider semantics must not make NexaRoute reject otherwise usable last-resort capacity. Durable rolling-window RPM/TPM debt and hard throttling are separate future capabilities.

### model fields (additions per deployment)

| Field | Default | Meaning |
|---|---|---|
| `context_window` | `0` (unknown) | Advertised usable context window in tokens; requests estimated to exceed it skip this deployment. |
| `input_cost_per_mtok` | `0` | USD per million input tokens, used for estimated-spend accounting. |
| `output_cost_per_mtok` | `0` | USD per million output tokens, used for estimated-spend accounting. |

## Model intelligence scorecards and evaluation

The evaluation plane is an opt-in, admin-only observation plane. It never affects routing:
no scorecard value is read by the router, the health manager or the decision
plane (enforced by a structural import guard test). It is disabled by default
and, while disabled, accepts no runs and writes no scorecards.

### evaluation (new object, opt-in)

| Field | Default | Bounds | Meaning |
|---|---|---|---|
| `enabled` | `false` | — | Master switch. While false, `POST /admin/api/evaluation/run` returns 409 and no artifact is imported. |
| `live_enabled` | `false` | — | Second, independent switch for **live** physical-deployment evaluation (`"mode":"live"`). While false, live runs return 409 and no prompt leaves the gateway. Turning it on creates no traffic by itself: live calls happen only when an admin posts a run with `"mode":"live"` and an explicit `deployment_id`. |
| `max_runs` | `64` | 1–512 | Bounded retained evaluation runs (memory and state file). |
| `max_scorecards` | `1024` | 1–4096 | Scorecard registry bound (deployments). |
| `import_path` | `""` | ≤ 4096 bytes | Read-only scorecard artifact (JSON). Re-read on config reload; a malformed artifact is rejected as a whole and reported as `import_error`. |
| `state_path` | `""` | ≤ 4096 bytes | Optional durability file: one atomic, mode-0600 JSON document holding bounded runs and scorecards. |
| `max_artifacts` | `128` | 1–512 | Per-run artifact bound. |
| `latency_target_ms` | `0` | 0–600000 | Optional latency scoring target. A latency value is only produced when a target exists. |
| `ttft_target_ms` | `0` | 0–600000 | Optional TTFT scoring target, same rule. |

### Evaluation modes

`POST /admin/api/evaluation/run` accepts an explicit `mode`:

- `"mode":"replay"` (default) — grades recorded `artifacts[]`. Performs **no**
  upstream I/O.
- `"mode":"live"` — sends `prompts[]` to exactly one explicitly selected
  physical deployment (`deployment_id`), one request per prompted case. Requires
  `evaluation.enabled` **and** `evaluation.live_enabled`. It bypasses every
  DecisionProvider and cannot change production health, affinity, cache, quota
  or routing state. Prompts are inputs only: they are never written to run
  records, scorecards, events, metrics or logs.

```json
"evaluation": {
  "enabled": true,
  "live_enabled": false,
  "max_runs": 64,
  "max_scorecards": 1024,
  "import_path": "/etc/nexaroute/scorecards.json",
  "state_path": "/var/lib/nexaroute/evaluation-state.json",
  "max_artifacts": 128,
  "latency_target_ms": 2000,
  "ttft_target_ms": 800
}
```

### Scorecard artifact shape (`import_path`)

```json
{"scorecards": [
  {"deployment_id": "chat2api/deepseek", "provider_id": "chat2api", "model": "deepseek-chat",
   "version": 1, "generated_at": "2026-09-26T12:00:00Z",
   "values": {"coding": {"score": 0.8, "provenance": "imported",
                          "sample_count": 12, "confidence": 0.375, "source": "vendor-bench"}}}
]}
```

Every value must carry a provenance (`operator_config`, `imported`,
`evaluation`, `production_telemetry`). Unknown fields, duplicate deployments,
missing `generated_at`, missing provenance and missing samples for measured
provenances reject the whole artifact. Values without evidence are never
invented: a deployment with no evidence simply has no scorecard.

### Evaluation runs

`POST /admin/api/evaluation/run` replays **recorded artifacts** (never prompts)
through deterministic suites:

```json
{"suite_id": "coding", "deployment_id": "chat2api/deepseek",
 "artifacts": [{"case_id": "coding-bugfix", "status": "ok",
                "unit_tests": {"compiled": true, "passed": 3}}],
 "latency_target_ms": 2000}
```

Unknown fields, unknown suites, unknown deployments, mismatched
`provider_id`/`model`, missing artifacts and oversized payloads are rejected. A
run that produced too little evidence to meet the suite's minimum sample count
stores the run but writes **no** scorecard (`scorecard_written: false`). Model
outputs never appear in events, metrics or the admin snapshot.

## Optional asynchronous video gateway

The video gateway is disabled by default. When enabled, `auth_token_env` must
name a non-empty bearer-token environment variable in the gateway process; the
runtime refuses to start otherwise. Keep the token in the deployment's secret
injector, not in a literal config value. The example configuration is
[`configs/video.example.json`](../configs/video.example.json).

```json
"video": {
  "enabled": false,
  "store_path": "video-jobs.json",
  "storage_root": "video-data",
  "max_asset_bytes": 536870912,
  "queue_size": 32,
  "workers": 2,
  "auth_token_env": "NEXAROUTE_VIDEO_TOKEN",
  "development_fake_provider": false
}
```

`store_path` and `storage_root` may be absolute or relative to the gateway's
runtime base directory. The JSON store, queue and cost ledger are single-process;
the local asset sink enforces a per-file limit (default 512 MiB, configurable
from 1 byte through 8 GiB). Startup recovers queued jobs and resumes jobs with
known provider job IDs; ambiguous submissions without a provider job ID require
manual action instead of automatic resubmission. The fake provider is for tests
and local development only, writes a text placeholder, and is not a real video
provider. See [Video Gateway](VIDEO_GATEWAY.md) and [Known Gaps](KNOWN_GAPS.md).
