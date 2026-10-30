# Issue #664 — long-duration soak on current main 7daa36d + coverage-gate verification

Date of this record: 2026-10-30 (checks run during the 10:2x–10:5x UTC window).
Status of the soak itself: **RESULTS PENDING — NOT A PASS.** Nothing in this
file claims the 7daa36d soak passed; it had not finished when this file was
written. Issue #664 stays open until the run below completes, its artifact
summary is validated against the commit and the 7200 s sustained duration,
and a results report is filed.

## 1. Duplicate check (done before any action)

- `gh pr list --state open` returned zero open PRs at check time.
- `git ls-remote --heads origin` showed no `supervisor/issue664-soak-7daa36d`
  branch and no other soak/coverage branch for 7daa36d before this branch.
- In-progress workflow runs on 7daa36d at check time were `opencode` lanes
  (issue_comment events, dispatched ~10:13Z). Those lanes were not touched,
  and no branch of theirs was used.
- Two Soak runs on 7daa36d were already in progress (see §2). Because
  `soak.yml` sets `concurrency: group: soak-${{ github.ref }}` with
  `cancel-in-progress: true`, dispatching a further soak on main could have
  **cancelled** a running one. No soak was dispatched by this lane.

## 2. Soak runs on 7daa36d (both already running — neither dispatched by this lane)

### 2a. PRIMARY tracked run: 41040550445

- Workflow: `Soak` (`.github/workflows/soak.yml`), `workflow_dispatch`
- Run: [41040550445](https://github.com/ali-shortcuts/nexaroute/actions/runs/41040550445)
- Dispatch branch: `soak-run-7daa36d` (dedicated branch, so later main pushes
  cannot cancel it via the soak concurrency rule)
- Head SHA: `7daa36da962bf3031b353487f760506cd60aedb2` (current main,
  "Merge pull request #764")
- Created: 2026-10-30T10:24:45Z; status at last check: `in_progress`,
  conclusion `null`.
- Dispatch parameters (deliberate dispatch, owner tap-choice 14:51 +0430):
  rounds=3, duration=2h, timeout_minutes=180, runner ubuntu-24.04.
  Expected completion ~12:40Z, hard cap ~13:24Z (dispatching lane's record;
  the GitHub API does not expose `workflow_dispatch` inputs on the run
  object, so the authoritative parameter record will be the run's own
  artifact summary — `requested_duration`, `stress_rounds`,
  `total_duration_seconds` — once it completes).
- Monitor: goal-owned cron `nexaroute-soak-41040550445-monitor` (30 min,
  self-removing) validates the artifact summary against
  `--expected-commit 7daa36d` and `--expected-duration-seconds 7200` plus
  10 stages. This lane does not poll the run to completion and does not
  merge or close anything on its behalf.

### 2b. CONCURRENT run (noted only, NOT relied on): 41040133996

- Run: [41040133996](https://github.com/ali-shortcuts/nexaroute/actions/runs/41040133996),
  `workflow_dispatch` on main at the same SHA, created 2026-10-30T10:21:24Z,
  job started 10:21:27Z, status at last check: `in_progress`.
- It is NOT relied on as evidence: its dispatch inputs are not verifiable
  from this lane, and it sits on main under the soak cancel rule (a later
  main soak dispatch could cancel it). It was not cancelled, not interfered
  with, and is tracked separately (monitor
  `nexaroute-soak-664-monitor-41040133996`). If both runs complete, the
  primary run (§2a) is the tracked evidence; the concurrent run is at most
  corroboration, never a substitute.

## 3. Coverage-gate verification on the same SHA (first-hand, from the retained CI artifact)

Latest successful Coverage run on 7daa36d at check time (no newer successful
Coverage run on this SHA exists):

- Coverage run: [41013985974](https://github.com/ali-shortcuts/nexaroute/actions/runs/41013985974)
  (push event, `completed`/`success`, created 2026-10-30T06:25:23Z)
- Retained artifact (not expired):
  [12959051528](https://github.com/ali-shortcuts/nexaroute/actions/runs/41013985974/artifacts/12959051528)
  (`coverage-7daa36da962bf3031b353487f760506cd60aedb2-41013985974`)
- Artifact `environment.txt`: commit
  `7daa36da962bf3031b353487f760506cd60aedb2`, ref `refs/heads/main`,
  **Go 1.27.3** (`go version go1.27.3 linux/amd64`).
- Recomputation from the artifact's own `coverage.out` (awk over statement
  counts, mode line skipped): covered **41,895** / total **43,865** =
  **95.5089%**; `functions.txt` tail reports **95.5%** (same profile).
- Gate run (unmodified `scripts/coverage-gate.sh` from main 7daa36d, threshold
  95.0, commit and Go version passed in as report-only env):
  `coverage gate: PASS total=95.5% (threshold >= 95.0%)`, exit 0.
- Package results in the artifact: 63 lines in `packages.txt`, all `ok`,
  0 FAIL lines.
- Margin over the 95% bar: 95% of 43,865 is 41,671.75, i.e. 41,672 statements
  required; 41,895 covered gives a margin of **+223 statements**.

| Check | Result |
|---|---|
| SHA matches current main | 7daa36d — yes |
| Go version (artifact, not local) | 1.27.3 |
| Covered / total (recomputed) | 41,895 / 43,865 = 95.5089% |
| Gate (`coverage-gate.sh`, 95.0) | PASS, exit 0 |
| Packages | 63 ok, 0 FAIL |

Deliberate non-comparison: no local Go 1.27.0 total is quoted here as a CI
total. Local and CI denominators differ; only the CI artifact above counts
for the gate.

## 4. Prior-commit soak context (not current-main evidence)

- Run [40891002045](https://github.com/ali-shortcuts/nexaroute/actions/runs/40891002045)
  on `bb5865f` (success) was the previously cited long-soak PASS.
- Newer, also prior-commit: run
  [41011189189](https://github.com/ali-shortcuts/nexaroute/actions/runs/41011189189)
  on `ccce7524` (an ancestor of 7daa36d, 16 commits behind), success
  2026-10-30T05:56:34Z–08:00:13Z, artifact
  [12962408534](https://github.com/ali-shortcuts/nexaroute/actions/runs/41011189189/artifacts/12962408534):
  3 stress rounds, sustained duration 2h (7200 s), total 7419 s, result PASS,
  Go 1.27.3, 4-vCPU runner, 10 stages PASS.
- Neither binds to 7daa36d. Only the primary run 41040550445 (§2a), once
  complete and validated, can serve as tracked long-soak evidence for
  7daa36d.
- Run 40983541160 remains CANCELLED, not a pass, and is not relied on.

## 5. Limitations (explicit)

- The soak runs on a single GitHub-hosted `ubuntu-24.04` runner
  (same-host evidence). It is **not** multi-host HA evidence and proves
  nothing about failover, backup/restore, or measured RPO/RTO (issue #652).
- A short CI smoke test is not long-duration stability proof; only the
  completed 2 h sustained run counts, and it has not completed yet.
- Soak evidence binds to exactly one commit (7daa36d). Any later main
  movement requires a fresh soak for the new head.
- No live, paid, cloud, or production action was taken by this lane; no
  secrets appear in this file.

## 6. What remains pending

1. Primary run 41040550445 completes (expected ~12:40Z, hard cap ~13:24Z).
2. Its artifact summary is validated: commit = 7daa36d, sustained duration
   7200 s, 10 stages PASS, validator exit 0 (monitor
   `nexaroute-soak-41040550445-monitor`).
3. A results report is filed and issue #664 is re-evaluated against its
   acceptance criteria (still open at the time of this file).
4. Concurrent run 41040133996 is informational only; no outcome of this
   lane depends on it.
