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

## 4. Health and status

- `GET /healthz` is a liveness endpoint.
- `GET /readyz` reports whether an eligible deployment is ready.
- `GET /admin/api/health` reports deployment-level health details and requires the configured Admin access boundary.

A deployment must satisfy the configured health and capability requirements before it is eligible for traffic. See [configuration](CONFIGURATION.md) for probe cadence, leases, and recovery behavior.

## 5. Admin surface, events, and logs

Admin API access is privileged. Keep it loopback-only unless a strong key and external network/TLS controls are in place. Provider keys, pooled credentials, custom header values, and proxy URLs are write-only and redacted on Admin reads and exports. The event feed uses Server-Sent Events; clients can reconnect with `Last-Event-ID`, and should refresh the snapshot after an epoch change or sequence gap.

The default app-owned file sink is bounded and mode `0600`; console mirroring is rate-limited. Request/response bodies are not written as normal access-log fields. Preserve log rotation limits and restrict access to the log directory.

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
preflight commands are implemented and covered by the repository gates. The
measured repository test coverage is 79.4%; see [`docs/KNOWN_GAPS.md`](KNOWN_GAPS.md)
for the below-80% package list and the next coverage targets.

Phase 2 has **not started**. In particular, RBAC/SSO, strict provider egress
policy, runtime-integrated durable store adapters, and OS keyring or external
KMS integration are not implemented. Do not treat Phase 1 transport security
or client keys as an enterprise identity, egress-policy, or distributed-state
substitute.
