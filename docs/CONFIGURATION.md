# Configuration — v0.3

## Runtime environment overrides

```text
NEXAROUTE_CONFIG                  config path used by the CLI default resolver
NEXAROUTE_LISTEN                  override listen address
NEXAROUTE_ADMIN_KEY               override admin API key
NEXAROUTE_ADMIN_BIND_LOCAL_ONLY   true/false override
```

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

## Routing settings

Available strategies:

```text
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
- `failure_threshold` (legacy/other routing strategies)
- `cooldown_seconds` (default `1800` for ready-queue recovery)
- `request_timeout_ms`
- `latency_weight`
- `failure_weight`
- `retry_backoff_ms`
- `max_retry_after_seconds`


### Ready-queue supervisor semantics

With `routing.strategy = "ready_queue"`:

- a deployment must pass a health probe before it can serve Claude traffic;
- automatic background sweeps do **not** re-probe deployments already marked `healthy`;
- the first eligible routed failure removes the deployment from the ready queue immediately;
- the recovery supervisor owns retry/cooldown until the deployment proves healthy again;
- changing provider Base URL, credentials, auth mode, proxy, endpoint paths, forwarded headers, or the upstream model ID invalidates the old health proof;
- display-name-only changes do not unnecessarily invalidate a working deployment;
- a temporary all-key `429` cooldown is a wait state and does not spend the five recovery attempts;
- the explicit **Probe all models** action remains available when an operator intentionally wants a full retest.

## Probe settings

Ready-queue recovery adds:

- `recovery_attempts` — supervisor probes after a quarantined model fails; default `5`
- `recovery_retry_ms` — delay between failed recovery probes; default `500`
- a model returns to the ready queue immediately on the first successful recovery probe
- after all recovery attempts fail, `routing.cooldown_seconds` is applied before the next recovery cycle



Default behavior:

- interval: 120 seconds
- max output: 1 token
- timeout: 8 seconds
- concurrency: 16
- failure threshold: inherited by deployment health policy

At 100 models, continuous probing consumes real quota even with tiny prompts. Tune the interval or disable background probes if a provider is expensive or quota-constrained.

## Admin settings

For local-only use, the default is safest:

```json
{"bind_local_only": true, "api_key": ""}
```

For Docker/LAN access, set an admin key and put TLS/reverse-proxy controls in front if the environment is not fully trusted.
