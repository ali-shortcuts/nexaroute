# M1-6 — Release Workflow Dispatch Dry-Run

**Date:** 2026-10-11

**State:** IN PROGRESS — dispatch evidence pending

## Planned workflow behavior

The existing `workflow_dispatch` path already built and uploaded release assets while the release-publish job was tag-only. This change adds an explicit artifact round-trip to that manual path: after upload, it downloads the same versioned artifact into `dist-roundtrip`, checks every required file exists and is byte-identical to the original `dist` file, then verifies the downloaded `SHA256SUMS`.

The publish job condition is now explicit: it can run only for a tag **push** whose ref begins with `v`. A manual dispatch, including one dispatched against a branch with a version-like name, cannot publish a GitHub Release. The workflow's default `contents: read` permission remains in force for the verification job; only the tag-gated publish job has `contents: write`.

## Dry-run plan

Once the PR is open, dispatch `.github/workflows/release.yml` on `completion/m1-6-release-dry-run` with a unique dummy version such as `v0.0.0-dryrun-20261011`. Verify that the upload/download round-trip and checksums pass, the `publish` job is skipped, and the uploaded artifact contains only the expected build outputs. No release, tag, or publication is intended.

**Dispatch run ID, artifact metadata, and raw output will be added after execution.**
