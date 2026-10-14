# M3-8 — Hashed-at-rest per-client keys and per-client TPM/quotas

## Current state (verified by inspection, 2026-10-14)

- Virtual keys (`client_auth.virtual_keys`) already store only a SHA-256
  digest (`key_hash`); plaintext is returned once at create/rotate.
  Per-key RPM and TPM token buckets already exist, keyed by the digest
  of the presented key, bounded (4096 buckets, idle eviction, fail
  closed when full), with 429 + `Retry-After` responses.
- Legacy keys (`client_auth.keys`) are stored as **plaintext** in the
  config model. The config file encrypts secret fields at rest (nxs1)
  when a master key is configured, but the model — and any plaintext
  config — holds usable credentials, unlike virtual keys.
- TPM admission charges a conservative request **estimate**
  (input bytes/4 + requested max output tokens; fail-closed when the
  request cannot be estimated) and never reconciles with the actual
  provider-reported usage. Legacy clients have no TPM ceiling at all
  (only the global RPM).

## Requirements

1. **Hashed at rest for every client key.** Legacy plaintext keys are
   migrated to SHA-256 digests (`client_auth.key_hashes`) automatically:
   in memory on every config load, persisted by the runtime load path
   and by every config save, so plaintext client credentials never
   remain in — or return to — the config file. (LoadBase's pre-existing
   nxs1 secrets migration may additionally encrypt a secret-bearing
   file on any load; the hash migration adds no rewrite of its own to
   the read-only load path.) Migration must not
   invalidate any existing key: the same secret keeps authenticating.
   Authentication stays fail-closed for unknown/revoked/expired keys.
   Plaintext supplied programmatically or via an admin settings save is
   converted before it can be persisted.
2. **Per-client TPM for every client class.** A global
   `client_auth.tpm` ceiling (mirroring `client_auth.rpm`) applies to
   legacy clients; a virtual key's own `tpm` overrides it. Buckets stay
   per-client (keyed by the presented key's digest): one client's usage
   never consumes another's quota.
3. **Usage reconciliation.** TPM admission keeps the conservative
   estimate as a *reservation*. When the provider reports actual usage
   (non-streaming response or streaming usage events, funnelled through
   `recordUsage`), the reservation is settled to the actual total:
   over-estimates are refunded, under-estimates are charged, clamped to
   the bucket. Requests that never report usage (failure before a
   response, cancellation without a final usage event, non-generation
   endpoints) keep the reservation as their charge — a documented,
   fail-closed estimation policy that cannot be used to bypass quota.
4. **No secret exposure.** Keys and digests are never logged; error
   responses stay generic; admin list responses never return hashes
   (unchanged) or plaintext.
5. **Compatibility.** Routing, provider failover, streaming, guardrails
   and the tenant/project/team policy intersection are unchanged. No
   new dependencies or external services. Quota state stays
   single-process in-memory (documented limitation, unchanged).

## Threat model notes

Client keys are 256-bit random secrets (`nrk_` + 32 random bytes);
unsalted SHA-256 is appropriate for high-entropy tokens (the same
construction already used for virtual keys) and avoids a server-side
pepper that would have to live next to the hashes. A leaked config now
yields no usable client credential — only digests that cannot be
replayed and are infeasible to brute-force at this entropy. Digests are
compared in constant time, as before.
