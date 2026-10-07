# NexaRoute v0.16.1 Operations Manual

Version: v0.16.1
Target: Gateway Operators & Administrators

---

## 1. Startup & Configuration

### 1.1 Running the Gateway
NexaRoute is compiled as a single self-contained binary `nexaroute` (or via `cmd/gateway`).

```bash
# Start with default configuration file (configs/config.example.json or NEXAROUTE_CONFIG_PATH)
./nexaroute

# Specify custom configuration file
./nexaroute -config /etc/nexaroute/config.json
```

### 1.2 Environment Variables
- `NEXAROUTE_CONFIG_PATH`: Path to JSON configuration file (default: `configs/config.example.json`).
- `NEXAROUTE_LISTEN_ADDR`: Data plane listen address (default: `:8080`).
- `NEXAROUTE_ADMIN_LISTEN_ADDR`: Control plane admin listen address (default: `127.0.0.1:8081`).
- `NEXAROUTE_ADMIN_API_KEY`: Administrative API key for securing `/admin/*` endpoints.

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
  "admin": {
    "listen": "0.0.0.0:8081",
    "api_key": "YOUR_SECURE_ADMIN_KEY_HERE"
  }
}
```

Include the key in administrative requests:
```bash
curl -H "X-Admin-API-Key: YOUR_SECURE_ADMIN_KEY_HERE" http://localhost:8081/admin/api/snapshot
```

### 3.2 Secret Management & Redaction
- All credentials stored in configuration files or uploaded via admin APIs are masked on output.
- Reading provider details via `GET /admin/api/providers` returns `********` for API keys.
- Configuration export (`GET /admin/api/config/export`) automatically redacts secrets by default.

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
