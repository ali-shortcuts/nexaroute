# Vault runner acceptance (#648 KV, #649 Transit) — workflow ready, run pending

Status 2026-10-31: **workflow and script are written and reviewed in this
PR; the run itself is NOT_RUN.** Nothing in this document is acceptance
evidence. It becomes evidence only when the owner dispatches the workflow
on GitHub Actions and sends back the run result (see "What to send back"
below), and only for the runner-loopback scope it actually covers.

## Why a runner workflow

This environment reaches only github.com and package registries. It has no
private server and no external Vault, so it cannot run a Vault acceptance
itself — even with an address and a key. The real path agreed with the
owner is: the workflow and script live in a PR, the owner runs them on
GitHub Actions (runners have internet and Docker), the owner sends the
result, and the report and docs are completed from that result.

## What the workflow does

Workflow: `.github/workflows/vault-runner-acceptance.yml`
Script: `scripts/vault-runner-acceptance.sh` (invoked with `bash`, so it
does not depend on the executable bit surviving a push)

On a GitHub-hosted runner, in order:

1. Builds the gateway from the PR checkout (Go 1.27.x).
2. Starts a disposable, single-node, **non-dev** Vault container
   (`hashicorp/vault:1.18.5`, file storage, init 3 shares / threshold 2,
   unsealed with 2 shares) and records its real version from
   `/sys/seal-status` — the version is never assumed from the image tag.
3. Enables a KV v2 mount (`secret`) and a Transit mount (`transit`,
   key `nexaroute`, type `aes256-gcm96`), creates three least-privilege
   tokens (KV acceptance, Transit encrypt/decrypt/rewrap on that key
   only, Transit rotate only), then revokes the root token. The restart
   test unseals with shares, not with root.
4. **#648 KV:** runs the shipped `nexaroute vault-accept` harness
   (`--expect-policy nexaroute-accept`, outcome must be PASS with
   0 failed / 0 blocked and `evidence_class=loopback`), then a real
   gateway boot with a Vault-held key (`/healthz` 200,
   `nxs1_fields=1`, `verified_fields=1`, no local `.key` file),
   wrong-token boot fail-closed, KV rotation recovery, and a Vault
   stop/start + unseal + boot recovery.
5. **#649 Transit:** migrates that nxs1 config to nxs2 with the real
   Transit API (`migrate-to-transit` with the Vault-held key as the
   legacy key, `nxs2_fields=1`, `transit_key_versions=1`, verify PASS),
   tamper fail-closed, wrong-token fail-closed, Vault-side key
   rotation + `rewrap-transit` (`transit_key_versions=2`, verify PASS),
   rollback to the pre-rewrap backup (verify PASS — the old version
   must remain recoverable), and unavailable-Vault fail-closed.
6. Writes two redacted reports and leak-scans them (no token, key,
   `key_b64`, ciphertext, or wrapped DEK may appear):
   - `vault-kv-acceptance.json` (the vault-accept report)
   - `vault-runner-summary.json` (per-check PASS/FAIL, Vault version,
     topology, and the limitations below)

## How the owner runs it

1. Merge this PR (separate owner approval; this PR merges nothing
   itself and closes no issue).
2. In the repository on GitHub: Actions → "Vault runner acceptance" →
   "Run workflow" → Run workflow.
3. Wait for the job (timeout 45 minutes; the script itself is bounded
   well under that). Download the `vault-runner-reports-<sha>`
   artifact.

## What to send back

The run URL, the artifact (or its two JSON reports), and the job log
excerpt showing the `PASS`/`FAIL` lines. The summary JSON alone is not
enough to close anything: the vault-accept report is the KV evidence,
and the summary's check list is the Transit evidence. If any check
fails, send the FAIL line and the step log — do not re-run silently
until the failure is understood, because a re-run overwrites the
evidence of the first failure.

## What this run does NOT prove (read before citing it)

- **NOT staging:** the Vault is a container on the same runner, over
  loopback HTTP. A PASS is `evidence_class=runner-loopback`, never
  `external`.
- **NOT HA:** single node only. The restart test is a container
  stop/start + unseal, not an HA failover event. `vault.ha_failover`
  in the vault-accept report remains `NOT_RUN`.
- **NOT production, HSM, or KMS:** no HSM/KMS backing, no long-running
  token renewal, no AppRole/Kubernetes auth, no multi-host topology.
- **NOT a real provider credential:** the gateway config uses a dummy
  provider key (`acceptance-plaintext`), never a real provider secret.
- **NOT a closure of #648 or #649:** both issues stay OPEN after this
  run. It upgrades only the runner-loopback scope: KV custody and
  Transit interoperability against a real (container) Vault API,
  instead of the fake Vault API the unit tests use. The staging,
  HA, and tenant steps in both issues remain BLOCKED until an
  operator runs them on a real staging instance.

## If the run fails

A FAIL line names the check (for example `transit.rewrap` or
`kv.gateway_boot_restart`). The script stops at the first failure, so
later checks are not run and must not be quoted. Fix the script or the
workflow in a follow-up commit on the same branch, re-dispatch, and
record both runs — the failed run is evidence too, not something to
hide by re-running.

## Update 2026-10-31: first live dispatch and harness fixes

The first live dispatch (run 41194842427 on main `67ecfaf`) reached Vault
boot, init, unseal, KV mount, Transit mount, mount readback and policies,
then failed at `vault.transit_key` because the script expected HTTP 204
while the pinned Vault 1.18.5 returned HTTP 200 for
`POST /v1/transit/keys/nexaroute`. It exited before the summary writer,
so the artifact upload also failed with `No files were found`.

The script now:

- expects HTTP 200 for Transit key creation and verifies the key with a
  readback (`data.name=nexaroute`, `data.type=aes256-gcm96`);
- writes a sanitized `vault-runner-summary.json` from the EXIT trap on
  any exit after `OUT_DIR`/`CHECKS_FILE` exist, preserving the real exit
  code, so a failed run still uploads check IDs/statuses and safe
  environment metadata (never tokens, key bytes, ciphertext, DEKs, or
  HTTP bodies);
- runs its leak scan before the success summary;
- has a deterministic `--self-test` (also run by `scripts/verify.sh`)
  covering the success and early-failure summary paths and the Transit
  contract.

This is harness work, not acceptance evidence. A successful rerun on the
new SHA is runner-loopback evidence only; #648 and #649 stay OPEN pending
operator staging/non-loopback evidence.

## Update 2026-10-31: second live dispatch — root-revoke cascade fix

The second live dispatch (run 41206938283 on main `2f638da`, after the
first harness fixes merged as #802) reached Transit key creation and
readback, then failed at `kv.vault_accept`. `vault-accept` reported
`vault.token_metadata` as BLOCKED: the acceptance token could no longer
read `auth/token/lookup-self`.

Root cause: the script revoked the root token (`POST
auth/token/revoke-self`) before running `vault-accept`, and the service
tokens it had created were children of that root token. Revoking the
parent cascades to children, so the KV token was already invalid.
Reproduced locally on `2f638da` with a disposable Vault 1.18.5
container: identical `PASS vault.root_revoke` followed by
`BLOCKED vault.token_metadata`.

Fix: `create_token` now sets `"no_parent":true`, making the three
acceptance tokens orphans that survive the root revoke. A new
deterministic self-test check (`selftest.acceptance_tokens_orphan`)
pins it; `scripts/test-vault-runner-acceptance.sh` now runs 4 checks.

This is still harness work, not acceptance evidence. The next live
runner dispatch must be treated as a fresh run; a PASS remains
runner-loopback only, and #648/#649 stay OPEN pending operator
staging/non-loopback evidence.

## Update 2026-10-31: full local loopback PASS after three more harness fixes

After the orphan-token fix, a local disposable Vault 1.18.5 run on the
same checkout still exposed three deterministic harness defects, all
fixed before any claim:

1. `secrets migrate-to-transit --dry-run` was invoked without the
   legacy key source, although a dry run still decrypts each nxs1
   envelope. It now passes `--legacy-key-file "$WORK/legacy.key"`.
2. `VAULT_ADDR` was a shell variable, not exported, so the Go secrets
   CLI saw an empty address and failed with
   `transit_config_invalid: Vault Transit requires VAULT_ADDR`. It is
   now exported (the unavailable-Vault check still overrides it inline).
3. Vault 1.18.5 returns HTTP 200 for `POST
   transit/keys/nexaroute/rotate` (not 204). The script now expects 200
   and verifies `data.latest_version=2` via readback; the rotate policy
   gained read on `transit/keys/nexaroute` for that readback only.

Local run (not a GitHub runner, not staging): disposable
`hashicorp/vault:1.18.5` on loopback, custom ports, checkout at the
branch head. Result: **exit 0, 43/43 checks PASS, 0 FAIL**, plus
`vault-accept` 16/16 PASS (`vault.ha_failover` and `vault.transit` are
NOT_RUN by design in that KV harness). Report:
`.artifacts/vault-fixed5/vault-runner-summary.json` (local, not
committed).

This proves the harness is now capable of a complete runner-loopback
run. It is **not** acceptance evidence for #648/#649: no operator
staging Vault, no HA/failover, no production/HSM was involved, and the
GitHub Actions dispatch still must be re-run on the new main SHA.
