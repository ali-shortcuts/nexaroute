# Vault KV acceptance automation (#648)

Status 2026-10-27: opt-in automation added and exercised against loopback
fixtures. No real staging Vault was available to this run, so the real-service
checks and Vault HA/failover remain **BLOCKED/NOT_RUN**, not PASS. Vault dev
mode is never production evidence.

## What it does

`nexaroute vault-accept` drives the shipped KV v2 helper
(`scripts/key-provider-vault-kv.sh`) against an operator-supplied Vault and
writes a redacted JSON report. It records Vault version, storage type, HA flag,
seal/standby state, static-token metadata, policy names, and per-check
PASS/FAIL/BLOCKED/NOT_RUN statuses.

| Check | What PASS means |
|---|---|
| `vault.health` | `/sys/seal-status` and `/sys/leader` were readable; Vault is unsealed; topology metadata was recorded |
| `vault.token_metadata` | `auth/token/lookup-self` succeeded; policy names, TTL, and renewable flag were recorded (not the token). A token carrying the `root` policy FAILS this check and the run stops before any write (see below). Since 2026-10-30 a BLOCKED metadata result also stops the run before any write: previously it fell through to preflight and put, writing keys under an unasserted token |
| `vault.policy_assertion` | only with `--expect-policy`: every named policy is present on the token and `root` is absent. A missing expected policy FAILS the run before preflight and before any write. Without the flag no assertion runs and the report records that manual policy review is required for staging |
| `vault.ref_preflight` | the selected KV v2 path was empty, so the run cannot overwrite existing key material |
| `vault.mount_version` | GET on `sys/mounts/<mount>` (mount = everything before `/data/` in the ref, so nested mounts work) confirmed type `kv` version `2` **before any write**. A wrong type/version or an absent mount FAILS the run; a 403 BLOCKS it (the token lacks sys/mounts read) and the run stops without writing. This closes the hole the post-round-trip metadata probe left: on a KV v1 mount the helper round-trip alone would have passed and a key would already have been written before the metadata probe failed |
| `vault.kv_put_get` | helper `put` then `get` returned a byte-identical 32-byte key (bytes never logged) |
| `vault.kv_version` | GET on the key's KV metadata path confirmed KV v2 semantics (200). A 404 FAILS the run even though the helper round-trip alone would pass on a KV v1 mount; a 403 BLOCKS it (the token lacks metadata read) |
| `vault.gateway_decrypt` | an `nxs1` config field encrypted and decrypted successfully with the Vault-held key |
| `vault.kv_rotation` | a new 32-byte key replaced the entry and was read back byte-identically |
| `vault.wrong_token` | an invalid token produced a nonzero helper exit and empty stdout |
| `vault.expired_token` | a disposable 2s child token was polled until the helper get failed with empty stdout. A create failure is BLOCKED (permission gap); only a child token still accepted past a 10s deadline is a FAIL |
| `vault.revoked_token` | a disposable 60s child token read the path, was revoked via `revoke-self`, then failed closed with empty stdout |
| `vault.outage_fail_closed` | a deliberately unreachable endpoint produced a nonzero helper exit and empty stdout (no fallback source) |
| `vault.cleanup` | the acceptance key's KV metadata was deleted from the dedicated path |
| `vault.ha_failover` | always `NOT_RUN`: needs an operator-controlled HA cluster and a real failover event |
| `vault.transit` | always `NOT_RUN`: Vault Transit envelope mode is a separate provider path and is **not tested by this harness**; a KV pass proves nothing about Transit (see #649) |

The harness does not revoke the operator's primary token. The revocation check
creates a child token with a 60s TTL, verifies it can read the acceptance path,
revokes it with `auth/token/revoke-self`, and then proves it fails closed. If
the primary token lacks `auth/token/create`, that check is `BLOCKED` and names
the missing capability; other checks still run.

Harness hardening, 2026-10-29 (landed on main via #741 and #750): the check set
grew from 12 to 13 with `vault.kv_version`, so a full pass is now 11 selected
PASS + 2 `NOT_RUN` (the 2026-10-27 loopback result of 10/12 predates this
check and is not restated as 11/13). Three gaps found in the 2026-10-28 audit
are closed: (1) KV v2 is now proven behaviorally via the metadata probe,
not by path shape; (2) a `root` token fails `vault.token_metadata` and the
run stops before writing anything — non-root is necessary but not sufficient
for least-privilege, and staging still needs a manual policy review; (3) an
explicitly set `--timeout` is propagated to the helper as
`VAULT_HELPER_TIMEOUT` (otherwise the operator's exported value is honored,
otherwise the script default of 30s applies), and the report records the
effective bound as `bounds.helper_timeout_seconds`. The staging policy shape
documented below already grants `read` on the metadata path, so a
`vault.kv_version` 403 on staging means the policy was narrowed, not that
the mount is v2.

Harness hardening, 2026-10-30 (this change; the 2026-10-28 follow-up branch
`supervisor/issue648-vault-followup` was never pushed, so its two checks are
ported here onto current main rather than merged from that branch): the check
set grows to 16 selected checks (`vault.mount_version`,
`vault.policy_assertion`, `vault.expired_token` added; a full pass is 16
selected PASS + 2 `NOT_RUN`). (1) `vault.mount_version` proves KV v2 from
`sys/mounts/<mount>` before any write — the post-round-trip metadata probe
alone would only fail after a key had been written to a non-v2 mount, and
the helper round-trip passes on a KV v1 mount. The mount is everything
before `/data/` in the ref (the follow-up's first-segment derivation would
have been wrong for nested mounts). (2) `--expect-policy` asserts the
token's policy set before preflight and before any write; without the flag
the report records that manual policy review is required. (3) A BLOCKED
`vault.token_metadata` result now stops the run before any write (recorded
as a follow-up in the #741 merge comment: previously it fell through to
preflight and put). (4) `vault.expired_token` polls a disposable 2s child
token to expiry. (5) The helper script itself now rejects any ref that is
not a plain KV v2 data path (no `/data/`, traversal, whitespace, query, or
fragment) before any Vault request — a `?version=` query in a ref would
otherwise have pinned a version silently. The staging policy below now
includes `sys/mounts/<mount>` read: without it `vault.mount_version` is
BLOCKED and the run stops before writing, by design.

## Safety controls

- `--confirm-live` is mandatory. Without it the command exits `2` before any
  Vault request or write.
- There is no built-in Vault endpoint. `--ref` is mandatory and must be a
  relative KV v2 `.../data/...` path with no traversal, query, or fragment.
- The path must be empty before the run; occupied paths are refused without a
  write so an operator key is never clobbered.
- Plain HTTP is accepted only for loopback endpoints. Staging/production must
  use HTTPS.
- In-memory storage (`storage_type=inmem`, Vault dev mode) is rejected by
  default. `--allow-dev` permits it only for harness checks and labels the
  report `evidence_class=dev` with an explicit "MUST NOT" production warning.
- Reports are written atomically with mode `0600`; parent directories use
  `0700`. No token values, key bytes, or Vault response bodies are written.
- The helper receives the token through its environment, never through argv.
- A token with the `root` policy is refused before any write: the run exits
  with `vault.token_metadata` FAIL and no acceptance key is created.
- The helper's per-request bound is explicit: `--timeout` (when set) wins,
  then `VAULT_HELPER_TIMEOUT`, then the script default of 30s. An exported
  but invalid `VAULT_HELPER_TIMEOUT` is a usage error (exit 2) before any
  Vault request, not a mid-run helper failure.
- `--skip-cleanup` is available, but then the acceptance key remains at the
  selected path and the report says so.

## Exact invocations

Real disposable/staging Vault (operator shell; never commit the token):

```bash
export VAULT_ADDR='https://vault.staging.example:8200'
export VAULT_TOKEN='...'
nexaroute vault-accept \
  --ref secret/data/nexaroute/acceptance/master-key \
  --helper /opt/nexaroute/scripts/key-provider-vault-kv.sh \
  --confirm-live \
  --out docs/reports/verification-slices/vault-kv-acceptance-$(date -u +%Y%m%dT%H%M%SZ).json
```

No Vault infrastructure (records a BLOCKED report; makes no request):

```bash
env -u VAULT_ADDR -u VAULT_TOKEN \
  nexaroute vault-accept \
  --ref secret/data/nexaroute/acceptance/master-key \
  --helper scripts/key-provider-vault-kv.sh \
  --confirm-live \
  --out docs/reports/verification-slices/vault-kv-acceptance-blocked.json
```

Explicit non-production dev-mode harness run (never cite as acceptance):

```bash
nexaroute vault-accept ... --allow-dev
```

Suggested least-privilege policy for the acceptance token (replace the mount
and path; keep it scoped to the disposable acceptance path):

```hcl
path "secret/data/nexaroute/acceptance/*" {
  capabilities = ["create", "read", "update"]
}
path "secret/metadata/nexaroute/acceptance/*" {
  capabilities = ["read", "delete"]
}
path "auth/token/create" {
  capabilities = ["update"]
}
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
path "auth/token/lookup-self" {
  capabilities = ["read"]
}
path "sys/mounts/secret" {
  capabilities = ["read"]
}
```

`sys/seal-status` and `sys/leader` are normally readable without an explicit
policy. No `root` policy and no access to production key paths are required.
The policy above is illustrative; the report records the actual policy names
returned by `lookup-self`, never the token. The `sys/mounts/<mount>` read is
required for `vault.mount_version` to PASS: without it that check is BLOCKED
and the run stops before writing (the mount claim is then unverified, not
passed).

## Local execution evidence (2026-10-27)

Focused fixture tests (loopback fake Vault; includes put/get, rotation,
wrong/revoked token, outage fail-closed, dev-mode rejection, occupied-path
refusal, missing-config BLOCKED report, redaction, and report permissions):

```bash
go test ./cmd/gateway -run '^TestVaultAccept' -count=1 -v
```

Result: 9 tests PASS.

```bash
go test -race ./cmd/gateway -run '^TestVaultAccept' -count=1
go test ./internal/secrets -run '^TestVaultKVHelper' -count=1 -v
```

Result: both PASS.

Blocked real-service report generated in this environment with no Vault
endpoint/token:

`docs/reports/verification-slices/vault-kv-acceptance-blocked-2026-10-27.json`

Outcome `BLOCKED`, `evidence_class=none`, `vault.config` BLOCKED because
`VAULT_ADDR` was not set; `vault.ha_failover` and `vault.transit` are
`NOT_RUN`. No request was attempted and no key was written.

## Live execution evidence (2026-10-27, non-dev loopback — supersedes the
BLOCKED note above for single-node KV custody only)

A disposable, single-host Vault was stood up for this run and destroyed
afterwards; it was **not** an operator staging cluster and **not** an HA
cluster, so this section upgrades only the single-node KV evidence below
and changes nothing about HA, external staging, or Transit.

- Vault: real binary v2.2.1, `storage_type=file`, TLS on loopback
  (`https://127.0.0.1:18201`, self-signed cert with SAN IP 127.0.0.1),
  `operator init` 3 shares / threshold 2, unsealed, KV v2 mount.
  `vault-accept` report: `outcome=PASS`, `evidence_class=loopback`,
  10 passed / 0 failed / 0 blocked / 2 not-run,
  `ha_enabled=false`, `ha_known=true`, `dev_mode=false`,
  policies `default, nexaroute-accept`, token TTL 1h, renewable.
- `vault-accept` (with the bounded helper below) — PASS for
  `vault.health`, `vault.token_metadata`, `vault.ref_preflight`,
  `vault.kv_put_get`, `vault.gateway_decrypt`, `vault.kv_rotation`,
  `vault.wrong_token`, `vault.revoked_token`,
  `vault.outage_fail_closed`, `vault.cleanup`; `vault.ha_failover` and
  `vault.transit` remain `NOT_RUN`.
- Report: `docs/reports/verification-slices/vault-kv-acceptance-live-2026-10-27.json`
  (sanitized: no token values or key bytes; verified programmatically
  against the run's token files before committing).
- Extra live checks that `vault-accept` does not cover, all on the same
  disposable Vault:
  - Full gateway boot with Vault-held key: config `api_key` encrypted
    in place (`nxs1`), encrypted backup written, no local `.key` file,
    `/api/hello` 200, `secrets verify` `OK provider:stub.api_key`
    (`verified_fields=1`).
  - Wrong token: `secrets verify` fails closed, exit 1
    (`key provider command failed`).
  - Expired token: a 10s gateway-policy token read the existing key
    before expiry (exit 0) and failed closed after expiry (exit 1,
    empty stdout).
  - Least-privilege denial: the acceptance token got HTTP 403 on the
    gateway path and on an unrelated path; post-run acceptance data
    and metadata reads returned 404 (cleanup verified independently).
  - Vault outage: gateway boot with Vault stopped failed closed,
    exit 1, in under a second (connection refused, no fallback).
  - Vault restart recovery: after a process restart the Vault was
    sealed, unsealed with 2 shares, and the gateway booted again
    (`/api/hello` 200, `secrets verify` OK).
  - Key rotation recovery (external providers are not rotated by
    `secrets rotate` by design): decrypt config under the old Vault
    key, replace the KV entry, prove the old envelope fails closed
    under the new key (exit 1), then boot on the plaintext copy so
    it re-encrypts under the new key (`/api/hello` 200,
    `secrets verify` OK).
- Bounded-helper fix found by this run: the shipped helper's `curl`
  calls had no `--max-time`/`--connect-timeout`, and the gateway loads
  its master key with `context.Background`, so a never-responding Vault
  hung the helper until an external timeout killed it (pre-fix probe:
  exit 124 at 15s). The helper now bounds every request with
  `--connect-timeout 10 --max-time VAULT_HELPER_TIMEOUT` (default 30s;
  `VAULT_HELPER_TIMEOUT` must be a positive integer). Regression test:
  `TestVaultKVHelperSlowServerTimesOut` (slow server sleeps 5s,
  timeout 1s, load fails in under 4s). The live `vault-accept` run
  above was re-run with the fixed helper and still PASSes 10/12.

## What remains BLOCKED / NOT_RUN

- Real operator-provided disposable/staging Vault (non-loopback,
  `evidence_class=external`) put/get, rotation, wrong/revoked token,
  and outage behavior: **BLOCKED** (no staging service or credentials
  were supplied for this run; the loopback run above does not replace
  it).
- Real Vault HA cluster, failover mid-run, and long-running token
  renewal/lease behavior: **NOT_RUN** (no HA infrastructure; the harness does
  not simulate a real failover).
- Auth methods other than a static token (AppRole, Kubernetes auth): **NOT_RUN**;
  token acquisition remains operator-owned.
- Vault Transit envelope mode: **NOT IMPLEMENTED**; this report makes no
  Transit claim.
- A loopback fixture PASS is harness evidence only. Only a real staging run
  can produce `evidence_class=external`, and it still does not by itself prove
  HA behavior.

## Local execution evidence (2026-10-30, non-dev loopback — harness validation only)

A disposable, single-host Vault was stood up for this run and destroyed
afterwards; it was **not** an operator staging cluster and **not** an HA
cluster, so this section validates the new harness checks only and changes
nothing about external staging, HA, or Transit. Full per-check record:
`docs/reports/verification-slices/vault-kv-acceptance-2026-10-30.md`.

- Vault: real binary v2.2.1, `storage_type=file`, TLS on loopback
  (`https://127.0.0.1:18973`, fresh 1-day CA, SAN IP 127.0.0.1),
  `operator init` 3 shares / threshold 2, unsealed, KV v2 mount `secret`
  verified out-of-band before the run, KV v1 mount `kv1` as a negative
  control only. Auth method: static tokens (operator-owned); policies
  `nexaroute-accept`, `nexaroute-gateway`, `nexaroute-provision`,
  `nexaroute-kv1probe` (names only; no token values recorded).
- `vault-accept` with `--expect-policy nexaroute-accept --timeout 10s`:
  `outcome=PASS`, `evidence_class=loopback`, all 16 selected checks PASS
  including the three new ones (`vault.mount_version`,
  `vault.policy_assertion`, `vault.expired_token`), plus
  `vault.ha_failover` and `vault.transit` `NOT_RUN`. Post-run acceptance
  path read returned 404 (cleanup verified).
- Negative controls, both with zero writes (path read 404 afterwards):
  KV v1 mount run exited 1 with `vault.mount_version` FAIL; policy-mismatch
  run (`--expect-policy nexaroute-accept,nexaroute-missing`) exited 1 with
  `vault.policy_assertion` FAIL.
- Gateway path on the same disposable Vault (actual gateway binary, not
  only the helper): provision-put/gateway-get byte-identical round-trip;
  boot with a Vault-held key (`/healthz` 200, config migrated in place to
  `nxs1_fields=1`, `secrets verify` `verified_fields=1`); wrong-token boot
  failed closed in 0.1s; Vault-down boot failed closed in 0.1s; rotation
  (old config fails verify, re-encrypted config verifies and boots);
  Vault restart + unseal + byte-identical key re-read + boot.
- Hygiene: all tokens including root revoked, instance directory destroyed,
  port dead, no Vault process left, harness reports leak-scanned (no token
  values, no `key_b64` fields). Driver: local evidence copy only, not
  committed (it holds no secrets but references the destroyed instance).
- Staging verdict unchanged: **BLOCKED** — no operator authorization, instance
  pointer, mount/path, policy, or token injection was supplied for a
  non-loopback Vault. #648 stays OPEN.
