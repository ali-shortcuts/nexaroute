# M3-8 plan

1. **config (`internal/config`)**
   - `ClientAuthConfig` gains `KeyHashes []string` (`key_hashes`) and
     `TPM int` (`tpm`).
   - `Config.MigrateClientKeysToHashes()` converts `Keys` → digests
     (deduplicated, order-preserving), clears `Keys`, returns the count.
   - `LoadBase` converts in memory after unmarshal and records the
     pending count on the returned Config; `Load` (the runtime path)
     persists the migrated config via `SaveAtomic` and fails closed if
     persisting fails (same posture as the nxs1 secrets migration).
     Read-only tools using `LoadBase` never rewrite the file.
   - `SaveAtomic` converts before marshaling, so no save can persist
     plaintext client keys.
   - `Validate` accepts `KeyHashes` as satisfying "keys configured",
     enforces 64-char hex digests, the 1024-entry bound, and the TPM
     range.
2. **auth (`internal/httpapi/clientauth.go`)**
   - `findClientIdentity` accepts a legacy identity via `KeyHashes`
     (constant-time digest compare) or the transitional plaintext list.
   - Effective TPM = virtual-key TPM if set, else the global
     `client_auth.tpm`. `clientTPMReserve` returns a reservation
     (bucket id, capacity, charged estimate) stored in the request
     context next to the identity; `clientTPMAllow` remains as a
     wrapper for existing callers/tests.
   - `clientTPMSettle` reconciles a reservation against a reported
     actual total under the existing bucket lock, with refill and
     clamping; repeated usage reports settle incrementally.
   - `recordUsage` (`identity_usage.go`) settles the request's
     reservation with `input + output` on every usage report.
3. **Docs**: CONFIGURATION (new fields + migration), SECURITY (design,
   threat model, accounting policy), UNRELEASED, KNOWN_GAPS (drop
   hashed-at-rest keys from the gap list), checklist M3-8 →
   IN PROGRESS with evidence, header recount.
4. **Tests**: config migration (load/save/no-plaintext/auth-equivalence,
   validation), httpapi (hash-only auth, global TPM for legacy keys,
   settle refund + overage, per-client isolation, race pass via
   verify.sh).
