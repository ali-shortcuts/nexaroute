# NexaRoute — v0.3

A self-hosted Go gateway for routing Anthropic-compatible and OpenAI-compatible clients across many LLM providers/models. The first target is **Claude Code -> NexaRoute -> Chat2API / other OpenAI-compatible or Anthropic-compatible providers**.

This package intentionally stays named **v0.3** until the user validates it on the target Ubuntu machine. The code is runnable and heavily tested, but no software can honestly be guaranteed to contain zero bugs.

## What v0.3 currently implements

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

- `adaptive_round_robin` (default)
- `adaptive`
- `priority`
- `round_robin`
- `least_latency`

Implemented resilience:

- model aliases such as `coding`
- virtual catch-all models `auto` and `claude-auto`
- optional fallback when the client asks for an unknown model
- weighted and priority-aware deployment selection
- EWMA latency and failure-rate scoring
- retries/failover before response bytes are committed
- failover on transport errors, selected 4xx provider/auth failures, `429`, and retryable `5xx`
- `Retry-After` handling with a configurable cap
- per-deployment circuit breaker
- default policy: 5 consecutive failures -> 1 hour cooldown
- half-open recovery after cooldown
- immediate re-cooldown when a half-open deployment fails
- credential-level rotation and cooldown independent of model-level health
- bounded concurrent background micro-probes (default 1 output token), prioritized half-open/unknown/degraded before healthy deployments
- manual **Probe all models** with pass/fail results


### How the smart routing loop actually works

The health loop is deliberately **event-driven + periodic**, not a wasteful sub-second broadcast to every model:

- every real client request updates the selected deployment's health immediately;
- background probes use a tiny request (`max_tokens=1` by default) to refresh idle deployments;
- each probe cycle uses a bounded priority work queue: **half-open -> unknown -> degraded -> healthy**, oldest checks first;
- cooldown deployments are excluded until their deadline, then re-enter as **half-open** and are tested before normal healthy models;
- candidate order combines configured model priority/weight, health state, EWMA response-header latency, and historical failure rate;
- the default breaker is **5 consecutive failures -> 3600-second cooldown**;
- a real Claude Code request tries candidates in routing order and fails over before client-visible response bytes are committed.
- capability routing inspects the parsed request structure for images and reasoning controls, so words such as “image” in ordinary user text do not cause false capability requirements.

`probe.interval_seconds` is configurable down to 1 second, but continuously probing every model multiple times per second is intentionally not the default: with large provider pools it would burn quota, trigger rate limits, and make health worse rather than smarter.

Model **quality** is represented explicitly by configured `priority` and `weight`; a one-token health probe can prove availability/latency, but it cannot honestly measure which LLM is intellectually stronger.

### Claude Code / Anthropic behavior

For native Anthropic-compatible upstreams, the gateway prefers passthrough and preserves unknown JSON fields instead of needlessly normalizing them. This is important for fields that can evolve independently of the gateway.

For Anthropic -> OpenAI-compatible routing, v0.3 includes:

- text conversion
- system content
- tool definitions
- tool calls / `tool_use`
- tool results
- parallel tool-call streaming
- common image/data-URL conversion paths
- Anthropic-style SSE events
- Anthropic-style error envelopes on Anthropic ingress
- forwarding of explicitly allowed headers such as `anthropic-beta` and `anthropic-version`

The gateway does **not** pretend to resume a stream on a different model after client-visible bytes have already been sent. A broken committed stream fails rather than fabricating continuity.

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
8. Test provider/models
9. Save
10. Re-open **Edit** later

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
- config rollback

## Fastest Ubuntu test: use a release package

After downloading and extracting a tagged release package, run:

```bash
cd nexaroute
chmod +x install-user.sh
./install-user.sh
```

This installs:

```text
~/.local/bin/nexaroute
~/.config/nexaroute/config.json
```

Run:

```bash
~/.local/bin/nexaroute -config ~/.config/nexaroute/config.json
```

Open:

```text
http://127.0.0.1:8080/
```

The sample providers are disabled, so the gateway will not contact any real provider until you configure and enable one.

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
docker build -t nexaroute:0.3 .
docker run --rm -p 8080:8080 \
  -e NEXAROUTE_ADMIN_KEY='replace-with-a-strong-random-secret' \
  nexaroute:0.3
```

Do not expose the admin UI directly to the public internet without TLS and additional perimeter controls.

## What is deliberately not claimed

The supported path is strong, but v0.3 is **not** a universal implementation of every LLM protocol. Native OpenAI Responses, Gemini native `generateContent`, Bedrock, Vertex AI, Azure-specific deployment semantics, embeddings/rerank, encrypted-at-rest secret vaults, distributed state, cost/budget routing, and full internet-facing RBAC/CSRF hardening are not implemented.

Cross-protocol reasoning/thinking metadata can also be provider-specific. Native Anthropic passthrough is the safest path for Anthropic-only fields.

Read:

- `ARCHITECTURE.md`
- `TEST_REPORT.md`
- `docs/COMPATIBILITY.md`
- `docs/KNOWN_GAPS.md`
- `docs/SECURITY.md`
- `ROADMAP.md`

before treating v0.3 as production infrastructure.

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
