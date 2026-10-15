# NexaRoute v0.16.3 Operations Manual

Version: v0.16.3

Target: Gateway operators and administrators

## 1. Start and configure

NexaRoute is a self-contained gateway binary. The default config path is selected by the CLI resolver; use `-config` to select an explicit file:

```bash
./nexaroute
./nexaroute -config /etc/nexaroute/config.json
./nexaroute -config /etc/nexaroute/config.json -no-browser
```

The data-plane listener defaults to `127.0.0.1:8080`. Admin is local-only by default and shares the gateway listener; it is not a separately configured listener. The runtime overrides are:

| Variable | Purpose |
|---|---|
| `NEXAROUTE_CONFIG` | Default config path used by the CLI resolver |
| `NEXAROUTE_LISTEN` | Listener address |
| `NEXAROUTE_ADMIN_KEY` | Process-scoped Admin key override |
| `NEXAROUTE_ADMIN_BIND_LOCAL_ONLY` | Boolean override for local-only Admin mode |
| `NEXAROUTE_LOG_FILE` | Log path override; `off` disables the app-owned file sink |
| `NEXAROUTE_STRICT_CONFIG` | Refuse startup when implicit defaults/legacy migration are required |
| `NEXAROUTE_MASTER_KEY` | Base64-encoded 32-byte master-key override |
| `NEXAROUTE_MASTER_KEY_FILE` | Raw 32-byte master-key file override, mode `0600` |

Master-key source precedence, encryption coverage, and failure behavior are described in the canonical [security model](SECURITY.md). Do not use legacy environment-variable names copied from older operations guides.

## 2. Secret storage, backup, and restore

Secret-bearing config values are encrypted at rest. The default auto-managed master key lives at `<config-path>.key`; the config and key are separate files and both are needed to restore encrypted values. Protect the key independently from the config backup. Loss of the only matching key is unrecoverable.

### Back up before an upgrade or key rotation

1. Restrict the backup directory (`0700`) and the backup files (`0600`). Use an encrypted, access-controlled backup target separate from the gateway host where practical.
2. Copy the active encrypted config and its exact matching master key as a pair. If an explicit key source is configured, back up that source through its secret-management system instead of copying a different key.
3. Record the pairing securely and test restoration in an isolated environment. Never assume that a config backup can be decrypted by the current key after later rotations.
4. Preserve any `.enc.bak` migration files together with the matching master key. These backups are authenticated encrypted blobs, not standalone plaintext JSON configs.

Example for an auto-managed key (adapt paths and secure storage to the deployment):

```bash
umask 077
install -d -m 0700 "$BACKUP_DIR"
install -m 0600 "$CONFIG" "$BACKUP_DIR/config.json"
install -m 0600 "${CONFIG}.key" "$BACKUP_DIR/config.json.key"
```

To restore, stop the gateway, restore the selected config and its matching key, set the key mode to `0600`, verify directory permissions, then run `nexaroute secrets verify --config "$CONFIG"` before starting the listener. Do not create a replacement key to troubleshoot authentication failures.

### Rotate the auto-managed key

`nexaroute secrets rotate` currently supports the automatically managed sibling key only; it refuses while `NEXAROUTE_MASTER_KEY` or `NEXAROUTE_MASTER_KEY_FILE` is set. For environment/KMS-managed keys, use the deployment's approved key-management workflow; this CLI does not rotate those sources.

```bash
nexaroute secrets status --config "$CONFIG"
nexaroute secrets verify --config "$CONFIG"
# Back up the current encrypted config and matching key before proceeding.
nexaroute secrets rotate --config "$CONFIG"
nexaroute secrets verify --config "$CONFIG"
```

Rotation retains the previous key as `<config-path>.key.previous`, stages the replacement as `.key.next`, replaces the encrypted config atomically, and promotes the staged key. At startup, NexaRoute checks which key authenticates the config before promoting a staged key. A staged key that does not match the current config is not activated. The fixed `.key.previous` is replaced on the next rotation, so preserve a securely paired copy before rotating again.

### Recover an interrupted rotation

If the process stops during rotation, preserve the config, `.key`, `.key.next`, and `.key.previous`; do not delete or overwrite any of them. Restart or run `nexaroute secrets verify --config "$CONFIG"` with the same config path: when the config authenticates with the staged key, NexaRoute promotes `.key.next` to `.key`. If verification does not succeed, restore a known-good encrypted config and its matching key from backup. Do not start an older gateway with an encrypted config unless it supports the same `nxs1` envelope format.

## 3. Secrets CLI and plaintext handling

- `nexaroute secrets status --config PATH` reports encrypted-field counts only.
- `nexaroute secrets verify --config PATH` authenticates/decrypts in memory and reports field identifiers and status, never secret values.
- `nexaroute secrets decrypt --config PATH --to-stdout --allow-plaintext` is a deliberate plaintext export. Both flags are mandatory. Restrict terminal capture and shell redirection; if a plaintext file is unavoidable, create it in a private directory with mode `0600`, use it only for the specific migration/rollback, and securely remove it afterward.
- The first load of a legacy plaintext config migrates it atomically and leaves a timestamped encrypted `.enc.bak`; normal runtime/Admin config saves do not create a backup copy.

## 3a. Audit chain verification

Admin audit records in the security store form a tamper-evident hash
chain (M3-6): every record carries a monotonic sequence, the previous
record's hash, and its own SHA-256 hash over a canonical encoding of its
fields. Retention/count pruning deletes only a prefix of the chain and
records a pruning anchor (the newest pruned record's sequence and hash),
so the surviving window stays verifiable.

- `nexaroute audit verify --config PATH` opens the store **read-only**
  and revalidates every record: hash recomputation, predecessor
  linkage, sequence order, anchor linkage, and chain-head consistency
  (which detects tail truncation). Exit codes: `0` verified, `1`
  integrity problems (each printed), `2` usage/operational error.
  `--format json` emits the structured report. Output is metadata only
  (counts, sequences, head hash) — never event payloads.
- The store is a single-writer bbolt file: while the gateway is
  running it holds the lock and verification fails with exit `2`.
  Stop the gateway for offline verification.
- Records written before the chain existed are reported as a *legacy
  pre-chain* segment. They are counted, never claimed as verified.
- **Limits:** the chain is local and unanchored. It detects edits,
  deletions, insertions, reordering, and truncation of the stored
  records, but an attacker who controls the whole database file —
  records *and* chain metadata — can rewrite history consistently.
  Record the reported head hash in an external system (ticket, log
  archive, second host) after each verification if you need evidence
  that survives full-store compromise. Investigate any `FAILED`
  result before trusting or rotating the store: preserve the file,
  compare against your last externally recorded head hash, and treat
  unexplained gaps as a security incident.

## 3b. Privacy audit CLI

`nexaroute privacy audit --config PATH` inspects the configuration and the
runtime policy surfaces derived from it (providers, route profiles,
candidate pools, virtual keys, egress policy, cache, logging) and reports
privacy problems that can be determined locally. It is **read-only**: it
never decrypts, migrates, or rewrites the config file, and it sends no
provider requests. Findings name configuration objects and fields only —
the audit never prints API keys, credential values, authorization headers,
URL userinfo, or request bodies.

```bash
nexaroute privacy audit --config "$CONFIG"              # human-readable findings
nexaroute privacy audit --config "$CONFIG" --format json  # machine-readable report
```

Exit codes are deterministic:

| Exit | Meaning |
| --- | --- |
| `0` | Audit completed; no error-severity findings |
| `1` | Audit completed; at least one error-severity finding |
| `2` | Usage error, or the config could not be read or parsed |

Error findings include plaintext credentials stored in the config file
(`PLAINTEXT_SECRET`), credentials embedded in provider URLs
(`URL_CREDENTIALS`), cleartext `http://` endpoints on non-loopback hosts
(`CLEARTEXT_ENDPOINT`), and `no_training` requirements (route profile or
virtual key) that no reachable deployment can satisfy
(`NO_TRAINING_UNSATISFIABLE`) — those requests fail closed at runtime, so
the audit mirrors the runtime gate exactly. Warnings and infos cover
partial `no_training` pools, `logging.access_mode=all`, an enabled
response cache, undeclared provider data-handling classes, and an empty
operator egress deny policy.

## 4. Health and status

- `GET /healthz` is a liveness endpoint.
- `GET /readyz` reports whether an eligible deployment is ready.
- `GET /admin/api/health` reports deployment-level health details and requires the configured Admin access boundary.

A deployment must satisfy the configured health and capability requirements before it is eligible for traffic. See [configuration](CONFIGURATION.md) for probe cadence, leases, and recovery behavior.

Separately from live health, the gateway computes **static eligibility warnings** (M3-3) from the configuration alone: no enabled model deployments, a referenced candidate pool that matches no enabled deployment, a route profile whose whole fallback chain is unusable, and `no_training` profiles or virtual keys with no deployment satisfying the fail-closed privacy rule. These warnings are printed at startup with the other config warnings, returned by `PUT /admin/api/settings`, and exposed in `GET /admin/api/snapshot` as `eligibility_warnings`. They are advisory only — they never fail config validation (including strict mode). The snapshot also carries `route_eligibility`: per-profile candidate counts with current health-based exclusion reasons (privacy-filtered, provider unavailable, retired, cooldown, health not yet established, not healthy). Those counts reflect pool membership and health only; per-request filters (model, capabilities, context window) are not applied.

### Shared control-plane state

The optional `control_plane` section defaults to the atomic local `file`
backend. Its state path is relative to the config directory unless absolute;
the file and parent directory are created with restrictive permissions. The
gateway writes versioned snapshots with optimistic revisions and never logs
the contents of environment-provided DSNs or URLs. `memory` is test-only.

Before changing a backend, copy the current state file to a protected backup:

```bash
install -m 0600 "$CONFIG.controlplane.json" "$BACKUP_DIR/controlplane.json"
```

Set `control_plane.migration_dry_run=true` for startup validation without a
state write. For Redis this means only a bounded connectivity `PING`: dry-run
does not read, initialize, or mutate a snapshot and does not validate the
deployment's persistence or recovery configuration.

Redis stores each namespace in a hash under
`nexaroute:control:snapshot:<namespace>` with no expiration. Lua scripts apply
revision checks atomically on the Redis server executing the script; they do
not promise multi-primary consistency or cross-replica safety. A successful
connection is not a durability guarantee. Configure, monitor, and test AOF
and/or RDB persistence, durable storage, replication/failover, and backup/
restore according to the deployment's recovery objectives. An acknowledged
write may still be lost during a Redis failure or promotion depending on that
configuration.

The PostgreSQL and Redis control-plane clients are wired and have disposable-
service integration coverage. These tests verify adapter behavior, not a
deployment's recovery guarantees. Store switching does not automatically copy
or convert state: preserve the previous backend and its protected backup until
the destination state is verified. To roll back to the local file backend,
stop the gateway, restore the validated file backup to the configured state
path with mode `0600`, set `control_plane.backend` to `file`, run
`config validate`, and restart. A partial temp file is not a committed state;
never delete the last valid backup until the replacement has been verified. The
Redis-backed admin security-store adapter and cross-replica breaker
synchronization remain outside this control-plane implementation.

## 5. Admin surface, events, and logs

Admin API access is privileged. Keep it loopback-only unless OIDC, TLS, and external network controls are configured. Provider keys, pooled credentials, custom header values, and proxy URLs are write-only and redacted on Admin reads and exports. The event feed uses Server-Sent Events; clients can reconnect with `Last-Event-ID`, and should refresh the snapshot after an epoch change or sequence gap.

The default app-owned file sink is bounded and mode `0600`; console mirroring is rate-limited. Request/response bodies are not written as normal access-log fields. Preserve log rotation limits and restrict access to the log directory.

### OIDC and session operations

When `admin.oidc.enabled` is true, human users sign in through the configured Authorization Code + PKCE flow. Before rollout:

1. Register the exact HTTPS redirect URI `/admin/auth/oidc/callback` with the issuer and provision the named client-secret environment variable through the service manager/secret injector.
2. Configure `issuer_url`, `client_id`, `audience`, and a reviewed `role_mappings` allowlist. The discovered issuer must match exactly. Do not map unreviewed provider groups to `admin`.
3. Set `emergency_access_enabled` deliberately. It is not a provider-outage fallback; with OIDC enabled, the API key is not accepted if discovery/login is unavailable. Old configs missing this explicit flag load with emergency access disabled.
4. Place `security_store_path` on durable local storage. Relative paths are resolved against the config directory; the default is `security/nexaroute-security.db`, whose missing `security/` child is created mode `0700`. For an explicit absolute path, create a private parent when needed (for example, `install -d -m 0700 /var/lib/nexaroute`). Startup creates a 0600 bbolt file and fails if an existing parent is not private or the file is a symlink/group/world-readable. Back it up using access controls appropriate for identity and audit records. Do not share this DB over NFS or across gateway instances.
5. Use one gateway instance or sticky routing for the full OIDC login round-trip: pending state/nonce/PKCE/browser-binding data is process-local. Server-side sessions and audit records survive restarts on the same host, but distributed session/audit coordination is not implemented.

The UI session cookie is opaque, `HttpOnly`, `SameSite=Lax`, scoped to `/admin/`, and `Secure` on HTTPS. The server stores only its hash and the verified identity/roles. `session_ttl_seconds` sets the absolute lifetime; `idle_timeout_seconds` revokes idle sessions. Browser mutations and logout require same-origin validation and the strict CSRF cookie/header pair. Login rotates any prior session; logout revokes the server-side record before clearing cookies. A changed OIDC policy fingerprint invalidates old sessions.

Structured audit records contain request ID, actor, action, target, method/path, outcome, status, permission, and authentication method; no body, code, token, cookie, or session ID is recorded. Retention is limited by both `audit_retention_days` and `audit_max_events`. The security-store HTTP API is admin-only. A failed required audit write returns 503 and stops the protected action; investigate disk permissions, free space, filesystem health, and DB lock state rather than disabling audit. A recovery from provider outage requires a trusted operator to change configuration explicitly, restart, and restore OIDC after break-glass use; do not rely on silent fallback.

## 6. Graceful shutdown and rollback

NexaRoute handles `SIGINT` and `SIGTERM` by stopping new accepts, canceling background probes, and draining in-flight work. The drain window is based on the request timeout and the longest configured stream idle timeout, with a minimum safety window; it is not a fixed 15-second deadline.

A binary rollback must account for encrypted config compatibility. Do not point a pre-encryption binary at an `nxs1` config. Prefer restoring a pre-upgrade config/key pair. If the only usable source is encrypted, use the current binary's explicitly gated decrypt command to create a temporary private plaintext config, protect it as mode `0600`, and follow the older binary's documented config path; after the rollback window, remove the plaintext file according to the host's accepted data-sanitization policy (unlinking alone may not securely erase flash-backed storage). Test this process before relying on it in production.

For unresolved network exposure, keyring, CSRF/session, RBAC, or provider-egress limits, consult [docs/KNOWN_GAPS.md](KNOWN_GAPS.md) and [docs/SECURITY.md](SECURITY.md).

## 7. TLS, mTLS, and config preflight
The shared listener can be configured for HTTPS with `tls.enabled`, certificate/key paths, and optional `client_ca_file`. TLS 1.2 is the minimum; certificate files are re-read for new handshakes. `require_client_cert_admin` and `require_client_cert_data_plane` independently protect the Admin and `/v1/*` routes. Keep the Admin API key enabled: a client certificate does not replace authorization.

Before rollout, use the read-only checks with the exact config path:
```bash
nexaroute config validate --config /etc/nexaroute/config.json
nexaroute config diff --config /etc/nexaroute/config.json --against /etc/nexaroute/previous.json
nexaroute config dry-run --config /etc/nexaroute/config.json
```
`diff` emits paths/status only and never secret values. `dry-run` opens no listener and sends no probes. Loading a legacy plaintext config may perform the documented encrypted migration side effect; this is the only expected on-disk mutation.

## 8. Phase status and deliberate boundaries

The Phase 1 hardening work is complete on `main`: encrypted secret storage,
TLS/mTLS, browser CSRF checks, and the read-only `config validate|diff|dry-run`
preflight commands are implemented and covered by repository gates. Current
coverage and WP4 acceptance status are recorded in PR #224 and
[`docs/reports/WP4B_REVIEW.md`](reports/WP4B_REVIEW.md); do not use older
baseline percentages in this manual as a current measurement.

Phase 2 is **in progress**. The opt-in video runtime is partial: its local fake
provider is for development/tests, and no real external provider is claimed
verified. PR #224 implements OIDC-backed `viewer`/`operator`/`admin` sessions,
route/method RBAC, and a local durable security/audit store; live
external-IdP interoperability is unverified. The provider egress policy and
local control-plane file backend are merged, while distributed sessions/audit,
live Postgres/Redis runtime interoperability, and real external KMS/keyring
interoperability remain unverified. Do not treat transport security or client
keys as an egress-policy or distributed-state substitute.

Virtual-key tenant/project/team scopes are evaluated as parent-to-child
intersections. A wildcard on a child key cannot widen a configured parent
allow-list; unknown or inconsistent references deny the request. Keep these
scopes explicit in configuration and test the deny path before exposing the
data plane.

The video runtime is opt-in through `video.enabled` and fails closed unless
`video.auth_token_env` names a non-empty bearer token in the process environment.
It uses a bounded worker queue and an atomic local JSON job store; that store is
single-process and is not a multi-node coordination mechanism. On restart,
queued jobs are recovered, known provider job IDs are resumed, and ambiguous
submissions without a provider job ID require manual action rather than risking
a duplicate billable submission. Shutdown cancels and joins the worker pool
before returning. Use the fake provider only for local development/tests; no
real external provider integration is claimed verified. See
[`docs/VIDEO_GATEWAY.md`](VIDEO_GATEWAY.md) and [`docs/KNOWN_GAPS.md`](KNOWN_GAPS.md)
for the supported surface and remaining limits.
