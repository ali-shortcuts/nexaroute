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

## Second wave (this update, commit 787dbd1 on top of 7c8b128)

Closes the gaps the first wave left against the #664 acceptance
brief: retries are covered via idempotent re-settle/re-claim paths,
cancellation at the worker level, backend (persist) failures with
rollback, process restart/recovery, corrupted persisted state,
double claims/settlements, lost updates, and resource growth. No
parallel framework was created: `scripts/soak.sh` was extended
(video packages added to the stress rounds and the race-soak stage,
plus a new `video-recovery-soak` stage); `scripts/stress.sh` needed
no change — its video stages now pick up the new `TestStress*`
tests automatically.

### Category 1 — bounded deterministic stress (CI-suitable)

Cost ledger (`internal/video/cost/mission95_stress2_test.go`):

- `TestStressLedgerSameJobSettleRace`: 32 goroutines settle one
  reserved job with distinct values behind a start gate — exactly
  one wins, spent equals the winner's value, the rest get conflict
  errors, and a same-value re-settle stays idempotent.
- `TestStressLedgerDoubleReserveCountsOnce`: 32 concurrent duplicate
  reservations succeed but count once (reserved == upper bound
  once, exactly one entry).
- `TestStressLedgerFixedSeedWorkload`: a seeded per-job chain
  workload (`NEXAROUTE_STRESS_SEED`, default 42) run concurrently
  and replayed sequentially; both agree exactly with the model
  (spent/estimated/reserved/entries). The seed fixes the workload,
  not the goroutine schedule; assertions are schedule-invariant.
- `TestStressLedgerInvalidAmountsLeaveTotalsUntouched`: unknown
  price, negative, NaN, infinite, and upper<estimate inputs are
  rejected with totals and entries unchanged, under concurrency.
- `TestStressLedgerReleaseThenSettle`: release frees the
  reservation, a later settle still accounts, double release never
  drives reserved negative.
- `TestStressLedgerGrowthIsAccounted`: 2000 reserve+settle cycles
  leave exactly 2000 entries — the ledger does NOT prune, so
  growth is linear and accounted, not bounded. Long-lived ledgers
  grow with jobs served; this is documented, not claimed away.

Queue (`internal/video/queue/mission95_stress2_test.go`):

- `TestStressSameOwnerConcurrentRenewal`: 32 same-owner renewals
  all succeed, owner/lease unchanged, foreign owner still blocked.
- `TestStressDuplicateCreateAndIdempotency` (memory + file):
  duplicate JobID creates yield exactly one success with the
  original untouched; shared idempotency keys yield one success
  and `ErrIdempotencyConflict` for the rest, winner retrievable.
- `TestStressTerminalUpdateRace`: 16 distinct terminal updates
  race — exactly one wins, the final record equals that winner
  exactly (no torn write), identical re-update allowed, differing
  update refused with `ErrTerminalJob`.
- `TestStressConcurrentUpdateNoTornWrite`: 32 whole-record updates
  to a non-terminal job — final record equals exactly one
  candidate.
- `TestStressFileStoreRestartRecoversFullState`: a mixed set
  (live lease, expired lease, terminal job, idempotency keys,
  progress) survives reopen exactly; a stray temp file from an
  interrupted persist does not break loading; post-restart rules
  (live blocks, expired frees, terminal guard, duplicates fail)
  all hold. This goes beyond the pre-existing single-lease reopen
  unit test.
- `TestStressFileStoreCorruptStateFailsClosed`: garbage,
  truncated JSON, and duplicate persisted idempotency keys are all
  refused, and a failed open leaves the file bytes untouched.
- `TestStressFileStorePersistFailureRollsBack`: with the store
  directory deterministically replaced by a regular file, Create
  and TryClaim fail and their in-memory effects roll back (job
  absent, lease absent). No permission tricks (unreliable as
  root).
- `TestStressClaimWorkerStopsOnCancel`: workers ack each claim,
  so the test knows claims finished before cancelling; after
  cancellation no further job is claimed. It also pins the
  store-level fact that contexts are currently ignored by the
  stores — cancellation must live in the caller loop, as tested.

### Category 2 — soak/recovery (bounded rounds, logged)

- `TestSoakVideoQueueRestartRounds`
  (`internal/video/queue/mission95_soak_test.go`): 5 rounds × 20
  jobs, reopen after every round, full state verified, plus a
  corrupt-copy episode (corrupt copy fails to load, original
  intact). Logged: per-round duration and file bytes
  (10497 → 52321 across rounds, ~524 bytes/job, linear and
  accounted), 100 jobs, 0 errors.
- `TestSoakVideoLedgerSustainedTotals`
  (`internal/video/cost/mission95_soak_test.go`): 5 rounds × 100
  jobs, cumulative spent asserted exactly after every round
  (final 1772 over 500 jobs, 500 entries), plus a deterministic
  replay from the same seed (default 7) reproducing identical
  totals. Heap delta is logged as observation only.

### Evidence (local, Go 1.25.1 linux/amd64, this VM)

- `./scripts/stress.sh` → **STRESS PASS** (8 stages, 0 failures;
  video/cost 0.176s, video/queue 1.019s).
- `go test -race -run='^TestStress' ./internal/video/cost
  ./internal/video/queue` → **PASS** (cost 1.410s, queue 1.839s).
- Fixed-seed repeats: `NEXAROUTE_STRESS_SEED` 1, 42, 12345 →
  **PASS** each; `-count=3` over all video stress tests → **PASS**.
- `go test -race -run='^TestSoak' ./internal/video/cost
  ./internal/video/queue` → **PASS** (cost 1.179s, queue 13.726s).
- `go test -count=1 ./internal/video/...` → **PASS** (all 8
  packages: cost, orchestrator, providers, queue, render, runtime,
  storage, tts).
- `go vet` on both packages → clean; `gofmt -l` on the new files
  → clean; `bash -n scripts/soak.sh` → clean.
- Bounded `./scripts/soak.sh` (`NEXAROUTE_SOAK_ROUNDS=1`,
  `NEXAROUTE_SOAK_DURATION=0s`, label `local-20261028-664wave2`,
  2026-10-28) → **SOAK PASS**, 677s total, all 6 stages exit 0:
  stress-round-1 105s (now including video packages), http-hot-
  reload 56s, recovery 12s, **video-recovery-soak 17s** (cost
  0.138s, queue 8.539s), race-soak 447s (now including video
  packages), budget-leak-slope 39s. The soak ran on the wave-2
  working tree whose code content is commit 787dbd1 (the summary
  records base 7c8b128 plus 5 uncommitted entries — those entries
  are exactly the wave-2 files committed as 787dbd1; the local
  `.artifacts/soak/` output is gitignored and quoted here, not
  committed). Full `scripts/verify.sh` was NOT run locally on
  this branch; CI on the PR is the wider gate.

## NOT RUN / BLOCKED (honest boundaries)

- Long-form multi-hour soak (`scripts/soak.sh` with a sustained
  duration): NOT RUN — the soak above used `NEXAROUTE_SOAK_DURATION=0s`
  and bounded rounds. This VM is not a representative soak
  environment, and no operator multi-hour soak has been executed for
  this issue. The bounded soak above must not be described as proof
  of long-duration production stability.
- Multi-host failover, RPO/RTO, production SLOs: NOT RUN, not claimed.
  The file store is single-process by documented design; two
  processes sharing one store file are not made safe by it, and no
  test here claims otherwise.
- PostgreSQL-backed video ledger/queue stores: NOT RUN here (their
  suites skip without a DSN; no PG server was provisioned for this
  run). CI integration jobs cover PG separately.
- Store-level context cancellation: NOT honored (pinned by test);
  cancellation is a caller-loop responsibility.
- Ledger pruning: does not exist — entries grow linearly with jobs
  served (see the growth test). Retention pruning is future work,
  not a claim.
- Live provider / live tenant / live cluster acceptance: out of scope here;
  tracked under issues #646–#652.
