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
| `vault.token_metadata` | `auth/token/lookup-self` succeeded; policy names, TTL, and renewable flag were recorded (not the token) |
| `vault.ref_preflight` | the selected KV v2 path was empty, so the run cannot overwrite existing key material |
| `vault.kv_put_get` | helper `put` then `get` returned a byte-identical 32-byte key (bytes never logged) |
| `vault.gateway_decrypt` | an `nxs1` config field encrypted and decrypted successfully with the Vault-held key |
| `vault.kv_rotation` | a new 32-byte key replaced the entry and was read back byte-identically |
| `vault.wrong_token` | an invalid token produced a nonzero helper exit and empty stdout |
| `vault.revoked_token` | a disposable 60s child token read the path, was revoked via `revoke-self`, then failed closed with empty stdout |
| `vault.outage_fail_closed` | a deliberately unreachable endpoint produced a nonzero helper exit and empty stdout (no fallback source) |
| `vault.cleanup` | the acceptance key's KV metadata was deleted from the dedicated path |
| `vault.ha_failover` | always `NOT_RUN`: needs an operator-controlled HA cluster and a real failover event |
| `vault.transit` | always `NOT_RUN`: Vault Transit envelope mode is **not implemented** |

The harness does not revoke the operator's primary token. The revocation check
creates a child token with a 60s TTL, verifies it can read the acceptance path,
revokes it with `auth/token/revoke-self`, and then proves it fails closed. If
the primary token lacks `auth/token/create`, that check is `BLOCKED` and names
the missing capability; other checks still run.

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
```

`sys/seal-status` and `sys/leader` are normally readable without an explicit
policy. No `root` policy and no access to production key paths are required.
The policy above is illustrative; the report records the actual policy names
returned by `lookup-self`, never the token.

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

## What remains BLOCKED / NOT_RUN

- Real disposable/staging Vault put/get, rotation, wrong/revoked token, and
  outage behavior: **BLOCKED** (no Vault service or credentials in this
  environment).
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
