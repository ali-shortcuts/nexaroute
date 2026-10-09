# NexaRoute Operations Manual

Target: Gateway Operators & Administrators

---

## 1. Startup & Configuration

### 1.1 Running the Gateway
NexaRoute is compiled as a single self-contained binary `nexaroute` (or via `cmd/gateway`).

```bash
# Start with the per-user config file (or NEXAROUTE_CONFIG)
./nexaroute

# Specify custom configuration file
./nexaroute -config /etc/nexaroute/config.json
```

### 1.2 Environment Variables
- `NEXAROUTE_CONFIG`: Path to JSON configuration file (default: the platform user-config directory under `nexaroute/config.json`).
- `NEXAROUTE_LISTEN`: Listener address override (default: `127.0.0.1:8080`; UI, Admin API and data plane share this listener).
- `NEXAROUTE_ADMIN_KEY`: Admin API key override.
- `NEXAROUTE_ADMIN_BIND_LOCAL_ONLY`: boolean override for loopback-only Admin binding.

---

## 2. Health Monitoring & Status Interpretation

### 2.1 Health Status Endpoints
- `GET /healthz`: Gateway liveness endpoint (returns HTTP `200 OK` when process is alive).
- `GET /readyz`: Readiness endpoint (returns HTTP `200 OK` if at least one eligible deployment is ready).
- `GET /admin/api/health`: Admin status endpoint returning deployment-level breakdown.

### 2.2 Deployment Health States
- **`ready`**: Deployment is healthy, verified, and accepting routing traffic.
- **`cooling_down`**: Temporary failure state; traffic routed away until cooldown period expires.
- **`degraded`**: Experiencing elevated latency or partial error rates; deprioritized in selection.
- **`unreachable`**: Hard failure (TCP connection refusal, invalid auth credentials, DNS failure).
- **`retired`**: Disabled or removed via runtime configuration update.

---

## 3. Admin Surface & Security Controls

### 3.1 Admin Auth Setup
To expose admin APIs beyond loopback (`127.0.0.1`), configure `admin.api_key`:

```json
{
  "listen": "0.0.0.0:8443",
  "admin": { "bind_local_only": false, "api_key": "YOUR_SECURE_ADMIN_KEY_HERE" },
  "tls": {
    "enabled": true,
    "cert_file": "tls/server.crt",
    "key_file": "tls/server.key",
    "client_ca_file": "tls/client-ca.crt",
    "require_client_cert_admin": true,
    "require_client_cert_data_plane": false
  }
}
```

The UI, Admin API, and data plane share `listen`; there is no independent Admin listener. TLS applies to the complete listener. Certificate/key and optional CA paths are resolved relative to the config directory when not absolute. Use a valid server certificate and restrict access to the private key. Files are checked at startup and re-read for each new TLS handshake; atomically replacing a certificate/key pair takes effect for new connections without restarting. TLS 1.2 is the minimum.

When `require_client_cert_admin` is enabled, a client certificate validated against `client_ca_file` is required for `/admin/api/*`. `require_client_cert_data_plane` independently requires one for `/v1/*`. If enabling either option, install the CA and client certificates on authorized callers before switching the gateway. Keep `admin.api_key` enabled for remote Admin access: mTLS authenticates the TLS peer but does not replace the Admin API's authorization key.

Include the key in administrative requests:
```bash
curl --cacert /etc/nexaroute/tls/server.crt \
  --cert /etc/nexaroute/tls/admin-client.crt \
  --key /etc/nexaroute/tls/admin-client.key \
  -H "X-Admin-API-Key: YOUR_SECURE_ADMIN_KEY_HERE" \
  https://localhost:8443/admin/api/snapshot
```

With the example TLS configuration, use `https://<gateway-host>:8443/admin/api/snapshot`. For the browser UI, state-changing Admin API requests use a same-origin CSRF token. The gateway issues an `HttpOnly`, `SameSite=Strict` CSRF cookie; it is marked `Secure` on HTTPS (or when a trusted TLS-terminating proxy supplies `X-Forwarded-Proto: https`). The dashboard obtains the associated token and sends it in `X-NexaRoute-CSRF`. Configure a trusted proxy to overwrite, not append, forwarded-protocol headers. Stateless API clients that send no browser `Origin`/`Referer` and no CSRF cookie continue to authenticate with the Admin key and do not use a browser session.

### 3.2 Secret Management & Redaction
- All credentials stored in configuration files or uploaded via admin APIs are masked on output.
- Reading provider details via `GET /admin/api/providers` returns `********` for API keys.
- Configuration export (`GET /admin/api/config/export`) automatically redacts secrets by default.

### 3.3 Config checks before rollout

Run the read-only checks with the exact config path before restarting or changing listener settings:

```bash
nexaroute config validate --config /etc/nexaroute/config.json
nexaroute config diff --config /etc/nexaroute/config.json --against /etc/nexaroute/previous.json
nexaroute config dry-run --config /etc/nexaroute/config.json
```

`validate` checks schema and TLS files; `diff` shows changed field paths without rendering credential values; `dry-run` validates and prepares configured adapters but opens no listener and sends no probes/upstream requests. Config loading may perform a required on-disk format migration for a legacy config when such a migration is enabled; otherwise the commands do not alter runtime settings. Exit codes are 0 for valid/no differences, 1 for invalid config or preparation failure, and 2 for usage errors. Keep the previous config and a tested rollback procedure when changing listener or certificate settings.

---

## 4. Real-Time Event Stream & SSE Recovery

### 4.1 Connecting to Event Stream
Administrative clients subscribe to real-time events via Server-Sent Events (SSE):
```bash
curl -N -H "X-Admin-API-Key: YOUR_KEY" http://localhost:8081/admin/api/events
```

### 4.2 Handling Disconnects & Sequence Gaps
- The dashboard automatically tracks `(epoch, seq)`.
- If an SSE disconnection occurs, client reconnects passing `Last-Event-ID`.
- If a process restart is detected (epoch mismatch) or sequence numbers skip, the dashboard performs an atomic refresh using `GET /admin/api/snapshot`.

---

## 5. Graceful Shutdown & Rollback Procedures

### 5.1 Graceful Shutdown
NexaRoute handles `SIGINT` and `SIGTERM` signals gracefully:
1. Ingress listener stops accepting new incoming connections.
2. In-flight requests are granted up to `shutdown_drain_timeout_seconds` (default: 15s) to complete.
3. Health micro-probe loops and SSE subscriber channels close cleanly.

### 5.2 Safe Rollback Guidance
- **Backward Compatibility**: v0.16.1 maintains backward-compatible configuration loading with earlier NexaRoute configuration schemas.
- **Rollback Process**:
  1. Stop v0.12.0 gateway instance (`kill -TERM <pid>`).
  2. Restore binary to previous version (e.g. `v0.11.0`).
  3. Restart instance using existing JSON configuration file. No database migrations or persistent state cleanups are required.
