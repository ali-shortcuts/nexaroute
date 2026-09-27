# Security

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

Saved provider credentials are write-only: the provider editor/API never returns literal, pooled, or resolved environment keys, including legacy reveal requests. Custom headers and proxy URLs are also write-only. Base URLs and other public metadata must not contain credentials. Untouched credential fields preserve saved values. Config contains plaintext secrets at rest (0600); protect the config directory (0700). Treat admin access as privileged because it can change providers and routing.

## Client-facing authentication boundary

NexaRoute does **not** provide a separate built-in authentication policy for the client-facing `/v1/*` data plane. Provider credentials are never treated as client credentials. If the listener is reachable from an untrusted network, put the client-facing routes behind a trusted reverse proxy/API gateway, firewall, VPN, or equivalent access-control layer.

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

## Remote decision transport (SSRF hardening)

The outbound remote-decision HTTP client (`internal/decision/remote`) is hardened against SSRF:

- every connection is made only to an IP address that was resolved and approved once per connection attempt (DNS pinning). The original hostname is never re-resolved at dial time, which closes the DNS-rebinding/TOCTOU window. TLS SNI and certificate verification still use the original hostname, and HTTP Host semantics are unchanged;
- loopback (v4/v6), `localhost`/`*.localhost`, RFC1918, IPv6 unique-local, link-local (including `169.254.169.254` and cloud metadata endpoints such as `metadata.google.internal`), multicast, unspecified, reserved/CGNAT ranges and degenerate numeric "IP" spellings are rejected, before DNS and again for every resolved address. Mixed public/private DNS answers are rejected as a whole;
- redirects are validated on every destination and are never followed, so Authorization credentials cannot leak across targets and redirect chains cannot walk toward internal services;
- environment proxy variables (`HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`) are deliberately **not** honored by this client. With an implicit proxy, NexaRoute would only validate the connection to the proxy while the proxy itself could reach blocked targets, silently voiding the SSRF guarantee. Traverse proxies via an egress gateway with its own enforced policy instead of re-enabling `http.ProxyFromEnvironment`;
- TLS verification cannot be disabled (no `InsecureSkipVerify`), the minimum version is TLS 1.2, and response bodies are size-bounded.

Do not insert unrelated browser-session tokens into provider configuration unless the target service explicitly supports that use and you accept the account/security implications.

## Log retention and sensitive output

Operational logs are written to an app-owned rotating file by default and are capped by `logging.max_size_mb` and `logging.max_backups`. Files are mode `0600`; old numeric backups are removed automatically. Console output is independently rate-limited so a failure storm cannot flood journald at request rate.

Request/response bodies and provider credentials are not part of normal access lines. Upstream error snippets are bounded and credential-redacted. Keep the log directory private and do not disable the retention limits on shared systems.
