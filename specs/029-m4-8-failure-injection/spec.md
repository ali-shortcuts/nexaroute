# M4-8 — Failure-injection and soak suite with documented failure semantics

## Already covered before this PR (referenced, not duplicated)

- Soak: TestSoakConcurrentTrafficAndHotReload (httpapi),
  TestSoakRepeatedRecoveryCyclesReleaseState (probe), soak.yml.
- Control-plane failure modes: TestManagerLastKnownGood,
  TestManagerFailClosedDoesNotUseStaleSnapshot (controlplane).
- Store-level integrity failures: securitystore chain tamper suites,
  backup/restore hardening suites (M4-3), Redis torn-snapshot (M4-2b).

## This PR

- internal/httpapi failure-injection tests driving the real request
  path with faulting upstreams:
  F1 upstream connection refused (documented error + health failure
  accounting), F2 failover from a refused primary to a healthy
  secondary, F3 upstream hang bounded by routing.request_timeout_ms,
  F4 mid-stream connection cut (partial-stream semantics, gateway
  stays healthy), F5 admin security store locked by another handle →
  startup surfaces the store error (fail-closed) + store-level
  bounded lock timeout.
- A flapping-upstream soak test (env-gated like the existing soaks):
  alternating healthy/failing upstream phases under sustained mixed
  traffic; asserts resolution of every request, consistency of the
  exported counters, and goroutine counts returning to baseline.
- docs/FAILURE_SEMANTICS.md: the fault → observable-behavior →
  evidence map for operators, including the rows covered elsewhere.
