# NexaRoute Real-Time Event Stream Contract

Version: v0.12.0
Endpoint: `/admin/api/events` (Server-Sent Events)

---

## 1. Overview & Architecture

NexaRoute provides a real-time event bus (`internal/events`) that broadcasts gateway operational events to administrative clients and the embedded UI dashboard over Server-Sent Events (SSE).

### 1.1 Key Principles
- **Monotonic Sequence Ordering**: Events are uniquely identified and ordered by `(epoch, seq)`.
- **At-Least-Once Delivery & Idempotency**: SSE reconnection may replay previously seen events; consumers must handle duplicate events safely.
- **Non-Blocking Producers**: Event bus publishing uses bounded ring buffers. A slow SSE client will drop intermediate events without blocking request proxying or routing paths.
- **Explicit Gap Recovery**: If sequence numbers skip unexpectedly, clients must request a fresh state snapshot via GET `/admin/api/snapshot` to reconcile local UI state.

---

## 2. Event Payload Schema

All events emitted over SSE share a canonical JSON payload structure:

```json
{
  "epoch": 1727539200,
  "seq": 1042,
  "type": "route_decision",
  "timestamp": "2026-09-28T12:00:00.123456Z",
  "data": {
    "request_id": "req-987654321",
    "model": "claude-3-5-sonnet",
    "provider_id": "anthropic-primary",
    "deployment_id": "dep-claude-primary-us-east",
    "status": "success",
    "latency_ms": 142.5,
    "prompt_tokens": 512,
    "completion_tokens": 128
  }
}
```

### 2.1 Core Header Fields

| Field | Type | Description |
|---|---|---|
| `epoch` | `int64` | Process boot epoch (Unix timestamp in seconds at gateway startup). |
| `seq` | `int64` | Monotonically increasing sequence number starting at `1` for the current epoch. |
| `type` | `string` | Categorical event type name (see Section 3). |
| `timestamp` | `string` | RFC3339 formatted event generation timestamp with sub-second precision. |
| `data` | `object` | Bounded payload object specific to the event type. |

---

## 3. Supported Event Types

### 3.1 `route_decision`
Emitted upon completion of a routing decision and proxy attempt.

```json
{
  "request_id": "req-12345",
  "provider_id": "openai-secondary",
  "deployment_id": "dep-gpt4o-eu-west",
  "status": "success | failover | error",
  "latency_ms": 85.2,
  "prompt_tokens": 250,
  "completion_tokens": 50,
  "estimated_cost": 0.00125
}
```

### 3.2 `health_change`
Emitted when a deployment's health state changes (e.g., ready, cooling_down, degraded, or unreachable).

```json
{
  "deployment_id": "dep-claude-primary-us-east",
  "old_status": "ready",
  "new_status": "cooling_down",
  "reason": "http_503_service_unavailable",
  "cooldown_until": "2026-09-28T12:05:00Z"
}
```

### 3.3 `config_reload`
Emitted when the gateway reloads runtime configuration from disk or environment.

```json
{
  "version": "v0.12.0",
  "source": "admin_api",
  "deployments_count": 8,
  "timestamp": "2026-09-28T12:01:00Z"
}
```

### 3.4 `keepalive`
Transport-level keepalive frame sent every 15 seconds to prevent intermediate proxy timeouts. Carry no business state.

```json
{
  "type": "keepalive",
  "epoch": 1727539200,
  "seq": 1043,
  "timestamp": "2026-09-28T12:00:15Z"
}
```

---

## 4. Reconnection & Resynchronization Rules

1. **Reconnection Header**: Clients pass `Last-Event-ID` or query parameter `last_seq` containing the last processed sequence number upon reconnect.
2. **Epoch Check**: If the server's `epoch` differs from the client's cached `epoch`, the gateway process has restarted. The client MUST discard existing sequence numbers and fetch a new full snapshot (`GET /admin/api/snapshot`).
3. **Gap Detection**: If `received_seq > last_seq + 1` within the same epoch, an event buffer overflow occurred. The client MUST trigger a full snapshot refresh.
4. **Polling Fallback**: If SSE connection fails repeatedly (3+ retries), dashboard falls back to polling `GET /admin/api/snapshot` every 5 seconds. Snapshot polling deduplicates against SSE state using `(epoch, seq)`.

---

## 5. Security & Secret Redaction Invariants

- **No Secret Leakage**: Event payloads MUST NEVER include authorization keys, bearer tokens, custom secret headers, or raw prompt/response text bodies.
- **Authentication**: When `admin.api_key` is set, `/admin/api/events` enforces authentication using `X-Admin-API-Key` or `Authorization: Bearer <key>`. Unauthenticated SSE requests fail immediately with HTTP `401`.
