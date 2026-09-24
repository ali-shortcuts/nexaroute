# Configuration — v0.4

## Runtime environment overrides

```text
NEXAROUTE_CONFIG                  config path used by the CLI default resolver
NEXAROUTE_LISTEN                  override listen address
NEXAROUTE_ADMIN_KEY               override admin API key
NEXAROUTE_ADMIN_BIND_LOCAL_ONLY   true/false override
NEXAROUTE_LOG_FILE                override bounded log path; "off" disables the app-owned file sink
```

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


### Ready Mesh supervisor semantics

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

For Docker/LAN access, set an admin key and put TLS/reverse-proxy controls in front if the environment is not fully trusted.


### Ready Mesh controls

- `session_affinity` — preserve a successful conversation/deployment relationship while it remains healthy.
- `session_ttl_seconds` — idle affinity lease; default `3600`.
- `p2c_window` — maximum number of best-priority candidates considered before the power-of-two pick; default `8`.
- `capacity_weight` — penalty for live active/waiting provider pressure; default `35`.
- The Web UI exposes all Ready Mesh controls, including session affinity/TTL, P2C window, capacity weight, capability thresholds/cooldown, global in-flight admission, and ready-health lease.
- `capability_failure_threshold` — consecutive scoped failures before a capability circuit opens; default `2`.
- `capability_cooldown_seconds` — scoped circuit cooldown; default `300`.

Provider presets are served by the gateway itself through the Admin API so the Web UI does not maintain a second hard-coded provider catalog. Custom Provider remains fully editable. `models_path` may be a normal path or an absolute URL for compatible providers whose discovery endpoint lives on a different host/path.

**Test connection** checks endpoint reachability/auth separately from **Test selected models**, which performs actual minimal model inference.

## v0.5 routing, cache, client auth and model fields

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
