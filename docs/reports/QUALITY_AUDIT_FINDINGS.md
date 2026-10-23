# Quality Audit Findings - Issue #593

- Audit timestamp: **2026-10-21T11:33:58Z**
- Audited `main`: `bfa93b69cd9fc5041b2ddb822a49c03a32eeb13a`
- Work branch: `opencode/issue593-20261021110358`
- Checkpoint commit: `41169e9a4c880dd25894b3ff39fe08abbb49cc42` (plus report/final lint
  fixes in the follow-up commit on the same branch)
- Release `v0.23.0` is unchanged.

This report is authoritative for the current checkpoint. Earlier PR #595/#596
findings are summarized under "Already merged" below; full text remains in git
history.

## Summary counts

> **Checkpoint-era numbers (2026-10-21).** This table records the state
> at the #593 audit checkpoint. It is superseded by the 2026-10-22
> addendum at the end of this file (final: golangci-lint 0, staticcheck
> 0, deadcode 0, gosec baseline 87, coverage 85.1%) and by the
> 2026-10-23 final-completion addendum. Individual findings below carry
> inline supersession notes where later work changed their status.

| Class | Count | Status |
| --- | --- | --- |
| Confirmed code/test defects fixed in this checkpoint | **4** | Fixed with tests or verified behavior preservation |
| Security hardening delivered | **1** | Trusted-proxy scheme configuration |
| CI/evidence workflow defects fixed | **1** | Browser evidence PR publication |
| Staticcheck findings before -> after | **46 -> 23** | 22 ST1005 style, 1 proven SA4017 false positive |
| Gosec findings before -> after | **157 -> 98** | G115/G705/G104 cleared; G101/G124 false positives annotated |
| Confirmed false positives annotated | **21** | 14 G115, 6 G101, 6 G124, 1 SA4017 |
| Unverified / environment-limited | **6 areas** | Live PG/Redis/OIDC, browser, CI for new SHA, load, release signing, provider live APIs |
| Dead code (test-inclusive) | **4** | Unreachable test-only helpers; no production leak found |
| Repository statement coverage | **84.5%** | `go test ./...` profile, whole repository |

Do not read "98 gosec findings" as 98 exploitable vulnerabilities. The remaining
rules are concentrated in operator-controlled file paths and command execution;
their dispositions are below. No finding was closed by disabling a check or
deleting a test.

## Confirmed defects and fixes

### SEC-01: proxy deployments could not mark cookies Secure

**Symptom.** Behind a TLS-terminating reverse proxy, `requestIsSecure` returned
`false` because the gateway saw plain HTTP. Admin session, CSRF, and OIDC flow
cookies therefore lost the `Secure` attribute even though the browser-facing
connection was HTTPS. Enabling `Secure` unconditionally would have broken
plain-HTTP deployments, and trusting `X-Forwarded-Proto` from any peer would let
a direct client turn it on or off at will.

**Fix.** A new explicit list, `tls.trusted_proxy_cidrs`, identifies proxy
networks. `withTrustedProxyContext` marks a request only when its *peer address*
is inside a listed network; `requestIsSecure` honors a single unambiguous
`X-Forwarded-Proto: https` only for those requests. Direct TLS is always secure.
Unlisted peers, `Forwarded`-only requests, duplicate/chained values, and empty
values are rejected.

**Files.** `internal/config/config.go:485`, `internal/config/config.go:505`,
`internal/httpapi/config_snapshot.go:25`, `internal/httpapi/server.go:1438`,
`internal/httpapi/server.go:1459`, `internal/httpapi/server.go:1472`,
`docs/CONFIGURATION.md`, `docs/SECURITY.md`.

**Tests.** `internal/config/trusted_proxy_test.go` covers bare IPs, CIDRs,
IPv4-mapped IPv6, empty lists, >64 entries, malformed values, and the two
"trust everything" prefixes. `internal/httpapi/trusted_proxy_test.go` covers
trusted/untrusted peers, plain HTTP, ambiguity, `Forwarded`-only, direct TLS,
admin session/CSRF/OIDC cookies, and same-origin behavior.

**Residual risk.** A proxy that forwards from an address outside the configured
list behaves as before (cookies not Secure). This is explicit and fail-closed.
**Superseded 2026-10-23 (PR #635, owner order):** the residual behavior
above no longer exists. All six admin cookies now carry `Secure: true`
unconditionally; the browser admin dashboard therefore requires HTTPS
(direct TLS or a TLS-terminating trusted proxy) or a loopback
secure-context origin to hold a session. `tls.trusted_proxy_cidrs` now
governs only the same-origin scheme comparison. See `docs/SECURITY.md`.
The list is configuration, so a compromised config remains inside the trust
boundary.

### BUG-01: nested Anthropic tool-start error was discarded

`anthropicEmitter.Emit` recursively emitted a `StreamToolStart` when a delta
arrived without a start, but ignored the recursive error. A write failure could
therefore be lost while the emitter continued.

**Fix.** Return the recursive error. Regression surface is covered by the
existing canonical stream tests; the full package suite passes.

**File.** `internal/protocol/canonical/stream.go:606`.

### BUG-02: dead cooldown expression in admin snapshot

`internal/httpapi/admin.go` computed
`!time.Now().Before(st.CooldownUntil) == false && ...` and then immediately
overwrote the value with the authoritative `IsCooldown` call. The first line had
no runtime effect and confused the real source of truth.

**Fix.** Removed the dead expression; the authoritative assignment is unchanged.

**File.** `internal/httpapi/admin.go:334` (before fix).

### TEST-01: budget-alert regression loop never iterated

`TestBudgetAlertFailureDoesNotAffectRequests` wrapped its follow-up request in a
`for` loop that always executed `break` after the first request. The test only
ever sent one follow-up request despite claiming to wait for background delivery.

**Fix.** Replaced the fake loop with the single follow-up request it actually
performed. Assertions are unchanged; the test is now honest about its coverage.

**File.** `internal/httpapi/budget_alerts_test.go:405`.

### BUG-03: intentional probe budget looked like caller-controlled input

`httpAdapter.Probe` accepted `maxTokens`, overwrote it with `1`, then used the
variable. Staticcheck correctly flagged the overwritten assignment. The intent
was to ignore the argument permanently.

**Fix.** Added `_ = maxTokens` and a named `probeTokens = 1` constant used by
all provider bodies. Behavior is unchanged and the invariant is explicit.

**File.** `internal/providers/http_adapter.go:1084`.

### CI-01: browser evidence run could fail on PR permission denial

`browser-evidence-pr.yml` used `GH_TOKEN` and failed the whole workflow when
branch push or PR creation was denied, even though the evidence itself had been
produced successfully. It also used a runner-temp path and unpinned action
references.

**Fix.** Evidence is stored under `${{ github.workspace }}/.artifacts/...`,
actions are pinned by full SHA, unrelated inputs are enumerated, and denied
push/PR publication records a `blocked` status plus a manual compare URL without
destroying the evidence run.

**Files.** `.github/workflows/browser-evidence.yml`,
`.github/workflows/browser-evidence-pr.yml`.

**Verification limit.** The workflow syntax and shell paths are reviewed and the
repository's own workflow tests pass, but GitHub-hosted execution cannot be
observed locally. This remains unverified until the next workflow run.

## Staticcheck disposition (46 -> 23)

| Rule | Count remaining | Disposition |
| --- | --- | --- |
| ST1005 | 22 | **Accepted style debt**: user-facing error strings begin with a capital letter. Changing them would alter API/error text. Deferred to an owner-approved copy change. |
| SA4017 | 1 | **False positive**: `internal/protocol/canonical/stream.go:117`. A minimal probe under `.artifacts/scprobe` reproduced staticcheck 2026.2.2 flagging a `switch` branch whose function call does have side effects. No source workaround was added. |

Fixed in this checkpoint: SA4004 (fake test loop), SA4009 (overwritten probe
argument), SA1012 (intentional nil context in tests, justified lint ignore),
SA4006/no-op expression, S1002/S1009/S1011/S1016/S1017/S1030/S1034/ST1018
(style-only cleanups). The `cooldown_active` dead expression and recursive
`Emit` error were also surfaced here and fixed as BUG-01/BUG-02.

Command: `staticcheck ./...` -> exit 1, 23 diagnostics, stored in
`.artifacts/issue593-final/staticcheck.txt`.

## Gosec disposition (157 -> 98)

Command: `gosec -quiet -fmt=json -out=.artifacts/issue593-final/gosec.json ./...`
-> 98 issues, 232 files, 75,654 lines, 28 justified `#nosec` annotations.

| Rule | Count | Disposition |
| --- | --- | --- |
| G304 | 54 | **Operator-controlled paths**: config, media, video, and audit files opened from validated/allow-listed settings. Not attacker-chosen request paths. Needs path-root hardening only if config becomes untrusted. |
| G703 | 28 | **Tainted path analysis**: same operator-configured media/video locations. The code sanitizes names for traversal; gosec cannot see the allow-list. |
| G204 | 12 | **Configured binaries**: `ffmpeg`/`ffprobe`/TTS commands executed with argument arrays, never through a shell. Binary paths come from operator config. |
| G704 | 4 | **CLI SSRF**: `cmd/videogen` intentionally connects to the operator-supplied CLI base URL. It is a client tool, not a server-side fetch boundary. |

Cleared in this checkpoint:

- **G115 (14)**: all are bounded modulo results or intentional two's-complement
  reinterpretation in the EventStream protocol, audit key timestamps, or
  rotation indexes. Each has a line-local justification.
- **G705 (1)**: admin CSRF response now uses `json.NewEncoder` instead of
  `fmt.Fprintf` with a token.
- **G104 (32)**: response body closes, health check writes, and stream buffer
  writes now explicitly discard or check errors.
- **G101 (6)**: provider endpoint URLs, enum/classification names, response map
  keys, and environment-variable *names*; annotated as non-credentials.
- **G124 (6)**: cookie `Secure` is now computed by `requestIsSecure`; gosec
  cannot follow the helper. Annotated with the trusted-proxy invariant.
  **Superseded 2026-10-23 (PR #635):** all six admin cookie sites now set
  `Secure: true` unconditionally; `requestIsSecure` governs only the
  same-origin scheme comparison. The CodeQL `go/cookie-secure-not-set`
  alerts (22–27) closed as *fixed* by that change.

## Architecture debt and proposals (not implemented here)

These require product/owner decisions or multi-package migrations. This
checkpoint deliberately did not start them without approval; no partial parallel
implementation was left behind.

### ARCH-01: `config.Config.Validate` is ~1,320 lines

**Problem.** One function validates unrelated domains (admin, TLS, routing,
providers, video, logging), making error order and ownership hard to reason
about. Current complexity is a maintenance and review risk.

**Proposed safe split.** Keep `Validate` as the ordered orchestrator and extract
`validateAdmin`, `validateRouting`, `validateProviders`, `validateVideo`, and
`validateLogging`, preserving call order and exact error strings. Add table
tests that assert the *first* error for a config with several invalid fields.

**Status.** OPEN. Deferred because moving 1,300 lines without dedicated
differential tests is a large behavior-preserving refactor, and the task forbids
risky architecture changes without owner approval.
**Resolved 2026-10-23 (PR #636):** the split was executed exactly as
proposed — `Validate` is now a thin orchestrator over per-section
validators (`validateAdminPlane`, `validateRuntimeRouting`,
`validateAccessPlanes`, `validatePlatformPlanes`, `validateProviders`,
`validateEvaluation`, `validateDecisionCore`,
`validateDecisionProvidersAndChains`, `validateCrossReferences`), with
equivalence proven by the golden-corpus differential test
`internal/config/validate_golden_test.go` (corpus generated from the
pre-refactor code; error strings and precedence unchanged).

### ARCH-02: virtual-key create/rotate/revoke duplication

**Problem.** `internal/httpapi/client_keys.go` repeats key generation, lookup,
revocation, and config mutation in three handlers. The route layer owns domain
rules that should be reusable by future admin clients.

**Proposed change.** Extract a small `virtualKeyService` (or package-level
functions) with `Create`, `Rotate`, `Revoke`, and a single ID generator;
handlers only decode, call, and render. Add tests for duplicate IDs, expired
keys, and rollback on config validation failure.

**Status.** OPEN. Scoped but not implemented in this checkpoint; it is safe to do
next without changing API shapes.

### ARCH-03: distributed circuit breaker

**Problem.** Circuit-breaker state is process-local. Behind multiple replicas,
one instance can keep routing to a failing provider while another is open.

**Options.** (a) Redis/PostgreSQL shared state with TTL keys; (b) accept
per-replica state and document it; (c) leader-owned breaker with gossip.
Option (a) adds a hard dependency to request routing and needs failure-mode
design; option (b) is current behavior.

**Status.** BLOCKED on owner decision. No distributed state was fabricated or
simulated. **Superseded — see the 2026-10-22 addendum (ARCH-03):
implemented as opt-in breaker sync in PR #626 under ADR 0003.**

### ARCH-04: durable video job queue

**Problem.** Video jobs are in-process. Restart loses queued work and
multi-replica deployments cannot share progress.

**Options.** (a) PostgreSQL-backed job table with `FOR UPDATE SKIP LOCKED`;
(b) Redis streams; (c) keep in-process and document single-replica constraints.

**Status.** BLOCKED on product decision (durability/HA expectations). The
current behavior is documented, not silently claimed as durable.
**Superseded — see the 2026-10-22 addendum (VID-LEDGER-01) and ADR 0004
(Accepted — Option B): durable PostgreSQL job store and cost ledger
shipped; multi-node video remains undeclared by decision.**

### ARCH-05: control-plane/HA ownership

The admin control plane is still single-writer for config mutations. Splitting
config writes from routing would require an explicit consistency model and
revision protocol. **Status: OPEN/owner decision.**
**Resolved by ADR 0001 (Accepted 2026-10-22, Option 1):** local config
stays authoritative; no split or shared-config implementation is
planned.

### ARCH-06: browser and live-infrastructure acceptance

Browser acceptance is skipped locally because Chromium + Python Playwright are
absent. PostgreSQL, Redis, OIDC, KMS, and live providers are not available.
`verify.sh` release-artifact trust-chain tests do pass. **Status: PARTIAL;
requires CI or staging evidence.**

## Already merged (PR #595/#596 history)

PR #595 fixed nine confirmed defects: egress TLS `ServerName` pinning, proxy
`allowed_hosts`, media `no_training` bypass, structured-output removal,
Anthropic thinking budget, emergency-key/OIDC precedence, truncated media
responses, hedge capability filtering, and Anthropic cache-usage reporting.
PR #596 recorded the baseline and verification for that batch. Those fixes are
present in `main` at `bfa93b6` and are not re-claimed here.

## Dead code

`deadcode -test ./...` reports four unreachable functions:

- `internal/decision/jev/provider.go:441` `CloneForHotReload`
- `internal/decision/remote/errors.go:70` `IsUnavailable`
- `internal/httpapi/budget_alerts.go:131` `NewFileBudgetAlertStore`
- `internal/scorecards/feedback.go:186` `FeedbackTracker.Cards`

All are test/dispatch-only or reserved API helpers. No production call was
removed; deleting exported/package API needs an owner decision.

## Limitations and unverified areas

1. No authenticated `gh`; cannot observe CI for the new commit or download
   artifacts. CI status must be checked after PR creation.
2. Chromium + Python Playwright unavailable; browser acceptance is a real skip,
   not a pass.
3. No live PostgreSQL, Redis, OIDC, KMS, or provider credentials; integration
   paths are compile/unit-tested only. *(Checkpoint scope note: this was
   written before the CI integration suites for breaker sync and the
   video cost ledger landed — those paths now have real-PostgreSQL and
   real-Redis CI evidence; the credential-gated live boundaries listed
   elsewhere in this report are unchanged.)*
4. No load/soak test, no multi-replica failure injection, no destructive
   restart test.
5. Release signatures/assets were not re-verified against the published
   release; `verify.sh` only proves the local trust-chain tests.
6. Gosec G304/G703/G204/G704 remain plausible risks if operator config is ever
   attacker-controlled. They are classified, not proven harmless.

---

## Addendum — 2026-10-22 remaining-work mission (PRs #624–#629)

This addendum records the follow-on mission executed on 2026-10-22 under
issue #593. Audited `main` at mission start: `4bb6b2e5`; final `main`:
`6ca8ac5f5d6949c1c7a5832df6472dbce375e784`. Fresh scanner/coverage numbers
were measured by audit workflow run `39717951099` on the final `main`
(and identically on the coverage branch run `39716902380`).

### LINT-01 (P3, confirmed process defect): capped lint reporting hid real findings

The audit workflow's golangci-lint artifact reported 45 findings because
the tool's default display caps suppress repeated findings
(`max-same-issues`). An uncapped, clean-cache run with the pinned
golangci-lint v2.15.0 on `main` found **69** real findings. PR #624
resolved all 69 to **0** — unused parameters, shadowing, an inefficient
string comparison, staticcheck style classes — with no behavior change
and no rule disabled (one scoped, justified test-only SA1012 exclusion).
Lesson recorded in `AGENTS.md`-equivalent mission notes: always triage
with `--max-issues-per-linter=0 --max-same-issues=0` and a clean cache.

### DEAD-01 (P3): the four remaining deadcode functions (resolved in this addendum's disposition)

The baseline listed four unreachable functions. Each was verified to have
zero callers repository-wide and removed in PR #625:
`CloneForHotReload` (decision/jev), `IsUnavailable` (decision/remote;
the `ErrUnavailable` sentinel stays, still returned by live code),
`NewFileBudgetAlertStore` (httpapi; the store type stays, constructed
directly at its call site), `FeedbackTracker.Cards` (scorecards).
`deadcode -test ./...` now reports **0**. This supersedes the earlier
note in this file that deletion "needs an owner decision": the removals
were executed under the owner's standing decision delegation, each with
caller evidence.

### ARCH-03 (distributed circuit breaker): implemented, opt-in (PR #626)

Status change: from "proposal, not implemented" to **implemented with
safe defaults**. Cross-replica breaker synchronization ships opt-in
(`control_plane.breaker_sync`, default off = previous behavior exactly),
per ADR 0003: a background syncer exchanges cooldown/retired state over
the existing control-plane store contract (revision CAS); remote
retired markers apply only as bounded cooldowns, never full local
retirements; a store outage aborts the round with local state untouched.
Acceptance evidence is real-service: CI runs two-instance propagation /
partition / heal / restart scenarios against real PostgreSQL and real
Redis (`TestBreakerSyncPostgresTwoInstances`,
`TestBreakerSyncRedisTwoInstances`, verified from job logs). Three
policy questions remain the owner's and are recorded as open in
ADR 0003 (honor remote retired as full retirement? default-on for
shared backends? production cadence).

### VID-LEDGER-01 (P2, confirmed defect): video cost ledger was volatile even with a durable job store

With `video.job_store_backend=postgres`, jobs were durable but the cost
ledger was still the in-memory `Ledger`: a restart silently zeroed
spent/reserved accounting and replicas sharing the job store kept
divergent books. Fixed in PR #627 by `internal/video/cost.PostgresLedger`,
which mirrors the in-memory contract exactly (same validation errors and
state machine) persisted per job row: every mutation is a transaction
locking the job row, aggregates are computed from rows on every read,
duplicate settlements charge exactly once, a conflicting actual cost is
an error, and wiring fails hard at startup rather than falling back to
volatile accounting. Real-PostgreSQL CI tests cover concurrent duplicate
reserves, duplicate/conflicting settlement, estimate→actual upgrade,
restart recovery, invalid amounts and store failure.

### VID-ASSET-01 (P3, confirmed defect): stored video assets were served without integrity verification

`storage.Local.Read` validated paths and sizes but never content: a
corrupted or replaced object under the storage root was served
silently. PR #627 makes `Read` verify the recorded size and the SHA-256
that `Put` computed; a mismatch fails the read. Regression test covers
intact, same-length-corrupted and truncated objects; legacy records
without a checksum read as before.

### COV-01 (P3): coverage target met, and the metric itself was understated

Repository coverage rose from **84.1%** (2026-10-22 baseline) to
**85.1%** (final-main audit run), meeting the documented ≥85% target,
via behavioral tests in PR #629 (livecanary extended conformance set,
video/api ForceFail + universal-plan + durability labels, video/render
filter-graph contracts, video/queue file-store lifecycle). The same PR
fixed a measurement defect: the audit coverage job lacked ffmpeg, so the
render suite skipped by design and reported 3.5%; with the toolchain
installed it measures ~87%. Part of the "gap" was the ruler, not the code.

### Reserved decisions (recorded, NOT settled by this mission)

> **Update 2026-10-22:** all four decisions below were settled the same
> day under the owner's delegation (PRs #631–#633). The "(Proposed)"
> statuses in this section are the state at the time of writing.

- Shared configuration: ADR 0001 (Proposed) — recommends keeping local
  config authoritative. **Accepted 2026-10-22 — Option 1.**
- Multi-node video positioning: ADR 0004 (Proposed) — jobs and ledger
  are now multi-process safe on PostgreSQL; assets are not, so
  declaring multi-node video before a shared asset backend exists is
  not recommended. **Accepted 2026-10-22 — Option B (not a product
  goal).**
- Autopilot keep-and-test vs remove: ADR 0005 (Proposed) — live and
  e2e-tested, but five public `Controller` methods have 0.0% direct
  package coverage. **Accepted 2026-10-22 — Option A (keep, with
  direct tests added in PR #631).**
- Lint ratchet gating: ADR 0006 (Proposed) — golangci/staticcheck/
  deadcode are at zero and could gate today; gosec needs a checked-in
  baseline and coverage needed its target met (now true) first.
  **Accepted 2026-10-22 — Option A (ratchet gating, PR #632).**
All four were merged as Proposed records in PR #628. The placeholders in
force are named in each ADR; none was decided by default.

### Fresh dispositions on final main (audit run 39717951099)

- staticcheck: **0** (was 46 at #593 open, 23 at the checkpoint).
- gosec: **53**, all G304, all previously triaged as bounded
  operator-path opens (`docs/reports/audit-slices/G304.md`).
  **Corrected 2026-10-22 (ADR 0006 implementation):** 53 was the CI
  artifact's count, produced while the CI gosec analysis was silently
  incomplete — gosec v2.29.0's bundled x/tools cannot decode Go
  1.27.3 export data, so the SSA/taint rules (G701–G704) reported
  nothing (the artifact's own "Golang errors" section named the
  decode failure on 158 files). A working analysis finds **87**
  findings (53 G304 + 30 G703 + 4 G704); all 87 were re-triaged at
  source on 2026-10-22 as operator-controlled boundary operations
  (CLI flags, env vars, config values, constant bundle entry names,
  the operator-run videogen client) — zero confirmed defects, the
  same classification the pre-CI local audits had recorded. The
  full 87 are now the checked-in baseline
  (`docs/reports/gosec-baseline.txt`), the gosec job pins Go 1.27.0
  so its analysis is complete, and the baseline checker fails on
  any incomplete analysis or any finding outside the baseline.
- golangci-lint (pinned, uncapped): **0**.
- deadcode: **0**.
- coverage: **85.1%** repository-wide.
- govulncheck: PASS on every mission PR (Security workflow).


## Addendum — 2026-10-23 final completion pass (findings register)

A fresh five-lane audit (breaker sync / docs / security / video
durability / routing-runtime) was run against `8fcd2499` and every
finding below was re-verified at source by the reviewing agent before
any fix. Dispositions:

**Documentation findings — all reconciled in the docs PR of
2026-10-23:**
- F-01: `docs/CONFIGURATION.md` never documented the `breaker_sync*`
  keys — section added (defaults, 5..3600 s validation, bounded
  remote-`retired` semantics).
- F-02: convergence wording "within one sync interval" understated
  the tick-driven worst case — corrected in ADR 0003 and
  `docs/HA_STATE_INVENTORY.md` (≈2 intervals + store latency).
- F-03: `docs/KNOWN_GAPS.md` claimed live evaluation was "not
  implemented" — false since the evaluation plane shipped; rewritten
  as the two-mode contract (offline default; live is opt-in via
  `evaluation.live_enabled`, spends real quota, external behavior
  UNVERIFIED).
- F-04: `HA_STATE_INVENTORY.md` self-contradictions (denied the
  Postgres job store exists; absolute per-instance claims vs. opt-in
  breaker sync) — corrected; `check-ha-docs.sh` green.
- F-06: `docs/CONFIGURATION.md` claimed the video cost ledger "stays
  process-local" — false under the Postgres backend; corrected
  (ledger follows the backend; assets remain per-instance).

**Code findings — fixed test-first:**
- F-05 (latent, unreachable): video `Handler.authorized` compared an
  empty presented token equal to an empty configured token when
  `RequireAuth=true` and no token was configured. Not exploitable
  (header trimming + production wiring refuses empty tokens —
  disclosed on the PR), hardened anyway in PR #638 (merged
  `37fa779e`): explicit empty-token rejection, contract pinned.
- F-07 (Low): video asset **download** handler never verified the
  recorded SHA-256 (every other read path does) — same-size tampering
  was served silently. Fixed in PR #639: hash-before-serve,
  `asset_integrity_error` on mismatch. Red proven in CI (tests-only
  head) and locally.
- F-08 (Low, reporting): Postgres ledger aggregate/entry reads
  silently degraded to 0/nil on store error. Fixed in PR #639:
  failures are logged at the degradation point; contract (mutations
  fail-closed, reporting degrades) pinned by test.
- F-09 (Low): `PostgresStore.Update` decoded its terminal-guard
  baseline from the JSONB payload without overlaying the authoritative
  lease columns, so replaying the current record after a claim failed
  with a spurious `ErrTerminalJob`. Fixed in PR #639 (full-column
  select + `overlayColumns` before the guard; guard can only
  over-block). Red/green proven in CI `integration-postgres`.
- F-10 (Very Low): `PostgresStore.Create` mapped a failed
  conflict-probe to `ErrIdempotencyConflict`. Fixed in PR #639:
  the wrapped probe error is returned instead.
- D1 (Medium): Responses built-in tools (and tool_choices naming
  one) were silently dropped, even on Responses→Responses routes.
  Fixed in PR #640: explicit unsupported-type error at decode. The
  same work fixed `decodeOpenAIToolChoice` dropping the standard
  Responses function tool_choice shape (`{"type":"function","name":…}`,
  the shape this gateway's own encoder emits).
- D2 (Medium): a non-stream Responses body with `status:"failed"`
  decoded to a successful `end_turn` and was re-encoded to the client
  as `completed`. Fixed in PR #640: decode error carrying the upstream
  message/code, mirroring the streaming `response.failed` path.
- D3 (Medium): audio content parts were silently dropped (Responses
  ingress) or forwarded as raw-JSON text (Chat ingress → translated
  upstreams). Fixed in PR #640: explicit unsupported-content errors in
  both decoders; `DecodeOpenAIChatRequest` returns an error; the
  hedging builder maps it to the bounded protocol-mapping error
  (e2e-pinned: 502, zero upstream calls). System content keeps its
  documented text-only contract.
- S2: `previous_response_id` was parsed and silently discarded.
  Fixed in PR #640: rejected at decode — the gateway does not forward
  `store`, so chaining could never resolve.
- S3: a Chat error envelope inside an HTTP 200 body decoded to an
  empty success on the canonical path (legacy paths reject it via
  `validateOpenAIResponseJSON`). Confirmed by probe; fixed in
  PR #640 inside `DecodeOpenAIChatResponse`.
- S4 (Low): three orchestrator terminal paths discarded
  `SettleEstimate` errors. Fixed in PR #640: logged at each site
  (job outcome unchanged).
- S1 (accepted behavior, doc corrected): half-open recovery under
  non-ready strategies admits concurrent trial requests (bounded by
  admission control and the provider semaphore); there is no
  single-flight trial lease. `docs/FAILURE_SEMANTICS.md` previously
  implied a single trial request; wording corrected 2026-10-23.
- F-11 (Medium-Low): a whitespace-only system prompt was not
  round-trip stable — the decoders admitted blank system text, but
  every system encoder gates emission on trimmed text, so decode →
  encode → decode lost the system part (`FuzzRequestRoundTrip`
  instability, found by the extended-fuzz job of audit run
  `39941279599` on `1ba6ea32`; A/B-verified pre-existing on
  `8fcd2499`, not a regression of PR #640). Fixed in PR #641:
  decode-side rejection of whitespace-only system values (Responses
  `instructions` must be non-blank; blank Chat system values fold
  away), matching the rule "decode only what encoders can
  represent". The crasher seed is committed to the fuzz corpus;
  `blank_system_test.go` pins both forms plus real-prompt controls.

**Security lane result:** no new confirmed exploitable defects
(7 scopes re-audited: admin gate, OIDC, client keys, secret reveal,
SSRF, body caps, cookies). **Program state after this pass:** merged
in PRs #638 (`37fa779e`), #639 (`c32ce0b5`), #640 (`1ba6ea32`),
#641 (final code tree `79df87d5`); standing UNVERIFIED boundaries
unchanged (see `docs/EXTERNAL_VERIFICATION.md`).
