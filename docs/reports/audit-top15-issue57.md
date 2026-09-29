# Audit re-verification: issue #57 top-15 on main final (issue #63 follow-up)

- Base commit audited: `377768e8fd9f95419a468af437e852004b8436c8` (`origin/main` at audit time; `git rev-parse HEAD` == `git rev-parse origin/main`).
- Audit branch (report only): `opencode/issue63-20260929165027` — no code fixes in this step, no `main` push, no merge, no `.github/workflows/*` change, no secret change.
- Source of the top-15 list: issue #63 body items 1–15 (retry of the read-only audit from issue #57). Each item below was re-verified with a real command on the base commit. No imaginary bugs, no fabricated metrics.
- Method: `grep`/`sed` on the checked-out tree, `go test -cover` for coverage numbers, `go build` / `go vet` / `go test` / `go test -race` gates, `node --check` for the two dashboard JS bundles.

## Acceptance gates on the base commit (this branch adds docs only)

| Gate | Command | Result (this run) |
|---|---|---|
| build | `go build ./...` | exit 0 |
| vet | `go vet ./...` | exit 0 |
| unit/integration | `go test -count=1 ./...` | exit 0, 27 packages `ok`, zero FAIL |
| race | `go test -race -count=1 ./...` | exit 0, 27 packages `ok`, zero FAIL |
| JS syntax | `node --check internal/httpapi/web/app.js && node --check internal/httpapi/web/control-plane-v2.js` | both OK |
| coverage spot-check | `go test ./internal/compat/ ./internal/route/ ./internal/feature/ ./internal/core/ ./cmd/gateway/ -cover` | compat 51.6%, route 56.3%, feature 59.4%, core 30.4%, gateway 12.9% — exactly the numbers quoted in the issue |
| browser E2E | `scripts/verify.sh` browser leg (`python3 scripts/test-browser-e2e.py`) | SKIPPED in this runner: `python3 -c 'import playwright'` → `ModuleNotFoundError: No module named 'playwright'` (chromium binary exists at `/usr/bin/chromium`). `verify.sh` itself emits `WARN ... skipping browser acceptance` in that case. Full `scripts/verify.sh` was not run to completion here because it repeats the full suite 10x + 3x race + fuzz; the equivalent gates above were run directly and are green. |
| Security/CI | not runnable from this checkout (`gh` has no token: `gh: To use GitHub CLI ... set the GH_TOKEN environment variable`); must be read from the PR checks page after push. No workflow files touched. |

## Dedup / prior-coverage analysis (required first pass)

- Intra-list duplicates: none. The 15 items are pairwise distinct (different files/lines/concerns). Checked by mapping each item to its file+line anchor (see per-finding sections).
- PRs #64–#68 equivalence: `gh` is unusable in this runner (no `GH_TOKEN`), so PR numbers could not be resolved via API. Instead every remote branch was diffed against `origin/main` (`git diff --stat origin/main..origin/<branch>`):
  - `chore/ci-flake-and-cleanup`: only `.gitignore`, `README`, `docs/reports/CI_FLAKE_INVESTIGATION.md`, doc moves, `scripts/test-browser-e2e.py` — no overlap with any of the 15 findings.
  - `feat/dashboard-live-ring-v2`: `web/app.js` ring rendering + `dashboard_ring_test.go` — does not fix null-safety/XSS/SSE-backoff/listener-cleanup/apiFetch-dedup; no overlap counted.
  - `feat/dashboard-telemetry-panels-v2`: telemetry panels + event bus — no overlap counted.
  - `fix/health-api-and-logs-v2`: health API + production log events — no overlap counted.
  - `fix/provider-edit-persistence-v2`: provider edit prefill + masked key — does not enforce proxy-credential/SSRF rejection or hide `api_key_env`; no overlap counted.
  - `fix/sse-resume-and-benchmarks-v2`: SSE resume tests + benchmarks + competitor docs — does not add client-side backoff/jitter/counter/toast; no overlap counted.
  - `fix/failover-resilience-evidence`: failover evidence tests A1–A6 — no overlap counted.
  - Conclusion: none of the A/B-track branches covers the top-15. They must NOT be used to close any finding.
- `fix/audit-findings-batch-1` (`origin/fix/audit-findings-batch-1`, tip `c05baa1dee46eb53addb6f6626578508273b5ea2`, parent `d326c6e`): diff vs `origin/main` is 20 files, +1039/−58, and implements fixes for 14 of the 15 findings (everything except #11, which would require a workflow-file change). Evidence: `git diff --stat origin/main..origin/fix/audit-findings-batch-1` (full list in repo; key paths: `internal/config/config.go`, `internal/httpapi/web/app.js`, `internal/httpapi/web/control-plane-v2.js`, `internal/httpapi/server.go`, `internal/httpapi/routing_helpers.go`, `internal/decision/jev/provider.go`, `internal/httpapi/eval_live_test.go`, `internal/httpapi/admin_secret_test.go`, `internal/httpapi/audit_fixes_test.go`, `internal/{compat,route,feature,core}/...`, `cmd/gateway/...`, `docs/KNOWN_GAPS.md`, `docs/reports/audit-batch-1-gate.txt`).
  - Therefore this step does NOT re-implement fixes (that would duplicate that branch and risk divergence). This step contributes only the re-verification report plus a decomposition into small independent traceable work items (below), each mapped to the existing fix commit where one exists, or marked OPEN where it does not (#11, plus any partial hardening left).
  - Paths A/B merge status at audit time: the A/B-track branches listed above are NOT merged into `origin/main` (main tip is `377768e`; each branch has 1+ commits on top). This report does not merge anything.

## Finding details (all CONFIRMED on `377768e`)

Conventions: `CONFIRMED` = reproduced on the base commit with the quoted command. `COVERED-BY batch-1` = fixed on `origin/fix/audit-findings-batch-1` (see traceability table). Proposed fix + regression test describe the minimal independent work item (what a small PR for that finding alone would contain).

### F1 (high) Frontend null-safety — CONFIRMED
- File/line: `internal/httpapi/web/app.js:19` (`const $ = q => document.querySelector(q)`), ~265 `$(...)` call sites (`grep -c '\$(' internal/httpapi/web/app.js` → `265`), e.g. lines 35, 94, 127–135, 142–144, 151, 156–161.
- Repro: `grep -n '\$(' internal/httpapi/web/app.js | head -n 20` shows unguarded dereferences such as `$('#adminKey').value`, `$('#toast').textContent`, `$('#' + b.dataset.tab).classList`. `node --check` passes (syntax ok) but there is no runtime guard: one renamed/missing element ID throws inside the render loop.
- Severity: high (single missing ID can kill the whole dashboard render loop).
- Proposed fix (independent item AUDIT-01): make `$` return a safe stub (or assert after DOM ready) and isolate per-section render so one section cannot break the others.
- Regression test: dashboard JS smoke test asserting render survives a missing element ID (batch-1 covers this behaviorally; a standalone PR would add a browser/JS-harness test).
- Evidence: counts above, measured on base commit. Status: COVERED-BY batch-1 (`web/app.js` null-safe `$` + render isolation).

### F2 (high) `cliSnippet` unescaped `innerHTML` XSS path — CONFIRMED
- File/line: `internal/httpapi/web/app.js:671` (`function cliSnippet(kind)`), interpolation of `base`/`veModel`/`veList` into `s.body` (lines 671–739), rendered unescaped at `742` (`$('#cliBody').innerHTML = ... <pre>${s.body}</pre>`). Title/note are escaped via `esc()` but `s.body` is not.
- Repro: `sed -n '671,760p' internal/httpapi/web/app.js` — `export ANTHROPIC_BASE_URL=${base}`, `export ANTHROPIC_MODEL=${veModel}`, `# virtual endpoints: ${veList}` flow verbatim into `innerHTML`. A `public_model` containing `<img onerror=...>` would execute.
- Severity: high (stored-XSS path if `public_model` is attacker-influenced).
- Proposed fix (AUDIT-02): escape `veModel`/`veList`/`base` in `cliSnippet()` or render the snippet via `textContent`.
- Regression test: unit/harness test feeding a malicious `public_model` and asserting no raw `<` survives in `cliBody`.
- Evidence: code excerpt above. Status: COVERED-BY batch-1 (escapes the three values).

### F3 (medium) `server.go:980` `panic(recovered)` edge — CONFIRMED with nuance
- File/line: `internal/httpapi/server.go:977-987`, specifically `980: panic(recovered)`.
- Repro: `sed -n '950,1009p' internal/httpapi/server.go` shows the recover wrapper already emits `internal_panic` + `request_id=... handler_panic` log + HTTP 500 (`errorJSON(sw, 500, "internal gateway error")`) for all panics EXCEPT `http.ErrAbortHandler`, which is re-panicked. That re-panic is required by the `net/http` contract (an `ErrAbortHandler` must propagate or the connection handling breaks).
- Severity: medium (edge-case correctness / policy clarity).
- Proposed fix (AUDIT-03): keep the re-panic but document it as intentional fail-closed policy (do NOT convert it to a 500), and add a regression test for the non-abort path (500 + `internal_panic` event).
- Regression test: handler-panic test asserting 500 + bus event (batch-1 `audit_fixes_test.go` does this).
- Evidence: excerpt above. Status: COVERED-BY batch-1 with documentation (not removal).

### F4 (medium) Proxy/base credential + SSRF enforcement missing — CONFIRMED
- File/line: `internal/config/config.go:1066-1074` (base/proxy URL validation), `1687` (`func ValidateProviderConfig`).
- Repro: `sed -n '1060,1085p' internal/config/config.go` — validation only checks `url.Parse` scheme (`http`/`https`) and non-empty host. `sed -n '1687,1700p'` shows `ValidateProviderConfig` delegates to `cfg.Validate()` with no `user:pass@`, private-IP, or metadata-target checks. `grep -rn 'strict-config\|STRICT_CONFIG' --include='*.go' --include='*.js'` → empty (no enforcement flag either).
- Severity: medium (docs-only SSRF/credential warning, not enforced).
- Proposed fix (AUDIT-04): reject `user:pass@` in `base_url`/`proxy_url`, reject/flag private-IP and cloud-metadata proxy targets in `ValidateProviderConfig`.
- Regression test: `config_test.go` cases for credential-in-URL rejection, metadata target rejection, private-IP proxy rejection (batch-1 adds these; one existing fixture using an embedded proxy credential had to be updated).
- Evidence: excerpts above. Status: COVERED-BY batch-1.

### F5 (medium) Silent config defaults — CONFIRMED
- File/line: `internal/config/config.go` probe validation `997-1023`, legacy `public_model` normalization `637` + `794`, routing/probe defaults; no `--strict-config` flag anywhere (`grep` empty, see F4).
- Repro: `grep -n 'strict\|max_tokens\|public_model\|probe\.' internal/config/config.go | head -n 40` shows range checks but no warning when `probe.max_tokens==0`, routing/probe sections are empty, or legacy `public_model` auto-migration fires.
- Severity: medium (silent behavior changes confuse operators).
- Proposed fix (AUDIT-05): log a stderr warning for each silent default/migration; add `--strict-config` / `NEXAROUTE_STRICT_CONFIG` turning them into hard errors.
- Regression test: config tests asserting warnings + strict-mode errors.
- Evidence: grep output above. Status: COVERED-BY batch-1.

### F6 (medium) `routeContext` zero-timeout `WithCancel` ownership — CONFIRMED (docs gap, callers OK)
- File/line: `internal/httpapi/routing_helpers.go:24-29`.
- Repro: `grep -rn 'routeContext' internal/httpapi/*.go` → 3 callers (`anthropic.go:148`, `canonical_path.go:484`, `openai.go:160`). Each is immediately followed by `defer routeCancel()` (verified: `grep -n 'routeCancel\|defer.*cancel'` shows `defer routeCancel()` at `anthropic.go:150`, `canonical_path.go:486`, `openai.go:162`). So no leaked-cancel bug today; the gap is that the `streaming || timeout <= 0 → WithCancel` branch has no documented cancel-ownership contract, and a future caller could forget `defer cancel()`.
- Severity: medium (latent leak risk).
- Proposed fix (AUDIT-06): document cancel ownership on `routeContext` + add a caller-guard test.
- Regression test: test asserting every `routeContext` call site is paired with a cancel (batch-1 adds docs + guard test).
- Evidence: grep output above. Status: COVERED-BY batch-1.

### F7 (medium) Test coverage below target — CONFIRMED (exact numbers)
- Files: `internal/compat`, `internal/route`, `internal/feature`, `internal/core`, `cmd/gateway`.
- Repro: `go test ./internal/compat/ ./internal/route/ ./internal/feature/ ./internal/core/ ./cmd/gateway/ -cover` → `compat 51.6%, route 56.3%, feature 59.4%, core 30.4%, gateway 12.9%` — byte-for-byte the numbers quoted in the issue. No fabrication.
- Severity: medium.
- Proposed fix (AUDIT-07, splittable per package): raise coverage with `audit_coverage_test.go`-style tests per package (batch-1 reports core 30.4→100%, route 56.3→91.4%, feature 59.4→71.1%, compat 51.6→63.0%, gateway 12.9→20.3%; those deltas are claimed by that branch and must be re-measured at merge time, not taken on faith).
- Regression test: the new coverage tests themselves.
- Evidence: command output above. Status: COVERED-BY batch-1 (partial uplift; gateway remains low — follow-up recommended).

### F8 (medium) Unbounded SSE retry — CONFIRMED
- File/line: `internal/httpapi/web/app.js:53-92` (`consumeLiveEvents`), specifically `89: await new Promise(resolve => setTimeout(resolve, 1500));`.
- Repro: `grep -n '1500\|setTimeout.*resolve\|malform\|backoff\|jitter' internal/httpapi/web/app.js` → only fixed `1500` with no backoff/jitter/counter/toast and no malformed-frame counter; `catch {}` at line 83 silently drops parse errors.
- Severity: medium (silent hot loop, no operator signal).
- Proposed fix (AUDIT-08): bounded/capped retry with backoff+jitter, error counter, surfaced toast, and a malformed-frame counter (e.g. `NexaRoute.sseStats()`).
- Regression test: SSE harness test for backoff caps + counter.
- Evidence: lines above. Status: COVERED-BY batch-1.

### F9 (low) Dead `_ = strings.ToLower("metadata_only")` — CONFIRMED
- File/line: `internal/decision/jev/provider.go:485`.
- Repro: `grep -n 'metadata_only\|ToLower' internal/decision/jev/provider.go` → line 485 `_ = strings.ToLower("metadata_only")` with the real invariant already stated in the comment at line 482.
- Severity: low (dead code).
- Proposed fix (AUDIT-09): delete the line.
- Regression test: `go build ./...` + package tests (no behavior change).
- Evidence: grep output above. Status: COVERED-BY batch-1 (line removed).

### F10 (low) Frontend listener/interval cleanup missing — CONFIRMED
- File/line: `internal/httpapi/web/app.js:668` (`setInterval(... footClock ...)`), `16/54-55/87` (`liveEventsAbort`), `666` (`window.addEventListener('resize', ...)`).
- Repro: `grep -n 'pagehide\|footClock\|removeEventListener\|disconnect\|stopLive\|liveEventsAbort' internal/httpapi/web/app.js` → no `pagehide` handler, no `removeEventListener`, no observer `disconnect`, `footClock` interval never cleared, no exported stop for `consumeLiveEvents`.
- Severity: low (resource hygiene).
- Proposed fix (AUDIT-10): `pagehide` cleanup, clear `footClock` interval, expose `stopLiveEvents`.
- Regression test: harness asserting cleanup stops timers/fetches.
- Evidence: grep output above. Status: COVERED-BY batch-1.

### F11 (low) Stress/soak always skipped — CONFIRMED, intentionally LEFT OPEN
- Files: `internal/router/stress_test.go:17-18`, `internal/events/stress_test.go:11-12`, `internal/probe/stress_test.go:20-21`, `internal/probe/soak_test.go:23-24`, `internal/logging/stress_test.go:13-14`, `internal/eval/stress_test.go:66-67`, `internal/httpapi/stress_test.go:21-22`, `internal/httpapi/soak_test.go:20-21`.
- Repro: `grep -rn 'NEXAROUTE_STRESS\|NEXAROUTE_SOAK' --include='*.go'` → every gate is `if os.Getenv(...) != "1" { t.Skip(...) }`, so default `go test ./...` always skips them.
- Severity: low.
- Proposed fix (AUDIT-11): run them in CI nightly (`NEXAROUTE_STRESS=1`, `NEXAROUTE_SOAK=1`). This requires a `.github/workflows/*` change, which is explicitly FORBIDDEN by the task constraints ("Do not touch any .github/workflows/* file", "workflow ... ممنوع"). So this item is documented here and deliberately left OPEN for a separate workflow PR; batch-1 also left it untouched for the same reason.
- Regression test: nightly CI job itself.
- Evidence: grep output above. Status: OPEN (blocked by workflow-touch ban).

### F12 (low) Conditional skip makes policy-payload inspection nondeterministic — CONFIRMED
- File/line: `internal/httpapi/eval_live_test.go:642` (`t.Skip("policy provider was never invoked; cannot inspect its request payload")`).
- Repro: `sed -n '630,660p' internal/httpapi/eval_live_test.go` shows the `if pol.Calls() == 0 { t.Skip(...) }` guard before the forbidden-substring scan over `pol.Last().Candidates`.
- Severity: low (test determinism).
- Proposed fix (AUDIT-12): always invoke the policy provider in that test so payload inspection is deterministic.
- Regression test: the hardened test itself.
- Evidence: excerpt above. Status: COVERED-BY batch-1.

### F13 (low) `api_key_env` name on admin surfaces — CONFIRMED (narrow)
- File/line: `internal/httpapi/admin.go:816` (`"api_key_env": p.APIKeyEnv` inside `providerSummary`).
- Repro: `sed -n '800,830p' internal/httpapi/admin.go` shows the env-var NAME (not value) returned on the admin summary surface alongside `has_secret`/`credential_count`/`has_proxy`. Secret VALUES are stripped (write-only credentials per `KNOWN_GAPS.md`), and this surface is the admin API (not the data plane). The remaining question — exactly what the issue asks — is whether exposing the NAME to non-admin surfaces is intentional and whether any log line ever pairs it with a value (`grep -rn 'api_key_env' internal/httpapi/*.go` → only this site).
- Severity: low.
- Proposed fix (AUDIT-13): either hide the name from non-admin surfaces or explicitly document it as intentional + add a guard test that no log line includes it alongside a value.
- Regression test: admin-secret guard test (batch-1 `admin_secret_test.go`).
- Evidence: excerpts above. Status: COVERED-BY batch-1 (guard test + documented intent).

### F14 (low) Duplicated `apiFetch` — CONFIRMED
- Files/lines: `internal/httpapi/web/app.js:26` (`async function apiFetch(url, opt = {})`) vs `internal/httpapi/web/control-plane-v2.js:741` (`apiFetch=async function(url,opt={}){` inside `installAdminKeyFlow`).
- Repro: `grep -n 'apiFetch' internal/httpapi/web/app.js internal/httpapi/web/control-plane-v2.js` shows both implementations with drifted 401-retry flows (`window.NexaUI?.requestAdminKey` vs `UI.requestAdminKey`, `q('#adminKey')` vs `$('#adminKey')`).
- Severity: low (auth-flow drift risk).
- Proposed fix (AUDIT-14): single source (`window.NexaRoute.apiFetch`), v2 delegates to it.
- Regression test: JS/harness test asserting both entry points share behavior.
- Evidence: grep output above. Status: COVERED-BY batch-1.

### F15 (low) `docs/KNOWN_GAPS.md` missing five audit items — CONFIRMED
- File: `docs/KNOWN_GAPS.md` (read in full; sections cover protocol scope, fidelity, token counting, secrets, admin security, discovery, state/HA, cache, probes, routing — none of the five: panic policy, proxy-credential enforcement, stress-check gating, frontend null/XSS hardening, gateway test coverage).
- Repro: full read of `docs/KNOWN_GAPS.md` on the base commit; `grep` for `panic policy\|proxy credential\|stress.*gat\|null.*XSS\|gateway.*coverage` → no hits.
- Severity: low (docs).
- Proposed fix (AUDIT-15): append the five items.
- Regression test: docs-only (`grep` for the new headings).
- Evidence: full-file read. Status: COVERED-BY batch-1 (`docs/KNOWN_GAPS.md` +60 lines).

## Decomposition into small independent traceable items

Each item is independently shippable; together they are the "top-15 → small tasks" mapping. IDs are stable for issue/PR titles.

| Task | Finding(s) | Proposed small PR title | Touches (only) | Regression test |
|---|---|---|---|---|
| AUDIT-01 | F1 | `fix(dashboard): null-safe $ and per-section render isolation` | `web/app.js` + JS harness test | missing-element render-survival test |
| AUDIT-02 | F2 | `fix(dashboard): escape cliSnippet values (XSS)` | `web/app.js` + harness test | malicious `public_model` test |
| AUDIT-03 | F3 | `fix(httpapi): document ErrAbortHandler re-panic policy + panic test` | `server.go` comment + `audit_fixes_test.go` | 500 + `internal_panic` test |
| AUDIT-04 | F4 | `fix(config): reject credentials in URLs + metadata/private-IP proxies` | `config.go` + `config_test.go` + affected fixture | rejection tests |
| AUDIT-05 | F5 | `feat(config): warn on silent defaults + --strict-config` | `config.go`, `cmd/gateway/main.go` flag wiring + tests | warning/strict tests |
| AUDIT-06 | F6 | `docs(httpapi): routeContext cancel-ownership + caller guard test` | `routing_helpers.go` + test | caller-guard test |
| AUDIT-07a..e | F7 | `test(<pkg>): raise audit coverage` (one PR per package: compat/route/feature/core/gateway) | per-package `audit_coverage_test.go` | coverage delta |
| AUDIT-08 | F8 | `fix(dashboard): bounded SSE retry with backoff/jitter/counter/toast` | `web/app.js` + harness test | backoff-cap/counter test |
| AUDIT-09 | F9 | `chore(jev): remove dead ToLower line` | `jev/provider.go` | build + pkg tests |
| AUDIT-10 | F10 | `fix(dashboard): pagehide cleanup + stopLiveEvents` | `web/app.js` | cleanup test |
| AUDIT-11 | F11 | `ci: run stress/soak nightly` — SEPARATE workflow PR (not this step) | `.github/workflows/*` only | nightly job |
| AUDIT-12 | F12 | `test(httpapi): deterministic policy invocation in eval_live` | `eval_live_test.go` | hardened test |
| AUDIT-13 | F13 | `fix(httpapi): api_key_env exposure guard + documented intent` | `admin.go` comment + `admin_secret_test.go` | guard test |
| AUDIT-14 | F14 | `refactor(dashboard): single apiFetch source` | `app.js`, `control-plane-v2.js` | shared-behavior test |
| AUDIT-15 | F15 | `docs: KNOWN_GAPS five audit items` | `docs/KNOWN_GAPS.md` | heading grep |

Unrelated changes, workflow edits, and secret handling changes are out of scope for every item except AUDIT-11 (which is quarantined to its own workflow PR).

## Traceability: finding → commit → test → evidence

`batch-1` = `origin/fix/audit-findings-batch-1` (tip `c05baa1dee46eb53addb6f6626578508273b5ea2`, fix commit `d326c6e`). `this` = this report-only branch/PR (docs only).

| Finding | Fix commit | Regression test | Evidence |
|---|---|---|---|
| F1 | batch-1 `d326c6e` (`web/app.js` null-safe `$` + isolation) | JS/harness render-survival (that branch) | this report F1 repro (265 sites); batch-1 gate file `docs/reports/audit-batch-1-gate.txt` on that branch |
| F2 | batch-1 `d326c6e` (escape `veModel`/`veList`/`base`) | malicious-model harness test | this report F2 excerpt |
| F3 | batch-1 `d326c6e` (`server.go` comment + 500 path kept) | `audit_fixes_test.go` panic→500+`internal_panic` | this report F3 excerpt |
| F4 | batch-1 `d326c6e` + `c05baa1` (fixture update) | `config_test.go` rejection tests | this report F4 excerpts |
| F5 | batch-1 `d326c6e` (warnings + `--strict-config`/`NEXAROUTE_STRICT_CONFIG`) | config warning/strict tests | this report F5 grep (no flag on base) |
| F6 | batch-1 `d326c6e` (cancel-ownership docs + guard test) | caller-guard test | this report F6 (all 3 callers already `defer`) |
| F7 | batch-1 `d326c6e` (per-package `audit_coverage_test.go`) | coverage tests; deltas re-measured at merge | this report coverage table (base numbers exact) |
| F8 | batch-1 `d326c6e` (SSE backoff/jitter/counter/toast/`sseStats`) | SSE harness test | this report F8 (line 89) |
| F9 | batch-1 `d326c6e` (dead line removed) | build + `jev` pkg tests | this report F9 (line 485) |
| F10 | batch-1 `d326c6e` (`pagehide` + `stopLiveEvents`) | cleanup test | this report F10 (no handler on base) |
| F11 | OPEN — no commit (workflow ban; batch-1 also untouched) | nightly CI job (future workflow PR) | this report F11 (8 Skip sites) |
| F12 | batch-1 `d326c6e` (`eval_live_test.go` deterministic) | hardened test | this report F12 (line 642) |
| F13 | batch-1 `d326c6e` (`admin_secret_test.go` guard) | exposure guard test | this report F13 (`admin.go:816`) |
| F14 | batch-1 `d326c6e` (single `apiFetch` source) | shared-behavior test | this report F14 (two impls) |
| F15 | batch-1 `d326c6e` (`KNOWN_GAPS.md` +60) | heading grep | this report F15 (five items absent on base) |
| report itself | this branch (docs only) | gates table above | this file |

Additional note from coverage work (not one of the 15, not fixed here): batch-1 reports a real `ParseContextTokens` bug ("8192 tokens" parsed as 8,192,000) found during coverage work. It is mentioned only for traceability; verification belongs to that branch's PR, not this report.

## PR / SHA reporting (honest status)

- `gh pr view` / `gh pr create` cannot run in this runner: `gh` prints `To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN environment variable` and no `GH_TOKEN`/`GITHUB_TOKEN` is present. No PR URL/number is claimed here — that would be fabrication.
- Push and PR creation for this branch are handled by the opencode infrastructure after this response (per the action context). The mergeable unit from this step is docs-only: `docs/reports/audit-top15-issue57.md` on branch `opencode/issue63-20260929165027` over base `377768e`.
- Do NOT merge this or any other branch from this step. CI/Security greenness must be read from the automatically opened PR's checks page; the local gates above are green but are not a substitute for CI.
- Real SHAs for traceability: base `377768e8fd9f95419a468af437e852004b8436c8`; batch-1 tip `c05baa1dee46eb53addb6f6626578508273b5ea2` (fix `d326c6e`); this step's commit SHA is whatever the infrastructure attaches to this branch (docs-only diff: `docs/reports/audit-top15-issue57.md`).

## Constraints honored

- New branch only; no direct `main` push; no merge performed.
- No `.github/workflows/*` change (hence F11 left OPEN by design).
- No secret material touched (F13 is a name-exposure guard + docs, no values).
- No unrelated code changes (this step is a single new report file).
- No `gh` claims, no invented metrics, no imaginary bugs. Every finding cites the exact file/line/command measured on the base commit.
