# M1-6 — Release Workflow Dispatch Dry-Run

**Date:** 2026-10-11
**State:** DONE — upload/download round-trip passed; publish skipped

## Implemented behavior

The manual `workflow_dispatch` path now downloads the release artifact immediately after upload into `dist-roundtrip`. It requires all expected files to exist, compares each byte-for-byte with the original `dist` output, and validates the downloaded `SHA256SUMS`.

The publish job is explicitly restricted to a `push` event on a `v*` tag. A manual dispatch cannot publish, even when the selected branch has a version-like name. The verification job retains `contents: read`; only the tag-gated publish job has `contents: write`.

The release smoke fixture now supplies a test-only token to its development fake video provider and uses that token only for localhost video requests. No production or external provider credentials are used.

## Successful dry-run

- Workflow: [`Release`](https://github.com/ali-shortcuts/nexaroute/actions/runs/38125574996), event `workflow_dispatch`.
- Ref: `completion/m1-6-release-dry-run`; SHA `0402990120a34b5f91c443761be34b8ea253738d`.
- Dummy version: `v0.0.0-dryrun-20261011-retry1`.
- Result: workflow **success**; `verify` job **success**; `publish` job **skipped**.
- Artifact: `nexaroute-release-v0.0.0-dryrun-20261011-retry1`.
- Upload, download, byte-comparison, and downloaded checksum verification all passed. The artifact was also downloaded locally; its exact nine-file inventory matched the expected release outputs, and `sha256sum -c SHA256SUMS` passed for every listed asset.
- Confirmed that neither a GitHub Release nor a Git tag exists for the dummy version.
- PR #260 CI: all four checks successful, including `CI/verify`.

The artifact contained `nexaroute-linux-amd64`, `nexaroute-linux-arm64`, `nexaroute-videogen-linux-amd64`, `nexaroute-videogen-linux-arm64`, `install.sh`, `video.example.json`, `VIDEO_GATEWAY.md`, `RELEASE_NOTES.md`, and `SHA256SUMS`.

## Initial failure and correction

The first dispatch, [run #38125153341](https://github.com/ali-shortcuts/nexaroute/actions/runs/38125153341), passed full verification, stress checks, local runtime smoke, and asset builds, then exposed a pre-existing release-smoke fixture defect: `video.auth_token_env` was absent. After adding the fake token environment, a local smoke retry exposed missing bearer headers on the localhost video requests. Both were corrected with the same test-only placeholder. `./scripts/release-smoke.sh dist` then passed locally, and the subsequent workflow dispatch completed successfully. The first run failed before upload; its publish job was skipped and no release or tag was created.

## Evidence

- Full successful workflow log: `docs/reports/m1-6-retry-workflow.raw.txt`.
- Run status, artifact inventory, checksums, and release/tag absence: `docs/reports/m1-6-retry-check.raw.txt`.
- First-run failure log: `docs/reports/m1-6-first-dispatch.failure.raw.txt`.
- Local `verify.sh` preflight output: `docs/reports/m1-6-verify.raw.txt`.
