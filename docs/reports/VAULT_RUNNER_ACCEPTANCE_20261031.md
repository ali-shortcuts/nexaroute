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
