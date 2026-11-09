# Live Visual Agent Evidence

Deterministic local-upstream acceptance and its technical evidence are present
for this checkpoint. The acceptance runner
[`scripts/test-live-visual-agent.py`](../../../../scripts/test-live-visual-agent.py)
starts the real gateway and two local upstreams; the first fails and the second
succeeds. Current artifacts in this directory are:

| Artifact | What it records |
|---|---|
| `lva-idle.png` | Actual application Overview in the idle state. |
| `lva-active-failover.png` | The active request and the first-upstream failure/failover sequence. This is not an exhausted terminal-failure screenshot. |
| `lva-success.png` | The same request after successful failover. |
| `lva-activity-correlation.png` | The request correlated in Activity. |
| `lva-performance.json` | Captured DOM/heap metrics and the reduced-motion preference asserted by the browser test. |

The runner verifies the real `route_attempt`, `route_fail`, `failover`, and
`route_ok` events; the
[convergence matrix](../../../../docs/reports/live-visual-agent-convergence.md)
records the technical evidence gates as passing. This directory does not contain
a separate screenshot of an exhausted terminal failure. Human visual review is
still required: concept art is not acceptable evidence, and the release gate
must not be marked complete before review of the actual application screenshots.
