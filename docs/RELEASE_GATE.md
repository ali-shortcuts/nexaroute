# Release Gate

The release gate answers one question, for one exact commit SHA: did
every required check run on *that* SHA, and did it pass? A result from
any other SHA is never substituted, and a gate with no check run on
the SHA is NOT_RUN, never PASS.

## Checking a SHA (no local toolchain needed)

```sh
# From any machine with `gh` authenticated; no Go, Docker, or test
# dependencies are installed on the operator's machine. CI runs the
# tests; this script only reads their recorded check runs.
bash scripts/release-gate-status.sh <full-40-hex-sha>
# Release-candidate verdict (also requires the manual-only gates):
bash scripts/release-gate-status.sh <sha> --rc --soak-summary /path/to/downloaded/summary.json
```

The script exits non-zero unless every required gate is PASS, and
prints `Release Gate Passed` only in that case. A gate that is
queued or running prints RUNNING and fails the gate until it
finishes; an unreadable Checks API response prints BLOCKED. In `--rc` mode a
successful check also needs a completed successful Actions run for the same
head SHA triggered by push or manual dispatch. PR checks may be attached to a
head while testing a synthetic merge commit, so they are UNVERIFIED for that
head, not release evidence. Unknown or unreadable run provenance is likewise
UNVERIFIED. A passing soak check is not enough: `--soak-summary` must name a
complete PASS report for that SHA with at least 7,200 seconds requested and
sustained. Keep the report's resource-samples file alongside it. The operator
must independently verify that the supplied report and samples came from the
matching successful Actions run; local files alone are not authenticated.
The script does not verify owner approval, external acceptance, the provenance
of arbitrary workflow code, or the artifact-to-run binding. Its own
regression tests (`bash scripts/test-release-gate-status.sh`) run
offline against a fake `gh` and cover pass, one-failed, missing
(= NOT_RUN), running, API-error (= BLOCKED), non-SHA refusal, the
latest-run rule (old success must not hide a newer failure, old
failure must not hide a newer success or a newer running run, ties
broken by check-run id), and `--rc` given without a repo argument.

Note on invocation: the release-gate scripts are invoked with
`bash` throughout this document because their Git executable bit
is not guaranteed on every push path (a snapshot-style push lands
them at mode 644); `bash scripts/...` works regardless of that
bit, while a bare `scripts/...` invocation does not. Only
`scripts/verify.sh` is executed directly by CI, so its mode
100755 must be preserved on every push.

## PR-required gates

These run on `pull_request` and are required for any PR verdict:

| Gate | Owning workflow / job |
|---|---|
| verify.sh, stress, browser acceptance, container build/smoke | CI: `verify`, `container` |
| Fuzz smoke, soak smoke | CI: `fuzz-smoke`, `ci-soak-smoke` |
| PostgreSQL and Redis integration | CI: `integration-postgres`, `integration-redis` |
| Vulnerability scan | Security: `Go vulnerability scan` (govulncheck) |
| Static analysis (SAST) | Security: `CodeQL (Go)` |
| Coverage, 95.0% inclusive, no exclusions | Coverage: `Fresh Go coverage` |
| Two real Gateway processes against PostgreSQL; real keyring round-trip | Real-system acceptance |

Existing PR coverage already includes `gofmt`, `go vet`, race
(`go test -race ./...`), short fuzz, browser acceptance (Playwright,
pinned Chromium; a skipped suite fails the CI job, it is never
reported as a pass), and the final-release-gate regression script,
all inside `scripts/verify.sh` / CI. This document does not add a
second copy of those runs.

## Release-candidate-only gates

These do not run on a normal PR and are required only for a release
candidate (`--rc`):

| Gate | How it runs |
|---|---|
| golangci-lint, staticcheck, gosec, deadcode | Architecture & Quality Audit workflow, `workflow_dispatch` only |
| Extended fuzz (45 s per target by default) | Audit workflow, `fuzz-extended` |
| Soak, 2 h sustained mixed workload | Soak workflow, `workflow_dispatch` with `duration: 2h` and a timeout above 2 h 30 m (see `docs/reports/ISSUE664_SOAK_PROCEDURE.md`); the soak report must name the same SHA |
| Browser evidence on main | Browser evidence workflow, `workflow_dispatch` / main push |

## v0.25.0 owner policy: completion only

The owner selected completion rather than exclusion for v0.25.0. For this exact
version the completion gate rejects any nonempty or malformed exclusion
manifest, including signed entries; the retained unsigned historical proposal
therefore blocks the gate until removed or made empty by reviewed release
preparation. Do that before selecting the final candidate SHA and collecting
its evidence: modifying the manifest later changes the commit and makes earlier
exact-SHA evidence stale. All #646–#654 must be closed with `state_reason=completed`; #654 must have a
GitHub closure timestamp strictly later than every individual issue. The
generic version-scoped exclusion mechanism below is preserved for future
versions and is **not** usable to bypass v0.25.0 acceptance.

The v0.25.0 script additionally resolves `refs/heads/main` through the GitHub
API and requires the candidate SHA to equal the current main tip; a missing,
malformed or stale ref response blocks. A conflicting tag ref/name or attempt
to override the v0.25.0 tag version is rejected. Recheck main immediately before
release; the comparison is not an atomic lock on main. This script cannot
prevent an authorized person or a historical tag workflow from publishing
outside the current gate: repository tag/release permissions and operator
controls are separately required. In particular, a tag pointing at an older
ancestor can run that ancestor's older workflow/gate. Do not create such a tag.

For each completed issue the gate requires a GitHub issue comment starting
with `NEXAROUTE_ACCEPTANCE_V1` on its own line followed by one JSON object:
`issue` (integer), `release` (`v0.25.0`), `commit` (exact 40-character SHA),
`result` (`PASS`), `run_id` and `artifact_id` (positive integers), and a
nonblank `summary`. The latest structured record from an OWNER or MEMBER must
match the candidate. The gate verifies that the run is a successful completed
manual dispatch on that SHA, and the unexpired artifact belongs to that run
and SHA. Each issue-comment page is fetched individually using `gh api -i`.
The gate validates every Link relation, canonical repository identity, issue,
page and page-size, following the validated sequence with constructed endpoints
rather than arbitrary URLs. It compares the unique comment count and issue
`updated_at` before and after traversal. Concurrent changes can require a
fail-closed retry. Malformed or incomplete pages, duplicate JSON keys or IDs,
API failures, stale or missing records and parser errors fail closed.
These reads are not an atomic snapshot: same-timestamp edits, races after the
last read, and an untrustworthy API remain outside the guarantee. Operator
review is still required.

This metadata is **not proof** that the artifact contains genuine acceptance;
an independent reviewer must read the artifact and verify every issue criterion,
topology and authorization. GitHub Actions metadata read permission is needed;
if unavailable the gate blocks, not passes. External CLI acceptance needs an
authorized workflow that retains the sanitized report for this metadata check.
Never put credentials, private prompts or ciphertext in records.

## Generic completion gate and version-scoped exclusions (other versions)

For versions other than v0.25.0, `.github/scripts/check-final-release-gate.sh`
is the last step before publication. It requires issues #646–#653 **and** #654 to be closed.

- Closed with `state_reason=completed`: accepted, no further action.
- Closed any other way (`not_planned`, no reason): accepted **only** when
  `docs/releases/<version>-exclusions.json` names that exact issue, for that
  exact version, with a non-blank `reason` and `approved_by`, and a valid calendar `approved_at` date in YYYY-MM-DD format. The manifest is a record, not proof of owner identity: reviewers must independently verify the approval.
- **Open**: always refused. An exclusion can never cover an open blocker.

The manifest is version-scoped and fails closed: an entry with any field
missing refuses the release, an entry for an issue outside #646–#654 refuses
the release, an issue identifier must be an integer (not a numeric string), a duplicate entry refuses the release, a manifest whose
`release` does not match the tag refuses the release, and a manifest is never
carried forward to a later version. When a blocker needs no exclusion, an
unused or stale manifest is ignored rather than allowed to fail a clean
release.

The policy this implements is the one stated for #654: a stable release must
wait for each remaining scope to be *completed* or *explicitly excluded with
reviewed, truthful release notes*. The mechanism makes the exclusion
reviewable and machine-checkable; it does not make it silent. Every exclusion
is printed as a `::warning::` at release time.

See `docs/RELEASE_BLOCKERS.md` for the current per-issue blockers and the
operator handoff and evidence limits; no single dispatch clears every criterion.

## Gaps this gate does not hide

- **Mutation testing: NOT_RUN.** No mutation tooling is wired into
  CI. The loopback validator is mutation-sensitive in practice (the
  PR #794 Go 1.27 failure was a real mutant-like escape: a
  normalization step silently changed the checked value), but no
  mutation score is claimed until a tool is added and run.
- **Visual regression: NO BASELINE.** The browser workflows produce
  evidence and run behavioral suites; no committed visual baseline
  exists, so no new baseline may be recorded without human approval
  and no visual difference may be reported as a pass or a failure
  on baseline grounds alone.
- **Real external systems: NOT_RUN unless separately recorded.**
  Real Vault Transit, real Kubernetes/HA, and live-provider runs are
  manual, operator-authorized, and recorded in
  `docs/EXTERNAL_VERIFICATION.md`; they are never implied by mock
  tests. No test in the normal gates spends real provider quota or
  touches production.
- **Branch protection / rulesets:** not changed by this PR. If the
  owner wants the gates above enforced by GitHub (rather than by
  this script and reviewer discipline), that is a settings change
  for the repository owner to make and verify; it is documented
  here, not silently applied.

## What "Release Gate Passed" does and does not mean

It means: on the named SHA, every gate above ran and passed, and
no critical security issue is known-open against that SHA. It does
not mean zero bugs, and it does not cover the manual-only gates
unless `--rc` was used. The per-SHA evidence (workflow run links
and artifacts) is the verdict; this document and the script are
only the procedure for reading it.
