# Security model and operations

This document is the canonical security reference for NexaRoute. The repository-root [SECURITY.md](../SECURITY.md) is only an entry point to this file.

## Defaults and trust boundaries

- The data-plane listener defaults to `127.0.0.1:8080`; Admin is local-only by default. Keep both on trusted interfaces for initial setup.
- Admin access is privileged. When remote administration is deliberately enabled, use a strong Admin API key and a trusted TLS-terminating proxy, firewall/network policy, VPN, or equivalent boundary.
- Provider credentials are distinct from client credentials. Provider secrets are never used as client keys or blindly forwarded from a client request.
- Saved provider keys, credential-pool keys, custom header values, and proxy URLs are write-only on Admin read surfaces. Secret-bearing Admin exports/snapshots use redacted values; they do not expose the stored ciphertext or decrypted secret.

## Encrypted secret storage

Secret-bearing JSON values are stored as versioned `nxs1` envelopes encrypted with AES-256-GCM. Each value receives a fresh random 256-bit data-encryption key (DEK); the DEK is wrapped by the master key. Independent random GCM nonces are generated for the value and its wrapped DEK. Associated data binds each envelope to its owner and exact field (including provider ID, credential-pool index, and custom-header name), so moving a ciphertext to a different field fails authentication.

The encrypted fields include provider API keys and credential-pool API keys, decision-provider keys, the Admin API key, client-auth keys, provider custom-header values, and proxy URLs. Environment-variable references remain references rather than copied secret values.

### Master-key selection and file permissions

The source precedence is:

1. `NEXAROUTE_MASTER_KEY`: base64 encoding of exactly 32 bytes. This overrides all file sources and makes the process environment/deployment secret injector part of the trust boundary.
2. `NEXAROUTE_MASTER_KEY_FILE`: a path to a raw 32-byte key file. The file must have exactly mode `0600`; invalid length or looser permissions fail closed.
3. With neither override, NexaRoute creates `<config-path>.key` with mode `0600` on first use. The config itself is atomically written with mode `0600`; the default config directory is created with mode `0700`.

A colocated automatically managed key protects against disclosure of the config file alone. It does **not** protect against theft of both files, a compromised host/root account, or a storage snapshot containing both. Back up the encrypted config and its exact matching key as separately protected objects. There is no OS keyring integration in this release.

### Migration and failure behavior

On load, legacy plaintext secret fields are encrypted and the config is replaced via a same-directory temporary file, file sync, atomic rename, and directory sync. Before replacing the file, NexaRoute creates a timestamped `.enc.bak` containing an authenticated encrypted backup—not a plaintext copy. Migration is idempotent.

Missing or wrong keys, malformed envelopes, tampering, or AAD mismatch fail closed. NexaRoute does not generate a replacement key for a config that contains encrypted secrets and does not start with partially decrypted settings or a plaintext fallback. Restore the exact matching key or restore an encrypted config backup together with its matching key. Loss of the only matching key makes those encrypted secrets unrecoverable.

### Secrets CLI

- `nexaroute secrets status --config PATH` reports ciphertext counts only; it does not print secret values.
- `nexaroute secrets verify --config PATH` decrypts in memory and reports field identifiers and success/failure, never the values.
- `nexaroute secrets rotate --config PATH` rotates the automatically managed `<config>.key`. It refuses to run while either master-key environment override is configured.
- `nexaroute secrets decrypt --config PATH --to-stdout --allow-plaintext` is the explicit plaintext export path. Both flags are required. Treat stdout, terminal capture, shell pipes, and redirected output as sensitive.

Rotation preserves the prior key at `<config>.key.previous`, writes the replacement durably to `<config>.key.next`, atomically replaces the encrypted config, then renames the staged key into place and syncs the directory. At startup, the key loader authenticates the staged key against the config before promoting it; a staged key is ignored when the old key still authenticates the config. If activation is interrupted, do not delete `.key`, `.key.next`, `.key.previous`, or the config: preserve all files, run `secrets verify`, and restore the matching config/key pair from the protected backup if recovery does not succeed. The fixed `.key.previous` file is the most recent rotation's recovery copy; archive each key/config pair securely before a later rotation overwrites it.

## API, logs, snapshots, and metrics

No normal Admin read surface, event, request log, or metric is intended to contain plaintext provider/client credentials. Secret-bearing Admin reads are redacted; logs avoid request/response bodies and credential fields, and upstream error snippets are bounded and credential-redacted. Metrics use bounded operational labels rather than request bodies or secret values. Regression tests exercise secret canaries across the relevant read surfaces. The explicit `secrets decrypt` command is the only documented plaintext export path and requires both opt-in flags.

Keep log directories private. App-owned rotating logs are mode `0600` and bounded by `logging.max_size_mb` and `logging.max_backups`; console logging is separately rate-limited.

## Network and browser security

- Built-in TLS and mTLS for Admin/data plane are implemented and can be scoped independently, but they do not replace a trusted network boundary or Admin authorization. Use a trusted reverse proxy, firewall/network policy, VPN, or equivalent for untrusted networks.
- Browser CSRF-token and same-origin checks are implemented for state-changing Admin requests. A hardened server-side cookie-session identity framework is not implemented: the dashboard keeps an entered Admin key in in-memory JavaScript state, which is lost on reload. Do not expose the Admin UI/API to untrusted origins or networks.
- The fail-closed RBAC matrix in `internal/authz` is transport-independent and tested, but it is not yet wired to HTTP routes. OIDC/SAML, server-side SSO sessions, and role-claim mapping remain disabled; do not describe the current Admin API-key boundary as SSO.
- Virtual-key tenant/project/team policy is an intersection, not a union: configured parent allow-lists are ceilings, unresolved scope references deny, and duplicate model/token metadata is handled conservatively. These checks are authorization hardening, not a replacement for RBAC/SSO.
- The opt-in video data-plane handler requires `video.auth_token_env` to resolve to a non-empty token and compares bearer tokens in constant time. Keep the token in the process environment/secret injector, not in browser Admin state. Video jobs and worker shutdown are bounded to a single process; the local-development fake provider is not a production provider or trust boundary. No real external provider adapter is claimed verified.
- Multi-user RBAC, SSO, and identity-aware access are not implemented. Optional client auth provides shared static keys and bounded RPM controls, not user identity or a complete enterprise access system.
- Administrators may configure arbitrary provider/proxy URLs. A general provider egress allow/deny policy is not built in, so an authorized admin can point a provider at internal services. Do not give admin access to untrusted users.
- The separate remote-decision HTTP client (`internal/decision/remote`) is hardened against SSRF: it resolves and pins approved IPs per connection, rejects loopback/private/link-local/reserved destinations and mixed public/private DNS answers, rejects redirects, ignores environment proxy variables, requires TLS verification with TLS 1.2 minimum, and bounds response bodies. These protections do not imply that arbitrary provider URLs are similarly restricted.

## Other implemented controls and limitations

The Admin key comparison is constant-time; client headers are not blindly propagated upstream; provider auth is applied after custom headers; provider client-header forwarding uses an allowlist; provider concurrency and global data-plane in-flight work are bounded; and a stream-idle watchdog cancels stalled upstream work. These controls do not replace host access control, network segmentation, TLS, or key backups.

For configuration examples and operator procedures, see [docs/CONFIGURATION.md](CONFIGURATION.md), [docs/OPERATIONS.md](OPERATIONS.md), and [docs/KNOWN_GAPS.md](KNOWN_GAPS.md).

## Built-in TLS, mTLS, and browser CSRF
The shared listener optionally serves HTTPS with TLS 1.2 minimum using `tls.enabled`, `tls.cert_file`, and `tls.key_file`; certificate and key material is re-read for each new handshake, so atomically replacing the files updates new connections without restarting. Optional client certificates can be required independently for `/admin/api/*` and `/v1/*` using `tls.client_ca_file`, `tls.require_client_cert_admin`, and `tls.require_client_cert_data_plane`. mTLS is an additional peer check and does not replace the Admin API key.

Browser state-changing Admin requests require same-origin `Origin`/`Referer` validation plus a random `HttpOnly`, `SameSite=Strict` CSRF cookie mirrored in `X-NexaRoute-CSRF`; the cookie is `Secure` for HTTPS. Stateless non-browser Admin clients without browser origin metadata continue to use Admin-key authentication. This is not cookie-backed identity, RBAC, or SSO.
