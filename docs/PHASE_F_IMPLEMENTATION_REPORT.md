# Phase F — Implementation Report

Date: 2026-09-25. Branch `arena/01a0d9e5-nexaroute` (from `ba91c53`).
Normative contract: `docs/PHASE_F_EXTERNAL_DECISIONS.md`.

## 1. What was built

Phase F delivered the secure external DecisionProvider infrastructure plus
exactly one real adapter (Jev `model-route`), on top of a new generic
decision foundation owned by NexaRoute (the audit found no Phase D/E
artifacts in this checkout, so the minimal foundation is part of this
phase — see `docs/PHASE_F_CURRENT_STATE_NOTE.md`).

**New production code (~1 560 lines):**

- `internal/decision/` — provider contract, request/result types,
  registry, orchestrator (affinity → single call → validate → reorder),
  validator, constraints, closed reason codes, `local` + `policy`
  built-ins.
- `internal/decision/remote/` — external transport: single POST, no
  retry, redirects refused, TLS ≥ 1.2, 32 KiB / 64 KiB / 4 KiB caps,
  typed secret-safe errors.
- `internal/decision/jev/` — the single external adapter: request
  builder, opaque-ID mapper, envelope parser, provider (production
  endpoint `https://www.jevai.org/api/v1/decisions/model-route`,
  host allow-listed).
- `internal/httpapi/decision_wiring.go` — runtime build (production +
  test-endpoint options), candidate/feature mapping, `applyDecision`
  seam, atomic runtime swap with metrics carry, admin snapshot rows.
- `internal/config/config.go` — `decision{mode,provider,timeout_ms}` +
  `decision_providers[]` with full validation (mode/provider coherence,
  enabled-entry requirement, privacy default, 16-entry bound, credential
  resolution env-first).
- Seams: `applyDecision` hooks in `openai.go`, `anthropic.go`,
  `canonical_path.go` (band length ≥ 2, fail-open); atomic decision
  pointer + pre-disk runtime swap in `server.go`; `decision` +
  `external_decision_providers` in `admin.go`; type×outcome counters +
  latency summary in `metrics.go`; `SessionPin` exposure in
  `internal/router/router.go`.
- `configs/config.example.json` — annotated `decision` block + disabled
  `jev-main` entry. `scripts/smoke-jev.sh` — optional manual live smoke
  (`JEV_API_KEY=...`, network-gated, never in CI).

**New tests (~2 750 lines, 114 test functions + benchmarks + fuzz seeds):**

- `internal/decision/*_test.go` — constraints, validator, registry,
  orchestrator (short-circuit, single-choice, reorder, leapfrog
  rejection, abstain, error mapping, timeout), 2000-case adversarial
  property test (selection always in-band, order preserved),
  local-only benchmarks.
- `internal/decision/jev/*_test.go` — mapper (opaque IDs, 32 KiB fit,
  oversize-without-body), response parser (envelope, guidance dropped,
  mapping-only output), provider (opaque/no-retry/typed errors,
  timeout, abstain, oversize request/response, refused connection),
  parse fuzz seeds + mapping benchmarks.
- `internal/decision/remote/*_test.go` — headers, missing-key no-call,
  size caps, HTTP-error body hiding, single attempt, redirect-without-
  auth-forwarding, pre/mid-flight cancel, secret-free errors.
- `internal/config/decision_config_test.go` — defaults, validation
  matrix (mode×provider coherence, enabled requirement, timeout bounds,
  privacy default, 16-entry bound), credential resolution.
- `internal/httpapi/decision_test.go` — mock-Jev harness, runtime
  build/status, mapping, E2E (select / fail-open / affinity /
  violation), admin, metrics, hot-reload (disable + bad-reload
  rejection), credential coherence across reload.
- `internal/httpapi/decision_privacy_test.go` — prompt/key/remote-error
  canaries, opaque-ID proof, credential independence.
- `internal/httpapi/decision_protocol_test.go` — cross-protocol
  (chat/anthropic/responses), alias+auto vs direct, fallback order,
  max-attempts, capability/health boundary, model-health separation
  (both directions), local regression, session-pin seam, cache-hit no-call regression.

## 2. Strict-check matrix (Phase F §61)

| # | Check | Result | Evidence |
|---|---|---|---|
| 1 | Generic provider ownership (no vendor in generic layer) | PASS | `internal/decision/*.go` (non-jev) reference no vendor; Jev confined to `internal/decision/jev` |
| 2 | Metadata-only privacy (no prompt/params/PII to external) | PASS | `TestPromptCanaryNeverLeavesGateway`, `TestTaskSummaryMetadataOnly`, `TestDescribeCandidateNeverLeaksIdentity` |
| 3 | Opaque candidate IDs (no physical IDs externalized) | PASS | `TestOpaqueIDsHidePhysicalDeployments`, `TestDecideValidSelectionOpaque`, `TestBuildMappingOpaqueAndBounded` |
| 4 | Primary-selection guardrails (affinity, single-choice, earliest-first, membership, band preservation, single call) | PASS | Orchestrator suite + `TestPropertySelectedAlwaysInPrimaryBand` (2000 cases) + `TestAssistedAffinityPreventsExternalCall` |
| 5 | Assisted mode off/local/assisted + fail-open no-retry single-call | PASS | `TestAssistedFailureFailsOpen`, `TestPostJSONNoRetryOnFailure`, `TestDecideHTTPFailuresTyped` (one hit each), `TestLocalModesUnchanged`, `TestCacheHitMakesNoExternalCall` |
| 6 | SSRF/TLS/redirect safety | PASS | Host allow-list (`TestHealthMissingKey` + validation tests), redirects refused (`TestPostJSONRejectsRedirectWithoutForwardingAuth`), TLS ≥ 1.2 default verifier |
| 7 | Hot-reload atomic swap (pre-disk build, metrics carry, bad-reload rejection) | PASS | `TestHotReloadDisablesExternalCalls`, `TestHotReloadRejectsBadDecisionConfig`, `TestCredentialSnapshotCoherenceAcrossReload` |
| 8 | Bounded observability (type×outcome labels only, no high-cardinality) | PASS | `TestMetricsExposeBoundedDecisionCounters`, `TestAdminSnapshotExposesSafeDecisionStatus`, `TestRemoteErrorBodyCanaryContained` |
| 9 | Canary tests (prompt, key, remote-error) | PASS | `decision_privacy_test.go` — all three canaries absent from payload/events/metrics/admin/client response |
| 10 | Limits (≤32 KiB request pre-send, 64 KiB response, 512 candidates, key bound) | PASS | `TestBuildMappingMaxMetadataSetFits32KiB`, `TestDecideOversizeRequestSendsNothing` (0 hits), `TestDecideOversizeResponseFailsOpen`, `TestPostJSONRequestTooLargeMakesNoCall` |
| 11 | Cross-protocol parity (chat/anthropic/responses → same deployment) | PASS | `TestCrossProtocolSameSelection` (all `up-b`, 3 Jev hits) |
| 12 | VE/direct equivalence (alias + auto vs direct ID) | PASS | `TestVirtualVsDirectSameSelection` |
| 13 | Fallback order + max-attempts unchanged | PASS | `TestFallbackExactOrder` ([up-b,up-a,up-c]), `TestMaxAttemptsUnchanged` (Jev pick first, exactly 2 attempts) |
| 14 | Model-health separation (external failure touches nothing; executed failure records normally) | PASS | `TestExternalFailureDoesNotTouchModelHealth`, `TestExecutedSelectionRecordsNormalModelHealth` |
| 15 | Capability/health boundary (ineligible never sent) | PASS | `TestCapabilityAndHealthBoundary` (0 Jev hits), `TestIneligibleCandidatesExcludedFromJevPayload` |
| 16 | Full suite + race + repo gate | PASS | `go test ./...`, `go test -race ./...`, and `scripts/verify.sh` (gofmt, `-count=10` shuffled, vet, `-race -count=3` shuffled, fuzz seeds, linux amd64+arm64 builds) all green |

Notes on checks 6/12: "VE" = NexaRoute's virtual catch-alls (`auto` and
client aliases). The Jev contract was verified against the official Jev
docs (`POST https://www.jevai.org/api/v1/decisions/model-route`,
`{code,message,data}` envelope, 32 KiB cap) plus a live community
request/response example; the adapter sends `task + candidates(+priorities/
constraints/stakes)` and accepts only `data.decision` values from its own
opaque mapping.

## 3. Verification log

Toolchain: no Go in sandbox, so go1.23.12 was bootstrapped from source
(go1.4.3 → go1.17.13 → go1.21.13 → go1.23.12; `CGO_ENABLED=0` for the
go1.4.3 leg only, working around a gcc-12 PIE link failure in the 2015-era
build). All commands with the bootstrapped toolchain:

- `gofmt -l .` → clean; `bash -n scripts/*.sh` → clean.
- `go vet ./...` → clean.
- `go test -count=1 ./...` → all 20 packages ok.
- `go test -count=1 -race ./...` → all 20 packages ok, no data races.
- `scripts/verify.sh` → `VERIFY PASS` (includes `-count=10` shuffled
  tests, `-race -count=3` shuffled tests, short fuzz checks, and
  linux/amd64 + linux/arm64 builds).

Failures found and fixed during verification (all test-side, no
production-code defects): a missing `+` in a raw-string concatenation, the
admin path (`/admin/api/snapshot`), admin auth (`x-admin-key` +
`BindLocalOnly=false` in fixtures), the responses ingress not echoing
`X-Gateway-Deployment` (resolved via upstream-model capture instead), and
5 s `httptest.Server.Close` stalls in blocking-server tests (release
channels).

## 4. Files changed

Production: `internal/config/config.go`,
`internal/httpapi/{server,openai,anthropic,canonical_path,admin,metrics,
decision_wiring}.go`, `internal/router/router.go`,
`configs/config.example.json`, `scripts/smoke-jev.sh` (new),
`internal/decision/{constraints,provider,request,result,registry,
orchestrator,validator,reason,local/provider,policy/provider,
remote/{client,errors,limits},jev/{provider,request,response,mapper}}.go`
(all new). Tests: `internal/decision/{...,}/*_test.go` (new),
`internal/httpapi/decision_{,privacy_,protocol_}test.go` (new),
`internal/config/decision_config_test.go` (new). Docs:
`docs/PHASE_F_CURRENT_STATE_NOTE.md` (pre-existing audit),
`docs/PHASE_F_EXTERNAL_DECISIONS.md` (new),
`docs/PHASE_F_IMPLEMENTATION_REPORT.md` (this file).

## 5. Verdict

**Phase F: PASS.** All sixteen strict checks hold, the repository gate
(`scripts/verify.sh`) is green, and the work stops at Phase F — no Phase G
provider chains were started.
