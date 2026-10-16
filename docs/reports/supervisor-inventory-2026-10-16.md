# Supervisor live inventory + dependency-aware execution queue — 2026-10-16

Source issue: #439 (Roadmap supervisor). Roadmap tracker: #401. Audit triage: #371 (closed).
Verification method: public GitHub REST API (unauthenticated — no `GH_TOKEN` in this runner,
`gh` CLI refused auth) + local git state on the checked-out repo. Nothing merged, nothing
triggered, no tags/releases, no force-push. All timestamps UTC, 2026-10-16.

## 0. Proven local + remote state

- Local HEAD: `59c80524174865a94f25e4a0ee062c2385ab485b` (= origin `main`, merge of PR #423).
- Local branch: `opencode/issue439-20261016015848` (unique supervisor branch). Tree clean at start.
- Local evidence: `go build ./...` → OK (go1.25.0 toolchain download, zero build errors);
  `go test -count=1 ./internal/egress/` → ok (0.004s). Full `scripts/verify.sh` NOT rerun
  locally in this inventory-only run — main HEAD already has green CI+Security (see below);
  rerunning the ~25 min gate would add no new signal for a no-code supervision deliverable.
- Remote `main` HEAD: `59c805241748` (matches local). Remote branches (30-cap listing): `main`,
  `opencode/issue402-20261016012317` (PR #424 head, `74f212f8c2b5`), plus stale
  `feat/m3-8-tpm-admission-hardening`, `feat/m4-2a-securitystore-postgres`, `fix/appjs-tail-repair`.
- Required checks (from `.github/workflows/`): `CI` (verify + stress + smoke + browser E2E +
  release packaging + docker), `Security`, `integration-redis`, `integration-postgres`,
  `container`, `soak`.
- Latest `main` CI+Security (runs 38837457636/38837457643, created 01:52:33Z, post-#423): **success**.
- PR #424 (`Coverage 65.4%→92.4%, 2 bugs fixed`, branch `opencode/issue402-…`, base `main`):
  state **open**, `mergeable: true`, `mergeable_state: clean` (not merged). Checks on its head:
  `CI` success, `Security` success, `Redis control-plane integration` success (all 01:47:16Z).
  Body documents 2 real divergences fixed (bbolt `AuditRecords` ordering; Redis `RotateSession`
  self-rotation) with red→green proof, coverage 65.4%→92.4%, `verify.sh` VERIFY PASS claimed.
  **Next action: owner review + merge. NOT to be merged by any agent.**
- Active workflow jobs (Actions API, verified 02:0xZ): exactly **2 in_progress** runs, both `opencode`
  workflow on `main`: run `38837997230` (this supervisor run, triggered 01:58:37Z by the #439
  comment) and run `38837809470` (triggered 01:56:30Z — matches the owner `/opencode` trigger on
  **#403 Wave C-2 at 01:56:27Z**, so #403 is ACTIVE). All other recent `opencode` runs:
  `completed/skipped`. No other `in_progress` CI/Security runs.
- #371 triage (closed 01:48:46Z after supervisor-takeover audit on baseline `7bb6792`):
  **22 confirmed findings** (1 High F1 FileStore cross-process lost updates → #406; 1 High-display
  UI-F1 snapshot guardrails → #429; rest Medium/Low) triaged to scoped fixes #425–#433 (+ #406
  evidence); 19 suspicions documented without proof (not promoted). Opencode lane failed 3× on
  #371 (stalls); owner directive stands: **do not re-trigger opencode for #371**.
- #409: closed (complete). #423 (docs-only Phase 9 blueprint): merged → `main` at `59c8052`.
  #401 (roadmap): open, 0 comments — **could not be edited from this runner** (no authenticated
  token; `gh` refuses without `GH_TOKEN`, and `USE_GITHUB_TOKEN=false` in this workflow). This
  in-repo report is the verified-status + queue artifact; the owner-facing queue is carried in
  the supervisor comment / PR body instead of a direct #401 body edit.

## 1. Live status inventory (every open issue/PR in program scope)

Legend — next actions: `OWNER-MERGE` (needs owner review/merge), `ACTIVE-WAIT` (agent running,
do not duplicate), `QUEUED` (briefed, 먹는 sequential trigger only), `BLOCKED` (dependency/
overlap, do not start), `READY` (safe to trigger — none this run).

| # | Title (short) | Cat. | Live status (verified) | Done vs remaining | Blockers / overlap | Pri | Next action |
|---|---|---|---|---|---|---|---|
| 424 | securitystore coverage 65.4→92.4%, 2 bugs fixed (PR) | hardening PR | OPEN, mergeable clean; CI+Security+Redis green 01:47Z | #402 work complete on branch, awaiting merge | Awaiting owner merge | P0 | OWNER-MERGE then close #402 |
| 402 | Wave C-1 securitystore adversarial+coverage | hardening | OPEN; PR #424 contains the work | Done pending merge; no duplicate branch | Blocked on #424 merge | P0 | OWNER-MERGE #424; no new trigger |
| 403 | Wave C-2 fuzz codecs/egress/budget/cache | hardening | OPEN; owner triggered 01:56:27Z; opencode run in_progress | ACTIVE — agent working | Active runner; overlaps egress/budget/cache files | P0 | ACTIVE-WAIT; do not duplicate |
| 404 | Wave C-3 deterministic load-flake removal | hardening | OPEN; owner triggered 01:23:04Z; no PR / no bot comment yet | Unknown — may be active, stalled, or failed; NOT verifiable via public API | Possible active runner; touches test design across packages | P1 | BLOCKED — verify run outcome before any retrigger; do NOT retrigger blind |
| 405 | Wave C-4 leak invariants + soak | hardening | OPEN; QUEUED (do-not-start-early note) | Not started | Sequential Wave-C gate; overlaps budget/ledger with #403-active | P1 | QUEUED — trigger only after #403 lands + #404 resolved |
| 406 | Wave C-5 FileStore flock + crash safety | hardening | OPEN; QUEUED + #371 F1 repro evidence posted | Not started; premise empirically confirmed | Sequential gate; overlaps controlplane with nothing active — but queue order holds | P0 | QUEUED — first Wave-C successor after #403/#404 clear |
| 407 | Wave C-6 cmd/gateway + video coverage | hardening | OPEN; QUEUED | Not started | Overlaps video subsys with #431/#432; CLI with #428 backup paths | P1 | QUEUED |
| 408 | Wave C-7 property invariants egress/router | hardening | OPEN; QUEUED | Not started | Same egress files as ACTIVE #403 — must wait for #403 | P1 | QUEUED — only after #403 merges |
| 425 | Throttle OIDC login start (F3+F11) | confirmed bug (sec) | OPEN, 0 comments, no PR | Not started | Files: `internal/httpapi/server.go`, `oidc.go`. No file overlap with #403-active, BUT review queue loaded (1 PR pending + 1-2 active agents) and Wave-C review-first rule applies | P0 | BLOCKED this run — first security candidate once #403 clears / #424 merges |
| 426 | Redirect + credential-forwarding policy (F2/F7/F8/F9) | confirmed bug (sec) | OPEN, no PR | Not started | Shared redirect/egress transport; DIRECT overlap with ACTIVE #403 (egress classifiers) | P0 | BLOCKED on #403 |
| 427 | Egress 6to4/Teredo + proxy_url (F5/F6) | confirmed bug (sec) | OPEN, no PR | Not started | `internal/decision/remote/transport.go`, `internal/egress/policy.go` — DIRECT overlap with ACTIVE #403 | P0 | BLOCKED on #403 |
| 428 | Audit-chain integrity on read/backup (F4) | confirmed bug (sec) | OPEN, no PR | Not started | `internal/securitystore/store.go` + `cmd/gateway/backup.go` — DIRECT overlap with unmerged PR #424 (same package) + #407 CLI | P0 | BLOCKED on #424 merge |
| 429 | Admin snapshot contract (UI-F1..F4) | confirmed bug (UI/config) | OPEN, no PR | Not started | `internal/httpapi/admin.go` + monolithic `web/app.js`; serialize-after-security rule (#425–#428 first); frontend serialize-one-at-a-time | P0 | BLOCKED — after #425–#428, first of frontend queue |
| 430 | Dead Failover control + FA/PS titles (UI-F5/F8) | confirmed bug (UI) | OPEN, no PR | Not started | Same `web/app.js` monolith as #429 — must serialize after #429 | P1 | BLOCKED on #429 |
| 431 | Video idempotency + cost caps (UI-F6/F7) | confirmed bug (video/billing) | OPEN, no PR | Not started | `web/app.js` + `internal/video/{request,cost}`; needs #429/#430 frontend slot + #435 semantics note | P0 | BLOCKED — after frontend slot frees |
| 432 | Video force-cancel + ledger ordering (UI-F9/F10) | confirmed bug (video) | OPEN, no PR | Not started | `orchestrator.go`, `episode.go`, `ledger.go`; serialize with #431 (same subsystem) | P0 | BLOCKED on #431 |
| 433 | Perf: replace per-request config clones (C1) | perf investigation | OPEN, no PR | Not started; correctness NOT in question | Touches 82 `currentConfig()` sites incl. hot paths others edit — LAST of security wave, needs before/after benchmarks | P2 | BLOCKED — after #425–#432 |
| 393 | Provider Edit preserve credentials | feature | OPEN, no PR | Not started | `admin.go` provider form overlaps #427 proxy_url + #429 snapshot — after security/config fixes | P0 | BLOCKED |
| 394 | Provider catalog/model onboarding | feature | OPEN | After #393 | P1 | BLOCKED on #393 |
| 395 | Health probes + auto recovery | feature | OPEN | After #394; note #404 flake work touches probe tests — coordinate | P1 | BLOCKED |
| 396 | Smart routing/candidate pools/failover | feature | OPEN | After #395; #408 proves current math — coordinate | P0 | BLOCKED |
| 397 | Coding-client protocol conformance | feature | OPEN | Coordinate registry/tests with #436; adversarial robustness is ACTIVE #403 — no duplicate codec work | P1 | BLOCKED on #403 |
| 398 | Video adapter/job architecture | feature/spec | OPEN | Truth/semantics for video; #431/#432 fixes constrain it | P0 | BLOCKED on #431/#432 |
| 399 | Provider/routing UI | feature (UI) | OPEN | After blueprint #409/#423 + #411 IA + #416 tokens; single-frontend rule | P1 | BLOCKED |
| 400 | Final acceptance / release readiness | release acceptance | OPEN | LAST — only after waves integrated or formally deferred | P0 | BLOCKED — do not activate early |
| 410 | Live request/route waterfall | feature (UX) | OPEN | Powers traces; #417 reuses its contracts | P1 | BLOCKED (Phase D, post-foundation) |
| 411 | Navigation + progressive disclosure | feature (UX IA) | OPEN | Coordinates #399/#416; single-frontend rule | P1 | BLOCKED |
| 412 | Adaptive routing policy engine | feature | OPEN | Depends on measured #395/#396 behavior | P2 | BLOCKED on #395/#396 |
| 413 | Frontend/backend contracts + browser E2E | E2E gate | OPEN | Contract gate across UI+API; runs after UI waves settle | P0 | BLOCKED |
| 414 | Video Studio job/editing UX | feature (UX) | OPEN | Must respect #431/#432 + #435; after #398 | P2 | BLOCKED |
| 415 | Coding-agent profiles + presets | feature | OPEN | After #397 | P2 | BLOCKED |
| 416 | Design system + visual polish | feature (UX) | OPEN | Tokens/components; coordinate #399/#411; single-frontend rule | P1 | BLOCKED |
| 417 | WebChat with route transparency | feature | OPEN | Reuses #410 contracts | P2 | BLOCKED on #410 |
| 418 | Canary rollout + rollback + game-days | feature (safety) | OPEN | After policy engine #412 | P2 | BLOCKED |
| 419 | Task classification + eval-gated routing | feature | OPEN | Eval-gated; no LLM in hot path by default | P2 | BLOCKED |
| 420 | Real video adapters + storyboard | feature | OPEN | After #398; respects #431/#432/#435 | P2 | BLOCKED on #398 |
| 421 | Coding-session intelligence | feature | OPEN | After #415/#419 | P2 | BLOCKED |
| 434 | Multi-instance HA / distributed state | ops maturity | OPEN | Defines consistency BEFORE any global-budget/HA claims | P1 | BLOCKED (Phase E order) |
| 435 | Cost/quota/budget + billing provenance | ops maturity | OPEN | Depends on exact billing semantics (#431/#432 ledger) | P1 | BLOCKED |
| 436 | Protocol conformance registry + drift | ops maturity | OPEN | Coordinate (not duplicate) with #397 | P1 | BLOCKED |
| 437 | Perf/load profiles + fault injection | ops maturity | OPEN | Measures; after #433 + #435 | P2 | BLOCKED |
| 438 | Backup/restore drills + DR | ops maturity | OPEN | Verifies restore; overlaps #428 backup paths + #407 CLI | P2 | BLOCKED |
| 370 | Pre-v0.22.0 adversarial acceptance sweep | acceptance context | OPEN, 9 comments | Context only, not an implementation task | — | — | Reference for #400 |
| 371 | Wave-2 audit report | report | CLOSED 01:48:46Z | Deliverable landed; defects triaged to #406/#425–#433 | Do NOT retrigger (owner directive) | — | Closed; no action |
| 409 | Competitor UX benchmark / blueprint | design/spec | CLOSED (complete) | Merged via PR #423; blueprint at `docs/UX_BLUEPRINT_PHASE9_2026-10-16.md` | — | — | Closed; use as baseline, do not redo |
| 401 | Maturity roadmap tracker | tracker | OPEN, 0 comments | Body current as of #423 merge; verified-status update PENDING (no auth to edit from runner) | — | — | Owner/agent with token to append § queue below |
| 439 | This supervisor issue | coordination | OPEN | This run = inventory + queue; stays open until #400 verdict + reconciled tracker | — | — | Keep open |

No duplicate/stale issues found beyond the already-closed #371/#409. No PR other than #424 is open.

## 2. Dependency DAG and execution queue (ordered phases, non-overlapping)

- **Phase 0 — Review gate (now):** `OWNER-MERGE #424` → closes #402. THEN confirm #403 ACTIVE run
  outcome and #404 trigger outcome. Successors: everything below. Parallel-safe: nothing —
  review queue is the bottleneck (1 pending PR + 2 active runners).
- **Phase 1 — Wave C hardening (strict sequence, one active at a time):**
  `#403(ACTIVE)` → `#404(outcome-check)` → `#405` → `#406` → `#407` → `#408`.
  Prerequisites: #424 merged (esp. for #406-adjacent store work and #408 egress reference).
  #408 must come after #403 (same egress files). #404 resolution (pass/fail/stall) required before
  #405 starts (soak/statistics harness must not race flake-redesign edits).
- **Phase 2 — Confirmed security (strict sequence, P0):** `#425` → `#426` → `#427` → `#428`.
  Prerequisites: Phase 1 clear of overlapping files (#403 merged for #426/#427; #424 merged for
  #428). One shared redirect-policy helper (#426) then shared egress recursion (#427). Successor:
  Phase 3. May interleave with Phase 1 ONLY if files provably disjoint AND review queue free —
  not the case this run.
- **Phase 3 — Config/UI correctness:** `#429` → `#430` (same `web/app.js` monolith — serialize).
  Prerequisites: Phase 2 (snapshot/throttle semantics settled). Successor: Phase 4 + provider wave.
- **Phase 4 — Video lifecycle/billing:** `#431` → `#432` (same video subsystem — serialize).
  Prerequisites: frontend slot free (Phase 3) + #435 semantics noted. Successor: #398/#414/#420.
- **Phase 5 — Perf:** `#433` LAST of audit wave, with before/after alloc benchmarks.
- **Phase 6 — Provider/routing foundation:** `#393` → `#394` → `#395` → `#396`.
  Prerequisites: Phases 2–3 (provider form/snapshot settled). #395 coordinates with #404 probe-test
  work; #396 coordinates with #408 router math. Successors: #412 (needs measured behavior), #397.
- **Phase 7 — Protocol + intelligence:** `#397` + `#436` coordinated (one registry, not two);
  then `#412` (explainable policy, shadow first), `#419` (eval-gated, no hot-path LLM default),
  `#415` → `#421`.
- **Phase 8 — UX (single-frontend rule — never parallel):**
  blueprint #409/#423 → `#411` (IA) → `#416` (tokens) → `#399` (provider/routing) → `#413` (E2E gate);
  `#410` (traces) → `#417` (WebChat reuses contracts); `#414` (Studio, after #431/#432/#435);
  `#420` (adapters, after #398).
- **Phase 9 — Operations maturity:** `#434` (consistency first) → `#435` → `#436`/`#437` → `#438`.
  No HA/budget/DR claims without Phase 9 evidence.
- **Phase 10 — Final:** `#400` evidence-matrix verdict (PASS/FAIL/BLOCKED/UNVERIFIED), then reconcile
  and only then consider closing #439. #400 must be LAST.

Independent work that could safely parallelize (NOT activated this run — queue loaded):
#433 (perf, disjoint hot-path read work) vs UX Phase 8 — only with a free review slot and proof of
file disjointness at trigger time.

## 3. Lifecycle application this run (state machine §1–§10)

No issue satisfied READY (prerequisites green + no overlapping active branch/run + review capacity).
#425 is the closest (disjoint files from #403-active) but the review queue is saturated (PR #424
awaiting owner review, #403 agent active, #404 outcome unknown) and the Wave-C review-first rule
gives hardening precedence — triggering a third concurrent agent violates the one-per-subsystem
and owner-review-queue constraints. Per the brief: inventory delivered, NO `/opencode` trigger sent,
stopping safely.

## 4. Exact first next action for the owner (in order)

1. **Review + merge PR #424** (CI/Security/Redis green, mergeable clean). This closes #402 and
   unblocks #428 + the Wave-C successor queue.
2. **Let the ACTIVE #403 agent finish** (run 38837809470); on its PR, review fuzz targets + CI smoke
   cost (<~3 min rule) before merge.
3. **Report/verify the #404 trigger outcome** (triggered 01:23Z, no PR yet — check run status; if
   stalled/failed, owner decides retrigger — agents must NOT blind-retrigger).
4. Then the supervisor's next trigger is **#425** (OIDC throttle — smallest, most isolated P0),
   followed strictly by #426 → #427 → #428 → #429 → #430 → #431 → #432 → #433, one at a time.

## 5. What requires owner merge/review (nothing auto-merged by agents)

- PR #424 merge (owner only). All future phase PRs: owner review + merge, no auto-merge, no tags,
  no releases, no `COMPLETION_CHECKLIST.md` edits, no `/tmp` artifacts (all evidence in-repo).
- #401 tracker body append (verified statuses + queue §2/§4) needs a token-bearing edit — this
  runner had none; paste §2 + §4 into #401 or let the next authenticated supervisor run reconcile it.

## 6. Local verification evidence (this run)

- `git rev-parse HEAD` → `59c80524174865a94f25e4a0ee062c2385ab485b`; branch
  `opencode/issue439-20261016015848`; tree clean at start.
- `go build ./...` → success, zero errors. `go test -count=1 ./internal/egress/` → ok.
- Remote checks read via public API: main CI+Security success (01:52:33Z); PR #424 CI+Security+
  Redis-integration success (01:47:16Z); 2 in_progress opencode runs (this supervisor + #403).
- Deliberately NOT done: merges, releases, tags, branch deletions, `/opencode` triggers,
  parallel issue activation, #401 body edit (no auth), full `verify.sh` rerun (no-code run; main
  gate already green at this HEAD).
