# Issue #653 refresh — evening 2026-10-30, main `7a2623a9`

Scope: this refresh re-verifies the coverage measurement, the fail-closed
gate, and the branch-protection wiring on the current main head, and records
the soak state honestly. It does NOT close #653: the long soak on this exact
head is still in flight, and the external acceptance items #646–#652 are
still BLOCKED / NOT_RUN / OPEN.

## 1. Coverage on current main (verified first-hand, not copied)

- Main head: `7a2623a9252d14943ebad64b8945bbebafd4eef2` (merge of PR #778).
- Authoritative source: the retained CI artifact of Coverage report run
  41091704975 (push, head `7a2623a9`, conclusion success),
  artifact 12995010050, `coverage-7a2623a9-41091704975`.
  Artifact contents verified after download: `environment.txt` records
  commit `7a2623a9`, ref `refs/heads/main`, Go 1.27.3.
- Recomputed from the artifact profile (same awk as `scripts/coverage-gate.sh`,
  and the gate script itself run against the downloaded profile):
  **95.46% — 42,018 covered / 44,016 total statements, margin +202**
  whole statements above the 95% bar. Gate: **PASS** at inclusive 95.0%.
- Main moved during this refresh: `797eadfa` (merge of docs-only PR #784,
  one file, +66, zero Go changes — verified by diff). All measurements
  above bind to `7a2623a9`; the docs-only delta changes no Go statement,
  but the numbers are quoted against `7a2623a9` exactly, not against
  `797eadfa`, until the next Coverage report run on the new head lands.
  The denominator moved 43,865 → 44,016 across the merges since `7daa36d`.
  A prior third-party figure of 95.44% (42,009 / 44,015, synthetic commit
  97a8e05, not a main SHA) is NOT a main measurement and is superseded.
- Fresh local recomputation (canonical command, pinned toolchain via
  `GOTOOLCHAIN=go1.27.3`, `go test -count=1 -coverprofile=... ./...`,
  same denominator 44,016): the full local run hit ONE timing hang —
  `TestWave4AnthropicHedgePartnerSkippedAfterHedge` in `internal/httpapi`
  stuck for 5m21s and the package hit the default 10m test timeout, so its
  profile was truncated (local httpapi 9,674 covered vs CI 10,179; the
  whole-repo local total of 94.39% is exactly that truncation, not a
  coverage regression — every other package nets +35 vs CI). The same
  test passes standalone in 0.137s, and CI on this exact head is green
  (Fresh Go coverage success + verify success), so this is recorded as a
  sandbox-load timing artifact, not a main defect and not a coverage
  finding; no code or test change was made for it. Confirmed by re-running
  the package alone: `go test -count=1 -timeout 25m ./internal/httpapi/`
  → ok in 300.000s at 97.3% (10,179 / 10,459) — exactly the CI package
  numbers, closing the whole-repo gap. The CI artifact above
  remains authoritative.

## 2. Gate preserved, unchanged

- `scripts/coverage-gate.sh` (merged via PR #756) + count reporting
  (PR #759) + unfiltered trigger (PR #774) are intact on this head:
  threshold 95.0 inclusive, fail-closed on unreadable/unparseable profiles,
  zero exclusions (no package, file, or statement filtered), and the
  `Fresh Go coverage` job is a required status check (see §4).
- No threshold change, no package exclusion, no padding was made or needed.

## 3. Per-package review (from the CI artifact profile, 62 packages)

22 of 62 packages sit below 95%. The acceptance target is repository-wide;
no per-package 95% requirement is imposed, and none was added here.

Largest uncovered counts:

- `internal/nexaflow`: 81.6%, 712 uncovered. 30 of its functions are fully
  uncovered: 29 are PostgreSQL-store / PostgreSQL-delivery functions
  (integration-tag-only code, exercised only against a real database, not
  by the canonical unit-test profile) and 1 is `SortedNodeIDs`, a one-line
  alias for `TopologicalOrder` with no branch of its own. Testing the alias
  would be padding; testing the store without a database would be a fake.
- `cmd/gateway`: 92.4%, 386 uncovered, ZERO fully-uncovered functions.
  The uncovered statements are partial branches inside covered functions:
  the PostgreSQL control-plane success path (needs a real database; its
  fail-closed branches have unit tests) and error branches of the new
  vault-accept checks (e.g. `runVaultAcceptDecrypt` 58.6%,
  `runVaultAcceptExpiredToken` 53.5%, `runVaultAcceptMountVersion` 70.3%).
  The vault-accept code itself is heavily tested (most helpers 100%).
- All other below-95% packages have at most trivial fully-uncovered
  functions: one-line accessors (`Unwrap`, `Epoch`, `Cursor`) and the
  `cmd/videogen` entry-point `main`. Their remaining uncovered statements
  are partial branches inside covered functions.

Conclusion: no important uncovered path that is a defect was found, so
**no behavior test was added for coverage in this refresh**. Adding tests
to force the lowest packages to 95% would be padding or would pre-empt
open architecture decisions (notably the Phase-6 NexaFlow engine choice).

Below-95% packages (CI artifact, complete list):
`internal/nexaflow` 81.6% (712), `internal/video/storage` 84.4% (30),
`internal/desktop` 85.7% (8), `internal/budget` 85.9% (11),
`internal/evallive` 87.0% (14), `internal/video/audio` 90.3% (11),
`internal/video/agent` 90.4% (12), `internal/transport` 90.6% (3),
`internal/policysim` 90.9% (6), `internal/video/tts` 91.4% (17),
`internal/sharedstate` 92.0% (15), `internal/video/render` 92.1% (18),
`cmd/gateway` 92.4% (386), `internal/telemetry` 92.8% (7),
`internal/authz` 93.8% (1), `internal/retrysafety` 93.9% (3),
`internal/events` 94.0% (18), `internal/logging` 94.2% (9),
`internal/privacyaudit` 94.4% (9), `internal/route` 94.8% (9),
`cmd/videogen` 94.9% (16), `internal/video/composer` 94.9% (12).

## 4. Branch protection (verified first-hand, 2026-10-30 evening)

- Legacy branch-protection endpoint returns 404 ("Branch not protected").
  That is EXPECTED under rulesets: protection is enforced by ruleset, not
  by the legacy endpoint. The ruleset is the verification target.
- Ruleset `main-required-checks`, id 26200123, enforcement **active**,
  target `refs/heads/main`, updated 2026-10-30T15:55:48+04:30, requires
  exactly these 9 deterministic status contexts (names read from the
  ruleset itself, and each verified success on commit `7a2623a9` via the
  commit check-runs endpoint):
  `verify`, `ci-soak-smoke`, `fuzz-smoke`, `integration-postgres`,
  `integration-redis`, `container`, `Go vulnerability scan`,
  `CodeQL (Go)`, `Fresh Go coverage`.
  No live acceptance job (LLM canaries, OIDC, Vault, K8s, ComfyUI, HA) is
  required for ordinary merges — by design, so a missing operator
  credential can never deadlock a merge.
- `strict_required_status_checks_policy: false` (branches need not be up
  to date — a fast-moving main cannot deadlock merges), no required review
  count (the owner is the sole collaborator; a review requirement would
  deadlock merges), `deletion` and `non_fast_forward` blocked, and the
  admin role may bypass ONLY via pull request (direct pushes stay blocked
  for everyone; audited emergency path). Rollback: disable or delete
  ruleset 26200123.
- No ruleset change was needed in this refresh: the wiring from the
  earlier refresh is intact and its contexts are green on this head.

## 5. Layers, separated exactly

- **Code coverage**: MET on this head (95.46% CI, gate PASS). This layer
  alone never closes #653.
- **Bounded CI smoke**: `ci-soak-smoke` success on this head (short
  soak-smoke job in CI). This is NOT the long soak.
- **Long soak**: the `ccce752` PASS (run 41011189189, 7419s, 10/10 stages)
  and the `7daa36d` PASS (run 41040550445, 7437s, 10/10 stages) bind to
  their commits ONLY and are NOT evidence for `7a2623a9`.
  Fresh 2h soak on this exact head: run 41101588099 (workflow_dispatch,
  dedicated branch `soak-run-7a2623a9`, head `7a2623a9`, dispatched
  2026-10-30T17:47:56Z, rounds=3, duration=2h, timeout 180m) — adopted, NOT
  re-dispatched (a duplicate would race the soak workflow's
  cancel-in-progress rule). Status at this report: QUEUED / in flight.
  Validation (first-hand artifact check: PASS exit 0, sustained stage
  ≥ 7200s, 10/10 stages, commit binding) is owned by the armed 30-minute
  monitor `nexaroute-soak-7a2623a9-monitor`, which self-removes after
  reporting and merges/closes nothing. Until that verdict lands, the
  current-main soak criterion is PENDING.
- **Live external acceptance**: #646–#652 remain BLOCKED / NOT_RUN / OPEN
  (live LLM canaries, OIDC tenant, Vault KV staging, Vault Transit live,
  K8s live runner, ComfyUI real inference, multi-host HA drill). Nothing
  in this refresh evidences any of them.

## 6. Status

**#653 stays OPEN.** Coverage is met and enforced on `7a2623a9`, the
protection wiring is active and green, but the 2h soak on this exact head
is unvalidated and the #646–#652 external gates are unevidenced. It closes
only when a 2h soak is validated on the then-current main (or a newer main
it binds to) AND the external gates are evidenced — or the owner records
an explicit, reviewed release exclusion. Green main + 95.46% alone do not
close it.
