# ADR 0007: Vault Transit envelope encryption

- **Status:** Proposed — not accepted; implementation and security review required
- **Date:** 2026-10-25
- **Issue:** [#649](https://github.com/ali-shortcuts/nexaroute/issues/649)

## Context

The current `nxs1` envelope creates a random per-secret 256-bit data-encryption key (DEK), encrypts the secret with AES-256-GCM, and wraps that DEK with a raw 32-byte master key. The existing `KeyProvider.Load` interface intentionally returns the raw master key to the Gateway process. The Vault KV v2 helper therefore provides external custody of that raw key, but it is not Vault Transit envelope encryption.

A future Transit mode must protect the **key-encryption key (KEK)** inside Vault. A Transit decrypt request necessarily returns the plaintext DEK to the caller so the Gateway can decrypt the selected secret; the DEK is short-lived data-key material and is not the Vault KEK. Do not describe this design as keeping every plaintext data key inside Vault.

## Decision proposed

Add a separate envelope-key abstraction rather than changing the meaning of `KeyProvider.Load`:

- `RawKeyProvider` (existing behavior) supplies a 32-byte KEK for `nxs1`.
- `EnvelopeKeyProvider` exposes a stable non-secret key identifier plus `WrapDEK` and `UnwrapDEK` operations. It must never expose the Transit KEK.
- Vault Transit uses its HTTPS API to wrap a fresh random DEK during encryption and unwrap it during decryption. Requests carry credentials only in the Vault token header, enforce strict timeouts and response-size limits, refuse redirects, validate configuration before sending requests, and return redacted errors.
- A new `nxs2` envelope stores the version, a full hash of the configured Transit key reference, the data nonce, the Transit-wrapped DEK, and the data ciphertext. Use an unambiguous versioned serialization (for example, a base64url-encoded JSON payload); do not concatenate opaque Vault ciphertext with colon delimiters.
- Secret plaintext continues to be encrypted locally with AES-256-GCM under the random DEK. The existing owner/field AAD remains mandatory, so moving a ciphertext to another provider, credential slot, header, or admin field must fail authentication.
- Existing `nxs1` payloads stay readable by the existing raw-key path. Selecting Transit for a configuration that contains `nxs1` must fail closed with an actionable migration message. Migration requires explicit access to the old matching key, decrypts with the old provider, re-encrypts under Transit, writes an authenticated encrypted backup, and atomically replaces the config. Never generate a substitute key or silently downgrade.
- Transit key rotation uses Vault-managed key versions. The configuration must retain the same mount/key reference across version rotations so old ciphertext can be decrypted. Provide an explicit rewrap operation using Transit `rewrap` to move wrapped DEKs to the latest version; it must write atomically and be recoverable after interruption. Disabling an old Vault key version before rewrap is verified must be documented as an operator lockout risk.
- `secrets status`, `verify`, `decrypt`, config load/save and backup/restore must recognize envelope versions. External Transit mode must not pass through code paths that expect raw KEK bytes. Until Transit-aware backup/restore and migration are implemented and tested, the provider must be feature-gated and unavailable for production use.

## Alternatives rejected

1. **Keep using the existing command provider with a Transit helper that returns a raw key.** Rejected: it does not provide Transit envelope semantics; the KEK still leaves Vault.
2. **Encrypt the complete config blob with Transit for every read.** Rejected for this design: it changes config availability and request behavior, makes a single remote Transit call gate all encrypted config fields, and does not reuse the existing per-secret DEK/AAD boundary.
3. **Silently reinterpret `nxs1` as Transit.** Rejected: `nxs1` requires the raw KEK and has a different envelope format. There is no safe implicit conversion.
4. **Remove all raw-key providers.** Rejected: local, environment, keyring, and KV command modes remain valid documented deployment choices.

## Required implementation and acceptance gates

- Add the envelope-key interface and keep existing `nxs1` byte-for-byte compatible.
- Add `nxs2` round-trip, tamper, AAD-swap, key-reference mismatch, malformed envelope, and wrong/missing Vault credential tests.
- Test a fake Transit HTTP API for URL construction, request/response contract, timeout, body cap, redirect rejection, HTTP errors, and redaction.
- Test that the Transit implementation never returns or logs the KEK, and that no fallback path creates or reads `<config>.key` when Transit mode is enabled.
- Test migration `nxs1 -> nxs2`, interrupted atomic write/recovery, backup verification, Transit rewrap after a key rotation, and failure when a historical Transit key version is unavailable.
- Run a disposable real Vault Transit acceptance test only with operator-authorized credentials: encrypt/decrypt round-trip, denied-policy failure, key-version rotation/rewrap and gateway `secrets verify`. CI fake-API tests alone do not establish live Vault compatibility.
- Update `SECURITY.md`, `docs/SECURITY.md`, `docs/EXTERNAL_VERIFICATION.md`, configuration and operator runbooks. Do not claim production Vault/HA/HSM guarantees without target-system evidence.

## Risks and guardrails

- Transit outage blocks decrypt of any affected configuration; startup and CLI operations must fail closed and must not fall back to a local key.
- Vault policy should grant only the required encrypt/decrypt/rewrap capabilities on the configured Transit key. Keep Vault URL and token in environment/secret injection, never in the durable config or error output.
- Rewrap and migration must use same-directory temporary files, mode `0600`, sync, atomic rename, and an authenticated backup before replacement.
- Plaintext secret values and DEKs must never appear in logs, metrics, argv, reports, or committed fixtures. Tests should use canary values and verify the absence of them at each surface.
- The feature must remain marked **NOT IMPLEMENTED** until all code, CI, and at least one real Vault Transit acceptance run pass.
