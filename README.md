# NexaRoute — v0.6

A self-hosted Go gateway for routing Anthropic-compatible and OpenAI-compatible clients across many LLM providers/models. The first target is **Claude Code -> NexaRoute -> Chat2API / other OpenAI-compatible or Anthropic-compatible providers**.

This package is **v0.6**: the v0.4 translation + dashboard core, now extended with a provider-incident intelligence layer and a hedging/cache/usage tier that few if any open gateways combine. The code is runnable and heavily tested, but no software can honestly be guaranteed to contain zero bugs.

## What v0.5.2 adds on top of v0.4

### Provider incident intelligence and quota awareness

- **Provider-level incident circuits** are separate from per-deployment health: failures from multiple distinct deployments inside a bounded evidence window open a provider circuit, while model-specific 404s stay scoped to one deployment. Half-open provider failures reopen immediately; verified success closes the circuit; open providers are filtered from routing and readiness without discarding per-model health history.
- **Quota intelligence from rate-limit headers**: common OpenAI and Anthropic limit/remaining/reset headers are observed on every upstream response. v0.5.2 tracks request and token windows separately and overlays **in-flight local reservations** on top of the latest provider-reported remaining quota. Data-plane upstream legs reserve one request plus a conservative token bound while waiting for fresh evidence; long streams retain the reservation until body completion when the provider omits fresh remaining headers. Routing pressure therefore reacts to effective headroom instead of letting concurrent requests pile onto one stale quota snapshot. Providers remain available as last resort because quota semantics vary.
- **Opt-in cost-aware routing**: `routing.strategy="cost_aware"` keeps verified health, capability checks and configured priority tiers authoritative, then prefers lower bounded request cost among known-priced peers. Unknown pricing is never interpreted as free, and requests without an explicit output-token ceiling fall back to the ordinary score instead of inventing an output estimate.
- **Error-class-aware failure policy**: 400/422 are caller-invalid and health-neutral; 409/425 fail over without poisoning health; 404 isolates the deployment; transport/auth/billing/rate-limit/timeout/5xx feed provider incident evidence.
- **TTFT telemetry**: streaming time-to-first-byte EWMA is recorded separately from response-header latency and exposed per deployment in metrics and the dashboard.

### Request hedging (tail-latency race)

- When `routing.hedging_enabled` is true, the FIRST attempt of a request launches immediately against the primary candidate; if response headers have not arrived within `routing.hedging_delay_ms` (default 1500), the gateway launches a second attempt against the next eligible candidate and lets the upstreams race. First transport-level winner is served.
- Abandonment is a routing decision, **not provider evidence**: the losing leg records no failure, no quarantine, and no provider-incident signal, so hedging can never poison health data. Worst-case amplification is 2x for slow starts and 1x for the common fast path. Client disconnects and gateway deadlines cancel both legs immediately.
- This is the "tail at scale" speculative-execution pattern applied at the LLM-gateway boundary; most gateways only offer serial retries.

### Exact-match response cache (opt-in)

- `cache.enabled` (default false) memorizes complete, non-streaming, deterministic responses (temperature absent/0, top_p absent/1) under a bounded LRU with TTL, entry and byte caps.
- Hits are observable via the `X-NexaRoute-Cache: HIT` response header, a `cache_hit` event, and Prometheus counters. Clients can bypass per request with `x-nexaroute-no-cache`.
- Every successful config swap invalidates the cache wholesale, so a cached response can never outlive the routing topology that produced it.

### Client API keys (opt-in)

- `client_auth.enabled` gates `/v1/*` with static keys accepted from `Authorization: Bearer` or `x-api-key`, compared in constant time over SHA-256 digests, with an optional per-key requests-per-minute ceiling (`client_auth.rpm`) enforced by a bounded token bucket.
- Protocol-shaped 401/429 errors are emitted per ingress (OpenAI-style vs Anthropic-style).

### Context-window-aware pre-routing

- Model deployments can advertise `context_window`. The gateway estimates prompt size (chars/4 heuristic plus per-message overhead, deliberately biased to overestimate) plus requested `max_tokens`, and skips deployments whose advertised window cannot fit the request. A guaranteed-failure attempt wastes attempt budget and burns provider quota for an error the gateway can predict in microseconds.
- Deployments with unknown windows are never filtered.

### Usage and cost accounting

- Real upstream-reported prompt/completion tokens are recorded per deployment from all four response paths (native non-stream, native SSE, translated non-stream, translated SSE).
- Optional per-model pricing (`input_cost_per_mtok`, `output_cost_per_mtok`) yields cumulative estimated spend, resolved at snapshot time so price edits take effect immediately.
- Exposed via `nexaroute_deployment_tokens_total`, `nexaroute_estimated_cost_usd_total`, the admin snapshot, and two new dashboard KPI cards.

## What v0.4 currently implements

### Client-facing endpoints

- `POST /v1/messages` — Anthropic Messages API ingress
- `POST /v1/messages/count_tokens` — tries native Anthropic token counting when possible, otherwise returns a clearly marked local estimate
- `POST /v1/chat/completions` — OpenAI Chat Completions ingress
- `GET /v1/models` — physical deployments, aliases, and the virtual `auto` / `claude-auto` models
- `GET /api/hello` and `GET /version` — runtime/version diagnostics
- `GET /healthz` — process liveness
- `GET /readyz` — routing readiness
- `GET /metrics` — Prometheus text metrics
- embedded Web UI at `/`

### Upstream provider classes

- Generic OpenAI-compatible Chat Completions providers
- Generic Anthropic-compatible Messages providers
- Custom Base URL and endpoint paths
- Bearer, `x-api-key`, or no-auth mode
- Literal key, environment-variable key, or a pool of multiple credentials
- Per-provider HTTP(S) proxy
- Explicit custom headers plus a safe client-header forward allowlist
- Provider-level concurrency limit
- Stream idle watchdog/cancellation
- Model discovery from common `/v1/models` / `/models` response shapes
- Manual model IDs when discovery is unavailable

### Routing and resilience

Routing is done per **deployment** (`provider/model`), not just per provider.

Implemented strategies:

- `ready_mesh` (default): only pre-verified healthy deployments are routable; session affinity keeps a healthy conversation pinned while capacity-aware power-of-two selection spreads new sessions across the best-priority tier
- `ready_queue`: legacy deterministic sticky-strongest ordering over the same verified healthy pool
- `adaptive`
- `priority`
- `round_robin`
- `least_latency`

Implemented resilience:

- model aliases such as `coding`
- virtual catch-all models `auto` and `claude-auto`
- optional fallback when the client asks for an unknown model
- weighted and priority-aware deployment selection
- EWMA response-header latency, streaming TTFT, and recency-weighted failure-rate scoring
- retries/failover before response bytes are committed
- provider-diverse failover ordering inside each priority tier to reduce correlated retry storms
- provider-wide incident circuits require failures from multiple distinct deployments before suppressing a provider
- error-aware failure policy keeps request conflicts health-neutral, isolates model 404s, and treats transport/auth/billing/rate-limit/5xx failures as possible provider incidents
- failover on transport errors, selected 4xx provider/auth failures, `429`, and retryable `5xx`
- `Retry-After` handling with a configurable cap
- per-deployment circuit breaker
- first routed failure immediately quarantines that deployment; the recovery supervisor then probes it up to 5 times
- half-open recovery after cooldown
- if all 5 recovery probes fail, the deployment enters a 30-minute cooldown; after cooldown the supervisor automatically starts a fresh recovery cycle
- credential-level power-of-two load balancing plus independent per-key cooldown, so busy or failing keys are not selected blindly
- bounded global data-plane admission (`max_inflight_requests`, default 128) rejects excess work with 503/Retry-After while health, readiness, metrics and Admin diagnostics remain responsive
- self-rotating operational logs (32 MB × current + 3 backups by default), sampled access logging, console storm limiting, and bounded in-memory event/counter state prevent log/RAM growth with long uptime
- recovery uses a fixed 64-worker queue instead of one long-lived goroutine per failed deployment, so mass provider failure does not create thousands of sleeping recovery stacks
- environment-backed credential rotation is detected during hot reload; only providers whose resolved credentials or transport identity changed are rebuilt
- the HTTP listener opens immediately for liveness/UI observability, then the startup readiness sweep probes every enabled deployment; `/readyz` and ready-mesh routing remain unready until successful models enter the ready queue
- manual **Probe all models** with pass/fail results
- bounded CI stress gate covers 10k-deployment routing, 5k supervised recoveries, event floods, HTTP admission overload, and concurrent log rotation
- manual `Soak` workflow repeats the stress suite and adds same-process hot-reload/recovery cycles plus race-enabled soak checks


### How the smart routing loop actually works

The health loop is deliberately **event-driven + selective**, not a wasteful broadcast over models already proven healthy:

- every real client request updates the selected deployment's health immediately;
- startup probes use a tiny request (`max_tokens=1` by default) to establish the initial ready queue;
- automatic background sweeps probe new/unverified deployments and revalidate only healthy deployments whose ready-health lease has expired;
- every successful real Claude request refreshes that deployment's health lease, so actively used ready models normally receive no synthetic probe;
- an idle ready model is micro-probed after the lease expires, preventing a long-unused fallback from remaining falsely healthy forever;
- failed/degraded/cooldown deployments are owned by dedicated recovery loops and never receive Claude traffic;
- candidate order combines configured model priority/weight with verified ready state; under `ready_mesh`, an eligible session pin wins first, otherwise two candidates inside the best priority tier are compared using score and live provider pressure;
- recovery policy defaults to **5 supervisor attempts -> 1800-second cooldown**, with a 500 ms retry delay between failed recovery probes;
- temporary all-key `429` cooldown waits do not consume the five-attempt recovery budget;
- the explicit **Probe all models** admin action remains available when an operator intentionally wants to retest healthy models too;
- a real Claude Code request tries candidates in routing order and fails over before client-visible response bytes are committed.
- capability routing inspects the parsed request structure for images and reasoning controls, so words such as “image” in ordinary user text do not cause false capability requirements.

`probe.interval_seconds` is configurable down to 1 second. `probe.ready_lease_seconds` defaults to 300 seconds: real successful Claude traffic renews that lease, while an idle ready fallback is micro-probed after the lease expires. The router decision itself is local and fast; remote health checks still take normal network/provider latency. NexaRoute therefore keeps readiness warm in the background without repeatedly probing active models or blocking each Claude request on a new health check.

Model **quality** is represented explicitly by configured `priority` and `weight`; a one-token health probe can prove availability/latency, but it cannot honestly measure which LLM is intellectually stronger.

### Claude Code / Anthropic behavior

For native Anthropic-compatible upstreams, the gateway prefers passthrough and preserves unknown JSON fields instead of needlessly normalizing them. This is important for fields that can evolve independently of the gateway.

For Anthropic -> OpenAI-compatible routing, v0.4 includes:

- text conversion (string and content-part arrays in both directions)
- system content
- tool definitions with reversible name sanitization (MCP-style names survive)
- tool calls / `tool_use` (malformed arguments preserved via `{"_raw": ...}`)
- tool results (block arrays normalized, `is_error` preserved, ordering fixed)
- parallel tool-call streaming with real `include_usage` token accounting
- thinking/reasoning translation: Anthropic budgets ↔ OpenAI `reasoning_effort`
  with the `max_tokens > budget_tokens` invariant enforced
- `stop_sequences`, `top_k`-safe parameter handling, `metadata.user_id` mapping
- common image/data-URL conversion paths with media-type normalization
- Anthropic-style SSE events including `ping` and usage propagation
- Anthropic-style error envelopes on Anthropic ingress
- forwarding of explicitly allowed headers such as `anthropic-beta` and `anthropic-version`

For OpenAI -> Anthropic-compatible routing, v0.4 enforces the invariants that
naive relays miss:

- strict role alternation (consecutive same-role messages merge)
- first message must be a user turn (placeholder prepended when needed)
- parallel OpenAI tool results coalesce into one user message
- `reasoning_effort` maps onto Anthropic thinking budgets and is dropped over
  tool-using histories that cannot carry signed thinking blocks
- streamed thinking deltas surface as OpenAI `reasoning_content`
- role-first chunk, `{}`-padded empty tool arguments, refusal/pause_turn stop
  reason mapping, and final usage-only chunks before `[DONE]`

The gateway does **not** pretend to resume a stream on a different model after client-visible bytes have already been sent. A broken committed stream fails rather than fabricating continuity.

Unsigned thinking blocks are never fabricated toward Anthropic clients: replaying them against a native Anthropic upstream would poison the conversation. Reasoning text from OpenAI-compatible upstreams is therefore intentionally not surfaced as Anthropic thinking blocks in the cross-protocol response path.

## Web UI

The UI is embedded in the Go binary; there is no separate web server to install.

Provider workflow:

1. **Providers -> Add provider**
2. Choose a preset or Custom
3. Enter Name / Provider ID / Endpoint type
4. Enter Base URL
5. Configure auth and API key, environment reference, or credential pool
6. Configure proxy / endpoint overrides / forwarded headers if needed
7. Detect models or add model IDs manually
8. Run **Test connection** for reachability/auth
9. Run **Test selected models** for real inference
10. Save
11. Re-open **Edit** later

Editing does not silently destroy working secrets. The saved Base URL, protocol, models, proxy, endpoint overrides, forward headers, auth settings, credential pool, concurrency settings, and credential source are loaded back into the form. If the secret field is unchanged, `preserve_secret` keeps the prior secret exactly.

The dashboard includes:

- live multi-ring model topology (up to 100 visible deployments before compact overflow)
- health state: unknown / healthy / degraded / half-open / cooldown
- latency and failure counters
- provider/model tables
- routing strategy display
- live event feed
- manual probes
- runtime routing/probe settings
- provider pressure (active/waiting/capacity and credential cooling)
- provider incident state plus common OpenAI/Anthropic remaining-request/token quota hints
- capability-scoped health evidence
- active session-affinity count
- CLI Tools onboarding for Anthropic/Claude Code and OpenAI-compatible clients

## Ubuntu install and browser startup

Release assets are prepared but **not yet published**. Once the reviewed release
is available:

```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash
nexaroute
```

No Go or git required. The installer verifies SHA256 and installs into
`/usr/local/bin` (sudo for the binary only). The embedded UI opens automatically;
headless systems print the URL. Settings live in
`${XDG_CONFIG_HOME:-$HOME/.config}/nexaroute/config.json` and survive upgrades.
Keep the terminal open; Ctrl+C stops NexaRoute. Saved credentials are write-only.

See [Installation](docs/INSTALLATION.md) for options and release preparation, and
[Quickstart](docs/QUICKSTART.md) for adding providers, creating a route, and
connecting Claude Code.

## Quick local self-test

Before adding any real provider, you can verify the packaged binary, embedded UI, Admin API, provider edit persistence, secret redaction, and local token-count fallback without contacting an external service:

```bash
./scripts/smoke-local.sh
```

It uses a temporary loopback-only configuration and removes it when the test finishes.

## Chat2API example

In Web UI -> **Providers -> Add provider**:

```text
Preset:       Chat2API local
Name:         Chat2API
Type:         OpenAI Compatible
Base URL:     http://127.0.0.1:5000/v1
Authentication: whatever your Chat2API instance requires
```

Then:

1. Detect models.
2. Select the models you want.
3. Give multiple models a shared alias such as `coding` if desired.
4. Test them.
5. Save and enable the provider.
6. Run **Probe all models** and confirm healthy deployments appear.

If Chat2API does not expose a model-list endpoint, add model IDs manually.

## Claude Code connection

Point Claude Code's Anthropic-compatible base URL at NexaRoute, for example:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="local-gateway"
export ANTHROPIC_MODEL="coding"
claude
```

`ANTHROPIC_MODEL` can be a real model ID, a configured alias such as `coding`, or `claude-auto`. Client auth credentials are not forwarded to an upstream provider; NexaRoute applies each provider's own configured credential.

## Build and verify from source

Go 1.23+ is recommended for the exact verification path used for this package.

```bash
./scripts/verify.sh
```

The script checks formatting, repeated shuffled tests, `go vet`, the race detector, JavaScript syntax when Node is installed, short fuzz runs, and static Linux builds for amd64 and arm64.

Manual commands:

```bash
go test ./...
go vet ./...
go test -race ./...
node --check internal/httpapi/web/app.js
```

## User-level systemd service (optional)

After `./install-user.sh`:

```bash
./scripts/install-systemd-user.sh
systemctl --user enable --now nexaroute
journalctl --user -u nexaroute -f
```

## Docker

The image binds to `0.0.0.0:8080`. If the Web UI/admin API will be reached from outside loopback, configure an admin key:

```bash
docker build -t nexaroute:0.5.2 .
docker run --rm -p 8080:8080 \
  -e NEXAROUTE_ADMIN_KEY='replace-with-a-strong-random-secret' \
  nexaroute:0.5.2
```

Do not expose the admin UI directly to the public internet without TLS and additional perimeter controls.

## What is deliberately not claimed

The supported path is strong, but v0.5.2 is **not*** a universal implementation of every LLM protocol. Native OpenAI Responses, Gemini native `generateContent`, Bedrock, Vertex AI, Azure-specific deployment semantics, embeddings/rerank, encrypted-at-rest secret vaults, distributed state, invoice-perfect cost optimization or hard budget enforcement, and full internet-facing RBAC/CSRF hardening are not implemented.

Cross-protocol reasoning/thinking metadata can also be provider-specific. Native Anthropic passthrough is the safest path for Anthropic-only fields.

Read:

- `ARCHITECTURE.md`
- `TEST_REPORT.md`
- `docs/COMPATIBILITY.md`
- `docs/KNOWN_GAPS.md`
- `SECURITY.md`
- `ROADMAP.md`

before treating v0.5.2 as production infrastructure.

## Install from GitHub source

Clone the current repository:

```bash
git clone https://github.com/ali-shortcuts/nexaroute.git
cd nexaroute
./scripts/verify.sh
go build -trimpath -o nexaroute ./cmd/gateway
sudo install -m 755 nexaroute /usr/local/bin/nexaroute
```

The repository CI repeats formatting, tests, vet, race detection, and Linux amd64/arm64 builds on pushes and pull requests. Tagged releases build downloadable Linux binaries and SHA-256 checksums automatically.


## Ready Mesh maturity notes

NexaRoute now uses two routing levels:

1. **Deployment/provider level** — verified health, capability requirements, priority tier, session affinity, live provider concurrency pressure, latency/failure scoring, and power-of-two selection.
2. **Credential/key level** — within the selected provider, two available keys are compared by live in-flight load and failure history; 401/402/403/429 cooldown remains isolated to the affected key.

Every failover candidate is revalidated against current health and the hot-reloaded registry immediately before use. A candidate that became quarantined or was replaced after the initial request snapshot is skipped rather than being used from stale state.

The built-in provider preset catalog is intentionally limited to endpoints that fit NexaRoute's implemented OpenAI-compatible or Anthropic-compatible adapter contracts. A preset is configuration convenience, not a claim that every provider-specific extension is supported.
