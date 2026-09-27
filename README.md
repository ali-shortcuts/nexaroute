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
  write-only — never returned by any API, snapshot, metric, or log. See
  [SECURITY.md](SECURITY.md).
- **Web UI:** provider/model management with write-only secret editing, model
  discovery and testing, candidate pools / route profiles / virtual endpoints,
  live health topology, event feed, manual probes, routing settings, quota and
  incident visibility, and CLI onboarding for Claude Code.

## Claude Code

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="local-gateway"
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
3. Create a **candidate pool**, a **route profile**, and a **virtual endpoint**
   (public model name) in the sidebar pages; enable it.
4. Point your client at NexaRoute with the virtual endpoint's public model name.

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
docker build -t nexaroute:0.7.0 .
docker run --rm -p 8080:8080 \
  -e NEXAROUTE_ADMIN_KEY='replace-with-a-strong-random-secret' \
  nexaroute:0.7.0
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
- [SECURITY.md](SECURITY.md) — security model
- [docs/FINAL_ACCEPTANCE_REPORT.md](docs/FINAL_ACCEPTANCE_REPORT.md) — release acceptance

## What is deliberately not claimed

NexaRoute is **not** a universal implementation of every LLM protocol. Native
Gemini `generateContent` beyond the implemented adapter, Bedrock, Vertex AI,
Azure-specific deployment semantics, embeddings/rerank, encrypted-at-rest
secret vaults, distributed state, invoice-perfect cost optimization, hard
budget enforcement, and full internet-facing RBAC/CSRF hardening are not
implemented. Cross-protocol reasoning/thinking metadata can be
provider-specific; native passthrough is the safest path for provider-only
fields. Read [docs/KNOWN_GAPS.md](docs/KNOWN_GAPS.md) and
[SECURITY.md](SECURITY.md) before treating NexaRoute as production
infrastructure.
