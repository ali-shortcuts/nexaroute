# Live Visual Agent Performance Evidence

## Measurement run

The deterministic real-upstream browser acceptance is `scripts/test-live-visual-agent.py`. It launches the actual gateway, two local HTTP upstreams, a real Chromium page with reduced motion, and a real request that fails on the first target and succeeds on the second. Measurements were captured in `live-visual-agent/evidence/lva-performance.json`.

| State | DOM nodes | JS heap bytes | Semantic status |
|---|---:|---:|---|
| Idle | 159 | 2,132,760 | `idle` |
| Failover active | 217 | 3,040,809 | `failover` |
| Success | 211 | 3,159,623 | `success` |

The active state is bounded to one detailed execution in this flow. The production tracker caps retained executions at 64 and uses a four-second terminal settle timer. There is no continuous Visual Agent RAF while idle; the renderer uses CSS motion only for a live execution and is static under reduced motion.

## Verification

The following gates pass after the completed implementation: `node --check` for both frontend scripts, Python compilation for the live acceptance script, `git diff --check`, `go test ./...`, `go vet ./...`, race tests, fuzz checks, existing Chromium control-plane acceptance, live real-upstream LVA acceptance, and Linux amd64/arm64 builds.

These measurements are browser evidence for the implemented bounded slice, not a claim of a production load-test ceiling. Human review and larger burst/long-run tests remain appropriate before release.
