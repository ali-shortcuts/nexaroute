# Security — v0.4

NexaRoute defaults to localhost and should stay there for first testing.

## Implemented protections

- local-only admin API by default
- optional admin API key
- constant-time admin-key comparison
- sensitive client headers are not blindly propagated upstream
- provider auth is applied explicitly after configured custom headers
- provider client-header forwarding uses an allowlist
- rewritten config files use `0600`
- browser admin key is kept in `sessionStorage`
- provider concurrency is bounded
- global data-plane in-flight work is bounded and overload rejections are observable in metrics/events
- stream idle watchdog cancels stalled upstream work
- request logs contain metadata, not request bodies/API keys

## Secrets

Prefer environment references instead of literal keys in JSON when practical.

Provider API keys can be entered in the Web UI and are persisted in the 0600
config file (or via environment-variable references). After save, admin GET
responses, the dashboard, snapshots, metrics, and events never return the
plaintext key. Leave the editor field blank to keep the stored secret.
Treat access to the host and the config file as equivalent to access to
provider credentials.

## Client-facing authentication boundary

NexaRoute v0.4 does **not** provide a separate built-in authentication policy for the client-facing `/v1/*` data plane. Provider credentials are never treated as client credentials. If the listener is reachable from an untrusted network, put the client-facing routes behind a trusted reverse proxy/API gateway, firewall, VPN, or equivalent access-control layer.

The Admin API is a separate boundary: it remains loopback-only by default or requires the configured Admin key when remote administration is intentionally enabled.

## Remote exposure

Before exposing the UI/admin API beyond a trusted local machine:

- set a strong `NEXAROUTE_ADMIN_KEY` / `admin.api_key`;
- use TLS through a trusted reverse proxy;
- restrict source networks/firewall rules;
- do not publish the admin endpoint directly to the internet;
- consider additional CSRF/RBAC/SSO controls outside NexaRoute.

## Base URLs and proxies

The administrator can configure arbitrary HTTP(S) provider/proxy URLs. This is powerful and also means an authorized admin could intentionally point NexaRoute at internal services. A strict SSRF allow/deny policy is not yet built in, so do not give admin access to untrusted users.

Do not insert unrelated browser-session tokens into provider configuration unless the target service explicitly supports that use and you accept the account/security implications.

## Log retention and sensitive output

Operational logs are written to an app-owned rotating file by default and are capped by `logging.max_size_mb` and `logging.max_backups`. Files are mode `0600`; old numeric backups are removed automatically. Console output is independently rate-limited so a failure storm cannot flood journald at request rate.

Request/response bodies and provider credentials are not part of normal access lines. Upstream error snippets are bounded and credential-redacted. Keep the log directory private and do not disable the retention limits on shared systems.
