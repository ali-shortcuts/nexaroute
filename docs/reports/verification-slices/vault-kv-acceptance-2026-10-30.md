# Vault KV acceptance — 2026-10-30 harness validation (loopback only)

**Status: harness validation PASS on a local disposable Vault. This is NOT
staging acceptance. #648 stays OPEN: external staging, HA, and Transit remain
BLOCKED / NOT_RUN.**

## Scope and authorization gate

This run exercised the new harness checks from the 2026-10-30 #648 change
(`vault.mount_version`, `vault.policy_assertion`, `vault.expired_token`,
BLOCKED-metadata early return, helper ref validation) against a disposable
Vault created for this run only. No operator authorization, instance pointer,
mount/path, policy, or token injection for a non-loopback Vault was supplied
in the task, in #648 comments, or in the environment (Vault env names all
absent-or-empty at check time), so no live staging call was made and no
staging claim is made. The 2026-10-30 #648 comment's rule — no live Vault
call/mutation without written owner authorization and approved
instance/mount/path/policy/secret injection — was honored: the only Vault
touched was created and destroyed by this run on loopback.

## Instance (created and destroyed by this run)

- Vault binary: v2.2.1 (build 2026-10-22), single node, `storage_type=file`
- Network: TLS on loopback `https://127.0.0.1:18973`, fresh 1-day CA
  (SAN IP 127.0.0.1, DNS localhost), `disable_mlock=true`, UI off
- Init: 3 shares / threshold 2, unsealed with 2 shares
- Mounts: `secret` KV v2 (verified out-of-band via `sys/mounts/secret`
  before the run: type `kv`, version `2`); `kv1` KV v1 (negative control)
- Auth method: static tokens only (operator-owned lifecycle; the harness
  never renews tokens, gateway re-resolves on restart only)
- Policies (names only): `nexaroute-accept` (acceptance path data +
  metadata, `lookup-self`, `token/create`, `revoke-self`,
  `sys/mounts/secret` read), `nexaroute-gateway` (gateway path read),
  `nexaroute-provision` (gateway path write), `nexaroute-kv1probe`
  (kv1 path + `sys/mounts/kv1` read)
- Gateway binary: built from the 2026-10-30 worktree (this change)
- Date: 2026-10-30 (run completed the same day; per-check statuses above
  are the committed record — the raw harness JSON reports were destroyed
  with the instance after the leak scan, by design)

## Commands (sanitized: token values never appear)

```bash
# Harness main run
nexaroute vault-accept --confirm-live \
  --ref secret/data/nexaroute/acceptance/master-key \
  --helper scripts/key-provider-vault-kv.sh \
  --expect-policy nexaroute-accept --timeout 10s \
  --out <report>.json
# Negative control 1: KV v1 mount
nexaroute vault-accept --confirm-live \
  --ref kv1/data/nexaroute/acceptance/master-key \
  --helper scripts/key-provider-vault-kv.sh \
  --expect-policy nexaroute-kv1probe --timeout 10s \
  --out <report>.json
# Negative control 2: policy mismatch
nexaroute vault-accept --confirm-live \
  --ref secret/data/nexaroute/acceptance/policy-key \
  --helper scripts/key-provider-vault-kv.sh \
  --expect-policy nexaroute-accept,nexaroute-missing --timeout 10s \
  --out <report>.json
# Gateway path (provider env: NEXAROUTE_KEY_PROVIDER=command,
# NEXAROUTE_KEY_PROVIDER_REF=secret/data/nexaroute/gateway/master-key,
# NEXAROUTE_KEY_PROVIDER_COMMAND=<helper>)
nexaroute -no-browser -config <config>.json   # boot, /healthz poll
nexaroute secrets status -config <config>.json
nexaroute secrets verify -config <config>.json
```

## Per-check results (33/33 PASS)

| Check | Status | Detail |
|---|---|---|
| vault.boot / vault.unseal | PASS | seal-status sealed=true then unsealed |
| vault.mount_v2_out_of_band | PASS | type=kv version=2 before the run |
| vault.mount_v1_negative_control | PASS | kv1 mount created (HTTP 204) |
| vault.tokens | PASS | 4 tokens (+root) created |
| gateway.build | PASS | gateway built from this change |
| harness.main_run | PASS | exit 0, outcome=PASS, evidence_class=loopback |
| harness.vault.mount_version | PASS | new check |
| harness.vault.policy_assertion | PASS | new check |
| harness.vault.expired_token | PASS | new check |
| harness.vault.kv_version | PASS | metadata probe after round-trip |
| harness.vault.kv_put_get | PASS | byte-identical 32-byte key |
| harness.vault.kv_rotation | PASS | replacement key read back |
| harness.vault.wrong_token | PASS | nonzero exit, empty stdout |
| harness.vault.revoked_token | PASS | child revoked, then fails closed |
| harness.vault.outage_fail_closed | PASS | unreachable endpoint fails closed |
| harness.vault.cleanup | PASS | metadata deleted |
| harness.main_cleanup_empty | PASS | acceptance path read 404 after cleanup |
| negative.kv1_mount | PASS | exit 1, mount_version FAIL, path still 404 (zero writes) |
| negative.policy_mismatch | PASS | exit 1, policy_assertion FAIL, path still 404 (zero writes) |
| gateway.key_roundtrip | PASS | provision put / gateway get byte-identical |
| gateway.boot_healthz | PASS | /healthz 200 with Vault-held key |
| gateway.config_encrypted_by_boot | PASS | encrypted_fields=1, nxs1_fields=1 |
| gateway.secrets_verify_key1 | PASS | verified_fields=1 |
| gateway.boot_wrong_token | PASS | exit 1 in 0.1s |
| gateway.boot_vault_down | PASS | exit 1 in 0.1s |
| gateway.old_config_fails_after_rotation | PASS | exit 1 under the new key |
| gateway.boot_after_rotation_reencrypt | PASS | /healthz 200 |
| gateway.secrets_verify_key2 | PASS | verified_fields=1 |
| gateway.restart_key_reread | PASS | byte-identical after restart + unseal |
| gateway.boot_after_restart | PASS | /healthz 200 |
| hygiene.report_leak_scan | PASS | no token values, no key_b64 fields in reports |
| hygiene.instance_destroyed_port_dead | PASS | instance dir removed, port dead, no Vault process |

`vault.ha_failover` and `vault.transit` are `NOT_RUN` by design in every
harness run. Deterministic fixture results for the same change: vault-accept
tests PASS plain and with `-race`; Vault helper tests PASS plain and with
`-race`; gofmt/vet clean. Full `./scripts/verify.sh` was not run locally
(CI is the gate for the PR).

## What this does NOT prove

- External staging (non-loopback, operator-owned, TLS, dedicated empty
  KV v2 mount/path, least-privilege token incl. `sys/mounts` read):
  **BLOCKED** — no authorization or instance supplied.
- HA failover, long token renewal, AppRole/Kubernetes auth: **NOT_RUN**.
- Vault Transit envelope mode: **NOT_RUN** by this harness and not claimed
  by this KV result (see #649, separate).
- A loopback PASS is harness evidence only and must never be cited as
  production, staging, or HA acceptance.
