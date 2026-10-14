# M3-8 tasks

- [x] T1 Config: KeyHashes + global TPM fields, migration, load/save wiring, validation
- [x] T2 Auth: hash-based legacy identity, effective TPM, reservation + settle, recordUsage hook
- [x] T3 Config migration tests (load persists hashes, save never persists plaintext, validation)
- [x] T4 HTTP tests: hash-only auth, global TPM, settle refund/overage, per-client isolation
- [x] T5 Docs: CONFIGURATION, SECURITY, UNRELEASED, KNOWN_GAPS, checklist + header recount
- [x] T6 Full gates: go test, vet, check-deps, verify.sh, diff check
- [x] T7 Push, PR, monitor CI/Security on the PR head — PR #305; CodeQL initially flagged 2 high alerts (allocation-size-overflow on a map capacity hint in MigrateClientKeysToHashes); fixed properly by dropping the hint (commit 7955823), not by suppression. Final head 7955823: CI 38597936731 + Security 38597936727 + CodeQL check all success
