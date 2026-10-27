# Issue #664 — stress/soak evidence (bounded CI smoke + opt-in long soak)

Status: **bounded CI smoke test DONE and passing locally; long-form multi-hour
soak NOT RUN.** This document records exactly what was executed and what was
not. No multi-host failover, RPO/RTO, or long-duration stability claim is made
here — those require representative runs that have not happened.

## What already existed (not duplicated)

- `scripts/stress.sh` — bounded stress harness, already wired into CI
  (`.github/workflows/ci.yml`, "Bounded stress checks") and
  `.github/workflows/release.yml`. Six stages before this change: router
  scale, probe/recovery, event-state, HTTP admission, concurrent log rotation,
  evaluation plane. All `NEXAROUTE_STRESS=1`-gated `TestStress*` tests.
- `scripts/soak.sh` (277 lines, five stages: stress rounds, HTTP hot-reload
  soak, recovery soak, race soak, budget-leak slope) with JSON + markdown
  evidence, plus `.github/workflows/soak.yml` (workflow_dispatch, duration
  ≤ 6h). The long-form soak procedure is opt-in and stays NOT RUN until an
  operator actually executes it in a representative environment.
- `docs/reports/ISSUE437_SOAK_QUALIFICATION.md` from the earlier #437 work.

## What this change adds

Two new `scripts/stress.sh` stages, wired the same way (no workflow file
edits — the lane failure mode for #664 was a rejected workflow push, so this
takeover deliberately avoids workflow edits entirely):

1. **Video cost ledger stress** — `internal/video/cost/mission95_stress_test.go`
   (`TestStressLedgerConcurrentSettleTotals`): 32 goroutines × 50 distinct jobs
   reserve then settle concurrently; asserts exact totals (all amounts are
   integer-valued, so float64 sums are exact): spent == 26400, reserved == 0
   after settlement, 1600 entries, one sample reconciliation exact; then
   duplicate settlement with the same value is idempotent and a conflicting
   settlement is rejected without moving the total.
2. **Video queue claim stress** — `internal/video/queue/mission95_stress_test.go`:
   - `TestStressMemoryStoreSingleClaimWinner`: 32 goroutines race to lease one
     job — exactly one wins, losers see claimed=false with no error, the
     winner renews, a foreign owner is blocked while the lease is live, and a
     new owner wins after the TTL (expiry exercised by advancing the explicit
     `now` argument — no sleeps, no wall-clock dependence).
   - `TestStressFileStoreSingleClaimWinner`: the same property against the
     durable file store.
   - `TestStressMemoryStoreConcurrentCreateAndClaim`: 16 workers × 25 distinct
     jobs, each created and self-claimed concurrently; final list holds
     exactly 400 jobs.

Scope note, stated honestly: the file store's check-and-set is atomic within
one process (mutex-guarded). These tests prove the in-process single-winner
property only, not cross-process file locking.

## Evidence (local, branch under review)

- `./scripts/stress.sh` → **STRESS PASS** (2026-10-27, Go 1.27.0, this VM):
  router ok, probe ok, events ok, httpapi ok, logging ok, eval ok,
  video/cost ok, video/queue ok — 8 stages, 0 failures.
- `go test -count=1 -race -run='^TestStress' ./internal/video/cost ./internal/video/queue`
  → **PASS** (race-clean by design: ledger and stores are mutex-guarded).
- `go test -count=1 -v -run='^TestStress'` → all four tests **PASS**
  (executed, not skipped) under `NEXAROUTE_STRESS=1`.
- `go test -count=1 ./internal/video/...` → **PASS** (wider local gate).
- `go vet ./internal/video/cost ./internal/video/queue` → clean.
- `gofmt -l` on the two new test files → clean. `bash -n scripts/stress.sh`
  → clean.
- Full `scripts/verify.sh` (race `./...` + browser E2E) was **NOT** run locally
  on this branch; GitHub Actions CI on the PR is the wider gate.

## NOT RUN / BLOCKED (honest boundaries)

- Long-form multi-hour soak (`scripts/soak.sh`): NOT RUN — this VM is not a
  representative soak environment, and no operator soak has been executed for
  this issue. The procedure exists and is runnable on demand; its output must
  be attached before any long-duration stability claim.
- Multi-host failover, RPO/RTO, production SLOs: NOT RUN, not claimed.
- Live provider / live tenant / live cluster acceptance: out of scope here;
  tracked under issues #646–#652.
