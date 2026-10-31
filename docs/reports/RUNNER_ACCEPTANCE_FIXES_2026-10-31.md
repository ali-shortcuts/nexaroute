# Runner acceptance harness fixes — 2026-10-31

Scope: fixes for the two confirmed live runner failures on main
`67ecfaf` (the base of the runs below). This is harness/test/doc work
only. It is **not** live acceptance evidence and closes no issue.

## Confirmed failures (first-hand run records)

- **#650 Kubernetes live run 41194830342, artifact 13040417885.**
  The kind runner built and loaded `nexaroute:accept-prev` and
  `nexaroute:accept-current`, while the chart rendered
  `ghcr.io/ali-shortcuts/nexaroute:<tag>` with `imagePullPolicy: Never`.
  The pod therefore reported `ErrImageNeverPull` for both the preceding
  and current image, which caused the base `/healthz`, upgrade
  readiness, restart, rollback and re-upgrade phases to fail. The
  safe-failure phases correctly reported `NOT_RUN` after that.
- **#648/#649 Vault runner run 41194842427.** The script reached Vault
  boot, init, unseal, KV mount, Transit mount, mount readback and
  policies, then stopped at `vault.transit_key`: it expected HTTP 204
  while the pinned Vault 1.18.5 returned HTTP 200 for
  `POST /v1/transit/keys/nexaroute`. Because the failure exited before
  the summary writer, the artifact upload also failed with
  `No files were found`.

## Fixes

### `scripts/k8s-acceptance.sh`

- New `rendered_image_ref <chart-dir> <tag>` derives the image
  reference from the same `helm template` render the install uses
  (acceptance values + `--set image.tag`). The chart's fully qualified
  `image.repository` is therefore the single source of truth; no second
  local image name is maintained.
- New `build_and_load_images <prev-tree> <cluster>` tags the Docker
  builds and `kind load docker-image` calls with exactly that rendered
  reference for both `accept-prev` and `accept-current`.
- `--self-test` gained check 8: stub `docker`/`kind` binaries log their
  real argv, and the check independently renders the Deployment for both
  tags and requires the logged `docker build -t` and
  `kind load docker-image` names to match the rendered `image:` string.
  This is the regression test for the `ErrImageNeverPull` mismatch.
- `pullPolicy` remains `Never` for kind-loaded images. Nothing pulls
  from GHCR, and the test is hermetic (no cluster, no network).

### `scripts/vault-runner-acceptance.sh`

- `vault.transit_key` now expects HTTP **200** for Vault 1.18.5 and a
  new `vault.transit_key_readback` check reads
  `GET /v1/transit/keys/nexaroute`, requiring `data.name=nexaroute` and
  `data.type=aes256-gcm96`. No other endpoint expectation changed.
- The summary writer is now a reusable function called from the EXIT
  trap. Any exit after `OUT_DIR`/`CHECKS_FILE` exist writes
  `$OUT_DIR/vault-runner-summary.json` before cleanup, preserving the
  real exit code. The summary contains only check IDs/statuses, the
  exit code, and safe environment metadata (Vault version/storage/HA
  flag/image/topology/limitations). Failure details, HTTP bodies,
  tokens, key bytes, ciphertext and DEKs are never copied.
- The leak scan now runs before the success summary, so a leak failure
  also produces a FAIL summary through the same trap.
- `--self-test` runs two child processes through the real trap:
  a success path (exit 0, outcome PASS) and an early-failure path
  (exit 1, outcome FAIL, sentinel token absent from the summary), plus
  a deterministic contract check for the 200 + readback behavior.

### Tests

- New `scripts/test-vault-runner-acceptance.sh`, also invoked by
  `scripts/verify.sh`, wraps `--self-test` and pins the Transit-key
  contract.
- `scripts/verify.sh` now runs the Vault runner regression wrapper.

## Local verification (this checkout)

- `bash -n scripts/k8s-acceptance.sh scripts/vault-runner-acceptance.sh scripts/test-vault-runner-acceptance.sh` — clean.
- `bash scripts/k8s-acceptance.sh --offline-only --out-dir .artifacts/k8s-offline` — `failures=0 not_run=0`.
- `bash scripts/k8s-acceptance.sh --self-test --out-dir .artifacts/k8s-selftest` — `failures=0 not_run=0`; check 8 passes with
  `ghcr.io/ali-shortcuts/nexaroute:accept-prev` and
  `ghcr.io/ali-shortcuts/nexaroute:accept-current` as both the Docker
  tags and the kind-load names.
- `bash scripts/test-vault-runner-acceptance.sh` — PASS (3/3 self-test
  checks: success summary, failure summary, Transit contract).
- Local Helm is v3.22.1; CI pins v3.19.0. The render assertion depends
  only on chart values, not on the Helm minor version.

## Remaining blockers (unchanged)

- **#650:** a live PASS still requires a new dispatch of
  `k8s-live-acceptance.yml` on the new SHA. A successful kind run is
  ephemeral-cluster acceptance only, never production-cluster
  verification. Issue #650 stays OPEN.
- **#648/#649:** a live PASS still requires a new dispatch of
  `vault-runner-acceptance.yml` on the new SHA. This runner uses a
  disposable single-node loopback Vault; it is NOT operator staging,
  NOT Vault HA, and NOT production/HSM. A successful rerun is
  runner-loopback evidence only. Both issues stay OPEN pending
  staging/non-loopback evidence or an owner-approved exclusion (the
  owner has prohibited exclusions for this release).

## Follow-up 2026-10-31: second runner dispatch exposed three more harness defects

Runner dispatch 41206938283 on main `2f638da` (after #802) passed the
Transit key creation/readback but failed at `kv.vault_accept`: the
acceptance token was invalid because `auth/token/revoke-self` on the
root cascades to child tokens. Fixed by creating orphan tokens
(`"no_parent":true`).

A full local loopback run then exposed two further deterministic
defects, both fixed before claiming anything:

- `migrate-to-transit --dry-run` needs the legacy key source (it
  decrypts to prove migratability); the script now passes
  `--legacy-key-file`.
- `VAULT_ADDR` must be exported for the Go secrets CLI; an unexported
  shell variable produced `transit_config_invalid`.
- Vault 1.18.5 returns HTTP 200 (not 204) for `POST
  transit/keys/nexaroute/rotate`; the script now expects 200 and
  verifies `latest_version=2` by readback, with read added to the
  rotate policy for that check.

`scripts/test-vault-runner-acceptance.sh` now runs 5 deterministic
self-test checks (`success_summary`, `failure_summary`,
`transit_key_contract`, `acceptance_tokens_orphan`,
`vault_addr_exported`). Full local loopback run on the branch:
exit 0, **43/43 checks PASS**, 0 FAIL (`vault-accept` 16/16 PASS).
This is local loopback evidence only; a new GitHub runner dispatch on
the new main SHA is still required, and #648/#649 stay OPEN.
