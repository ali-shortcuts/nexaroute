# NexaRoute — 0.6.1-beta.1

A self-hosted Go LLM gateway with an embedded dashboard. Route OpenAI Chat,
Anthropic Messages and a **stateless subset** of OpenAI Responses across
OpenAI-compatible, Anthropic-compatible, Responses-native and Gemini upstreams.

This is a beta, not a promise of complete API parity or zero bugs. Native
Anthropic passthrough remains the highest-fidelity path for Claude-specific
extensions. See [the audit](docs/AUDIT-2026-09-24.md) and
[compatibility boundaries](docs/KNOWN_GAPS.md).

## Install

### One command from a published release (Linux/macOS, amd64/arm64)

Requires Bash, curl, tar, and sha256sum or shasum. No root or Go required.
The installer verifies the downloaded archive against release SHA256SUMS.
This command requires the matching GitHub Release to have been published:

```bash
curl -fsSL https://raw.githubusercontent.com/ali-shortcuts/nexaroute/v0.6.1-beta.1/install.sh | bash
```

### Install the review beta from source

Until the release tag exists, use the review branch. Requires Git and Go 1.23+
(verification uses Go 1.26); no root required:

```bash
curl -fsSL https://raw.githubusercontent.com/ali-shortcuts/nexaroute/fix/beta-compatibility-install/install.sh | bash -s -- --source fix/beta-compatibility-install
```

For an immutable source build, replace the source ref with a reviewed commit SHA.
You may also download and inspect `install.sh` before executing it.

### Downloaded package

Extract the archive for your OS and architecture, then run `bash nexaroute/install-user.sh`.
The installer preserves an existing configuration and atomically replaces the
binary, including when an older process is running. Restart that process to use
the upgrade.

Installed paths:

- Binary: `~/.local/bin/nexaroute`
- Config: `~/.config/nexaroute/config.json` (mode 0600)

Start:

```bash
"$HOME/.local/bin/nexaroute" -config "$HOME/.config/nexaroute/config.json"
```

Open <http://127.0.0.1:8080/>. Example providers are disabled. Add a provider,
enter its credentials, detect or add models, test inference, then save and enable.
A healthy model must enter the ready pool before ready-mesh routing can serve it.

Windows: use WSL for the Bash installer. A Windows amd64 `.exe` is also built;
run it in PowerShell with `-config .\config.json` and a copy of the example
configuration. The Windows and macOS binaries are cross-compiled, not runtime
certified on those operating systems by this audit.

## Endpoints

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/messages/count_tokens` | Native count when available; otherwise marked estimate |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | Stateless text/function-tool Responses subset |
| `GET /v1/models` | Deployments, aliases, `auto` and `claude-auto` |
| `GET /healthz` | Process liveness |
| `GET /readyz` | Routing readiness |
| `GET /metrics` | Prometheus metrics |
| `GET /api/hello`, `/version` | Consistent runtime version and protocol diagnostics |

Responses requires the full conversation in `input`. `previous_response_id`,
`store=true` and hosted tools such as web search are rejected with HTTP 400;
the gateway does not silently pretend to implement them. Native Responses
retrieval/deletion, background jobs and realtime protocols are not implemented.

## Routing and recovery

- Deployment-level routing with explicit aliases, capabilities, priorities,
  weights, context windows, provider pressure and session affinity.
- Strategies: `ready_mesh`, `ready_queue`, `adaptive`, `priority`,
  `round_robin`, `least_latency`, and opt-in `cost_aware`.
- Startup verification, renewable health leases and bounded supervised recovery.
  Defaults: five recovery attempts followed by a 30-minute cooldown.
- Provider incident circuits, deployment circuits and independent credential
  rotation/cooldown; health and feature incompatibility are separate evidence.
- Bounded pre-response retries, Retry-After handling and jittered backoff.
- Opt-in two-leg hedging on Chat/Messages; Responses currently uses serial failover.
- Advisory provider quota headroom and in-flight request/token reservations.
- Global admission control, upstream concurrency caps and stream idle cancellation.
- Deterministic, bounded repair of known unsupported optional parameters;
  semantics-critical unsupported capabilities fail or route elsewhere.

Once client-visible response bytes are sent, a failed stream is **not** retried
on another model. Routing measures health/load/latency, not model intelligence.
Model quality and task specialization remain operator-configured policy.

## Compatibility, operations and security

Text, images, function tools, tool results, parallel calls and SSE are translated
within the implemented subset. Provider-specific reasoning, signed thinking,
prompt caching and beta fields do not have universally lossless equivalents.
Foreign reasoning never receives a fabricated Anthropic signature.

The dashboard supports custom providers, endpoint/auth selection, model discovery,
manual models, inference tests, credential pools, proxies, health and events.
Saving existing models preserves configured context windows and pricing.

Optional client API keys and per-key RPM limits protect `/v1/*`. Admin access is
loopback-only by default; an admin key is required for remote management.
Secrets in JSON config are protected by file permissions, not encryption.
Health/usage/affinity/capability state is single-process and in memory.

Optional exact-response caching applies to eligible non-streaming Chat/Messages;
it is bounded and invalidated on config changes. Responses ingress does not use
that cache. Usage uses upstream token reports; configured prices give estimates,
not invoice-grade billing or hard spend enforcement.

## Client examples

Claude Code:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN=local-gateway
export ANTHROPIC_MODEL=coding
claude
```

Configure an alias named `coding`, or use `claude-auto`. If client authentication
is enabled, use an actual configured client key instead of `local-gateway`.
Client credentials are not forwarded to upstream providers.

OpenAI-compatible clients: use base URL `http://127.0.0.1:8080/v1` and a configured
model/alias. Upstream provider API keys belong in NexaRoute configuration.

## Verify and package

```bash
./scripts/verify.sh
./scripts/stress.sh
./scripts/smoke-local.sh
./scripts/package-release.sh
./scripts/test-installer.sh
```

Verification covers formatting, repeated shuffled unit/integration tests, vet,
race detection, short fuzz checks, JavaScript syntax/editor persistence and builds.
Release tags must match `internal/buildinfo/version.go`. A `v*` tag builds Linux
and macOS packages, a Windows executable and SHA256SUMS; prerelease tags remain
marked as prereleases.

Optional Linux user service after installing from an extracted package:

```bash
./scripts/install-systemd-user.sh
systemctl --user enable --now nexaroute
```

Docker:

```bash
docker build -t nexaroute:0.6.1-beta.1 .
docker run --rm -p 127.0.0.1:8080:8080 -e NEXAROUTE_ADMIN_KEY='replace-with-a-random-secret' nexaroute:0.6.1-beta.1
```

Persist `/config` with appropriate ownership if using Docker beyond a temporary
trial. TLS, multi-user RBAC, distributed state and hard budget enforcement require
additional infrastructure or development. Consult [SECURITY.md](SECURITY.md).
