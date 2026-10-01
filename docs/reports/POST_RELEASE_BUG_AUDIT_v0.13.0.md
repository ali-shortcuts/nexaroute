# NexaRoute v0.13.0 — Post-release Bug Audit

**Status:** local hardening pass complete; remote PR/CI pending  
**Baseline:** `main` / `v0.13.0` / `b650608b77e2339459f55c0a688869a215440fa4`  
**Branch:** `bugfix/v0.13-production-hardening`

## Scope and method

This pass started from a fresh clone of current `main` and verified the published `v0.13.0` tag before changing code. It exercised the provider lifecycle, protocol matrix, routing/failover tests, streaming/cancellation tests, browser E2E, explicit stress/soak suites, installer lifecycle, idle behavior, secret surfaces, weak-package coverage, and local regression gates.

## A. Confirmed bugs fixed

| Bug ID | Area | Severity | Reproduction | Root cause | Fix | Regression test | Status |
|---|---|---:|---|---|---|---|---|
| HARDEN-001 | Context-window error parsing | Medium | `ParseContextTokens("maximum context length is 8192 tokens, however you requested 9000 tokens")` returned `8192000,9000000` on the production baseline. | The parser checked whether the complete regex match contained `k`; the word `tokens` itself contains the letter `k`, so plain counts were multiplied by 1000. | Capture the optional `k` suffix as its own regex group and scale only when that group is present. | `TestParseContextTokens` now covers both `128k/200k` and plain `8192/9000` values. | Fixed locally; awaiting PR CI/merge. |

## B. Confirmed bugs still open

| Bug ID | Area | Severity | Evidence / boundary | Status |
|---|---|---:|---|---|
| None confirmed in the exercised production paths. | — | — | The audit does not claim that unexercised external-provider behavior is defect-free. | No confirmed open bug. |

Documented product/security boundaries remain in `docs/KNOWN_GAPS.md`: plaintext-at-rest config secrets, no built-in TLS/RBAC, single-process runtime state, provider/admin SSRF trust boundary, and opt-in long-form stress/soak tests.

## C. Intentional product boundaries

- Real-provider traffic was not generated because no external credentials were available in the environment. Deterministic mock upstreams were used instead.
- `nexaroute_http_requests_total` is a total HTTP middleware counter and therefore increases by one when the `/metrics` endpoint itself is queried. Idle validation accounts for the observation request and checks runtime events/logs for phantom traffic.
- Historical reports named for older releases remain historical artifacts; the current release identity is `v0.13.0`.
- `gitleaks` was not installed in the sandbox, so the repository secret scan was not independently executed locally. GitHub Security/CodeQL gates remain required for the PR.

## D. Improvements that are not bugs

- Increasing coverage in weak packages is useful but not itself a product defect. Current local coverage samples: `internal/core` 100.0%, `internal/route` 94.8%, `internal/feature` 79.6%, `internal/compat` 64.5%, and `cmd/gateway` 22.9%.
- The old `FINAL_ACCEPTANCE_REPORT.md` and dated competitor comparison contain historical release snapshots; they should not be interpreted as current release metadata. The README now links this current audit report.

## E. Tests that could not be executed and why

- Real OpenAI-compatible, Anthropic-compatible, and Gemini provider smoke traffic: no secure external credentials were available; deterministic mock providers covered the same protocol/error paths.
- Local gitleaks scan: `gitleaks` is not installed in the sandbox; remote Security/CodeQL remains mandatory.
- Docker build/runtime: to be recorded after checking Docker availability on the current execution device.
- A literal 30–60 minute idle run is not practical in the bounded task window; a 60-second equivalent was run, plus the repository's explicit stress/soak suites and repeated browser acceptance.

## Verification evidence

### Baseline and local functional coverage

- Fresh clone of `main` at `b650608b77e2339459f55c0a688869a215440fa4`.
- Tag `v0.13.0` verified as current published release.
- Dedicated branch pushed early: `bugfix/v0.13-production-hardening`.
- `go test ./... -count=1`: PASS.
- `go test ./... -race`: PASS.
- `go vet ./...`: PASS.
- JavaScript syntax and all embedded frontend tests: PASS.
- Short compatibility fuzz run: PASS.
- `gofmt` and `git diff --check`: PASS.
- Protocol matrix: all four non-stream and four streaming protocol directions passed.
- Failure matrix: auth, quota/rate-limit, model-not-found, 5xx and malformed-success cases passed across protocol directions.
- Claude failover, tool round-trip, cancellation, count-token and provider secret tests passed.

### Browser / runtime / lifecycle

Three repeated browser E2E runs passed, covering startup/navigation, localization/theme, provider create/edit/secret-preserve/secret-replace/delete, discovery failure/manual model, simple route lifecycle, advanced-route protection, settings, Connect output, observability, pause/resume and no JS page errors.

Explicit stress and soak suites passed:

- router scale;
- probe/recovery;
- event state;
- HTTP admission;
- concurrent log rotation;
- evaluation plane;
- repeated stress rounds;
- hot reload soak;
- recovery soak;
- race-enabled soak.

Installer verification passed after preparing the release fixtures:

- clean install;
- PATH and architecture selection;
- checksum/download rejection;
- browser/headless readiness;
- duplicate/port exclusion;
- upgrade/reinstall;
- restart persistence;
- write-only secrets;
- crash recovery;
- installer lifecycle E2E.

Idle observation passed with zero runtime events, stable log line count, and no phantom traffic during the observation interval; the only HTTP counter delta was the expected second `/metrics` observation request.

## Final acceptance matrix

| Gate | Local result | Remote result |
|---|---:|---:|
| Unit/integration | PASS | pending PR |
| Race | PASS | pending PR |
| Vet/format/diff | PASS | pending PR |
| Frontend/browser E2E | PASS, repeated 3x | pending PR |
| Stress/soak | PASS | pending PR |
| Installer lifecycle | PASS | pending PR |
| CodeQL | baseline release green; rerun required | pending PR |
| govulncheck | baseline release green; rerun required | pending PR |
| Secret scan | local tool unavailable | pending PR |
| Docker | pending device check | pending PR |
| Release build/checksum | v0.13.0 baseline verified | do not publish a new release until PR gates pass |

## Delivery plan

The fix is intentionally isolated to the parser and its regression test. The branch must receive a real PR, pass mandatory CI/Security checks, and only then be considered for a patch release recommendation such as `v0.13.1`. No new release is created merely because this audit is complete.
