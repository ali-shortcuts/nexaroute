# Issue #437 — operator qualification runbook

This runbook is the reproducible path for the bounded performance, soak and
fault-injection qualification that can be run without production traffic or
production credentials. It does not claim production SLOs or live production
failover; those boundaries are stated at the end.

## 1. What the path already covers

- `scripts/load.sh` — deterministic mixed-workload profile at pool sizes
  10/100/1000; reports local gateway overhead separately from the fixed
  upstream delay. See `docs/reports/ISSUE437_LOAD_PROFILE.md` and
  `docs/PERFORMANCE_SLO.md`.
- `scripts/soak.sh` — stress rounds, hot-reload soak, recovery soak,
  race-enabled soak, budget-leak slope, per-stage raw logs, GNU `time -v`
  resource data, a human summary and a machine-readable `summary.json`.
- `.github/workflows/soak.yml` — `workflow_dispatch` runner for the same
  script with configurable `rounds`, sustained `duration` and job
  `timeout_minutes`; raw evidence is uploaded as a 90-day artifact.
- Real PostgreSQL/Redis outage drills in the existing integration jobs
  (`TestPostgresIntegrationOutageRecoveryCancellationBackpressure`,
  `TestRedisIntegrationOutageRecoveryCancellationBackpressure`).

## 2. Prerequisites

- Repository checkout at the commit being qualified, with a clean worktree.
- Go toolchain available on `PATH` (the module toolchain line in `go.mod` is
  authoritative).
- Linux for `/proc/self/fd` sampling and GNU `time`; other platforms still
  run the soak, but FD samples are reported as `-1`/absent.
- No provider keys, cookies or production credentials. The load profile and
  soak use loopback `httptest` services only.
- Docker is only required for the disposable real-service outage drills.

## 3. Short CI-shaped qualification

```sh
NEXAROUTE_SOAK_ROUNDS=3 NEXAROUTE_SOAK_DURATION=0s \
  NEXAROUTE_SOAK_OUT=benchmarks/soak ./scripts/soak.sh
```

`duration=0s` keeps the original fixed-iteration soak shape. This is the
bounded run used by ordinary repository qualification.

## 4. Bounded sustained qualification

The operator chooses the wall-clock duration. It is used only to decide when
to stop the mixed workload, never as a latency assertion.

```sh
NEXAROUTE_SOAK_ROUNDS=3 \
NEXAROUTE_SOAK_DURATION=30m \
NEXAROUTE_SOAK_OUT=benchmarks/soak \
./scripts/soak.sh
```

Rules enforced by `scripts/soak.sh`:

- duration syntax is a positive Go-style value written in seconds, minutes
  or hours: `90s`, `30m`, `2h`, `6h`; `0s` disables sustained mode;
- the maximum accepted duration is `6h` (the GitHub-hosted job ceiling);
- the HTTP soak stage timeout is automatically set above the requested
  duration, so a long run does not fail on the Go test deadline;
- any non-zero stage exit makes the whole run fail.

For workflow dispatch, set the job timeout comfortably above the duration:

```sh
gh workflow run soak.yml \
  --ref main \
  -f rounds=3 \
  -f duration=2h \
  -f timeout_minutes=180

# Find the run id, then watch and download the raw evidence directory.
gh run list --workflow=soak.yml --limit 1
gh run watch <run-id>
gh run download <run-id> -D benchmarks/soak/ci-<run-id>
```

The uploaded artifact is named
`soak-<full-commit-sha>-<run-id>-attempt-<n>` and contains the whole
`.artifacts/soak/` tree.

## 5. Evidence contract

Every run directory contains:

| File | Meaning |
|---|---|
| `summary.json` | Machine-readable qualification record (schema `nexaroute.soak.qualification.v1`). |
| `summary.md` | Same facts for a human reviewer and the GitHub step summary. |
| `soak.raw.txt` | Complete ordered command/stdout/stderr transcript. |
| `NN-<stage>.txt` | Per-stage raw output. |
| `NN-<stage>.time` | GNU `time -v` output (maximum RSS, faults, context switches). |
| `http-hot-reload-samples.jsonl` | JSON-lines samples of elapsed time, goroutines, open FDs and heap allocation taken during the sustained mixed workload. |

`summary.json` records, at minimum:

- exact commit SHA, branch, workflow ref/head ref, and worktree entry count;
- start/finish timestamps, total duration, requested sustained duration,
  stress rounds and overall PASS/FAIL plus each stage exit code;
- Go version, kernel/uname, CPU model, `nproc` and `GOMAXPROCS`;
- per-stage duration, maximum RSS and raw-log path;
- the resource-sample file path when sampling was active.

Quick inspection:

```sh
python3 - <<'PY'
import json
d = json.load(open("benchmarks/soak/<run>/summary.json"))
assert d["result"] == "PASS", d["result"]
assert d["commit"] and len(d["commit"]) == 40
assert all(s["exit_code"] == 0 for s in d["stages"])
print(d["commit"], d["requested_duration"], len(d["stages"]))
PY
```

## 6. What fails a run

The run is not a performance verdict on shared CI hardware and contains no
tight wall-clock thresholds, but it does fail on real contract breaks:

- any stress, hot-reload, recovery, race or budget-leak stage exits non-zero;
- the recovery soak cannot re-admit a deployment within its cycle deadline,
  reports fewer than `cycles × 3` successful probes, or leaves recovery/probe
  state behind;
- the sustained mixed workload leaves in-flight admission or provider
  capacity non-zero, leaks sessions past the bound, or exceeds the
  worker-proportional goroutine/FD growth bound;
- the budget-leak slope exceeds its deterministic limits;
- the workflow cannot preserve raw evidence (`if-no-files-found: error`).

Latency percentiles are reported and compared against the bands in
`docs/PERFORMANCE_SLO.md`; they are not asserted as fixed wall-clock limits in
ordinary CI.

## 7. Real PostgreSQL/Redis outage drill

These drills are safe only against disposable services: they apply test
migrations and write control-plane keys. Never point them at production.

```sh
docker run -d --rm --name nexaroute-drill-redis -p 127.0.0.1:16379:6379 redis:7-alpine
docker run -d --rm --name nexaroute-drill-pg \
  -e POSTGRES_USER=nexaroute -e POSTGRES_PASSWORD=nexaroute-ci-only \
  -e POSTGRES_DB=nexaroute_test -p 127.0.0.1:15432:5432 postgres:16-alpine

NEXAROUTE_TEST_POSTGRES_DSN='postgres://nexaroute:nexaroute-ci-only@127.0.0.1:15432/nexaroute_test?sslmode=disable' \
NEXAROUTE_TEST_REDIS_URL='redis://127.0.0.1:16379/15' \
  go test -tags integration ./internal/controlplane/ \
  -run 'TestPostgresIntegrationOutageRecoveryCancellationBackpressure|TestRedisIntegrationOutageRecoveryCancellationBackpressure' \
  -count=5 -v | tee benchmarks/soak/drills/outage-drill-$(date -u +%Y%m%dT%H%M%SZ).raw.txt
```

Expected evidence: all iterations PASS; after the fault proxy restores the
service, the original revision/payload is intact and the next write advances
exactly one revision. CI runs the same drills against its disposable
`integration-postgres` / `integration-redis` service containers.

## 8. Explicit boundaries (not claimed)

- **Multi-hour soak**: the path supports up to six hours, but a duration is
  only evidence when a summary with that duration and a PASS result is
  committed or uploaded. No multi-hour run is claimed by this runbook alone.
- **Production-traffic SLOs**: `docs/PERFORMANCE_SLO.md` contains local
  regression bands, not production latency/error budgets.
- **Live production PostgreSQL/Redis failover**: the repository drills use
  disposable local containers and a local TCP fault proxy. A real managed
  failover, replica promotion or production outage drill requires an
  operator-approved staging/live environment, real service endpoints and a
  rollback plan; it is intentionally not run by this repository's CI.
- **Live operator game day**: remains unverified unless separately run and
  evidenced.

The remaining live command shape (staging only, with explicit approval) is the
same drill above with `NEXAROUTE_TEST_POSTGRES_DSN` /
`NEXAROUTE_TEST_REDIS_URL` pointing at staging endpoints reachable from the
operator host, followed by the same PASS/evidence checks. It cannot be run in
this CI environment because no staging/production services or credentials are
configured, and injecting an outage into a shared service is not a safe
default action.
