# Issue #661 follow-up — video cost and queue behavior coverage (2026-10-29)

## Scope

Only tests in `internal/video/cost` and `internal/video/queue` changed,
plus one test-only extension to the queue fake driver. No production
code, public API, or routing/protocol behavior changed.

Measurement commit: this branch's head (see PR; base main `ba5ff2f`).
Baseline commit: `ba5ff2f` (current main at branch creation; main has
since moved to `0d26dfe` via #693 and #721, which do not touch
`internal/video/cost` or `internal/video/queue` — verified with
`git diff ba5ff2f..0d26dfe -- internal/video/cost internal/video/queue`,
empty).

## Baseline correction — do not use the issue's numbers

The baselines quoted in #661 (cost 59.4%, queue 61.5%, repo 84.96%)
are from main `e6c3cdf3` and are stale: PR #665 already merged that
scope at 95.0% / 95.0%. Likewise `docs/KNOWN_GAPS.md` still cites
cost 47.3% from the 2026-10-25 artifact on `c786735` (pre-#665); that
section was not updated by this change, which is test-only and does
not revise the audit record.

Fresh scoped baseline on `ba5ff2f`, before any edit:

```bash
go test -count=1 -coverprofile=.artifacts/coverage/issue661-before.out ./internal/video/cost ./internal/video/queue
```

cost 95.0%, queue 95.0% — identical to the #665 result, so #665's gains
held on current main.

Repository baseline on `ba5ff2f`, canonical command
`go test -count=1 -coverprofile=... ./...` (exit 0, 63 ok), from the
independently retained coordinator artifact, recomputed here from its
profile: **91.8% (41,782 / 45,515 statements)**. A sibling run of the
same command on the same commit reported 41,783 — one statement of
run-to-run variation. A second local full run on this tree timed out
only in `internal/httpapi` (10m test timeout, no assertion failure)
and is NOT used as the baseline; its scoped numbers agreed (95.0% /
95.0%).

## Uncovered branches found before editing (unit profile, 28 blocks)

`internal/video/cost/ledger.go`: ReleaseForJob reserved clamp,
SettleActual estimated-spent and reserved clamps, SettleEstimate
reserved clamp, Reconciliation delta NaN/Inf branch, Reconciliation
estimate-settled branch.
`internal/video/cost/pg_ledger.go`: OpenPostgresLedger invalid-config,
success-construct, migrate-failure-close and return branches,
Reconciliation delta NaN/Inf branch.
`internal/video/queue/file_store.go`: NewFileStore non-NotExist read
error, persist marshal-failure, write/sync/close-failure and
rename-failure returns, Create duplicate job, Create idempotency
conflict, Update missing, TryClaim persist-failure rollback.
`internal/video/queue/pg_store.go`: OpenPostgresStore invalid-config,
success-construct, migrate-failure-close and return branches, Create
and Update encode failures, List rows-iteration failure.
`internal/video/queue/queue.go`: Enqueue pre-check context error,
Enqueue select Done-case context error.

## Tests added (deterministic, no sleeps, every test asserts)

`internal/video/cost/ledger_uncovered_test.go`:

- Reconciliation of an estimate-settled job.
- Defensive-clamp contract for corrupted in-memory state (white-box):
  release, settle-actual from reserved and from estimate-settled, and
  settle-estimate floor their totals at zero. These clamps are
  unreachable through the public API, which subtracts exactly what it
  added; the tests pin the clamp itself.
- Concurrent idempotent accounting: 32-way concurrent reserve counts
  once, concurrent settle-actual spends once, concurrent releases never
  go negative.

`internal/video/queue/file_store_uncovered_test.go`:

- NewFileStore surfacing a non-NotExist read error (path is a
  directory).
- Create duplicate job id and idempotency conflict; Update of a
  missing job.
- Persist marshal failure (NaN progress) rolls back job and
  idempotency key and leaves no file.
- Persist rename failure (store path is a non-empty directory) rolls
  back Create and TryClaim (lease restored).
- Concurrent claims of one job produce exactly one winner.

`internal/video/queue/queue_uncovered_test.go`:

- Enqueue on cancelled and deadline-expired contexts fails fast and
  leaves the queue usable.
- Enqueue's select Done case when the queue is full, via a context
  whose first Err() call passes the pre-check and later calls report
  cancellation (the select also has a default case, so the loop
  retries until the Done case is taken; no sleeps, no timing
  assertions). Observation, not a fix: when the queue is full and the
  context is cancelled, Enqueue may return ErrFull or context.Canceled
  depending on select's random choice among ready cases.

`internal/video/queue/pg_store_uncovered_test.go` (plus a test-only
fake extension: `failListRows` / `nextErr` on the fake rows):

- Create and Update encode failures (NaN progress): Create writes
  nothing, Update keeps the stored job.
- List rows-iteration failure surfaces as a `list rows` error with a
  nil list, not a partial result.

## Before and after

Scoped, `go test -count=1 -coverprofile=... ./internal/video/cost ./internal/video/queue`:

| Package | Before (`ba5ff2f`) | After (this head) | Statements |
| --- | --- | --- | --- |
| `internal/video/cost` | 95.0% (283/298) | 96.6% (288/298) | +5 |
| `internal/video/queue` | 95.0% (358/377) | 98.1% (370/377) | +12 |

Repository, `go test -count=1 -coverprofile=... ./...`
(full run on this head: 63 ok, 0 FAIL):

| Measurement | Repository |
| --- | --- |
| Before (`ba5ff2f`, coordinator artifact, recomputed) | 91.8% (41,782/45,515) |
| After (this head) | 91.8% (41,798/45,515) |

Net repository delta is +16, not +17: outside the scope, 5 statements
in 3 timing-sensitive blocks (gateway ComfyUI acceptance, probe
engine, canonical OpenAI chat) were covered in the baseline run but not
in the after run, partly offset by 4 newly covered statements
elsewhere (ComfyUI acceptance, httpapi hedging, telemetry). This diff
touches only cost/queue tests, so outside-scope variation is run
variation, not a regression. 95% repository-wide is NOT reached by
this scoped change; the remaining gap is outside these two packages.

## Verification (this head, Go 1.27.0)

```bash
go test -count=1 ./internal/video/cost ./internal/video/queue
go test -race -count=1 -timeout=120s ./internal/video/cost ./internal/video/queue
go vet ./internal/video/cost ./internal/video/queue
gofmt -l internal/video/cost internal/video/queue
./scripts/verify.sh
```

- Focused tests: ok, both packages.
- Focused race tests: ok, both packages (no data races).
- go vet: no findings. gofmt: clean.
- verify.sh: see below.

Security: local `govulncheck` (go1.27.0) reports 6 standard-library
findings whose traces run through pre-existing production code (e.g.
`pg_store.go` Update via `sql.Tx.Commit`); none traces to the new
tests, and this diff adds no imports or production code. CI CodeQL +
govulncheck on this head are the gate.

## Remaining uncovered scoped paths (unit profile; not padded)

- `cost/ledger.go` Reconciliation delta NaN/Inf branch and
  `cost/pg_ledger.go` Reconciliation delta NaN/Inf branch: unreachable
  through valid values — with finite, non-negative estimate and
  actual, their difference is always finite, and invalid values are
  caught by the preceding branch (already covered).
- `cost/pg_ledger.go` / `queue/pg_store.go` Open invalid-configuration
  branches: unreachable in this binary — `sql.Open("pgx", dsn)`
  returned nil for every malformed DSN probed (pgx parses at Connect,
  not Open), and the driver is always registered by the blank import.
  Verified by probe, not assumed.
- `cost/pg_ledger.go` / `queue/pg_store.go` Open success and
  migrate-failure-close branches: reachable only through a real
  PostgreSQL server; the build-tagged integration suites call Open
  (cost `pg_ledger_integration_test.go`, queue
  `pg_store_integration_test.go`, plus cold-open suites) and are the
  evidence path — CI `integration-postgres` on this head is the gate.
  Not claimed from the unit profile.
- `queue/file_store.go` persist write/sync/close failure branch:
  requires filesystem fault injection (full disk / failed write); the
  rename-failure and marshal-failure rollback paths added here prove
  the same rollback logic through reachable failures.

## verify.sh result (local)

Local `./scripts/verify.sh` exits 1, stopping at its go-test stage:
`cmd/gateway` hit the script's 3m-per-package cap (`panic: test timed
out after 3m0s`, FAIL at 180.261s) on code this diff does not touch.
The log contains zero `--- FAIL` assertion lines, and the scoped
packages pass in ~2s under the same script's invocation order. The
vet, race, and browser stages were not reached locally. This is the
known slow-VM timeout artifact already recorded for `ba5ff2f`
(httpapi / router / securitystore / gateway at the 3m cap), not a
regression from this test-only diff. CI verify on this head is the
gate for the full script.
