# Live Visual Agent Convergence Matrix

| ID | Requirement | Code evidence | Test evidence | Visual evidence | Status |
|---|---|---|---|---|---|
| LVA-001 | Existing Overview surface | `internal/httpapi/web/visual-agent.js` replaces the existing `.topology` contents | `scripts/test-browser-e2e.py` PASS; live E2E PASS | `lva-idle.png`, `lva-active-failover.png`, `lva-success.png` | PASS |
| LVA-002 | Go remains routing authority | tracker consumes events only; no route decisions or candidate ordering in JS | `go test ./...`, vet, race PASS | real failover shows backend-selected deployment sequence | PASS |
| LVA-003 | One frontend event transport | existing `startSSE()` is the only stream reader and forwards events to tracker | SSE tests + full verify PASS | live request appears in same Overview/Activity stream | PASS |
| LVA-004 | Real request correlation | tracker requires `request_id`; backend emits it on attempt/result events | `scripts/test-live-visual-agent.py` PASS | `lva-active-failover.png`, `lva-activity-correlation.png` | PASS |
| LVA-005 | Calm idle | tracker renders `Waiting for requests` and no agent without an execution | live E2E idle assertion PASS | `lva-idle.png` | PASS |
| LVA-006 | Evidence-backed result states | mappings for `route_attempt`, `route_ok`, `route_fail`, failover, exhaustion, timeout, disconnect, stream failure | real local first-failure → second-success E2E PASS | active failover and success screenshots | PASS |
| LVA-007 | Unknown/malformed safe | invalid events and missing request IDs are ignored | JS syntax/build + full verify PASS | not applicable | PASS |
| LVA-008 | Bounded state and cleanup | max 64 executions and bounded terminal settle timer | full race/verification PASS; browser DOM/heap evidence captured | `lva-performance.json` | PASS |
| LVA-009 | Reduced motion | CSS media query and static semantic rendering | live E2E launched with reduced motion and asserted preference | idle/static evidence generated under reduced motion | PASS |
| LVA-010 | Accessible equivalent | `role=status` live region in topology | live E2E inspected status and Activity correlation | screenshots include semantic status surface | PASS |
| LVA-011 | Duplicate historical events safe | sequence dedupe, cursor `since`, gap snapshot resync | SSE resume tests + live E2E PASS | reconnect-safe implementation; no duplicate visual replay observed | PASS |
| LVA-012 | Secret safety | visual tracker reads only request/event identity, deployment, status, latency | existing secret-safety suite + full verify PASS | no secret visual artifact | PASS |
| LVA-013 | Attempt-start truth | request path emits dashboard-only `route_attempt` at real primary/hedge boundaries | focused HTTP suite + live failover E2E PASS | two distinct real attempts captured | PASS |
| LVA-014 | Epoch-aware reconnect | `events.Event` carries process epoch; frontend detects epoch changes and resyncs | epoch bus test + SSE/full verify PASS | not applicable | PASS |

**Release conclusion:** the previously blocked LVA implementation gates are complete for this checkpoint. Human visual review and normal PR/CI review remain required before merge or release.
