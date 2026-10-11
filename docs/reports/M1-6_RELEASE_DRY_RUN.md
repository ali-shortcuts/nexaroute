# M1-6 — Release Workflow Dispatch Dry-Run

**Date:** 2026-10-11

**State:** IN PROGRESS — dispatch evidence pending

## Planned workflow behavior

The existing `workflow_dispatch` path already built and uploaded release assets while the release-publish job was tag-only. This change adds an explicit artifact round-trip to that manual path: after upload, it downloads the same versioned artifact into `dist-roundtrip`, checks every required file exists and is byte-identical to the original `dist` file, then verifies the downloaded `SHA256SUMS`.

The publish job condition is now explicit: it can run only for a tag **push** whose ref begins with `v`. A manual dispatch, including one dispatched against a branch with a version-like name, cannot publish a GitHub Release. The workflow's default `contents: read` permission remains in force for the verification job; only the tag-gated publish job has `contents: write`.

## Local verification

`./scripts/verify.sh` — **VERIFY PASS**; raw output is committed as `docs/reports/m1-6-verify.raw.txt`.

## Initial dispatch and correction

The first manual run, [#38125153341](https://github.com/ali-shortcuts/nexaroute/actions/runs/38125153341), passed full verification, bounded stress, local runtime smoke, and release-asset builds, then failed at **Integrated release smoke** before artifact upload. The error was `video.auth_token_env is required when video.enabled is true`. The `publish` job was skipped; upload/download were not reached, and no tag or release was created.

The smoke fixture enabled the local development fake video provider but omitted its required auth-token environment name/value. `scripts/release-smoke.sh` now supplies a test-only process-scoped placeholder to that fake provider; it is not a production credential and is not sent to an external provider.

The first local retry then returned HTTP 401 because the smoke's localhost client also omitted the fake provider's bearer header. The script now supplies the same test-only placeholder to the two authenticated video requests. After both fixes, `./scripts/release-smoke.sh dist` passed locally.

The first-run step log is preserved as `docs/reports/m1-6-first-dispatch.failure.raw.txt`.

## Retry plan

After pushing this fix, rerun `.github/workflows/release.yml` on `completion/m1-6-release-dry-run` with a unique dummy version. Verify that upload/download byte-comparison and checksum checks pass, `publish` is skipped, and the artifact contains only the expected build outputs. No release, tag, or publication is intended. Record the retry run ID, artifact metadata, and raw output here.
