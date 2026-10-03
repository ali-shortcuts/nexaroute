# Live Visual Agent Acceptance Checklist

| ID | Requirement | Code evidence | Test evidence | Visual evidence | Status |
|---|---|---|---|---|---|
| LVA-001 | Existing Overview surface | `internal/httpapi/web/visual-agent.js` replaces `.topology` contents | browser shell regression | pending real request capture | PASS |
| LVA-002 | Backend owns routing truth | tracker only consumes events; no routing API calls | static inspection + existing Go suite | pending | PASS |
| LVA-003 | One event transport | existing `startSSE()` calls tracker | existing SSE tests | pending | PASS |
| LVA-004 | Real request correlation | tracker requires `request_id` | targeted browser request needed | pending | PASS |
| LVA-005 | Calm idle | `Waiting for requests`, no active agent | browser idle assertion needed | pending | PASS |
| LVA-006 | Evidence-backed terminal states | mapping for `route_ok`, failure, failover | event-path tests needed | pending | PASS |
| LVA-007 | Unknown/malformed safe | normalization ignores invalid input | JS tests needed | n/a | PASS |
| LVA-008 | Bounded state | max 64 plus terminal settle cleanup | stress/performance test pending | pending | PASS |
| LVA-009 | Reduced motion | CSS + `setMotion` support | browser reduced-motion test pending | pending | PASS |
| LVA-010 | Accessible equivalent | `role=status` live region | browser accessibility test pending | pending | PASS |
| LVA-011 | Duplicate historical events safe | sequence dedupe | reconnect test pending | pending | BLOCKED |
| LVA-012 | No secret leakage | no secret fields read by tracker | existing security tests | pending | PASS |
| LVA-013 | Attempt-start truth | no current consistent backend event | backend audit | n/a | NOT IMPLEMENTED |
| LVA-014 | Epoch-aware reconnect | no epoch field in current event wire | SSE contract mismatch | n/a | BLOCKED |
