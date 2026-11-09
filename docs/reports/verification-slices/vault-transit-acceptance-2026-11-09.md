# Vault Transit acceptance — 2026-11-09

**Result: PASS, 50/50 checks** against the actual HashiCorp Vault 1.18.5 API on a disposable single-node local process. The recorded evidence class is `local-loopback`, bound to code SHA `35407ad0a0946de65bfcccc5ba3cd4b3779eb2d1` with a clean tracked worktree. This is not staging, production, HA, HSM, KMS, tenant, or multi-host acceptance.

The Vault binary was downloaded from HashiCorp’s official 1.18.5 release and its Linux AMD64 archive matched the published SHA-256 `dfd8619affbc6449a2f8b23a04f1e8632a00e9b8010b49f7f5daf253d181129d`. Vault used file-backed storage and a listener bound only to `127.0.0.1`; the acceptance script initialized and unsealed it, then removed the temporary data directory and stopped the process. No external Vault endpoint, provider credential, or paid service was used.

## What passed

The actual Transit API run migrated a config from `nxs1` to `nxs2`, verified it, started the normal Gateway against the Transit-backed config and reached `/healthz`, and rejected malformed ciphertext directly at `transit/decrypt` with HTTP 400. Wrong-token and unavailable-Vault checks failed closed without creating a local `.key` fallback.

The dedicated Transit key was `nexaroute`, type `aes256-gcm96`. The migrated envelope used key version 1. After rotation to version 2, the test raised `min_decryption_version` to 2 and confirmed that the version-1 config could no longer be verified. It then restored the floor to 1 and verified recovery. Rewrap moved the envelope to version 2; the saved pre-rewrap backup remained verifiable, and the rewrapped config verified again after the rollback check. Vault defines this floor as the minimum ciphertext key version allowed for decryption; the harness reads the key metadata after each change rather than trusting the update response alone.[1] [2]

Policies and tokens were generated only for this disposable run. `nexaroute-transit` had update access to the named key’s encrypt, decrypt, and rewrap endpoints. `nexaroute-rotate` had update access to the named key’s rotate endpoint and read access to its metadata. The short-lived `nexaroute-transit-config` policy had update access only to `transit/keys/nexaroute/config` and read access to `transit/keys/nexaroute`; it was used to test and restore the decryption floor. Token values, key bytes, ciphertext, and Vault response bodies were excluded from the retained reports, and the harness leak scan passed.

The retained [sanitized machine-readable summary](vault-transit-acceptance-2026-11-09.json) records the exact SHA, clean-worktree bit, Vault version, topology, evidence class, individual check IDs, and totals. The companion `vault-accept` output is explicitly a KV-only sub-report; its `vault.transit` NOT_RUN row does not negate the separate `transit.*` checks in the enclosing runner summary.

## Exact commands

```bash
export PATH="/home/ubuntu/opt/go1.27.3/bin:$PATH"
export GOTOOLCHAIN=local
bash scripts/test-vault-runner-acceptance.sh
go test ./cmd/gateway -run '^TestVaultAccept' -count=1
go test ./internal/secrets -count=1
go vet ./cmd/gateway
bash scripts/vault-runner-acceptance.sh \
  --vault-bin /home/ubuntu/opt/vault1.18.5/vault \
  --out-dir /tmp/nexaroute-issue649-local-loopback
```

The runner reported `local-loopback`, `outcome=PASS`, and `50/50` checks on Vault 1.18.5. The issued reports name the exact code SHA and confirm that the tracked worktree was clean at test start.

## Repository verification

On code SHA `35407ad0a0946de65bfcccc5ba3cd4b3779eb2d1`, the full `./scripts/verify.sh` returned `VERIFY PASS`. This included the clean unit/integration pass, `go vet`, race pass, all four real Chromium control-plane acceptance suites, short fuzz checks, release-gate regressions, and Linux AMD64/ARM64 builds. These repository checks are distinct from the Vault live-API result above; both are bound to the named code SHA.

## Boundary

This provides real-system interoperability evidence for the exercised Vault 1.18.5 local setup. It does not prove compatibility with a separately managed staging tenant, HA failover, production operations, HSM/KMS backing, multi-host deployment, other Vault versions, or organization-specific policies. Those remain separate operator/release claims and must not be inferred from this local-loopback PASS.

## References

[1]: https://developer.hashicorp.com/vault/api-docs/secret/transit "Vault Transit API"
[2]: https://developer.hashicorp.com/vault/docs/secrets/transit "Vault Transit secrets engine"
