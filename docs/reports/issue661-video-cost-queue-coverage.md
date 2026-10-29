# Issue #661 — video cost and queue behavior coverage

## Scope

Only tests in `internal/video/cost` and `internal/video/queue` changed. No
production code, public API, or routing/protocol behavior changed.

Measurement commit: `9f7e086bb7cacbfa6101ec5afe0f28fe58292a01`.
Baseline commit: `e6c3cdf3fe0a193dda0c5857e91e46b541d04729`.

## Before and after

Canonical command, run from a clean worktree of the baseline commit and then
from the measurement commit:

```bash
go test -count=1 -coverprofile=.artifacts/coverage/issue661/repo-{before,after}.out ./...
```

The baseline worktree used the same command with an absolute profile path
inside this repository. The `functions.txt` total lines were:

| Measurement | Repository | `internal/video/cost` | `internal/video/queue` |
| --- | --- | --- | --- |
| Before (`e6c3cdf`) | 85.0% | 59.4% | 61.5% |
| After (`9f7e086`) | 85.6% | 95.0% | 95.0% |

Exact per-package lines from the canonical runs:

```text
ok  github.com/ali-shortcuts/nexaroute/internal/video/cost   0.015s  coverage: 59.4% of statements
ok  github.com/ali-shortcuts/nexaroute/internal/video/queue  0.029s  coverage: 61.5% of statements
total: (statements) 85.0%

ok  github.com/ali-shortcuts/nexaroute/internal/video/cost   0.007s  coverage: 95.0% of statements
ok  github.com/ali-shortcuts/nexaroute/internal/video/queue  0.028s  coverage: 95.0% of statements
total: (statements) 85.6%
```

The repository-wide target of 95% was not reached by this scoped change.
The measured result is 85.6%; the remaining repository coverage is outside
the paths allowed by issue #661.

## Tests added

- `internal/video/cost/ledger_test.go`: zero-value ledger settlement and
  zero-estimate reservation reconciliation boundaries.
- `internal/video/cost/pg_ledger_fake_test.go`: deterministic
  `database/sql` fake exercising the PostgreSQL ledger adapter's migration,
  reserve/release, idempotent and conflicting settlement, estimate-to-actual
  upgrade, fallback estimates, entry listing, reconciliation sanitization,
  transaction failures, store read degradation, and defensive scan failures.
- `internal/video/queue/pg_store_fake_test.go`: deterministic
  `database/sql` fake exercising the PostgreSQL job store's migration,
  CRUD/idempotency, duplicate classification, terminal-overwrite guard,
  claim/renew/expiry behavior, lease-column overlay, list ordering, unique
  violation mapping, transaction rollback, and fail-closed error paths.

The fakes prove the adapters' application-level state machine and error
mapping without a service. They are not evidence of live PostgreSQL
semantics; the existing build-tagged integration suite remains the
real-service evidence path in `.github/workflows/ci.yml`
(`integration-postgres`). No live PostgreSQL or external provider acceptance
was run for this report.

## Verification

Run at measurement commit `9f7e086`:

```bash
go test -count=1 ./internal/video/cost ./internal/video/queue
go test -race -count=1 -timeout=120s ./internal/video/cost ./internal/video/queue
go vet ./internal/video/cost ./internal/video/queue
./scripts/verify.sh
```

Results:

- Focused package tests: `ok` for both packages.
- Focused race tests: `ok` for both packages.
- `go vet`: no findings.
- `./scripts/verify.sh`: `VERIFY PASS` (exit 0). Go tests, race pass,
  formatting, vet, release/SBOM checks, fuzz smoke, and linux amd64/arm64
  builds passed. The browser acceptance step was skipped by the script
  because Chromium and Python Playwright were unavailable on this runner;
  no browser evidence is claimed.

Local toolchain was Go 1.25.0. GitHub Actions uses the workflow-pinned
Go toolchain; CI results are distinct from this local evidence.

## Remaining uncovered scoped behavior

- `OpenPostgresLedger` / `OpenPostgresStore` successful open, ping, and
  migration through a real server: covered by the build-tagged PostgreSQL
  integration suite, not by the local unit pass.
- Defensive clamps and non-finite guards that require corrupted in-memory
  state or unreachable arithmetic overflow.
- `json.Marshal` failure branches for `VideoJob`, whose concrete fields are
  serializable.
- File-store OS permission/write failure branches that are environment
  dependent; existing tests exercise the reachable rollback paths.
