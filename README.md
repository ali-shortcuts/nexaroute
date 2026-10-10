# NexaRoute

A self-hosted Go gateway that routes Anthropic-compatible and OpenAI-compatible
clients across many LLM providers and models — most commonly
**Claude Code -> NexaRoute -> Chat2API / other OpenAI-compatible or
Anthropic-compatible providers**.

## Install / Update

```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash
```

## Start

```bash
nexaroute
```

UI: <http://127.0.0.1:8080/>

The installer supports Linux amd64 and arm64, verifies SHA256 checksums, and
installs atomically. No Go, no git, no source compilation. Re-running the same
command upgrades or reinstalls without touching your configuration. Full
details: [docs/INSTALLATION.md](docs/INSTALLATION.md).

## What it does

- **Ingress:** `POST /v1/messages` (Anthropic Messages), `POST /v1/chat/completions`
  (OpenAI Chat Completions), `POST /v1/responses` (OpenAI Responses),
  `POST /v1/messages/count_tokens`, `GET /v1/models`.
- **Upstreams:** OpenAI-compatible, Anthropic-compatible, OpenAI Responses, and
  Gemini providers, each with custom base URLs, auth modes (bearer, x-api-key,
  none), credential pools, proxies, custom headers, and concurrency limits.
- **Routing:** per-deployment (`provider/model`) health and capability filtering,
  priority tiers and weights, session affinity, latency/failure scoring,
  provider-diverse failover ordering, optional request hedging, and opt-in
  cost-aware and quota-aware pressure handling.
- **Resilience:** retries/failover before response bytes are committed,
  per-deployment circuit breakers, supervised recovery (5 recovery probes,
  then a 30-minute cooldown and automatic re-entry), provider incident
  circuits, `Retry-After` handling, and bounded admission control.
- **Protocol translation:** full four-path matrix — Anthropic->Anthropic,
  Anthropic->OpenAI, OpenAI->OpenAI, OpenAI->Anthropic — with streaming,
  tool calls, tool results, reasoning/thinking handling, usage accounting,
  and request cancellation propagation. See [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).
- **Runtime:** persistent XDG config (`~/.config/nexaroute/config.json`, mode
  0600, preserved across upgrades), single-instance locking, browser
  auto-launch when a graphical session exists (URL printed on headless
  systems), asynchronous provider probing that never blocks the UI, and
  self-rotating bounded logs.
- **Security:** provider credentials, custom headers, and proxy URLs are
  write-only — never returned by any API, snapshot, metric, or log. Admin API
  route permissions are enforced server-side with unknown routes denied; the
  existing Admin key remains a break-glass owner credential. Multi-user SSO is
  not yet implemented. See [SECURITY.md](SECURITY.md).
- **Web UI:** provider/model management with write-only secret editing, model
  discovery and testing, candidate pools / route profiles / virtual endpoints,
  live health topology, event feed, manual probes, routing settings, quota and
  incident visibility, and CLI onboarding for Claude Code.

## Claude Code

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="your-nexaroute-client-key"
export ANTHROPIC_MODEL="coding"
claude
```

`ANTHROPIC_MODEL` may be a real model ID, a configured alias such as `coding`,
or `claude-auto`. Client auth credentials are never forwarded to upstream
providers; NexaRoute applies each provider's own configured credential.
Details: [docs/CLAUDE_CODE.md](docs/CLAUDE_CODE.md).

## First run

1. `nexaroute` — the UI opens (or visit <http://127.0.0.1:8080/>).
2. **Providers -> Add provider** — choose a preset or custom, enter the base URL
   and credentials, detect or add models, test, save.
3. Open **Routing → Create route**, choose the upstream deployments, select
   automatic or ordered fallback, and save a stable public model name such as `coding`.
   Advanced users can still edit Candidate Pools, Route Profiles, Virtual Endpoints,
   and Fallback Chains from **Routing → Advanced**.
4. Point your client at NexaRoute with that public model name.

Step-by-step: [docs/QUICKSTART.md](docs/QUICKSTART.md). Configuration
reference: [docs/CONFIGURATION.md](docs/CONFIGURATION.md).

## Optional: systemd user service

After the standard install:

```bash
./scripts/install-systemd-user.sh
systemctl --user enable --now nexaroute
journalctl --user -u nexaroute -f
```

## Docker

```bash
docker build -t nexaroute:local .
docker run --rm -p 8080:8080 \
  -e NEXAROUTE_ADMIN_KEY='replace-with-a-strong-random-secret' \
  nexaroute:local
```

Set an admin key before exposing the UI/admin API beyond loopback.

## Build and verify from source (contributors)

Go 1.23+ is required for development only; end users need neither Go nor git.

```bash
./scripts/verify.sh
```

`verify.sh` checks formatting, repeated shuffled tests, `go vet`, the race
detector, JavaScript syntax, short fuzz runs, benchmarks, and static Linux
builds for amd64 and arm64. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Documentation

- [docs/INSTALLATION.md](docs/INSTALLATION.md) — install, upgrade, uninstall
- [docs/QUICKSTART.md](docs/QUICKSTART.md) — first configuration and routes
- [docs/CLAUDE_CODE.md](docs/CLAUDE_CODE.md) — Claude Code connection
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md) — configuration reference
- [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) — protocol compatibility matrix
- [ARCHITECTURE.md](ARCHITECTURE.md) — design and component overview
- [docs/KNOWN_GAPS.md](docs/KNOWN_GAPS.md) — honest boundaries
- [docs/SECURITY.md](docs/SECURITY.md) — canonical security model and secret-key custody
- [docs/OPERATIONS.md](docs/OPERATIONS.md) — operations and secret backup/rotation runbook
- [docs/reports/REBUILD_COMPLETION_REPORT.md](docs/reports/REBUILD_COMPLETION_REPORT.md) — end-to-end rebuild evidence and remaining environment limits
- [docs/reports/POST_RELEASE_BUG_AUDIT_v0.13.0.md](docs/reports/POST_RELEASE_BUG_AUDIT_v0.13.0.md) — current post-release hardening and bug evidence
- [docs/reports/FINAL_ACCEPTANCE_REPORT.md](docs/reports/FINAL_ACCEPTANCE_REPORT.md) — release acceptance

## What is deliberately not claimed

NexaRoute is **not** a universal implementation of every LLM protocol. Native
Gemini `generateContent` beyond the implemented adapter, Bedrock, Vertex AI,
Azure-specific deployment semantics, embeddings/rerank, distributed state,
invoice-perfect cost optimization, hard budget enforcement, cookie-backed
identity, multi-user RBAC/SSO, and a complete internet-facing identity/session
framework are not implemented. Admin route-level permission checks, built-in
TLS/mTLS and browser CSRF checks are implemented,
but they do not replace Admin authorization, network segmentation, or an
enterprise identity system.
Secret-bearing configuration fields are encrypted at rest; keyring integration
and externally managed key-rotation workflows remain future work. Cross-protocol reasoning/thinking metadata can be
provider-specific; native passthrough is the safest path for provider-only
fields. Read [docs/KNOWN_GAPS.md](docs/KNOWN_GAPS.md) and
[SECURITY.md](SECURITY.md) before treating NexaRoute as production
infrastructure.

### Transport and configuration hardening
Optional built-in TLS/mTLS and browser CSRF protection are documented in [docs/SECURITY.md](docs/SECURITY.md). Virtual-key tenant/project/team scopes intersect fail-closed; child wildcards cannot widen parent policy and unresolved references deny. The `config validate`, `config diff`, and `config dry-run` commands provide read-only preflight checks; see [docs/OPERATIONS.md](docs/OPERATIONS.md).

The optional asynchronous video gateway is documented in [docs/VIDEO_GATEWAY.md](docs/VIDEO_GATEWAY.md). It provides a bounded local queue and test/development fake provider; no real external video provider is claimed verified.
