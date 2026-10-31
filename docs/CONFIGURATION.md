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
NEXAROUTE_KEY_PROVIDER            file|env|command|kms|keyring|transit
NEXAROUTE_KEY_PROVIDER_ENV        environment variable name for the env provider
NEXAROUTE_KEY_PROVIDER_COMMAND    helper executable for the command/kms provider
NEXAROUTE_KEY_PROVIDER_REF        reference passed to the command/keyring provider
NEXAROUTE_TRANSIT_ENABLED         must be exactly "true" to enable Transit mode (with NEXAROUTE_KEY_PROVIDER=transit)
VAULT_ADDR                        Vault address for Transit (https, or http on loopback only)
VAULT_TOKEN                       Vault token for Transit (never written to config)
VAULT_TOKEN_FILE                  file holding the Vault token, mode exactly 0600 (xor VAULT_TOKEN)
NEXAROUTE_TRANSIT_MOUNT           Transit mount, required, no default (multi-segment allowed)
NEXAROUTE_TRANSIT_KEY             Transit key name, required, no default
VAULT_NAMESPACE                   optional Vault namespace, sent as X-Vault-Namespace
NEXAROUTE_LEGACY_MASTER_KEY       base64 legacy nxs1 key for migrate-to-transit (or _FILE)
NEXAROUTE_LEGACY_MASTER_KEY_FILE  legacy nxs1 key file with mode 0600 for migrate-to-transit
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

### Vault Transit envelope mode (`nxs2`)

`NEXAROUTE_KEY_PROVIDER=transit` with `NEXAROUTE_TRANSIT_ENABLED=true` is an explicit opt-in that wraps each
per-secret data key through Vault Transit instead of loading a raw master key
into the gateway. The Transit KEK stays in Vault; only the short-lived DEK is
returned to the gateway. Configure:

```bash
export NEXAROUTE_KEY_PROVIDER=transit
export NEXAROUTE_TRANSIT_ENABLED=true
export VAULT_ADDR=https://vault.example:8200
export VAULT_TOKEN=...          # inject from a secret manager
export NEXAROUTE_TRANSIT_MOUNT=transit
export NEXAROUTE_TRANSIT_KEY=nexaroute
```

Vault policy needs `encrypt`, `decrypt`, and `rewrap` on the configured key.
There is no fallback to `<config>.key`, `NEXAROUTE_MASTER_KEY`, or a raw-key
provider; combining Transit mode with any raw-key source is a configuration
error. A missing token, denied policy, timeout, wrong key reference, or
tampered envelope fails closed. Existing `nxs1` configs are rejected in Transit
mode and must be migrated explicitly:

```bash
# The legacy key is the exact nxs1 master key; it is never stored in the config.
# It is demanded only when nxs1 is actually present.
nexaroute secrets migrate-to-transit --config "$CONFIG" --legacy-key-file /secure/old.key
nexaroute secrets verify --config "$CONFIG"
```

Migration proves Transit connectivity with a canary, writes a verbatim
timestamped `.pre-transit.bak` copy of the pre-migration bytes (mode `0600`,
SHA-256 reported), decrypt-verifies in memory, and atomically replaces the
config; a post-write verification failure rolls back to the backup. An
already-migrated config is verified only: nothing is written and no backup is
created. Plaintext fields are never auto-migrated: they fail with an
explicit-migration message. To move wrapped data
keys to the latest Vault key version, run:

```bash
nexaroute secrets rewrap-transit --config "$CONFIG"
nexaroute secrets verify --config "$CONFIG"
```

`rewrap-transit` decrypt-verifies before and after, calls only the Transit
`rewrap` endpoint, writes a verbatim `.pre-rewrap.bak`, and writes nothing
when already at the latest version. Both commands accept `--dry-run`.
`secrets rotate` refuses Transit mode. `secrets status` reports `nxs1_fields`
and `nxs2_fields` separately plus `transit_key_versions`. Saves in Transit
mode preserve unchanged `nxs2` envelopes byte-for-byte and seal only genuinely
new or changed plaintext fields. Backups of `nxs2` configs record
`envelope_versions`, `transit_key_ref_sha256`, and `transit_key_versions`,
reject `--include-master-key`, and can only be restored where Vault Transit is
available.

`rewrap-transit` writes a verbatim `.pre-rewrap.bak` backup and replaces the
config atomically. Do
not disable an old Transit key version until rewrap and verification succeed.
Transit mode is fake-API tested in CI but has **no real Vault Transit acceptance
evidence**; see [EXTERNAL_VERIFICATION.md](EXTERNAL_VERIFICATION.md). Do not claim
production/HSM verification for a deployment until that run is recorded.

## Optional shared control plane

The shared control plane is disabled by default. It carries runtime metadata
and budget-alert marks, not configuration: the control-plane store never stores
or reconciles the config file. Config remains per-instance even when the
control plane is enabled, and neither admin saves nor CLI edits propagate to
another replica. Distribute the config to every replica through deployment
tooling. The decision record and the unimplemented options are in
[ADR 0001](adr/0001-shared-config-snapshot.md).

When enabled, connection strings are read only from environment variables
named by the durable config; the connection strings themselves are never
written to JSON:

```json
{
  "control_plane": {
    "enabled": true,
    "backend": "file",
    "state_path": "controlplane/state.json",
    "migration_dry_run": false,
    "postgres_dsn_env": "NEXAROUTE_PG_DSN",
    "redis_url_env": "NEXAROUTE_REDIS_URL",
    "config_failure": "last_known_good",
    "identity_failure": "fail_closed",
    "budget_failure": "fail_closed",
    "rate_limit_failure": "fail_closed"
  }
}
```

`backend` defaults to `file` and may be `file`, `memory`, `postgres`, or
`redis`. File state is written atomically with mode `0600`; relative
`state_path` values resolve against the config directory. `memory` is for
tests only. DSNs and URLs are never stored or logged. On Linux the file
backend additionally serializes read-modify-write cycles across processes
with an advisory lock file (`.controlplane.lock`) in the state directory, so
two gateway processes may share one `state_path`: lock acquisition waits at
most 30 seconds and then fails closed with a typed timeout error (never an
indefinite hang, never an unlocked proceed), and concurrent writers observe
optimistic-revision `ErrConflict` instead of silently losing updates.
`BackupFileStore` writes backups atomically (temp + fsync + rename, `0600`)
like the store itself. On platforms without `flock` support, file-backend
writes fail closed with an explicit error while reads stay available.

### Cross-replica breaker synchronization (opt-in)

With the control plane enabled, breaker/cooldown markers can optionally be
shared across replicas (ADR 0003). Three keys under `control_plane` control
it:

```json
{
  "control_plane": {
    "breaker_sync": false,
    "breaker_sync_interval_seconds": 10,
    "breaker_sync_replica_id": ""
  }
}
```

- `breaker_sync` (bool, default `false`) — the opt-in. Setting it without
  `control_plane.enabled` is a validation error, so the request is never
  silently ignored. With it unset, no sync goroutine starts and no store
  traffic exists; breaker state stays per-instance.
- `breaker_sync_interval_seconds` (int, default `10`; `0` selects the
  default; validated to 5..3600 when set) — the publish/consume cadence.
  Replicas converge on a provider incident within two intervals plus store
  latency.
- `breaker_sync_replica_id` (string, optional) — this replica's identity in
  the shared marker document; defaults to a generated `hostname-pid`
  identity at startup.

Only markers are shared — active cooldowns and retirements — never failure
counters or probe state. A remote `retired` marker is applied locally only
as a bounded cooldown (at most 30 minutes, re-extended while the origin
keeps publishing); it never retires a local deployment. A store outage
never clears local breaker state: sync pauses and resumes on heal, and the
fleet degrades to ordinary per-instance behavior.

## Optional OTLP span export

Span export is disabled by default. When `telemetry.otlp_endpoint_env`
names an environment variable holding the full OTLP/HTTP traces URL
(the same env-name-only pattern as the control-plane backends), the
gateway exports one span per data-plane LLM request
(`/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions`,
`/v1/responses`) with GenAI semantic attributes. An invalid URL fails
startup. Spans never contain prompts, completions, keys or client
identities. See [OBSERVABILITY.md](OBSERVABILITY.md).

```json
{
  "telemetry": {
    "otlp_endpoint_env": "NEXAROUTE_OTLP_ENDPOINT",
    "service_name": "nexaroute"
  }
}
```

`postgres` is fully wired (pgx driver): the DSN is read only from the
environment variable named by `postgres_dsn_env`; startup fails closed
with a secret-safe error when the variable is missing or the database is
unreachable, and never falls back to another backend. The connection
pool is bounded (10 open / 5 idle / 30-minute lifetime), startup pings
with a 10-second timeout, and the versioned control-plane migrations
(checksum-verified, advisory-locked against concurrent replicas) are
applied on startup. `redis` is also fully wired (go-redis client): the
URL is read only from the environment variable named by `redis_url_env`,
connection timeouts/retries and the pool are bounded, and startup performs a
10-second `PING`. Startup fails closed with a credential-safe error and never
falls back to another backend. Snapshots are stored as per-namespace hashes
guarded by atomic compare-and-set scripts. Revision comparisons and increments
use exact decimal strings, so the full `uint64` range is not rounded by Lua
numbers; malformed records fail deterministically, stale writes/deletes return
`ErrConflict`, and deleting a missing snapshot returns `ErrNotFound`.
`migration_dry_run` validates backend selection and readiness without reading or
writing snapshot state (Redis does only the bounded `PING`) or applying
migrations.

**Redis durability warning:** this backend is only as durable as the operator's
Redis configuration. For durable state, explicitly configure and test AOF
(`appendonly yes`) and/or RDB snapshots, storage durability, replication,
failover, and backup/restore against the deployment's recovery objectives. A
default ephemeral Redis loses control-plane state on restart. Atomic revision
checks apply on the Redis server executing the script; they do not imply
multi-primary consistency, cross-replica safety, or durability beyond Redis's
configured guarantees. Failure modes are `fail_open`, `fail_closed`, and
`last_known_good`.

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
openai_compatible     OpenAI wire format, bearer auth, {base_url}/v1/chat/completions
anthropic_compatible  Anthropic wire format, x-api-key auth
openai_responses      OpenAI Responses API
gemini                Google Gemini wire format, x-goog-api-key auth
azure_openai          Azure OpenAI (M5-1a)
vertex                Google Vertex AI (M5-1a)
bedrock               Amazon Bedrock Converse API, SigV4 auth (M5-1b)
opencode-console      OpenCode Console gateway, bearer <redacted>, ingress-selected /openai|/anthropic paths
opencode-go           OpenCode Go gateway, bearer <redacted>, ingress-selected paths + derived x-opencode-session
```

(When the M5-1a PR also lands, this list gains `azure_openai` and `vertex`.)

A provider is generic; adding another OpenAI-compatible endpoint should normally require configuration, not a new code branch.

**Azure OpenAI** (`azure_openai`) speaks the OpenAI wire format but addresses deployments in the URL: requests go to `{base_url}/openai/deployments/{deployment}/chat/completions?api-version={api_version}`, where `{deployment}` is the model's upstream `model` value and `api_version` is a provider field (default `2024-10-21`). Authentication is the `api-key` header (the default `auth_mode` for this type) using the provider's configured credential.

**Vertex AI** (`vertex`) speaks the Gemini wire format at `{base_url}/v1/projects/{vertex_project}/locations/{vertex_location}/publishers/google/models/{model}:generateContent` (or `:streamGenerateContent?alt=sse` when streaming). `vertex_project` is required; `vertex_location` defaults to `us-central1`. Authentication is a Bearer OAuth2 access token supplied as the provider credential. **Token minting and refresh are not built in**: the gateway uses the credential it is given, so supply a token through your platform's broker/sidecar (or a short-lived token via `api_key_env`) and rotate it externally — Google service-account ADC flows are an operator-side concern, and an expired token simply fails upstream calls until replaced.

**Amazon Bedrock** (`bedrock`, M5-1b) speaks the Converse API: requests are addressed per model at `{base_url}/model/{modelId}/converse` (or `/converse-stream` when streaming; the AWS binary event stream is translated by the gateway). Authentication is AWS SigV4, signed per request:

- the access key ID comes from the provider's normal key fields (`api_key` or `api_key_env`);
- the secret access key comes **only** from the environment variable named by the provider field `secret_key_env` (never a config value); adapter startup fails closed when it is unset or empty;
- an optional session token comes from the env var named by `session_token_env`;
- `aws_region` selects the signing region (default `us-east-1`) and should match the region in `base_url`.

Documented limits: one credential pair per Bedrock provider (the credential-rotation pool does not apply); `tool_choice: "none"` withholds the tools because Converse has no none mode; reasoning/thinking blocks are not encoded to Converse (model-specific); document blocks are not mapped. Prompt-cache markers (`cache_control`) on system and message content (text and tool_use/tool_result blocks) are emitted as Converse `cachePoint` checkpoints (`{"type": "default"}`, plus `"ttl": "1h"` for 1h markers) on both `/converse` and `/converse-stream`; at most 4 checkpoints per request (excess markers are dropped, content never is); per-checkpoint minimum tokens are enforced server-side by Bedrock (below-threshold prefixes still return 200, just uncached); `ttl: 1h` requires a model that supports it. No per-tool-definition checkpoint is emitted (the IR carries no such marker).

**OpenCode upstream** (`opencode-console`, `opencode-go`) speaks three protocols behind one gateway base, selected by the *client ingress*, never by model id: OpenAI chat ingress → `{base}/openai/v1/chat/completions`, Responses ingress → `{base}/openai/v1/responses`, Anthropic ingress → `{base}/anthropic/v1/messages`. Console base is `https://opencode.ai/inference` (pay-as-you-go); Go base is `https://opencode.ai/inference/go` (subscription). Authentication is Bearer. Console documents a models listing at `{base}/v1/models`, so Console supports discovery; Go documents no listing endpoint, so Go discovery is unsupported and models are saved manually — the gateway never assumes a Go listing URL. There is no Zen profile: no Zen URL appears in the documented Console/Go material, so none is shipped. All opencode requests run through the canonical-IR path (never legacy passthrough), so usage, tool calls, and error envelopes follow the canonical contract; see `docs/COMPATIBILITY.md` for the fixture-verified dimensions (live opencode.ai behavior is UNVERIFIED).

Go requests carry a derived `x-opencode-session` header; Console requests do not get a gateway-derived header (Console operators who want client values forwarded may list the header in `forward_headers`). The Go value is `ses_` + the first 32 hex chars of SHA-256 over the gateway session key plus a salt (`opencode_session_salt`, a non-secret operator-configurable domain separator; empty means the documented default `nexaroute-opencode-session-v1`). A client-supplied `x-opencode-session` (via `forward_headers`) is forwarded unchanged and never overwritten. With no session key, the fallback is deterministic over provider id + model + salt. The header is never derived from an API key, email, prompt text, or user id, never uses the per-request id as a session, and is never logged. Separately, credential-level session affinity pins one conversation's requests to one credential (by session hash, bounded, 1h TTL) while that credential stays healthy and eligible, falling back to normal selection otherwise; it is distinct from router deployment pinning and from the header value itself.

A 403 from an opencode provider is classified as a model-entitlement denial, not a credential failure: the credential is not cooled, no other credential is tried for the same restricted model in the same request, and no deployment failover or quarantine follows. The client receives a diagnostic naming the restriction and the documented next step (use a paid model or a Go subscription, or pick a model the plan includes). The classification is status-based — no distinguishing error body is documented — so a genuinely invalid opencode key returning 403 surfaces the same diagnostic on that request; the next request re-evaluates independently and other models on the same key are not poisoned. Generic providers are unaffected: their 403 still cools the credential exactly as before.

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

Credential selection, in order, inside the adapter: (1) eligibility — enabled, not cooling (cooldown expiry restores a credential with no operator action; cooldown never flips `enabled` and never deletes an entry); (2) the minimum `priority` tier among available credentials (`priority` is an integer, default 0); (3) the credential-level session-affinity pin, only while that credential stays healthy and eligible; (4) the least-loaded tournament with effective load `Active/weight` (`weight` defaults to 1 when 0; a lower effective load wins, then fewer failures, then never-failed). With every priority 0 and weight 1, selection is exactly the legacy tournament. Capacity comes only from the configured provider `max_concurrency` and in-flight counts — there is no per-key soft capacity constant, and spreading load does not make quotas infinite: if total demand exceeds the sum of authorized quotas, rate limits will still be hit.

Each pool entry may also carry `email` (metadata only — authentication never uses it; admin surfaces show it masked, never in full, and it never appears in logs, traces, or credential-state output). Per-credential live state (index, name, tier, weight, last status, failures, in-flight count, cooling) is exposed redacted on the provider detail response when a live adapter exists, and the provider-test, discovery, and connection-test endpoints accept `credential_index` to test exactly one pool entry (out-of-range or disabled is a 400). Replacing the primary key with `replace_key` and an empty-array
`credentials` placeholder (what the UI sends when only the primary
key changes) preserves the pool; a nil/omitted credentials list
clears it and a non-empty list replaces it, per the tested
whole-set replacement semantics. Clearing the pool requires an explicit `credential_pool` replace with empty entries. Two related
contracts, regression-tested at the admin API: (1) `secret_source`
on the provider detail response reports the PRIMARY credential's
source (`literal` / `env` / `pool` / `none`), not whether a
supplementary pool exists — so a primary replacement that preserves
a pool still reports `literal`, and a pool-only provider reports
`pool`; (2) an explicit clear (`replace_key` with no primary
credential at all, the UI's clear-secret checkbox) clears the
primary AND the pool, so nothing remains configured — the
pool-preservation placeholder never applies to a clear. The admin UI renders per-credential state and a per-row Test button. A best-effort audit event is recorded per provider mutation naming the provider id and pool indices changed, never values.

Honest bounds (not claims): there is no "never hit a rate limit" or "infinite speed" guarantee — latency is bounded below by upstream plus network plus tokens. Streams are not passed through zero-copy wholesale: usage accounting, protocol translation, tool-call handling, and redaction still run on these paths. Rolling latency routing, token-aware balancing, and cross-instance credential state are not shipped (deferred/future work). Compatibility is per provider, model, protocol, and feature in `docs/COMPATIBILITY.md` — never "all models/providers".

## Global egress policy

The top-level `egress` section lets operators restrict which hosts and networks
providers may connect to, beyond the built-in special-purpose IP blocklist.
Deny lists take precedence over everything else.

```json
{
  "egress": {
    "deny_hosts": ["internal.example.com"],
    "deny_cidrs": ["10.0.0.0/8", "192.168.1.50"],
    "allow_hosts": ["trusted-partner.example.com"],
    "allow_cidrs": ["203.0.113.0/24"]
  }
}
```

- `deny_hosts` — hostnames (exact, case-insensitive) that providers may never connect to.
- `deny_cidrs` — IP addresses or CIDR ranges that providers may never connect to. Checked at URL validation and again for every DNS-resolved address at dial time.
- `allow_hosts` / `allow_cidrs` — additional hosts/networks added to the per-provider allow-list.

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
- `max_concurrency` — per-provider in-flight request cap. Default `256` (range `1..4096`). This limits the number of concurrent upstream requests to a single provider. For most providers the default is appropriate. For free or rate-limited providers, set an explicit lower value (for example `32` or `64`) to avoid triggering upstream quota exhaustion. When omitted or set to `0`, the default applies. Explicit values in the configuration are never changed.
- `stream_idle_timeout_seconds`
- `allowed_hosts` — the complete list of hosts this provider may be
  dialed at, used for absolute-URL endpoint overrides and enforced
  again at dial time by the egress transport. **Setting it replaces
  the default**: when `allowed_hosts` is empty, only the `base_url`
  host is allowed; when it is non-empty, only the listed hosts are
  allowed — include the `base_url` host in the list if the provider
  must keep using it, or every ordinary request will be refused as
  not allow-listed. (The dial-time transport additionally permits
  the `proxy_url` host, since it dials the proxy hop; request
  building validates the final URL against `allowed_hosts` only.)

## Provider data-handling class and route/key privacy requirement

Providers may declare a descriptive data-handling class (config, validation,
and admin read surface only; it does not change routing):

```json
"data_handling": {
  "trains_on_data": "unknown",
  "retention": "unknown",
  "note": ""
}
```

- `trains_on_data`: `yes` | `no` | `unknown` (empty normalizes to `unknown`).
- `retention`: `none` | `limited` | `unknown` (empty normalizes to `unknown`).
- `note`: at most 200 characters, no control characters.
- `data_handling` is exposed on provider list/detail Admin API responses; it
  is not a secret. Secrets stay write-only.

Routes and keys may express a privacy requirement:

- `route_profiles[].privacy`: `""` or `any` (no requirement) | `no_training`.
- `client_auth.virtual_keys[].require_privacy`: same values.

Route profiles can roll out a canary deployment (M5-3):

- `route_profiles[].canary_deployment_id`: a deployment (`provider/model`)
  from the profile's candidate pool.
- `route_profiles[].canary_percent`: 0–100. Sessions are bucketed
  deterministically by their stable key (session key, else selection
  key): the same session always lands in the same arm. A bucketed
  session gets the canary promoted to the front of its candidate
  list — but only when the canary is already an eligible candidate
  for that request (capabilities, health and the privacy gate all
  apply first; rollout reorders eligibility, it never creates it).
  Keyless requests are never rolled out. When the profile also
  names a decision policy, the policy's selection still governs
  the final pick.

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
priority
round_robin
least_latency
```

(`adaptive` was removed: it was listed but never implemented — the router
treated it identically to the default ordering. Configs using it now fail
validation with a clear error instead of silently degrading.)

Important controls:

- `fallback_on_unknown_model`
- `max_attempts`
- `max_inflight_requests` — global admission limit for expensive data-plane POST requests; default `128`, range `1..10000`. Health, readiness, metrics, model listing, and Admin API remain observable when the data plane is saturated.
- `failure_threshold` (legacy/other routing strategies)
- `cooldown_seconds` (default `1800`) — request-driven circuit-breaker cooldown. Recovery cooldown is configured separately via `probe.recovery_cooldown_seconds` (default `900`).
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

### Autopilot policy canary (`autopilot`, opt-in)

The autopilot is a supported, opt-in lifecycle for changing one routing
policy: a candidate policy version is staged, served a bounded share of
real traffic, and judged against a gate built from observed signals
(error rates, failover rate, no-eligible rate, budget anomalies, latency
and TTFT) compared with the incumbent over the same window. Missing or
thin evidence always means *hold* — a candidate is never promoted on
assumption. A gate violation rolls back to the incumbent atomically and
records a visible incident; a lease already issued is never switched, so
an in-flight request finishes under the policy it started with.

- `enabled` — turns the controller on. With it unset, no autopilot state
  exists and routing is exactly the configured policies.
- `policy_id` — the decision policy under change (defaults to
  `decision.policy` when empty). Must reference an existing policy.
- `candidate_policy_id` — the `decision_policies[]` entry staged as the
  candidate at boot. Empty means the operator starts the canary
  explicitly through the admin API (`/admin/api/autopilot`).
- `canary_percent` (0–100) and `canary_max_requests` (0 = unbounded by
  count) bound the candidate's traffic share.
- Gate thresholds: `min_samples` (no verdict other than hold below
  this), `min_promote_samples`, `max_error_rate`,
  `max_error_rate_delta` (candidate-minus-incumbent regression margin),
  `max_failover_rate`, `max_no_eligible_rate`, `max_budget_anomalies`,
  and the optional latency ceilings `max_latency_p95_ms` /
  `max_ttft_p95_ms` (0 disables). Promotion additionally requires
  complete signal coverage: a request whose no-eligible or
  budget-anomaly observation is missing counts as *unobserved*, never
  as a pass.

The whole surface is validated at config load; an unknown policy
reference or an out-of-bounds value fails startup rather than degrading
silently.

## Probe settings

Supervised ready-strategy recovery adds:

- `ready_lease_seconds` — maximum age of a healthy proof before an idle ready model is revalidated; default `300`
- `recovery_attempts` — supervisor probes per recovery cycle after a quarantined model fails; default `5`
- `recovery_retry_ms` — delay between recovery probes in a cycle; default `500`
- `recovery_success_threshold` — successful probes required within the cycle before re-admission; default `3`. Set it equal to `recovery_attempts` for strict N-of-N recovery (for example `5` for strict 5-of-5). Values above `recovery_attempts` are clamped down at load so an upgrade never blocks startup.
- `recovery_cooldown_seconds` — cooldown applied after a cycle fails to reach the threshold, before the next automatic cycle; default `900` (15 minutes). This is independent of `routing.cooldown_seconds` (default `1800`), which still governs request-driven circuit breaking.
- a model returns to the verified ready pool once it reaches `recovery_success_threshold` successful probes in the cycle (the previous first-success behavior is available by setting the threshold to `1`)
- after a cycle fails to reach the threshold, `recovery_cooldown_seconds` is applied before the next automatic recovery cycle

Failure evidence from probes is classified (`credential_failure`, `quota_exhausted`, `rate_limit`, `provider_incident`, `transient_timeout`, `transient_transport`) and surfaced on health events (`error_type`) and deployment state (`last_error_class`). A tiny text probe never asserts tool, vision, or reasoning support; capability conformance remains a separate `capability_probes` concern.



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

`security_store_backend` selects the admin security-store backend: `bbolt` (the default; local file at `security_store_path`, zero external services), `postgres` or `redis` (M4-2; sessions and the audit chain in the shared service, safe to share between gateway processes — the Redis adapter commits through version-checked Lua scripts, the PostgreSQL adapter through serialized transactions). Selecting `postgres`/`redis` requires `security_store_url`. `security_store_url` may be a literal connection string or `env:VARIABLE_NAME` to read it from the environment — the env form is recommended so credentials stay out of durable config. A configured backend that cannot be reached **fails closed at startup**: the gateway never silently substitutes a different store. Only admin sessions and the audit chain live in this store; rate-limit buckets, provider health, caches and quota reservations are process-local by design (see `specs/019-m4-1-storage-backend/`).

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
    "require_client_cert_data_plane": false,
    "trusted_proxy_cidrs": ["127.0.0.1/32"]
  }
}
```

`client_ca_file` is required when either client-certificate flag is enabled. Client certificates are verified against that CA during the TLS handshake; NexaRoute then requires a verified certificate for `/admin/api/*` and/or `/v1/*` according to the corresponding flag. Use a CA and certificates dedicated to the intended clients. mTLS is an additional peer check and does not replace OIDC role authorization or the explicitly configured emergency-access policy. Keep certificate/key files readable by the gateway process and protect the private key with restrictive filesystem permissions.

`trusted_proxy_cidrs` is optional and empty by default. It is only for a TLS-terminating reverse proxy that reaches NexaRoute over plain HTTP. When the TCP peer address matches one of these IP/CIDR entries, a single unambiguous `X-Forwarded-Proto: http|https` value may describe the original client scheme. That scheme then controls the same-origin `http`/`https` comparison for browser Admin requests; Admin session/CSRF/OIDC cookies always carry `Secure` regardless of transport, so a browser dashboard session requires HTTPS (direct, or via this proxy) or a loopback origin, which browsers treat as a secure context. Direct clients, peers outside the list, chained values such as `https, http`, duplicated header lines, and the `Forwarded` header are always ignored. List only proxy addresses you control (for example a loopback or private load-balancer network); `0.0.0.0/0` and `::/0` are rejected. The gateway never trusts a forwarding header for authentication or authorization. When NexaRoute terminates TLS itself, this setting is unnecessary.

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

For real-tenant onboarding evidence, use the bounded `nexaroute provider-accept` command described in [Provider onboarding live acceptance](PROVIDER_ACCEPTANCE.md). It never saves provider/model changes or credentials and requires `--confirm-live`; public CI runs only loopback fake upstreams.

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

### Semantic cache (`cache.semantic`, M5-4)

A separately opt-in layer beside the exact-match cache. Prompts are
embedded through a configured deployment and matched by cosine
similarity, so paraphrases of a cached prompt can be served
without an upstream call.

- `cache.semantic.enabled`: default off; independent of `cache.enabled`.
- `cache.semantic.embedding_deployment`: `provider/model`; the provider
  must be `openai_compatible`. Embeddings go straight through that
  provider's adapter (never through the gateway's own handlers), and
  any embedder failure bypasses the cache silently.
- `cache.semantic.embedding_path`: default `/v1/embeddings`.
- `cache.semantic.threshold`: cosine in (0.5, 1]; default 0.95.
- `cache.semantic.max_entries`: default 1024 (own LRU + byte bounds).
- `cache.semantic.ttl_seconds`: default reuses `cache.ttl_seconds`.

Eligibility mirrors the exact cache and adds: no tools, prompt text
extractable and ≤ 32 KiB. Similarity is computed only inside an
isolation scope (client-key digest + ingress path + model +
forwarded-header context), so one client's entries never serve
another. Every entry carries privacy provenance — origin
deployment, origin provider's `trains_on_data`, whether the storing
request required `no_training`, the embedding deployment — and a
`no_training` request can only be served by an entry stored under
`no_training` whose origin declares `trains_on_data: "no"`. Hits are
observable: `X-NexaRoute-Cache: SEMANTIC-HIT`,
`X-Gateway-Cache-Provenance`, a `semantic_cache_hit` event, and
`nexaroute_semantic_cache_*` metrics.
### guardrails (object, opt-in)

Request-content guardrails, evaluated inside client authentication
(so they are active when `client_auth.enabled` is true) on every
data-plane request:

| Field | Default | Meaning |
|---|---|---|
| `guardrails.mode` | `off` | `off` (no scanning), `audit` (scan; on a match, emit a `guardrail_audit` event naming the matched kinds and serve the request), or `block` (scan; reject matched classes with 403). |
| `guardrails.reject_pii` | `false` | In block mode, reject PII classes: `pii_email`, `pii_phone`, `pii_credit_card` (Luhn-validated), `pii_ssn` (SSA structure-validated). |
| `guardrails.reject_secrets` | `false` | In block mode, reject `secret_api_key`: sk-/AKIA/GitHub (classic + fine-grained)/Slack/Google key shapes, JWTs, PEM private-key blocks, connection strings with embedded passwords, and Bearer tokens. |
| `guardrails.reject_prompt_injection` | `false` | In block mode, reject `prompt_injection` phrasings (ignore/disregard previous instructions, reveal/print the system prompt or instructions, jailbreak/developer mode, override safety, do anything now). |
| `guardrails.max_body_bytes` | gateway default | Body size cap for scanning; larger requests fail 413. |

Findings are classifications only — matched content is never
logged, evented, or returned. Detection is pattern + validator
based (no ML model); expect it to catch common shapes, not
adversarial obfuscation.

### client_auth (new object, opt-in)

| Field | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Gate `/v1/*` endpoints with static keys. |
| `keys` | `[]` | Transitional plaintext input (8–512 byte keys; presented via `Authorization: Bearer <key>` or `x-api-key`). Converted to `key_hashes` on load and before every save — plaintext is never persisted. Prefer `virtual_keys` for new clients. |
| `key_hashes` | `[]` | SHA-256 hex digests of legacy client keys (M3-8). This is the stored form; a listed digest authenticates the corresponding key exactly like a virtual key's `key_hash`. |
| `rpm` | `0` | Per-key requests-per-minute ceiling; `0` disables the ceiling. |
| `tpm` | `0` | Per-client tokens-per-minute ceiling for legacy clients (a virtual key's own `tpm` overrides it); `0` disables the ceiling. Admission reserves a conservative estimate (input bytes/4 + requested max output tokens); when the provider reports actual usage, the reservation is settled to the actual total. Requests that never report usage keep the estimate as their charge. A request whose estimate exceeds the entire bucket is rejected without `Retry-After`; otherwise a denial returns a calculated refill delay. Quota state is per-process in-memory (see SECURITY.md). |

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

### Versioned price book (`pricing.versions`, M5-6)

A versioned price book overrides model prices for both cost-aware
routing and spend accounting. Costs are computed **when usage is
recorded**, under the version active at that moment — a later
price change never reprices recorded costs, and the usage snapshot
attributes spend per version (`cost_by_version_usd`). Usage without
a recorded cost is estimated from model-config prices (see the
fallback below), and those estimates can change when the fallback
rates change.

```yaml
pricing:
  versions:
    - version: "2026-q4"
      effective_from: "2026-10-01T00:00:00Z"
      prices:
        "provider/model": { input_per_mtok: 3, output_per_mtok: 15, cache_read_per_mtok: 0.3, cache_write_per_mtok: 3.75 }
```

- The active version for a deployment is the latest
  `effective_from <= now` **that prices that deployment**; a
  deployment missing from the book falls back to its model-config
  prices (version label `config`), then to zero cost (label
  `unpriced`; tokens are still counted).
- `cache_read_per_mtok` / `cache_write_per_mtok` are optional; when
  omitted, the input rate applies.
- Cache billing conventions follow the providers: Anthropic's
  `input_tokens` exclude cache tokens (priced additively);
  OpenAI/Gemini counts include the cached share in the input
  count, so the cached share is moved to the cache-read rate —
  never double-charged.

### Virtual-key budgets (`client_auth.virtual_keys[].budget_usd`, M5-6)

A cumulative USD spend cap per virtual key (`0` = unlimited). At
admission the gateway atomically reserves a conservative estimate
(priced against the most expensive deployment serving the model:
body-derived input estimate + the request's output allowance, or
4096 tokens when unset). The first usage report settles the
reservation to the **actual** recorded cost; cache hits release
the hold (no upstream spend). A request is rejected with
`402 client budget exceeded` when committed + reserved + estimate
would exceed the budget. Reservations that never settle (e.g. a
transport failure with no usage report) are committed at their
estimate after 15 minutes. Budget state is runtime state, like
the usage tracker itself: **it resets on gateway restart**.

### Budget threshold webhook alerts (`budget_alerts`, per-key overrides)

When a budget's **recorded** spend crosses a configured threshold, the
gateway POSTs a JSON alert to an operator-configured webhook endpoint —
so an operator learns a budget is nearly exhausted without opening the
Cost page. Alerts are **disabled by default**: with no webhook URL
configured anywhere, no delivery is attempted and the zero-service
self-hosted default works unchanged.

```json
"budget_alerts": {
  "webhook_url": "https://alerts.example.com/nexaroute-budgets",
  "thresholds": [50, 80, 100],
  "timeout_seconds": 5,
  "state_path": "/var/lib/nexaroute/budget-alerts.json"
}
```

| Field | Default | Bounds | Meaning |
|---|---|---|---|
| `budget_alerts.webhook_url` | `""` (disabled) | ≤ 4096 bytes; https, or http for loopback hosts only; no userinfo/query/fragment | Global outbound target for all budgets. |
| `budget_alerts.thresholds` | `[50, 80, 100]` when a target exists, else unset | 1–8 finite percentages in (0, 100]; duplicates merge | Spend ladder as percent of the budget limit. |
| `budget_alerts.timeout_seconds` | `5` | 1–30 | Per-attempt delivery timeout. |
| `budget_alerts.state_path` | `""` (in-memory) | ≤ 4096 bytes | Optional durability file for fired marks (atomic, mode-0600 JSON). |
| `client_auth.virtual_keys[].budget_webhook_url` | `""` (inherits global) | same URL rules | Per-budget target override. |
| `client_auth.virtual_keys[].budget_alert_thresholds` | inherits global | same threshold rules | Per-budget ladder override. |
| `client_auth.virtual_keys[].budget_period` | `""` | `""` or `"monthly"` | Dedup period: lifetime of the current `budget_usd` value, or the UTC calendar month. |

Semantics:

- **Exactly once per (budget, threshold, budget period).** The default
  period is the lifetime of the current `budget_usd` value: editing the
  limit starts a new period and re-arms fired thresholds. `"monthly"`
  uses the UTC calendar month (`2026-10`).
- **Asynchronous, never blocking.** The usage-settle path only records
  the mark; delivery runs in a background goroutine with the configured
  timeout. Timeouts, connection failures and non-2xx responses are
  logged and dropped — they never fail or delay a data-plane request.
- **At-most-once delivery per period.** A failed attempt is not
  retried later in the same period; the next period re-arms it.
- **Egress/SSRF policy enforced.** Delivery goes through the same
  egress machinery as provider connections: operator `deny_hosts` /
  `deny_cidrs` take precedence, a configured `allow_hosts` list gates
  the target, DNS-rebinding and embedded-IP (NAT64/6to4) tricks are
  refused, redirects cannot downgrade to non-loopback http, and no
  credentials, cookies, or request content are ever sent (the payload
  carries no secrets, so none are forwarded).
- **Payload** (JSON): `budget_id`, `budget_name`, `threshold_percent`,
  `spend_usd`, `limit_usd`, `currency` (`"USD"`), `period`,
  `timestamp` (RFC3339). No secrets, key hashes, or request content.
- **Durability (honest).** Fired marks are deduplicated in memory;
  across restarts they survive only where a durable store is
  configured. Two options, in priority order: (1) the durable
  control-plane backend (`control_plane` with file/postgres/redis —
  wired automatically into the `budgets/alerts` namespace; remote writes
  are best-effort background writes so a slow database never stalls the
  data plane); (2) `budget_alerts.state_path` (synchronous local file
  write). With the in-memory default a restart may re-fire alerts for
  the same period. Note the interaction with budget state itself
  resetting on restart: a persisted mark means a post-restart crossing
  in the SAME period stays silent — exactly-once per period, not
  at-least-once.
- **Admin surface.** `GET /admin/api/client-keys` lists the per-key
  `budget_webhook_url`, `budget_alert_thresholds` and `budget_period`
  beside `budget_usd` (never secret material); `POST
  /admin/api/client-keys` accepts the same three fields for new keys
  (malformed targets are rejected with 400); `GET
  /admin/api/snapshot` carries a `budget_alerts` section with policy,
  per-budget spend vs. ladder, and fired marks for the current period.
  The Budgets & Cost page itself is unchanged.

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
| `suites_path` | `""` | ≤ 4096 bytes | Optional operator suite pack: a JSON file (one suite, an array of suites, or a `{"suites": [...]}` envelope) or a directory of `*.json` packs in the existing suite format. Loaded atomically: a malformed pack refuses the whole load with a `suites_error` and never partially registers, and ids colliding with built-ins are rejected. Operator suites list alongside built-ins in `GET /admin/api/evaluation/suites` (`source: "operator"`) and run through the same deterministic runner. |
| `telemetry_ingest_enabled` | `false` | — | Opt-in automatic production-telemetry ingestion. While false (default), completed gateway traffic writes no `production_telemetry` scorecards. While true (and `enabled`), each completed request contributes one aggregated sample (success/failure + numeric latency/TTFT only — no prompts, responses, error text or user data) and scorecards materialize through the existing `FromTelemetry` constructor once `telemetry_min_samples` is reached. Ingestion never changes routing, health, or candidate order. |
| `telemetry_min_samples` | `5` | 1–1000000 | Minimum aggregated samples per deployment before a `production_telemetry` scorecard is materialized. |

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
  "ttft_target_ms": 800,
  "suites_path": "/etc/nexaroute/eval-suites",
  "telemetry_ingest_enabled": false,
  "telemetry_min_samples": 5
}
```

### Operator suite pack shape (`suites_path`)

A pack is one JSON suite in the existing suite format (same fields the
built-in catalog uses):

```json
{"id": "operator-smoke", "version": "1", "dimension": "coding", "MinSamples": 1,
 "cases": [{"id": "op-case-one", "weight": 1,
            "expectation": {"kind": "exact_match", "expected": "ok"}}]}
```

A file may also hold an array of suites or a `{"suites": [...]}` envelope; a
directory loads every `*.json` file in filename order (max 32 suites total,
1 MiB per file). Unknown JSON fields, invalid expectations, duplicate case or
suite ids, and ids colliding with built-ins reject the whole load.

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
  "development_fake_provider": false,
  "comfyui_enabled": false,
  "comfyui_base_url": "http://127.0.0.1:8188",
  "comfyui_workflow_path": "comfyui-api-workflow.json",
  "comfyui_model": "comfyui-workflow",
  "comfyui_max_duration_seconds": 60,
  "comfyui_prompt_node_id": "",
  "comfyui_prompt_input": "text",
  "comfyui_negative_node_id": "",
  "comfyui_negative_input": "text",
  "comfyui_width_node_id": "",
  "comfyui_width_input": "width",
  "comfyui_height_node_id": "",
  "comfyui_height_input": "height",
  "comfyui_frames_node_id": "",
  "comfyui_frames_input": "length",
  "comfyui_fps_node_id": "",
  "comfyui_fps_input": "frame_rate",
  "comfyui_seed_node_id": "",
  "comfyui_seed_input": "noise_seed",
  "piper_enabled": false,
  "piper_binary": "piper",
  "piper_model_path": "",
  "code_render_enabled": false,
  "code_render_planner_base_url": "",
  "code_render_planner_api_key_env": "",
  "code_render_planner_model": "",
  "code_render_planner_models": [],
  "code_render_planner_json_mode": false,
  "code_render_max_duration_seconds": 120,
  "code_render_work_root": "",
  "code_render_retention_hours": 24
}
```

For the Universal AI-to-Video engine (`code-render` provider), set `code_render_enabled` to `true`. `code_render_planner_base_url` is the base URL of an OpenAI-compatible chat-completions endpoint used for planning; when empty, the gateway points it at its own data plane on loopback, so the planning call reuses the gateway's configured providers, routing, failover, and credentials — no second upstream key is stored. Set `code_render_planner_api_key_env` only when that endpoint requires a bearer token (for the gateway data plane, a NexaRoute client key). `code_render_planner_model` / `code_render_planner_models` name the selectable planning models; when the list is empty, the gateway advertises its configured public/deployment model names and aliases. `code_render_max_duration_seconds` caps total plan duration (default 120), `code_render_planner_json_mode` requests JSON-object response format from planners that support it, and `code_render_work_root` overrides the per-job render work directory (default: `<storage_root>/code-render-work`). `code_render_retention_hours` bounds how long a finished code-render job and its work directory are kept before an hourly sweep reclaims them (default 24, maximum 8760; set it negative to keep them forever). Plain-HTTP planner URLs are accepted only for loopback hosts. See [Video Gateway](VIDEO_GATEWAY.md#universal-ai-to-video-code-render).

For local narration, set `piper_enabled` to `true`, `piper_binary` to the installed Piper CLI path (or `piper` if it is on `PATH`), and `piper_model_path` to an existing `.onnx` voice model. Its matching `.onnx.json` sidecar must sit beside it. Model paths may be relative to the gateway configuration directory. Piper is optional; when disabled, requests containing narration text or subtitle intent fail before any video provider is submitted.

When using the local ComfyUI adapter, set `comfyui_enabled` to `true`, point `comfyui_base_url` to the local ComfyUI API (default `http://127.0.0.1:8188`), and set `comfyui_workflow_path` to an API-format workflow JSON file readable by the gateway process. Fill the required prompt, width, height, and frame-count node IDs to match that workflow. Negative-prompt, FPS, and seed node IDs are optional; their input names default to the values shown above. The adapter currently supports text-to-video only and rejects requests asking it to synthesize narration, audio, dialogue, or subtitles. Plain HTTP ComfyUI endpoints are restricted to loopback; use HTTPS for remote endpoints. Real model inference is not run by CI. After provisioning a real ComfyUI instance and compatible workflow, run `nexaroute comfyui-accept --config <config> --confirm-live` and keep the sanitized report; see [ComfyUI real-inference acceptance](reports/verification-slices/comfyui-real-inference-acceptance.md).

`store_path` and `storage_root` may be absolute or relative to the gateway's
runtime base directory. The JSON store, queue and cost ledger are single-process;
the local asset sink enforces a per-file limit (default 512 MiB, configurable
from 1 byte through 8 GiB). Startup recovers queued jobs and resumes jobs with
known provider job IDs; ambiguous submissions without a provider job ID require
manual action instead of automatic resubmission. The fake provider is for tests
and local development only, writes a text placeholder, and is not a real video
provider. See [Video Gateway](VIDEO_GATEWAY.md) and [Known Gaps](KNOWN_GAPS.md).

`job_store_backend` selects the video job-store backend: `file` (the default;
single-host JSON file at `store_path`) or `postgres` (shared PostgreSQL for
multi-process deployments — job metadata only, not asset storage). Selecting
`postgres` requires `job_store_url`, which may be a literal connection string
or `env:VARIABLE_NAME` to read it from the environment (recommended, so the
credential stays out of durable config). A configured PostgreSQL backend that
cannot be reached **fails closed at startup**: the gateway never silently
falls back to the file store. The cost ledger follows the job-store
backend: with `postgres` it is a durable PostgreSQL ledger in the same
database — shared by every process on that database, with exactly-once
settlement; with `file` it stays process-local and in-memory. Only job
records and ledger entries are shared; assets remain per-instance local
storage in both backends.

## Optional NexaFlow workflow service (`nexaflow`, issue #765)

Disabled by default. When `enabled` is true, the production gateway
attaches the canonical Task 8 workflow service (Studio routes,
template instantiation, draft validation, run listing) and runs
startup recovery once before serving; a recovery failure fails the
attach, so the gateway never serves with unrecovered workflow state.
When disabled, no routes are mounted and no store is opened, so
gateway behaviour is unchanged.

| Field | Meaning |
|---|---|
| `enabled` | Opt-in flag. Default `false`. |
| `store_backend` | `"file"` (default when empty) or `"memory"`. `"memory"` is volatile and test/ephemeral-only. No other backend is offered; `"postgres"` is rejected. |
| `store_path` | Required for the file backend. There is no implicit default path. A second process on the same path fails at open time on the file lock; a corrupt file fails at open time. Neither falls back silently. |
| `gateway_base_url` | Loopback address backend nodes call. Empty (default) derives `http(s)://127.0.0.1:<listen-port>` from the configured listener (https when TLS is enabled). Must be an `http(s)` URL when set. |
| `gateway_token_ref` | Name of the environment variable holding a client-auth token for loopback calls. The reference, never the secret, lives in config. Empty (default) means `llm.chat` nodes fail closed at client authentication. |
| `worker_id` | Lease identity. Empty (default) means the fixed worker `"gateway"`, stable across restarts so leases stay attributable. A configured value overrides it. The fixed default is correct only for the single-process file/memory stores wired here; a future multi-process store must require explicit distinct worker ids. |

Backend nodes (chat, video, MCP, HTTP) execute for real through the
loopback gateway client; health/compat ports use a direct backend.
Still not wired by this section: scheduler daemon (no tick loop),
webhook listener (no secret source configured), the PostgreSQL
store, `workflow.*` permissions (admin routes keep the interim
`config.read`/`config.write` mapping), any tenant dimension
(single-tenant admin deployment; runs are filtered by principal
subject only), and any BudgetGuard (`budget_check` stays
report-only).

## MCP gateway (`mcp`, M6-1)

When `mcp.enabled` is true, the gateway brokers registered Model
Context Protocol servers behind `POST /mcp` (JSON-RPC 2.0,
streamable HTTP), authenticated with the same client keys as the
data plane.

| Field | Default | Meaning |
|---|---|---|
| `mcp.enabled` | `false` | Master switch; `/mcp` returns 404 while off. |
| `mcp.rpm` | `0` | Per-key MCP call rate (falls back to `client_auth.rpm`; 0 = unlimited). |
| `mcp.max_request_bytes` | `1048576` | Cap on inbound JSON-RPC request size (1024–67108864). Oversized requests are rejected before an upstream call. |
| `mcp.max_response_bytes` | `1048576` | Cap on upstream response size (1024–67108864). |
| `mcp.call_timeout_seconds` | `30` | Per-call upstream timeout (1–600). |
| `mcp.servers[].id` | — | Unique server id; tools are exposed as `<id>__<tool>`. |
| `mcp.servers[].url` | — | Upstream MCP endpoint (http/https; credentials in the URL are rejected). |
| `mcp.servers[].auth_env` | — | Env var NAME whose value is sent verbatim as the `Authorization` header. Secrets are never inline. |
| `mcp.servers[].allowed_tenants` | all | Tenant allowlist; other tenants cannot see or call the server's tools. |
| `mcp.servers[].tools` | all | Tool allowlist for the server. |

Behavior: tools/list aggregates all visible servers (a dead server
is skipped, never fails the list; 30 s per-server cache);
tools/call is brokered over a lazily-initialized per-server
session with one re-initialize + retry on session loss. Every
call emits an `mcp_tool_call` audit event (server, tool, tenant,
result, argument byte count — never argument content) and
`nexaroute_mcp_calls_total` metrics. Batch JSON-RPC requests are
rejected. MCP server URLs are operator configuration and are
dialed directly.
