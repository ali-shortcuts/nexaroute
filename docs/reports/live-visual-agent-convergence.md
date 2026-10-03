# Live Visual Agent Convergence Matrix

| ID | Requirement | Code evidence | Test evidence | Visual evidence | Status |
|---|---|---|---|---|---|
| LVA-001 | Existing Overview surface | `internal/httpapi/web/visual-agent.js` replaces the existing `.topology` contents | `scripts/test-browser-e2e.py` PASS | Current browser screenshot covers control-plane flow, not a live request | PASS |
| LVA-002 | Go remains routing authority | tracker consumes events only; no route decisions or candidate ordering in JS | `go test ./...`, `go vet ./...`, race PASS | Not applicable | PASS |
| LVA-003 | One frontend event transport | existing `startSSE()` is the only stream reader and forwards events to tracker | SSE tests + full verify PASS | Not captured | PASS |
| LVA-004 | Real request correlation | tracker requires `request_id` and deployment comes from event | backend suite PASS; deterministic browser request not yet added | Live request screenshot absent | BLOCKED |
| LVA-005 | Calm idle | tracker renders `Waiting for requests` and no agent without an execution | existing browser acceptance PASS for no activity | Dedicated idle LVA screenshot absent | BLOCKED |
| LVA-006 | Evidence-backed result states | mappings for `route_ok`, `route_fail`, failover, exhaustion, timeout, disconnect, stream failure | Go regression PASS; focused browser request/failover proof absent | Success/failure/failover captures absent | BLOCKED |
| LVA-007 | Unknown/malformed safe | invalid events and missing request IDs are ignored | JS syntax/build PASS; dedicated JS unit harness absent | Not applicable | PASS |
| LVA-008 | Bounded state and cleanup | max 64 executions and bounded terminal settle timer | full race/verification PASS; browser burst measurement absent | Performance artifact absent | BLOCKED |
| LVA-009 | Reduced motion | CSS media query and static semantic rendering | CSS/syntax PASS; browser reduced-motion assertion absent | Reduced-motion capture absent | BLOCKED |
| LVA-010 | Accessible equivalent | `role=status` live region in topology | existing shell browser PASS; dedicated accessibility assertion absent | Accessibility capture absent | BLOCKED |
| LVA-011 | Duplicate historical events safe | sequence dedupe in tracker; historical reset does not animate | SSE resume backend tests PASS | Reconnect visual proof absent | BLOCKED |
| LVA-012 | Secret safety | visual tracker reads only request/event identity, deployment, status, latency | existing secret-safety suite + full verify PASS | No secret visual artifact required | PASS |
| LVA-013 | Attempt-start truth | deliberately not invented; gap documented | backend event audit completed | Not applicable | NOT IMPLEMENTED |
| LVA-014 | Epoch-aware reconnect | current `events.Event` has no epoch; contract gap documented | current SSE tests cover seq/Last-Event-ID only | Not applicable | BLOCKED |

**Release conclusion:** this checkpoint is not release-complete. Human visual acceptance, deterministic real-request/failover evidence, performance measurements, and epoch/cursor reconciliation remain required.
