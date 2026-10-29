# Issue #661 remainder: video cost/queue migration-failure evidence

Date: 2026-10-29. Base: main `808ebde260cc0deae44414d8895b5f1e4118b9df`
(merge of PR #722). Toolchain for all local measurements below: Go 1.27.0.
This PR is test-only. No production code, public API, or routing/protocol
behavior changed. Related to #661 and #653; it does not close either issue
by itself — see "What remains" below.

## What was already on main before this change

PR #665 (merged) and PR #724 (merged, commit `75e405b`) had already
brought the scoped packages to the numbers below. This change does not
re-claim their work, and it does not duplicate any test they added.

## Fresh baseline on main `808ebde` (canonical unit command, no PG)

```bash
export PATH="$HOME/go/bin:$PATH" TMPDIR=$HOME/workspace/.tmp GOTMPDIR=$HOME/workspace/.tmp
go test -count=1 -coverprofile=.artifacts/coverage/cost-queue-baseline.out \
  ./internal/video/cost ./internal/video/queue
```

| Package | Unit coverage | Statements |
| --- | --- | --- |
| `internal/video/cost` | 96.6% | 288/298 |
| `internal/video/queue` | 98.1% | 370/377 |

The 17 uncovered unit statements, from the cover profile, were:

| File:lines | Statements | Nature |
| --- | --- | --- |
| `cost/ledger.go:237-238` | 1 | Reconciliation delta NaN/Inf return |
| `cost/pg_ledger.go:67-68` | 1 | `sql.Open("pgx", …)` error return |
| `cost/pg_ledger.go:77-84` | 7 | `OpenPostgresLedger` post-ping path: setup, migrate call, migrate-failure close/return, success return |
| `cost/pg_ledger.go:352-353` | 1 | PG Reconciliation delta NaN/Inf return |
| `queue/file_store.go:85-86` | 1 | `persist` write/sync/close error return |
| `queue/pg_store.go:59-60` | 1 | `sql.Open("pgx", …)` error return |
| `queue/pg_store.go:69-74` | 5 | `OpenPostgresStore` post-ping path: setup, migrate call, migrate-failure close/return, success return |

## What this PR adds (2 new files, integration-tagged only)

- `internal/video/cost/pg_ledger_open_migrate_test.go`:
  `TestOpenPostgresLedgerMigrateFailureFailsClosed`
- `internal/video/queue/pg_store_open_migrate_test.go`:
  `TestOpenPostgresStoreMigrateFailureFailsClosed`

Both follow the repository's existing convention for these packages:
real-PostgreSQL tests carry the `integration` build tag and read
`NEXAROUTE_TEST_POSTGRES_DSN`; ordinary unit runs never touch a real
server. No fake SQL driver is used here, because the point is the real
constructor's failure path, and the existing fake-driver tests already
cover the per-operation paths.

Each test, against a disposable PostgreSQL 16 server:

1. Creates a fresh database (so `CREATE TABLE IF NOT EXISTS` cannot skip
   creation, as it would in the shared integration database where the
   table already exists).
2. Creates a login role with no `CREATE` privilege on that database's
   public schema. Ping and `BEGIN` succeed; the advisory lock succeeds;
   `CREATE TABLE` fails with a permission error.
3. Asserts the constructor returns a migration error, a **nil**
   ledger/store, an error mentioning "migration", and an error that does
   not contain the test credential.
4. Asserts, through a connection to the fresh database, that the failed
   migration left no table (and, for the queue store, no index) behind —
   i.e. the migration transaction rolled back.
5. Cleans up: terminates stray sessions, drops the fresh database, then
   drops the role. Verified after the run: no `nr661*` database or role
   remained.

Determinism notes: no sleeps, no timing assertions, no `t.Parallel`
(the tests create databases and roles). The only time-dependent value is
a numeric suffix used to make database/role names unique.

## After this change

Unit profile (canonical command, no PG) is unchanged by design — the new
tests are integration-tagged and therefore excluded from it:

| Package | Unit coverage |
| --- | --- |
| `internal/video/cost` | 96.6% (288/298) |
| `internal/video/queue` | 98.1% (370/377) |

Integration-inclusive profile (same two packages, `integration` tag,
disposable PostgreSQL 16, DSN `postgres://postgres@127.0.0.1:55433/nexaroute_test?sslmode=disable`):

```bash
NEXAROUTE_TEST_POSTGRES_DSN='postgres://postgres@127.0.0.1:55433/nexaroute_test?sslmode=disable' \
  go test -tags integration -count=1 -coverprofile=.artifacts/coverage/cost-queue-integration2.out \
  ./internal/video/cost ./internal/video/queue
```

Result: both packages `ok`, exit 0.

| Package | Integration-inclusive coverage | Statements |
| --- | --- | --- |
| `internal/video/cost` | 99.0% | 295/298 |
| `internal/video/queue` | 99.5% | 375/377 |

The 12 post-ping statements from the baseline table are now covered
(7 cost + 5 queue): the success path was already covered by the
pre-existing integration tests, and the migrate-failure path is covered
by the two new tests.

## What remains: 5 statements, classified as defensive/unreachable

These 5 are the only uncovered statements in the scoped packages under
unit + integration measurement, and each was checked against the actual
implementation rather than assumed:

1. `cost/ledger.go:237-238` and `cost/pg_ledger.go:352-353` — the
   `Reconciliation` delta NaN/Inf return. Both call sites sanitize the
   operands first: an invalid estimate/actual is replaced by 0, and the
   earlier branch returns when either operand was invalid. The remaining
   operands are therefore finite and >= 0, so their difference is always
   finite. The check is defense-in-depth against a future edit that
   skipped sanitization; no input through any public or fake-driver path
   can reach it without changing the sanitization.
2. `cost/pg_ledger.go:67-68` and `queue/pg_store.go:59-60` — the
   `sql.Open("pgx", …)` error return. `database/sql.Open` returns this
   error only for an unregistered driver name, and `pgx` is registered
   by the blank stdlib import in the same package. The branch is dead
   while that import stands; removing the import to "cover" it would
   break every real PostgreSQL path.
3. `queue/file_store.go:85-86` — the `persist` write/sync/close error
   return. The marshal error and the rename error are already covered
   (PR #724). Reaching this return needs a write, sync, or close failure
   on a temporary file in a writable directory — i.e. a disk-full or
   filesystem fault, which this package does not fault-inject. It is
   not reachable with an ordinary writable directory, read-only
   directory, or directory-as-path input (those fail earlier, at
   `MkdirAll`, `CreateTemp`, or `Rename`, all already covered).

No statement was covered by weakening a check, by an assertion-free
test, or by excluding a package. No production behavior was changed to
gain a percentage point.

## Reconciliation with #730 and #740

- `gh pr diff --name-only` for PR #730 and PR #740, checked 2026-10-29,
  lists **zero** files under `internal/video/cost/` or
  `internal/video/queue/`. Both are #663 repo-wide waves, not #661 work.
- This PR's two new test files have no name collision with #730's
  `TestIssue663*` tests or #740's `Continue95*` tests, and it touches
  no file either of those PRs touches.
- CI status at PR creation (for context, not as evidence for this PR):
  #730 checks mostly pass with verify/container pending; #740's Fresh
  Go coverage and verify checks fail. Nothing in this PR depends on,
  duplicates, or fixes either wave, and this PR must not be presented
  as fixing #740's red checks.

## Verification on the PR head (local, Go 1.27.0)

```bash
go test -count=1 ./internal/video/cost ./internal/video/queue
go test -race -count=1 -timeout=120s ./internal/video/cost ./internal/video/queue
go vet ./internal/video/cost ./internal/video/queue
gofmt -l internal/video/cost internal/video/queue
go vet -tags integration ./internal/video/cost ./internal/video/queue
NEXAROUTE_TEST_POSTGRES_DSN='postgres://postgres@127.0.0.1:55433/nexaroute_test?sslmode=disable' \
  go test -tags integration -count=1 ./internal/video/cost ./internal/video/queue
```

All pass locally (unit, race, vet, gofmt clean; integration `ok` for
both packages). Repository-wide `scripts/verify.sh` and the full
canonical coverage run are reported separately in the PR body / CI:
they are the deciding gates, and a local scoped pass is not presented
as a substitute for them.

## What this does NOT claim

- The canonical unit numbers (96.6% / 98.1%) did not move, because the
  new tests are integration-tagged by repository convention. The
  integration-inclusive numbers (99.0% / 99.5%) are a separate
  measurement and must not be quoted as the canonical unit numbers.
- Repository-wide 95% is not reached by this scoped change; that is
  #663's target, and #730/#740 are its open waves.
- No live-service acceptance is claimed: the PostgreSQL server used
  here was disposable and local, and no external tenant, credential,
  or paid service was involved.
