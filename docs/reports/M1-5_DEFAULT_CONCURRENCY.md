# M1-5 — Default Provider Concurrency Integration Test

**Date:** 2026-10-11

**State:** DONE — verification passed

## Test added

`internal/httpapi/stress_test.go::TestDefaultProviderMaxConcurrencyAllowsMoreThan32Inflight` exercises the actual gateway HTTP handler with a mock OpenAI-compatible upstream.

- Starts with `config.Default()` and does not set `MaxConcurrency` or override the default routing/admission settings.
- `testGateway` applies defaults; the test asserts that the configured provider cap is **256**.
- Launches **64** simultaneous chat-completion requests behind a start gate.
- The mock upstream increments an atomic in-flight counter and blocks responses until the test observes a peak **greater than 32**, so requests remain in flight while that threshold is measured.
- Releases the upstream, waits for all requests, and asserts every response is HTTP 200.
- This test is unconditional (not gated by `NEXAROUTE_STRESS`).

## Results

- Standalone targeted race run: **PASS**, with an observed upstream peak of **47** concurrent requests across the 64-request run; all 64 responses passed the test's HTTP 200 assertions.
- Full `./scripts/verify.sh`: **VERIFY PASS**. Its mandatory clean race pass included `internal/httpapi`; browser acceptance, fuzz checks, and linux/amd64 + linux/arm64 builds also passed.

The measured peak can vary with scheduler timing; the acceptance criterion is strictly `>32`, not a fixed peak value.

## Evidence

- Test source: `internal/httpapi/stress_test.go`.
- Focused race result: `docs/reports/m1-5-target-race.raw.txt`.
- Full verification output: `docs/reports/m1-5-verify.raw.txt`.

No production behavior, configuration defaults, dependencies, workflow files, or `scripts/verify.sh` were changed.
