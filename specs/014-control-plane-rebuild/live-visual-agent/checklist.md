# Live Visual Agent Acceptance Checklist

| ID | Requirement | Code evidence | Test evidence | Visual evidence | Status |
|---|---|---|---|---|---|
| LVA-001 | Existing Overview surface | `visual-agent.js` replaces `.topology` | existing + live browser acceptance | idle/active/success captures | PASS |
| LVA-002 | Backend owns routing truth | tracker has no routing authority | Go, vet, race | backend-selected failover | PASS |
| LVA-003 | One event transport | one `startSSE()` owner | SSE/full verify | shared Activity/Overview stream | PASS |
| LVA-004 | Real request correlation | request ID required | live real-request E2E | Activity correlation screenshot | PASS |
| LVA-005 | Calm idle | no execution means no agent | live idle assertion | `lva-idle.png` | PASS |
| LVA-006 | Evidence-backed terminal states | real event mapping | live failover E2E | failover/success captures | PASS |
| LVA-007 | Unknown/malformed safe | safe normalization | syntax/full verify | n/a | PASS |
| LVA-008 | Bounded state | max 64 + settle cleanup | full verify + metrics | performance JSON | PASS |
| LVA-009 | Reduced motion | media query + static semantics | live reduced-motion browser run | reduced-motion idle capture | PASS |
| LVA-010 | Accessible equivalent | live `role=status` region | live browser run | status and Activity captures | PASS |
| LVA-011 | Duplicate historical events safe | seq dedupe + cursor/gap resync | SSE resume/full verify | no replay in live run | PASS |
| LVA-012 | No secret leakage | only privacy-safe fields consumed | secret-safety/full verify | no secret fields shown | PASS |
| LVA-013 | Attempt-start truth | backend `route_attempt` at real boundaries | focused HTTP + live failover E2E | two distinct attempts | PASS |
| LVA-014 | Epoch-aware reconnect | process epoch + frontend resync | epoch test + full verify | n/a | PASS |

Human visual acceptance remains a required review gate; implementation and evidence gates are complete for this checkpoint.
