# Live Visual Agent Performance Evidence

## Verified in this checkpoint

The frontend has no continuous Visual Agent RAF while idle: the renderer only creates CSS motion when a real request-correlated execution is active, and terminal state uses one bounded settle timer. Execution retention is capped at 64 objects. The existing SSE bus and Go event ring remain bounded and the full verification gate passed the repository race suite.

The following gates passed after the implementation: `node --check` for both frontend scripts, `git diff --check`, `go test ./...`, `go vet ./...`, full `./scripts/verify.sh`, real Chromium control-plane acceptance, and linux amd64/arm64 builds.

## Not yet measured

The required browser measurements for idle CPU, active CPU, burst aggregation, DOM/node count, long-run memory, background-tab pause, and pagehide RAF cleanup were not available from the existing browser acceptance script. A deterministic real upstream request/failover harness and running-app visual capture are still needed. These remain **BLOCKED**, not inferred as passes.
