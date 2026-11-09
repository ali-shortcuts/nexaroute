# Vault Transit acceptance — operator lab run (2026-11-09)

This report records an operator-authorized disposable acceptance run for issue #649. It is intentionally separate from production, HSM/KMS, multi-host, and managed-staging claims.

## Evidence identity

- Repository: `ali-shortcuts/nexaroute`
- Code SHA exercised by the operator: `debbe202cd5895e275528fe7c162f7b2969c29f7`
- Current `main` contains this SHA in its ancestry; this report does not claim the run was executed at the later `main` tip.
- Evidence class: `single-host-multi-node-raft`
- Vault image: `hashicorp/vault:latest`, observed server version `2.2.1`
- Topology: three Vault containers on one operator workstation, separate Raft data directories, loopback-published HTTP API ports `18200`, `18201`, and `18202`
- Transit mount: `transit/`
- Transit key: `nexaroute`, type `aes256-gcm96`
- Secrets, tokens, unseal shares, ciphertexts, config contents, and private file contents are excluded.

## Topology and recovery

- All three nodes initialized and unsealed: PASS.
- Raft peers `vault1`, `vault2`, and `vault3` present as voters: PASS.
- Initial leader `vault2`, with `vault1` and `vault3` followers: PASS.
- `vault2` was stopped; `vault1` became active while `vault3` remained standby: PASS.
- `vault2` was restarted, unsealed with two Shamir shares, and rejoined as a follower: PASS.

## Transit and NexaRoute checks

| Check | Result |
|---|---|
| Transit engine enabled | PASS |
| Least-privilege encrypt/decrypt/rewrap policy | PASS |
| Separate rotation/config policies | PASS |
| Encrypt/decrypt round trip through real Transit API | PASS |
| Transit token denied rotation | PASS |
| Key rotation through versions 1, 2, and 3 | PASS |
| Invalid ciphertext rejected | PASS |
| Wrong token rejected | PASS |
| Temporary token revocation rejected on subsequent use | PASS |
| Gateway boot with KV-backed `nxs1` config | PASS |
| `nxs1` to `nxs2` dry-run migration | PASS |
| `nxs1` to `nxs2` migration with pre-migration backup | PASS |
| `secrets verify` after migration | PASS |
| No local `.key` fallback file after migration | PASS |
| Gateway boot with Transit-backed `nxs2` config | PASS |
| Tampered `nxs2` envelope rejected | PASS |
| Rewrap to Transit key version 3 | PASS |
| `secrets verify` after rewrap | PASS |
| Pre-rewrap backup remains recoverable | PASS |
| Verification fails closed with unavailable Vault | PASS |

Observed application results included `migrated_fields=1`, `rewrapped_fields=1`, `nxs2_fields=1`, `transit_key_versions=3`, and `verified_fields=1`.

## Boundary

This run establishes real Vault Transit interoperability, migration, rotation, rewrap, rollback-readability, fail-closed behavior, and single-host Raft leader recovery for the disposable operator lab. It does **not** establish production readiness, HSM/KMS isolation, multi-host failure independence, or an externally managed staging SLA.
