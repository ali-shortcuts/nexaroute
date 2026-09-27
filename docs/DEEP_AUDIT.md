# Deep audit: decision plane, evaluation plane and support packages

Date: 2026-09-27
Base: `e05823b47c8faeeb10b20bd207fc209dfa12f099` (canonical `main`)
Branch: `arena/01a0e13e-nexaroute`

Scope of this session (nothing else was touched):

| Package | Area |
| --- | --- |
| `internal/decision` (`chain.go`, `orchestrator.go`) | chain execution, budget, deadline, fail-open |
| `internal/decision/jev` | external intelligence trust boundary |
| `internal/eval` | replay evaluation plane |
| `internal/evallive` | live physical-deployment evaluation |
| `internal/desktop` | local CLI lifecycle (lock, browser launch, readiness) |
| `internal/scorecards` | provenance-bearing scorecards |
| `internal/taskprofile` | deterministic task classification |
| `internal/usage` | cumulative token/cost accounting |

Explicitly out of scope (owned by other hardening sessions):
`internal/decision/remote/transport.go`, `internal/compat/errors.go`, core
failover logic in `internal/httpapi`, router/health/probe core, and the web UI.

## Method

1. Read every production file in scope end to end; invariants, state transitions,
   locking, cancellation, resource lifetime, trust boundaries, secrets, bounds,
   error handling, persisted state, races and panic paths.
2. Generated package coverage and targeted the untested paths.
3. Wrote a **failing reproduction test before every fix**; each test was run and
   observed to fail on the pre-fix code, then to pass on the fixed code.
4. Full gate: `scripts/verify.sh` (gofmt, `go test -count=1 ./...`,
   `go test -shuffle=on -count=10 ./...`, `go vet ./...`,
   `go test -race -count=1 ./...`, `go test -race -shuffle=on -count=3 ./...`,
   short fuzz targets, benchmark smoke, linux amd64 + arm64 builds), plus
   `scripts/stress.sh`.
5. No test was weakened, deleted or relaxed to accommodate a fix.

## Real bugs found and fixed

### 1. Jev provider: any `http://` base_url disabled the external transport protection — HIGH

`internal/decision/jev/provider.go`.

The constructor sniffed the scheme of the operator-supplied `base_url` and, for
any `http://` URL, set `AllowHTTPForTest` and then rebuilt the client with
`remote.NewTestClient`, which installs `NewTestTransport` — a transport with **no
SSRF dial guard** and no HTTPS requirement.

Consequences, reachable from configuration alone:

* `base_url: "http://169.254.169.254/latest/meta-data/…"` sent the Jev API key in
  an `Authorization: Bearer …` header to a cloud metadata endpoint.
* any plaintext `http://` base_url put the same credential on the wire in the
  clear.
* `config.Validate` deliberately permits `http://` to any host ("we will enforce
  in transport layer"), so nothing upstream blocked it.

The same function also sliced `cfg.BaseURL[:7]` to sniff the scheme, which
**panics** for any base_url shorter than seven bytes.

Fix: `newRemoteClient` now treats `base_url` as a trust boundary. HTTPS plus the
SSRF host guard stay in force for every config-driven provider; the only
relaxations are (a) an HTTP client injected by Go code — the test-only path — and
(b) a plaintext **loopback** endpoint, which is the documented local-testing
override in `config.base_url` and the only way the existing integration tests can
address an `httptest` server. Loopback targets get the loopback-permitting
transport; everything else keeps the production transport. The hot-reload path
(`UpdateFromConfig`) now uses the same helper, so a reload cannot move a provider
onto a plaintext remote or link-local endpoint. The fixed-offset slicing is gone,
so short base_urls are rejected instead of panicking.

Blocked after the fix: `http://<remote-host>`, `http://169.254.169.254`,
`http://[::ffff:169.254.169.254]`, `http://10.0.0.1`, `http://192.168.1.1`,
`http://127.0.0.1.nip.io`, `https://169.254.169.254`, `https://10.0.0.1`.
Still allowed: `http://127.0.0.1:port`, `http://localhost:port`, `http://[::1]:port`,
`https://<public host>`.

Regression tests: `internal/decision/jev/provider_baseurl_test.go`
(`TestNewProviderRejectsInsecureBaseURLFromConfig`,
`TestNewProviderAcceptsLoopbackBaseURL`,
`TestNewProviderKeepsInjectedTestClient`,
`TestUpdateFromConfigRejectsInsecureBaseURL`,
`TestNewProviderShortBaseURLDoesNotPanic`).

Reproduction before the fix:

```
--- FAIL: TestNewProviderRejectsInsecureBaseURLFromConfig/http://169.254.169.254/latest/meta-data/
    base_url "http://169.254.169.254/latest/meta-data/" produced a usable client without an injected test client
--- FAIL: TestNewProviderShortBaseURLDoesNotPanic
    NewProvider panicked on a short base_url: runtime error: slice bounds out of range [:7] with length 2
```

### 2. Evaluation: `case_timeout_ms` did not bound grading — MEDIUM

`internal/eval/runner.go`.

`RunWithExecutor` derived a per-case context, handed it to the executor, and then
**cancelled it before grading** and graded on the parent context instead:

```go
outcome, err := exec.Execute(caseCtx, c)
if cancel != nil { cancel() }          // released before grading
...
verdicts = append(verdicts, e.Evaluate(ctx, c, outcome))   // parent ctx
```

A judge — the one evaluator the runner does not control, and the only one that can
block — therefore never saw the case timeout. A judge that ignores cancellation
could hold an evaluation run (and the admin request driving it) open indefinitely
whatever `case_timeout_ms` said.

Fix: the per-case context is created inside a per-case closure, covers execution
*and* grading, and is cancelled when the case finishes. Grading uses it, so a
judge is bounded by the same deadline as the executor.

Regression test: `TestRunnerCaseTimeoutBoundsEvaluators`
(`internal/eval/runner_bounds_test.go`); before the fix the run took 6.0s for
three cases with `case_timeout_ms=20`, after the fix 0.02s.

### 3. Evaluation: unbounded `error_type` entered the durable record — LOW

`internal/eval/runner.go`.

`Outcome.Validate` bounded output, test logs, tool arguments and stream errors but
not `error_type`, and the runner copies it verbatim into `CaseResult.ErrorType` —
the field the bounded-by-count store keeps, the state file persists and the admin
surface returns. With a 4 MiB request body and 512 retained runs, a run of
operator-supplied error strings is retained with no per-field ceiling.

Fix: `Outcome.Validate` bounds `error_type` to `MaxReasonBytes`, the same bound the
package already applies to every other recorded string.

Regression tests: `TestOutcomeValidateBoundsErrorType`,
`TestRunnerRejectsArtifactWithOversizedErrorType`.

### 4. Usage: upstream token counts could wrap the cumulative counters — MEDIUM

`internal/usage/usage.go`.

`Record` added the upstream-reported `prompt_tokens`/`completion_tokens` directly
into int64 accumulators. Those values are untrusted input; a broken or hostile
provider returning `9223372036854775807` (or values whose running sum crosses
MaxInt64) wrapped the counters negative, corrupting per-deployment totals,
gateway-wide totals and every cost estimate derived from them, and desynchronised
the `Retain` rebalance.

Fix: every reported count is clamped to `maxTokensPerRecord` (2^40 — far above any
real response) before it is accumulated, and the accumulators add with saturation
so no sequence of reports can ever wrap them.

Reproduction before the fix:

```
counters wrapped to negative: prompt=-4611686018427387904 completion=-4611686018427387904
```

Regression tests: `TestTrackerSaturatesAbsurdTokenCounts`,
`TestTrackerMaxInt64DoesNotWrap`, `TestTrackerRetainKeepsTotalsConsistentAfterOverflow`.

### 5. Task profile: `Valid()` accepted a non-finite confidence — LOW

`internal/taskprofile/profile.go`.

`Valid()` range-checked confidence with `p.Confidence < 0 || p.Confidence > 1`.
NaN compares false against both bounds, so a profile with `Confidence: NaN` was
reported valid even though the field is documented as a finite `[0,1]` value and
the decision plane rejects non-finite confidence from providers.

Fix: reject NaN and Inf explicitly.

Regression test: `TestTaskProfileValidRejectsNonFiniteConfidence`.

## Additional test added (no production change)

`internal/evallive`: `TestIsolation_LiveExecutorDoesNotImportRoutingState`. The
Phase H isolation guard in `internal/eval` covers `eval` and `scorecards` but
cannot cover `evallive`, which depends on `internal/providers` by design. This
test asserts the part of the guarantee that still holds: live evaluation imports
no routing package (router, route, decision, health, probe, cache) and must
dispatch through `internal/providers` — no second client architecture.

## Non-bug observations (documented, deliberately not changed)

* `Orchestrator.Decide` copies the whole chain map on every call, before the
  `mode=off` short circuit. The documented "zero overhead" off path therefore
  allocates. Harmless in practice (`applyDecisionPlane` short-circuits before
  `Decide` when mode is off) — left alone to keep the diff to real fixes.
* `Budget.MaxProviderCalls == 0` means "no calls" on the single-provider path
  (`TestOrchestrator_BudgetMaxProviderCalls` pins this) but "default to chain
  length" on the hybrid path. Inconsistent but intentional and tested; the HTTP
  wiring always sets it explicitly.
* Chain affinity/exhausted results report the **chain id** as `ProviderID`, while
  a selected step reports the provider id. Bounded and config-validated, but
  semantically mixed.
* On the provider-error path the orchestrator appends two reason codes to the
  provider's own list without re-running `ValidateResult`, so a result can carry
  up to `MaxReasonCodes+2` codes. All values stay canonical, so event and metric
  cardinality are unaffected.
* `DecisionRequest.Candidates` shares its backing array with the caller across
  every chain step; a provider that mutated it in place could perturb the failover
  order. No in-tree provider does, and the contract only forbids returning
  unknown candidates — a per-step clone would add an allocation to the hot path.
* `jev.BuildOpaqueMapping` maps duplicate candidate ids onto one opaque id, so a
  duplicate in the eligible set produces an external request with a repeated id
  and one missing slot. Selection stays correct (the mapping is request-local and
  unknown choices are rejected); the router does not produce duplicate ids.
* `jev.ParsedResponse.RawData` retains the upstream `guidance`/`probabilities`
  fields (bounded by the 64 KiB response cap, never copied into a result, event or
  trace).
* `providers.LiveComplete` sets `UpstreamAttempt: true` even when `DoPath` fails
  before dialling (semaphore wait, credential cooldown, invalid endpoint), so
  `evallive.UpstreamCalls()` can count an attempt that never left the process.
  `internal/providers` is outside this session's scope.
* `desktop.OpenBrowser` passes the bare command name to `exec.Command` after
  `lookPath` resolved it, so the resolution happens twice; `WaitReady` builds a
  client per call. Both are safe — no shell, and callers always pass a bounded
  context.

## Architecture invariants re-verified

* Phase H scorecards remain routing-neutral: `scorecards` and `eval` import no
  routing package, and no routing package imports them (structural test in
  `internal/eval/isolation_test.go`, now extended to `evallive`).
* Replay evaluation performs no I/O (`ReplayExecutor.UpstreamCalls() == 0`,
  asserted per run).
* Live evaluation runs only through an evaluation-isolated adapter twin, only when
  `evaluation.live_enabled` is set, and only for one explicitly selected
  deployment; it never touches health, affinity, cache or routing state.
* Decision-plane failures fail open everywhere they did before: the new Jev
  rejection path leaves a provider unregistered (`Health` → unavailable →
  abstain, order preserved) rather than failing a request.
* No secrets in logs or events: the Jev key is never logged, `String()` and
  `HasAPIKey()` expose only a boolean, and evaluation events carry counts only.
* Bounded state: run store, scorecard registry, chain traces, verdict counts and
  usage rows all remain bounded, now including per-field bounds on retained
  evaluation error strings.

## Verification

`scripts/verify.sh` and `scripts/stress.sh` pass on the branch (gofmt clean,
`go vet ./...` clean, `go test ./...`, `-shuffle=on -count=10`, `-race`,
`-race -shuffle=on -count=3`, short fuzz targets, benchmark smoke, linux/amd64
and linux/arm64 builds).
