# ComfyUI real-inference acceptance automation (#651)

Parent issue: [#651](https://github.com/ali-shortcuts/nexaroute/issues/651).

## Current status

**Actual model inference: NOT_RUN (BLOCKED) in this repository environment.**
No provisioned ComfyUI server, compatible text-to-video model, or GPU/CPU
resource allocation was available to this agent. The deterministic tests use
loopback HTTP fixtures; fixture output is **not** real video inference and is
not recorded as acceptance evidence.

This slice adds the opt-in automation and its negative-path regression tests.
It does not promote ComfyUI from `UNVERIFIED` and does not claim live model
interoperability.

## Command

```bash
nexaroute comfyui-accept \
  --config /etc/nexaroute/config.json \
  --confirm-live \
  --out docs/reports/verification-slices/comfyui-acceptance-$(date -u +%Y%m%dT%H%M%SZ).json
```

The command:

- Refuses to run without `--confirm-live`, before loading a target or making a
  network request, and writes no report in that case.
- Reads only the existing `video.comfyui_*` configuration. It has no built-in
  ComfyUI endpoint, does not download models, and does not call any paid
  service.
- Requires a readable workflow and valid prompt/width/height/frames node
  mapping before contacting the server.
- Requires an installed `ffprobe` (override with `--ffprobe`). Without it,
  playability cannot be proven and the run is `BLOCKED` rather than `PASS`.
- Reads ComfyUI `/system_stats` to record the ComfyUI, Python, and PyTorch
  versions plus RAM/VRAM constraints.
- Submits one minimal text-to-video workflow, polls until completion (bounded
  by `--timeout`, default `10m`), downloads through the provider, persists
  through the same local asset store used by the gateway, then reads the asset
  back from disk to verify non-zero size, video metadata, matching SHA-256, and
  a decodable video stream via `ffprobe`.
- Removes a partial per-run asset workspace and attempts to delete a pending
  ComfyUI queue entry when submission, polling, download, or verification
  fails. It never sends ComfyUI's global interrupt for a running prompt.
- Writes an atomic `0600` JSON report with `PASS`/`FAIL`/`BLOCKED`/`NOT_RUN`
  checks, sanitized endpoint metadata, workflow SHA-256, prompt SHA-256 (not
  prompt text), server versions/resources, job state, asset facts, and cleanup
  status.

Exit codes: `0` PASS, `1` FAIL, `2` usage/config error, `3` BLOCKED, `4`
NOT_RUN. A successful real run keeps the verified output under the configured
`--asset-dir` (default `.artifacts/comfyui-acceptance/`) for operator review.
Do not commit large media outputs; commit only the sanitized report/runbook
evidence.

## What closes #651

An operator with a provisioned installation must run the command above with a
real compatible text-to-video workflow and attach the sanitized report to the
issue. The report must show:

- `outcome: PASS` and `inference_executed: true`;
- non-empty server version/resource metadata;
- a completed job with a prompt ID;
- one or more persisted assets with `content_type: video/mp4`, non-zero
  `size_bytes`, a 64-hex-digit SHA-256, ComfyUI `node_id`/`filename` metadata,
  and an `ffprobe` codec/width/height/duration;
- cleanup `local_status: PASS` (retained verified output) or an explicit
  operator note when a fixture/loopback target was used.

If any item is absent, the issue remains open with the missing evidence named.

## Deterministic negative coverage (hermetic, loopback only)

`cmd/gateway/comfyui_accept_test.go` covers:

| Scenario | Expected behavior |
|---|---|
| No `--confirm-live` | exit `2`, zero upstream calls, no report |
| ComfyUI not provisioned | exit `3`, `BLOCKED`, inference `NOT_RUN` |
| Unavailable server | exit `1`, `FAIL`, no fabricated version or asset |
| Invalid workflow/node mapping | exit `1`, preflight `FAIL`, server never contacted |
| Timeout | exit `1`, poll `FAIL`, pending prompt deleted, partial workspace removed |
| Oversized output | exit `1`, download `FAIL`, no partial local asset retained |
| Completed without MP4 | exit `1`, fail-closed, no asset reported |
| Successful fixture lifecycle | exit `0`, report/PASS/checks, retained fixture asset verified for size/metadata/SHA-256 |

`internal/video/providers/comfyui_test.go` additionally covers invalid node
mapping variants, unavailable server, context deadline, `/system_stats`
parsing, output-size limits, malformed history, and safe cancellation. These
tests prove the adapter and harness contract; they do **not** prove real model
inference.

## Limitations

- The command cannot distinguish a real ComfyUI instance from a fixture served
  on loopback. Its report records `evidence_class: loopback` and says a PASS is
  real inference only when the operator-provisioned target is a real model.
- `ffprobe` proves a decodable video stream, not subjective content quality or
  that the requested prompt was rendered correctly.
- A running ComfyUI prompt cannot be cancelled without the global interrupt;
  the harness reports that cleanup as `BLOCKED` instead of claiming success.
- Workflow/model licenses, model files, and resource allocation remain the
  operator's responsibility.
