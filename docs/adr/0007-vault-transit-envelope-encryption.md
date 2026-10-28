# ADR 0007: Vault Transit envelope encryption

- **Status:** Proposed — not accepted; implementation and security review required
- **Date:** 2026-10-27 (revision of the draft proposed in PR #655)
- **Issue:** #649
- **Supersedes:** nothing by itself. This is a revision proposal for the
  draft in PR #655. It does not merge, close, or replace that PR on its
  own; the maintainer decides which draft, if either, is accepted.
- **Security review basis:** Phase 1 security review of 2026-10-27,
  which found the original 49-line draft underspecified in eight
  critical areas (envelope schema, authentication binding,
  mixed-format handling, migration/rewrap state machines, interface
  definition, fail-closed matrix, feature gate/selection, and
  backup/restore). This revision closes those areas with normative
  decisions. Where this revision changes a suggestion in that review,
  it says so explicitly (see §5.3).

## 1. Context and threat model

### 1.1 What exists today

The current `nxs1` envelope (verified on main `e6c3cdf3`,
`internal/secrets/secrets.go`) creates a random per-secret 256-bit
data-encryption key (DEK), encrypts the secret locally with
AES-256-GCM under that DEK, and wraps the DEK with a raw 32-byte
master key (the key-encryption key, KEK). The existing
`KeyProvider.Load` interface intentionally returns the raw master
key to the gateway process. The Vault KV v2 helper
(`scripts/key-provider-vault-kv.sh`) therefore provides external
custody of that raw key, but it is **not** Vault Transit envelope
encryption: the KEK still enters the gateway process.

Secret-bearing fields are provider API keys, credential-pool API
keys, decision-provider keys, the Admin API key, client-auth keys,
provider custom-header values, and proxy URLs. Each is bound by
associated data (AAD) to its owner and exact field
(`internal/secrets.AAD`: owner, field, credential-pool index, header
name). Moving a ciphertext to a different field fails authentication.

### 1.2 What Transit mode must and must not claim

A future Transit mode must protect the KEK inside Vault. A Transit
decrypt request necessarily returns the plaintext DEK to the caller
so the gateway can decrypt the selected secret; the DEK is
short-lived data-key material and is **not** the Vault KEK. Do not
describe this design as keeping every plaintext data key inside
Vault, and do not describe a Transit-wrapped config as protected
against a compromised gateway host, a compromised Vault, or a
leaked Vault token.

Threat positions addressed by this design:

- **Config-file or backup reader without a Vault token** cannot
  decrypt `nxs2` secrets: they hold only wrapped DEKs and data
  ciphertext, and Vault will not unwrap without an authorised token.
- **Config-file writer / tamperer** cannot move a secret to another
  field, swap wrapped DEKs between fields, or rewrite the envelope
  header without failing the data authentication check (§5).
- **Network attacker between gateway and Vault** is stopped by TLS
  (§8); a redirected request must never carry the token (§8).

Threat positions explicitly **not** addressed:

- A leaked Vault token scoped to the Transit key is effectively a
  master key for every `nxs2` secret. Token scoping, TTL, and
  revocation (§9) are therefore part of the security design, not
  operator garnish.
- A compromised gateway host can read secrets it legitimately
  decrypts at load, exactly as with `nxs1`. Transit changes where
  the KEK lives; it does not change the gateway's own trust boundary.
- A compromised Vault, or a Vault audit device that retains DEK
  plaintext from Transit responses, defeats the design. The operator
  checklist (§9) must verify audit handling; the gateway cannot
  control it and must not imply otherwise.

### 1.3 Current-code ground truth this ADR must fit

- `secrets.Key()` returns raw KEK bytes; every caller (`LoadBase`,
  `SaveAtomic`, `secrets decrypt/rotate`) is written around raw bytes.
  There is no envelope-provider dispatch point today; one must be
  created, and every raw caller must be audited for the Transit branch.
- `LoadBase` auto-migrates legacy plaintext fields on load and
  writes a timestamped `.enc.bak` sealed under the raw KEK
  (`EncryptedBackup`, AAD `nexaroute-backup-v1`). There is no Transit
  equivalent yet; §7 defines one, and it is not the same format.
- `secrets rotate` has a file-based recovery machine
  (`<config>.key.previous` / `.key.next`, staged-key authentication
  on startup). It has no Transit analogue and must refuse Transit
  mode (§7.4).
- `secrets status`, `doctor`, the privacy audit, and backup/restore
  keyless logic all hardcode the `nxs1:` prefix today. A config
  containing `nxs2` strings would be misread by all of them (§6).
  Closing that gap is a precondition for any `nxs2` value existing,
  including in test fixtures loaded through production paths.

## 2. Decision

Add a separate envelope-key abstraction. Do not change the meaning
of `KeyProvider.Load`.

- `RawKeyProvider` (existing behaviour) supplies a 32-byte KEK for `nxs1`.
- `EnvelopeKeyProvider` (§4) exposes a stable non-secret key
  identifier plus `WrapDEK`, `UnwrapDEK`, and `RewrapDEK` operations.
  It must never expose, return, store, or log the Transit KEK, and it
  must not implement `KeyProvider` (no `Load`/`Store` returning raw
  key bytes). Making that a type separation, not a convention, is a
  requirement, not a style preference.
- Vault Transit is used through a new, small, in-process HTTP client
  (§8). The KV command-helper pattern (subprocess, key on stdout)
  must not be reused or imitated for Transit: there is no raw key to
  pass on stdout, and a subprocess boundary adds nothing.
- A new `nxs2` envelope (§3) stores the version, provider kind, full
  key-reference hash, Transit key version, Transit-wrapped DEK, data
  nonce, and data ciphertext, in one self-delimiting segment.
- Secret plaintext continues to be encrypted locally with
  AES-256-GCM under a fresh random 32-byte DEK per field. The
  gateway, not Vault, generates the DEK, the data nonce, and the
  data ciphertext.
- Existing `nxs1` payloads stay readable by the existing raw-key
  path, byte-for-byte. Selecting Transit for a configuration that
  contains `nxs1` must fail closed with an actionable migration
  message. Migration is an explicit command (§7.2), never implicit
  on load, never a substitute-key generation, never a silent
  downgrade, and never a fallback to a local or environment key.
- Transit key rotation uses Vault-managed key versions. The
  configuration retains the same mount/key reference across version
  rotations so old ciphertext can decrypt. Rewrap is an explicit
  command using Transit `rewrap` (§7.3), never unwrap-plus-wrap.
- Until Transit-aware backup/restore and migration are implemented
  and tested (§7, §10), the provider is feature-gated and
  unavailable for production use (§10.1). The feature remains
  marked **NOT IMPLEMENTED** in SECURITY.md, `docs/SECURITY.md`,
  and `docs/EXTERNAL_VERIFICATION.md` until code, CI, and at least
  one real Vault Transit acceptance run pass (§11).

## 3. The `nxs2` envelope (normative)

### 3.1 Outer form

```
nxs2:<base64url-no-padding of the inner JSON object>
```

Exactly one segment after the prefix. No colon concatenation: Vault
ciphertext is itself colon-delimited (`vault:v<version>:<base64>`),
so any colon-joined format would be ambiguous to parse. The inner
JSON is encoded with base64url **without padding**
(`base64.RawURLEncoding` semantics), and the parser must verify
that re-encoding the decoded bytes reproduces the input exactly
(strict base64, mirroring the strictness `nxs1` parsing already
applies). Any deviation — padding, non-URL-safe alphabet, trailing
data — is a parse failure, not a best-effort decode.

### 3.2 Inner JSON schema

Field order is insignificant; whitespace is insignificant. The
object must contain exactly the fields below, with exactly these
names and types. Unknown fields are rejected. Duplicate JSON keys
are rejected (the parser must detect duplicates explicitly; a
decoder that silently takes the last duplicate is not acceptable
for this format). Trailing data after the JSON object is rejected.

| Field | Type | Rule |
|---|---|---|
| `envelope_version` | integer | Must be exactly `2`. Any other value, including `1`, is rejected. This is a **format** version, not a key version. |
| `provider` | string | Must be exactly `"vault-transit"`. |
| `key_ref_sha256` | string | Exactly 64 lowercase hex characters: the full SHA-256 of the canonical key reference (§3.3). Not the truncated 8-byte `keyID()` used by `nxs1`. |
| `transit_key_version` | integer | Must be ≥ 1, with no leading-zero or non-numeric representation issues (it is JSON, so this is a value check). Must equal the version parsed from `wrapped_dek`; a mismatch is rejected **before** any Vault call. |
| `wrapped_dek` | string | Vault Transit ciphertext, strict format (§3.4). Length after decoding the outer segment: at most 4096 bytes. A wrapped 32-byte DEK is on the order of 100 bytes; 4096 is a generous bound, not a tight fit. |
| `data_nonce_b64url` | string | Base64url, no padding, strict re-encode check. Must decode to exactly 12 bytes (the AES-GCM nonce size). |
| `data_ciphertext_b64url` | string | Base64url, no padding, strict re-encode check. Must decode to at least 16 bytes (the GCM tag) and at most `maxConfigBytes` (16 MiB, the existing config size bound). |

Worked shape (placeholders only — no real ciphertext, token, or
key material; a concrete example with canary values belongs in
tests, not in this document):

```json
{
  "envelope_version": 2,
  "provider": "vault-transit",
  "key_ref_sha256": "<64 lowercase hex>",
  "transit_key_version": 2,
  "wrapped_dek": "vault:v2:<base64>",
  "data_nonce_b64url": "<base64url, 12 bytes decoded>",
  "data_ciphertext_b64url": "<base64url>"
}
```

### 3.3 Canonical key reference and its hash

The hash must be computable by an operator from the configured
reference, and must be stable across Vault key-version rotations
and across Vault endpoint changes (failover, DNS rename). It is
therefore defined over mount, namespace, and key — **not** over the
Vault address:

```
canonical = "vault-transit" + "|" + "namespace=" + <namespace or "">
          + "|" + "mount=" + <mount> + "|" + "key=" + <key>
key_ref_sha256 = hex( SHA-256( UTF-8(canonical) ) )
```

Validation, at configuration time, before any Vault call:

- Mount: one or more path segments separated by single `/`
  characters; each segment matches `[A-Za-z0-9._-]+`; no empty
  segment, no `.` or `..` segment, no leading or trailing slash,
  total length at most 255.
- Key: a single segment matching `[A-Za-z0-9._-]+`, length 1–128,
  no slash.
- Namespace (optional, `VAULT_NAMESPACE`): same segment rules as a
  mount, or empty. Empty and absent are the same canonical value.

The hash is a mismatch detector, not confidentiality: key names
are low-entropy (for example `nexaroute`), so an attacker holding
the config can dictionary-guess the reference from its hash. The
hash's purpose is to avoid storing Vault topology in cleartext in
every config file, backup, and report, and to make a wrong-key
config fail deterministically before a Vault call. Operators
diagnosing a mismatch compute the hash for a candidate reference
with the same canonical construction; the gateway's error message
names the field and the expected-provider mismatch class, never
the stored hash's preimage guess and never the ciphertext.

### 3.4 Wrapped DEK format (strict)

`wrapped_dek` must match, after splitting on `:` into exactly three
parts:

1. `vault`
2. `v<version>` where version is a decimal integer ≥ 1 with no
   leading zeros and no sign
3. base64 (standard alphabet, with padding as Vault emits it),
   strict decode with re-encode check, non-empty, at most 4096 bytes
   of ciphertext text

The version in part 2 must equal `transit_key_version` (§3.2).
Any other shape — missing prefix, missing version, extra colons,
non-base64 tail, version 0 — is rejected before any Vault call.
The gateway must not attempt to "repair" a malformed wrapped DEK
by trying another key, version, or provider.

### 3.5 What is *not* in the envelope

- No raw key reference (mount/key in cleartext) — only its hash.
- No Vault address, token, namespace header value, or policy name.
- No DEK, plaintext, or data-plaintext hint.
- No `context` value: for the pinned non-derived key type (§9),
  Vault ignores `context` on encrypt/decrypt, so storing or
  passing one would be false assurance (see §5.3).

## 4. The envelope-key interface (normative)

The interface below is the contract the revised ADR requires. Names
are normative for the implementation PR; changing a name in code
without amending this ADR is a scope change.

```go
type EnvelopeKeyProvider interface {
    // KeyRefHash returns the full SHA-256 hex of the canonical key
    // reference (§3.3). It is non-secret and contains no key material.
    KeyRefHash() string

    // WrapDEK asks Vault Transit to wrap a fresh DEK. It returns the
    // Vault ciphertext and the key version Vault used. It never
    // returns, stores, or logs KEK material (it never receives any).
    WrapDEK(ctx context.Context, dek []byte) (wrapped string, version int, error)

    // UnwrapDEK asks Vault Transit to unwrap a wrapped DEK. It returns
    // a fresh DEK that the caller owns and must wipe immediately after
    // the data authentication check (§5.4), on success and on failure.
    // A successful unwrap is NOT authentication of the field: the
    // data GCM open (§5) is the authentication step.
    UnwrapDEK(ctx context.Context, wrapped string) (dek []byte, version int, error)

    // RewrapDEK asks Vault Transit to rewrap a wrapped DEK to the
    // latest key version. It is implemented with the Transit rewrap
    // endpoint only (§7.3). It must never be implemented as
    // UnwrapDEK followed by WrapDEK.
    RewrapDEK(ctx context.Context, wrapped string) (wrapped string, version int, error)

    // Close releases client resources. It must not log or retain
    // DEKs, tokens, or wrapped DEKs.
    Close() error
}
```

Rules that apply to every implementation of this interface:

- **No `KeyProvider` embedding.** A Transit provider must not
  satisfy `KeyProvider`, so no raw-key caller can compile or run
  against it by accident.
- **No `Store` of raw key material.** There is no operation that
  writes a KEK to Vault through this interface; key creation and
  rotation are Vault-side operator operations (§9).
- **Context with timeout.** Every method receives a context that
  already carries a timeout (§8.3). A method must never use an
  unbounded `context.Background()` internally.
- **Caller owns DEK wiping.** See §5.4.
- **`secrets.Key()` is unreachable in Transit mode.** Provider
  selection branches *before* `Key()` is called. A test must assert
  that Transit load, save, verify, migrate, and rewrap never create,
  read, or write `<config>.key`, `<config>.key.next`, or
  `<config>.key.previous` — including when such a file happens to
  exist on disk from a previous raw-key deployment (it must be
  ignored, not read, by normal Transit load; migration is the only
  path that deliberately reads the old provider, §7.2).

## 5. Authentication binding (normative)

### 5.1 Data authentication: what the GCM tag covers

Each secret is encrypted locally with AES-256-GCM under its DEK.
The AAD for that GCM operation is the canonical binding below.
The binding is length-prefixed per component, so no component
boundary can be confused by a component containing a separator:

```
for each component c in order:
    append decimal(len(c)) + ":" + c + "\n"
components, in order:
    "nxs2-binding-v1"
    "2"                                   (envelope version, decimal)
    "vault-transit"                       (provider kind)
    key_ref_sha256                        (from §3.3)
    data_nonce, as base64url text         (the stored nonce encoding)
    owner                                 (provider id / "admin" / "client_auth")
    field                                 (field, credential-pool index, header name)
```

Changing any bound component — version, provider, key reference,
nonce, owner, or field — changes the AAD and fails the GCM open.
Moving a whole envelope to another provider, field, credential
index, or header name therefore fails, exactly as `nxs1` AAD
already guarantees for owner/field, extended here to version,
provider, and key reference.

### 5.2 Why wrapped DEK and key version are deliberately *not* in the AAD

This is a deliberate, rewrap-driven exclusion, and it is the one
place this revision corrects the Phase 1 review's suggestion
(which proposed including wrapped-DEK bytes in the AAD):

- Rewrap changes the wrapped DEK and the key version, and must
  **not** change the data ciphertext (§7.3). If either value were
  in the data AAD, every rewrap would invalidate every data
  ciphertext unless the data were also re-encrypted — which would
  require unwrapping the DEK, defeating the purpose of Vault
  rewrap.
- Security is not lost by the exclusion. A transplanted wrapped
  DEK unwraps (if at all) to the wrong DEK, and the data GCM open
  under that DEK fails. The AAD does not need to cover the wrapped
  bytes for the transplant to fail; it needs to cover the
  *identity* of the field and key, which it does. What the AAD
  must not do is cover values that legitimately change on rewrap.

Consequence, stated plainly: **Vault unwrap success is not
authentication.** The gateway must treat a successful Transit
decrypt as "a DEK was returned", and only the subsequent data GCM
open — with the §5.1 AAD, under that DEK — as authentication of
the field. Any code path that treats unwrap success as proof that
a field is correct is a defect.

### 5.3 Transit `context`: not a binding for this key type

Vault Transit `context` binds a wrapped value only for **derived**
keys. This ADR pins the key type to non-derived
`aes256-gcm96` (§9), for which Vault ignores `context` on
encrypt/decrypt. The gateway therefore **must not pass a `context`
parameter** to Transit encrypt/decrypt/rewrap in this design, and
must not claim Vault-layer binding from it. Passing a context
that Vault ignores would be worse than passing none: it would
make code reviewers and operators believe a binding exists that
does not. A future derived-key mode, if ever wanted, needs its own
ADR amendment covering context derivation, rewrap-with-context
semantics, and the interaction with §5.2 — it is not a flag flip.

### 5.4 DEK hygiene (normative)

- DEKs are generated with `crypto/rand`, exactly 32 bytes, fresh
  per field, never reused across fields, never derived from the
  secret or from any other DEK.
- A DEK returned by `UnwrapDEK` is wiped (`clear()`, or an
  equivalent explicit zeroing loop) immediately after the data GCM
  open, on both success and failure paths (use a deferred wipe in
  the decrypt path). The wipe must happen even when the GCM open
  fails.
- DEKs are never converted to `string` (a string copy cannot be
  wiped), never stored in a struct that outlives the field decrypt,
  never cached across loads to "save" Vault calls, never logged,
  never placed in an error, and never included in a report, backup
  manifest, migration journal, or Admin response.
- The in-memory plaintext config that `LoadBase` produces is, as
  today, the gateway's own decrypted configuration; Transit does
  not change that trust position, and this ADR does not claim
  otherwise. What Transit changes is that no KEK is ever in that
  memory alongside it.

## 6. Envelope classification and mixed-format refusal (normative)

Before any `nxs2` value can exist, the codebase needs one
envelope-classification function used at **every** entry point:
config load, config save, `secrets status`, `secrets verify`,
`secrets decrypt`, `secrets rotate`, `secrets migrate`,
`secrets rewrap`, backup, restore, `doctor`, privacy audit, and
Admin save. The classes are: `nxs1`, `nxs2`, plaintext (non-empty,
non-envelope), empty, and unknown-envelope (any value matching
`nxs<digit>:` that is not a known, supported version, or any
value that looks like an envelope but fails §3 parsing).

Rules:

1. **Unknown envelope → fail closed, everywhere.** Never treated
   as plaintext, never re-encrypted, never skipped, never saved.
2. **Mixed `nxs1` + `nxs2` in one config → fail closed, everywhere.**
   There is no partially-migrated steady state. A mixed config is
   an error naming the offending fields and the expected provider,
   not a migration in progress.
3. **Provider/envelope consistency is checked before any Vault call
   and before any file write.** Transit mode + any `nxs1` field →
   fail with the migration message (§7.2). Raw mode + any `nxs2`
   field → fail with the reverse message. The check runs in
   `LoadBase`, in `SaveAtomic`, and in the CLI paths, because
   Admin saves and CLI writes do not all flow through `LoadBase`.
4. **The raw `TransformConfig(key, …)` path must never be called
   with a Transit config.** Transit needs an envelope-aware
   transform that dispatches per envelope class and preserves
   existing `nxs2` envelopes on save (see §7.5). This is a
   call-site audit item for the implementation PR: every existing
   `TransformConfig` and `secrets.Key()` call site must be listed
   in the PR description with its Transit behaviour (refuse,
   dispatch, or preserve).
5. **Reporting surfaces count per version.** `secrets status`
   reports `nxs1_fields` and `nxs2_fields` separately (never a
   single count that hides the mix, never `plaintext_fields_may_exist`
   for a fully encrypted config). `doctor` and the privacy audit
   report `nxs2` fields as encrypted, not as plaintext secrets.
   No reporting surface prints an envelope value, a wrapped DEK,
   or a key-reference preimage.

The reason for the strictness: without rule 1–3, the current
`LoadBase` behaviour — trigger on `nxs1:` **or** on any non-empty
secret value, decrypt only `nxs1`, re-encrypt everything else —
would silently re-seal an `nxs2` string inside `nxs1` and rewrite
the file, corrupting the config while reporting a successful
migration. That failure mode, not a hypothetical attacker, is the
most likely way this feature causes data loss if the classifier
is added after the envelope.

## 7. Migration, rewrap, backup/restore, and the secrets CLI

### 7.1 The atomic-write discipline (applies to migration, rewrap, and save)

Migration and rewrap are all-or-nothing, single-file operations:

1. Read the whole config; classify every field (§6).
2. Perform the whole transformation **in memory** (decrypt old,
   wrap/rewrap new, as applicable).
3. Verify the whole transformed config **in memory** (decrypt every
   field; see the per-operation verification steps below) before
   any file write.
4. Write the new config through the existing same-directory
   temporary-file, mode `0600`, sync, atomic rename, directory-sync
   discipline (`writeAtomic`).
5. Verify the written file by loading it (`secrets verify`
   equivalent) before reporting success.
6. On any failure before step 4, the original file is byte-identical
   and no backup is needed for recovery. On failure at or after
   step 4, roll back by atomically renaming the pre-operation
   backup over the config, then verify the rollback before
   reporting failure.

**Field-by-field writes are forbidden.** A crash mid-loop would
leave a mixed `nxs1`/`nxs2` file, which §6 then correctly refuses
to load — a self-inflicted lockout. Likewise, no operation may
write a config that has not been fully verified in memory.

**Concurrency:** migration and rewrap hold an exclusive file lock
on the config for the whole operation. A second migration, rewrap,
or Admin save that cannot acquire the lock fails with a lock
message; it must never merge with or overwrite the first writer's
result. Current rotation has no such lock; Transit operations are
slower (network round-trips per field), so the race window is
larger and the lock is required here even though rotation lacks it.

### 7.2 Migration `nxs1` → `nxs2` (explicit command only)

New explicit command (name normative for the implementation PR;
the PR may propose a different name only by amending this ADR):
`nexaroute secrets migrate-to-transit --config PATH`, with a
`--dry-run` mode that performs no file write.

Steps, in order, each a hard failure (abort, no file change) if it
fails:

1. **Classify** (§6): config must contain only `nxs1` fields (and
   empty fields). Any `nxs2` field → "already migrated, nothing to
   do" (verify only, §7.2 step 6). Any plaintext secret field →
   fail with an explicit-migration message; plaintext is never
   auto-migrated into Transit (see §7.5). Any unknown envelope →
   fail.
2. **Decrypt old, fully, in memory**, using the old raw provider
   with explicit access to the old matching key. Wrong old key →
   fail before any Vault encrypt call and before any file write.
3. **Prove Transit connectivity without touching the config:**
   wrap and unwrap a fresh random canary DEK via Transit and wipe
   it. Failure → abort, original file untouched.
4. **Write the pre-migration backup first:** the pre-migration
   config bytes **verbatim**, mode `0600`, same directory, synced,
   named `<config>.<UTC-timestamp>.pre-transit.bak`, plus its
   SHA-256 recorded in the migration report (stdout, non-secret).
   This backup is **not** an `.enc.bak` `nxs1` seal: there is no
   raw KEK to seal a Transit backup with, and a Transit-sealed
   backup would depend on Transit availability and would not be
   a rollback. The backup is decryptable only by the old KEK, so
   the old KEK must be retained (archived, not deleted) until
   step 6 passes. Deleting the old key before verification
   destroys the rollback.
5. **Re-encrypt every field in memory** under Transit (fresh DEK
   per field, §5 AAD, §3 envelope), then **decrypt-verify every
   re-encrypted field in memory** (unwrap + data GCM open, compare
   against the step-2 plaintext held only in memory, wipe DEKs
   and plaintext copies on completion).
6. **Atomic replace** (§7.1), then **post-write verify** by
   loading the written file. Only then report success, naming the
   backup path and hash and instructing the operator to archive
   the old key pair until the next successful backup cycle.
7. **Re-running migration on an already-`nxs2` config** verifies
   only, writes nothing, and reports "already migrated".

Crash states, stated exhaustively:

- Crash during steps 1–3 or 5 (in memory): no file changed; re-run.
- Crash after step 4 (backup written), before step 6: original
  config intact, backup present; re-run (backup is overwritten
  with a new timestamped name, never appended to ambiguously).
- Crash after step 6 (rename), before the operator is told: new
  config is complete `nxs2`; re-running migration detects
  "already migrated" and verifies; it must not re-wrap or fail.
- Crash during post-write verification failure: roll back by
  atomically renaming the step-4 backup over the config, verify
  the rollback, then report failure. Never report success on a
  config that failed post-write verification, and never leave a
  mixed file in place "for the operator to sort out".

### 7.3 Rewrap (explicit command only)

New explicit command: `nexaroute secrets rewrap-transit --config PATH`,
with `--dry-run` (verify old decrypt, report counts and distinct
key versions present, no file write, no Vault rewrap call).

Vault-side rotation and gateway rewrap are different operations
and must not be conflated in docs, CLI help, or error messages:

| Operation | What changes | Gateway action |
|---|---|---|
| Vault key rotation | Vault creates a new key version; old ciphertext still decrypts (unless policy forbids it) | None. Decrypt uses the version embedded in the ciphertext. |
| Gateway rewrap | Wrapped DEKs move to the latest Vault key version; data ciphertext unchanged | This command, via Transit `rewrap` per field. |
| `secrets rotate` | Local `<config>.key` replaced | Must refuse Transit mode (§7.4). It cannot rotate a Vault key. |

Steps, in order:

1. **Classify** (§6): config must contain only `nxs2` fields.
   Any `nxs1` field → migration message, not rewrap.
2. **Decrypt-verify the old config fully in memory** before any
   rewrap call. A field that no longer decrypts aborts the whole
   operation before any rewrap call, with the version error
   (§8.4), not a partial rewrap.
3. **Rewrap every field in memory** via Transit `rewrap` only.
   The implementation must not implement rewrap as
   `UnwrapDEK` + `WrapDEK`: that exposes every DEK to the gateway
   during rewrap for no reason, and a fake-API test must assert
   the `rewrap` endpoint was called and `decrypt`/`encrypt` were
   not called for the rewrap itself (verification decrypts in
   steps 2 and 4 are distinct and must be distinguishable in the
   test's request log).
4. **Decrypt-verify the new in-memory config fully** (unwrap every
   rewrapped DEK, data GCM open with the §5.1 AAD — which is
   unchanged by rewrap, by design, §5.2 — compare against step-2
   plaintext in memory only).
5. **Atomic replace** (§7.1) only after step 4 passes, then
   post-write verify. Any failure before step 5 leaves the
   original file byte-identical; no backup is strictly needed for
   rewrap (the original file is the pre-image), but the operator
   must have a current backup before running rewrap, and the
   command must refuse to run if it cannot confirm a backup was
   taken in the current session — in practice, this means rewrap
   writes the same pre-operation backup as §7.2 step 4 (named
   `.pre-rewrap.bak`) before replacing, so a post-write failure
   has the same rollback path as migration.
6. **Idempotency:** rewrapping a field already at the latest
   version is a no-op: the returned wrapped DEK must be compared
   (in memory) with the original, and if identical, the field is
   left byte-identical. Rewrap never rewrites a file whose fields
   are all already at the latest version; it reports "already at
   latest version" and writes nothing. Latest version is learned
   from rewrap responses and ciphertext prefixes, not from a Vault
   `read` capability (see §8.2 on policy scope).

**Key-version destruction / `min_decryption_version` checklist
(operator lockout operation):** never raise
`min_decryption_version`, disable, or destroy a key version until
all of: (a) rewrap verified per steps 2–5, (b) post-write verify
passed, (c) a backup of the rewrapped config verified, and
(d) the distinct key versions present in the config (parsed from
ciphertext prefixes, reported by `secrets status` / rewrap
`--dry-run`) are all ≥ the version being retired. The gateway
surfaces a Vault version error as that actionable checklist
message, not as a generic decrypt failure.

### 7.4 `secrets rotate` and other CLI behaviour in Transit mode

- `secrets rotate` **refuses** Transit mode, exactly as it refuses
  other external providers today, with a message pointing at
  Vault-side rotation plus `secrets rewrap-transit` — not at a
  local key file. It must not be adapted to "rotate" a Vault key.
- `secrets status` reports per-version counts (§6 rule 5) and
  distinct Transit key versions present, and never prints envelope
  values.
- `secrets verify` decrypts every field in memory and reports
  field identifiers and success/failure, never values — the same
  contract as today, extended to `nxs2`.
- `secrets decrypt` keeps its existing both-flags requirement
  (`--to-stdout` and `--allow-plaintext`); no new plaintext export
  path (in particular no "Transit debug decrypt") may be added.
- `nexaroute backup` / `restore` for `nxs2` configs: see §7.6.

### 7.5 Load and save in Transit mode

- **Load:** `LoadBase` in Transit mode decrypts `nxs2` fields via
  `UnwrapDEK` + data GCM (§5), sequentially, one field at a time,
  wiping each DEK before the next field. Any field failure fails
  the whole load; there is no partially decrypted config and no
  start with undecrypted fields. Plaintext secret fields in
  Transit mode fail closed on load with an explicit-migration
  message. The existing auto-migration of plaintext on load is
  **disabled** in Transit mode: implicit network calls with a new
  DEK per field are not an acceptable side effect of a read path.
- **Save:** `SaveAtomic` in Transit mode preserves existing `nxs2`
  envelopes for unchanged fields byte-for-byte (no re-wrap, no
  new Vault call for them), encrypts only genuinely new plaintext
  fields via Transit, fails if any `nxs1` field is present, and
  fails (no file change) if Vault is unavailable. Preserving
  unchanged envelopes is what keeps an Admin save from becoming
  N Vault calls and a needless churn of wrapped DEKs — and it is
  also what makes §7.1's atomic discipline hold for saves.
- **Running gateway during a Vault outage:** an already-loaded
  in-memory config keeps serving (there is no per-request Transit
  call in the data plane as designed), but any new load, save,
  verify, migrate, or rewrap fails closed. Operators must not
  assume the data plane depends on Vault per request, and must
  not assume a running gateway can re-encrypt after an outage.

### 7.6 Backup and restore for `nxs2`

The existing backup manifest and restore logic are built around a
raw key and must be revised, not reused, for Transit:

- **Manifest:** for an `nxs2` config, the manifest records
  `envelope_versions` (e.g. `["nxs2"]`), `key_ref_sha256`, and the
  distinct `transit_key_versions` present. It records **no**
  `master_key_sha256` (there is no raw key to fingerprint) and
  **no** `master_key_included`. A bundle that claims to include a
  Transit key, or that carries a `master.key` entry alongside an
  `nxs2` config, fails validation.
- **`--include-master-key` is rejected** in Transit mode. The
  flag's meaning (bundle the raw key bytes) has no Transit
  equivalent, and silently ignoring it would mislead the operator
  about what the bundle can restore.
- **Keyless restore justification is rewritten for `nxs2`:**
  a restore of an `nxs2` config with Vault unavailable is
  `BLOCKED` (nonzero), never a pass and never a structural-parse-only
  install that pretends validation happened. With Vault available,
  restore verification decrypt-verifies via Transit (the staged
  load path, with the staged config's Transit configuration, not a
  staged `.key` file — restore must never stage a `.key` file for
  an `nxs2` config).
- **Migration backups (§7.2) are separate from `nexaroute backup`
  bundles.** The former are verbatim pre-migration bytes for
  rollback; the latter are the normal bundle format with the
  Transit manifest fields above. Neither format is the other's
  substitute, and neither is an `.enc.bak` `nxs1` seal for a
  Transit config.

## 8. Vault client contract and fail-closed matrix (normative)

### 8.1 Endpoints and request/response contract

The Transit client talks to exactly three Vault Transit endpoints,
under the configured mount (see §10.2 for the mount/key variables):

| Operation | Method and path | Request JSON (fields) | Response JSON (fields used) |
|---|---|---|---|
| Wrap (encrypt) | `POST /v1/<mount>/encrypt/<key>` | `plaintext`: base64 of the 32-byte DEK | `data.ciphertext` |
| Unwrap (decrypt) | `POST /v1/<mount>/decrypt/<key>` | `ciphertext`: the wrapped DEK string | `data.plaintext`: base64, must decode to exactly 32 bytes |
| Rewrap | `POST /v1/<mount>/rewrap/<key>` | `ciphertext`: the wrapped DEK string | `data.ciphertext` |

No other Vault endpoint is called by the gateway in Transit mode
(in particular no `read` on the key to learn its type or latest
version, no KV read, no `sys/` call). The client sends exactly
one credential header, `X-Vault-Token`, and, if a namespace is
configured, `X-Vault-Namespace`. It sends no other authentication
material, and it never puts the token in a URL, query string, or
request body.

### 8.2 Transport hardening (normative)

- **TLS:** HTTPS only, except for loopback destinations
  (`127.0.0.1`, `::1`, `localhost`) where HTTP is permitted for
  local development, mirroring the rule already applied to OIDC
  endpoints. TLS verification must never be disabled
  (`InsecureSkipVerify` is forbidden in all builds and tests).
  Minimum TLS 1.2. A custom CA bundle, if an operator needs one,
  is endpoint configuration outside the gateway's config; it must
  not be implemented as a gateway flag that weakens verification.
- **Redirects:** refused, not followed. The client sets
  `CheckRedirect` to return `http.ErrUseLastResponse` (or
  equivalent) and treats any 3xx as a fail-closed error. The
  default Go client behaviour of following redirects — potentially
  forwarding the token header to another host — is a defect in
  this client.
- **URL validation at configuration time, before any request:**
  scheme, host, and path are validated; userinfo, query, and
  fragment in `VAULT_ADDR` are rejected; mount/key are validated
  per §3.3; the final request URL is constructed by joining path
  segments, never by string-concatenating unvalidated operator
  input (no double-slash, traversal, or injection ambiguity).
- **Response size cap:** response bodies are read through a
  limiting reader with a cap of **64 KiB** before JSON decoding.
  A wrapped 32-byte DEK response is well under 1 KiB; 64 KiB is a
  generous bound that still prevents unbounded allocation from a
  malicious or broken Vault response. Oversized bodies are a
  `transit_bad_response` failure, and the body is never logged.
- **Request size:** request bodies are fixed-shape JSON for a
  32-byte DEK (wrap) or a bounded wrapped DEK (unwrap/rewrap,
  ≤ 4096 bytes); the client must not accept unbounded wrapped
  input from the config into a request without the §3.4 check
  first.
- **Logging:** the Transit client logs nothing at any level that
  contains a request body, response body, token, DEK, wrapped DEK,
  or ciphertext. Diagnostics carry field identifier, envelope
  version, Transit key version integer, and error class only.

### 8.3 Timeouts and retries (normative values)

These are decisions, not aspirations; the implementation PR must
use these values (or amend this ADR with justification):

- Per-request timeout: **5 seconds** (context timeout per Vault
  call). Typical staging/local Vault latency is well under 100 ms;
  5 s is a generous bound that still prevents a Vault outage from
  turning a startup into a hang multiplied by the field count.
- Overall operation timeouts: load/verify/save **30 seconds**;
  migrate/rewrap **120 seconds** (explicit operator operations).
  Exceeding the overall budget fails the whole operation closed,
  with no partial file change (for writes) or no start (for load).
- Retries: at most **2 retries**, only for HTTP 429 and 503, with
  backoff 100 ms then 300 ms plus jitter, and only within the
  overall operation budget. No retry for 400, 403, 404, timeouts
  where a response may already have been processed, or malformed
  responses. Retrying `encrypt` on 429/503 is safe because Vault
  encrypt is stateless: a discarded DEK is never stored, so a
  retried encrypt simply mints a new DEK. Retrying `decrypt` or
  `rewrap` on 429/503 is safe because both are idempotent for the
  same input. These two sentences are the retry justification;
  any broader retry policy needs its own justification in an ADR
  amendment.
- Fields are decrypted sequentially (not in parallel) on load.
  Parallelism would complicate DEK wiping and error attribution
  for a latency win the sequential design does not need at
  typical field counts; a future parallel design must address
  both in an amendment.

### 8.4 Fail-closed matrix (normative)

Error classes are the contract: gateway code, CLI output, and
tests must use these classes, and must never surface the raw
Vault response body, token, DEK, or wrapped DEK in an error string.

| Failure | Error class | Gateway behaviour |
|---|---|---|
| Vault unreachable, DNS failure, TLS failure, per-request timeout, overall timeout, 5xx (after §8.3 retries), 429 (after retries) | `transit_unavailable` | Startup, verify, decrypt, load, save, migrate, rewrap all fail. No fallback to local/env/file key, no partial config, no start with undecrypted fields. |
| HTTP 403 | `transit_forbidden` | Fail closed, no retry, no fallback. Message names the operation (encrypt/decrypt/rewrap) and advises a policy check. No policy, token, or Vault body in the message. |
| HTTP 404 | `transit_key_not_found` | Fail closed. Message advises a mount/key-reference check (against the configured reference, §3.3). |
| HTTP 400, malformed ciphertext, wrong key version, version disabled/destroyed, `min_decryption_version` exceeded | `transit_invalid_ciphertext` / `transit_key_version_unavailable` (version cases use the latter) | Fail closed. Never try another key, version, or provider. Version cases carry the §7.3 checklist message. |
| Malformed / oversized / unexpected Vault response (wrong JSON shape, missing `data.ciphertext` / `data.plaintext`, wrong plaintext length, version-prefix mismatch) | `transit_bad_response` | Fail closed. Body discarded, never logged. |
| Redirect (3xx) | `transit_bad_response` | Fail closed, redirect not followed, token never forwarded (§8.2). |
| Non-HTTPS Vault URL (non-loopback), userinfo/query/fragment in URL, invalid mount/key/namespace, missing token, both token env and token file set | `transit_config_invalid` | Rejected at configuration validation, before any request or file write. |
| One field fails during a multi-field load/save/verify | (the field's class, propagated) | Whole operation fails. No partially decrypted config, no partial file write. Matches current `TransformConfig` first-error-aborts behaviour, preserved for Transit. |
| Transit outage during a running gateway | `transit_unavailable` (for new operations) | Already-loaded config keeps serving; new load/save/verify/migrate/rewrap fail closed (§7.5). |
| Rewrap failure on any field | (the field's class, propagated) | No file change; original config and pre-rewrap backup intact (§7.3). |
| Feature gate off (§10.1) | `transit_not_enabled` | Fail with the gate message, not a decrypt error (so the operator does not troubleshoot Vault for a disabled feature). |
| Conflicting key configuration (§10.2) | `transit_config_invalid` | Fail at startup validation, before any Vault call or file write. |

## 9. Vault operator checklist (normative for acceptance, not just guidance)

The gateway cannot verify these from inside its policy scope, so
they are operator checklist items that the acceptance report (§11)
must record as checked, not assumed:

- **Key creation:** Transit mount enabled; key type
  `aes256-gcm96`; `derived=false`;
  `convergent_encryption=false`; `exportable=false`;
  `allow_plaintext_backup=false`; `deletion_allowed=false`
  (until rewrap is proven, §7.3); `min_decryption_version=0`
  (default; do not raise until rewrap is proven). Key creation
  and rotation are operator/Vault operations, never gateway
  operations.
- **Policy (least privilege, exactly one mount/key):**
  capabilities `update` on `transit/encrypt/<key>`,
  `transit/decrypt/<key>`, and `transit/rewrap/<key>` — and no
  other capabilities: no KV read/write, no `sys/*`, no key
  `read`, no other mount or key. A root token is forbidden for
  gateway use. If a future need for a `read` capability arises
  (for example, to display the latest key version), it needs an
  ADR amendment justifying the expanded scope; it must not be
  added silently in an implementation PR.
- **Token:** short TTL, scoped to the single key above, delivered
  by secret injection (env or file, §10.2), rotated by the
  operator's normal Vault token workflow, revoked after any
  incident and after acceptance runs. Long-uptime token renewal
  behaviour must be tested in the acceptance run: the gateway
  reads the token at startup and uses it for every Transit call,
  so an expired token fails closed on the next load/save/verify
  — which is the correct behaviour, but operators must know it
  is not silent renewal.
- **Audit logs:** before production use, verify the Vault audit
  device configuration for Transit endpoints: confirm whether
  Transit request/response plaintext (DEK base64) is retained,
  hashed, or excluded, and protect those logs with the same
  controls as secret material if they retain DEK values. Record
  the verification in the acceptance report. The gateway cannot
  control this; the ADR must not imply it does.
- **Availability:** Vault is a hard dependency for load, save,
  verify, migrate, and rewrap of `nxs2` configs (§8.4). Operators
  must plan Vault HA, backup, and seal/unseal procedures
  accordingly; a single non-HA Vault is a single point of failure
  for gateway startup, and the runbook must say so.

## 10. Selection, precedence, and feature gate (normative)

### 10.1 Feature gate

Transit is gated by an explicit opt-in that defaults to off, and
the gate is checked at every entry point listed in §6 — not only
at provider construction, where a bypass through another path
would go unnoticed:

- Gate variable: `NEXAROUTE_TRANSIT_ENABLED`. Value must be
  exactly `"true"` to enable; `"false"` or unset disables; any
  other value is a configuration error (`transit_config_invalid`).
- Gate off + `nxs2` config → `transit_not_enabled` failure (§8.4),
  at load, save, CLI, backup, restore, doctor, and Admin save.
- Gate on + raw config → raw behaviour unchanged; the gate does
  not force migration.
- The gate check itself is tested: gate off with an `nxs2`
  fixture must fail with the gate message and must make no Vault
  call.

### 10.2 Selection and precedence

Selection variables (all required in Transit mode unless marked
optional):

| Variable | Meaning |
|---|---|
| `NEXAROUTE_KEY_PROVIDER` | Must be exactly `transit` to select Transit mode. |
| `NEXAROUTE_TRANSIT_ENABLED` | Gate (§10.1). |
| `VAULT_ADDR` | Vault endpoint (§8.2 TLS rules). |
| `VAULT_TOKEN` **or** `VAULT_TOKEN_FILE` | Exactly one must be set. `VAULT_TOKEN_FILE` points to a file with mode exactly `0600` whose content is the token with only a trailing newline trimmed; other whitespace is rejected. Token via env or file only, never via argv, never via config file, never via a backup bundle. |
| `NEXAROUTE_TRANSIT_MOUNT` | Transit mount (§3.3). |
| `NEXAROUTE_TRANSIT_KEY` | Transit key name (§3.3). |
| `VAULT_NAMESPACE` | Optional namespace (§3.3, header §8.1). |

Conflict rules — all fail closed at startup validation
(`transit_config_invalid`), before any Vault call or file write:

- Provider `transit` but gate not `"true"` → gate error.
- Gate `"true"` but provider not `transit` → configuration error
  (a lone gate flag must not silently do nothing, nor silently
  enable anything).
- Provider `transit` plus any of `NEXAROUTE_MASTER_KEY`,
  `NEXAROUTE_MASTER_KEY_FILE`, `NEXAROUTE_KEY_PROVIDER_ENV`,
  `NEXAROUTE_KEY_PROVIDER_COMMAND`, or `NEXAROUTE_KEY_PROVIDER_REF`
  set → error. There is no precedence winner between a raw key and
  Transit; a conflict is an error, not a silent choice.
- Both `VAULT_TOKEN` and `VAULT_TOKEN_FILE` set → error.
- Missing mount, key, address, or token source in Transit mode →
  error before any request.

## 11. Alternatives rejected

1. **Keep using the existing command provider with a Transit helper
   that returns a raw key.** Rejected: it does not provide Transit
   envelope semantics; the KEK still leaves Vault. This is the KV
   helper's shape, and it is the wrong shape for Transit (§2).
2. **Encrypt the complete config blob with Transit for every read.**
   Rejected: it changes config availability and request behaviour,
   makes a single remote Transit call gate all encrypted config
   fields, and does not reuse the existing per-secret DEK/AAD
   boundary. Per-field envelopes also preserve the existing
   field-level reporting (status/verify per field).
3. **Silently reinterpret `nxs1` as Transit, or silently convert
   `nxs1` on load.** Rejected: `nxs1` requires the raw KEK and has
   a different envelope format and a different AAD construction.
   There is no safe implicit conversion; migration is explicit (§7.2).
4. **Remove all raw-key providers.** Rejected: local, environment,
   keyring, and KV command modes remain valid documented deployment
   choices. Transit is an additional mode, not a replacement.
5. **Include wrapped-DEK bytes or Transit key version in the data
   AAD.** Rejected for rewrap compatibility (§5.2): rewrap changes
   both values and must not invalidate data ciphertext. Security
   against transplant comes from the data GCM under the unwrapped
   DEK plus the §5.1 AAD, not from covering the wrapped bytes.
6. **Use Transit `context` as the binding for a non-derived key.**
   Rejected (§5.3): Vault ignores `context` for non-derived keys,
   so it would be false assurance. A derived-key mode needs its
   own ADR amendment.
7. **Implement rewrap as unwrap + wrap.** Rejected (§7.3): it
   exposes every DEK to the gateway during rewrap for no reason,
   when Vault `rewrap` exists precisely to avoid that.
8. **Migrate or rewrap field-by-field, or auto-migrate on load.**
   Rejected (§7.1, §7.5): field-by-field writes create mixed
   configs on crash; auto-migration turns a read path into
   implicit network writes with fresh DEKs.
9. **Cache DEKs across loads to reduce Vault calls.** Rejected
   (§5.4): it trades a small latency win for a larger in-memory
   exposure window, and it makes DEK lifetime reasoning (wipe
   points, error paths) substantially harder.
10. **Reuse the `.enc.bak` `nxs1` seal format for Transit backups.**
    Rejected (§7.2, §7.6): there is no raw KEK to seal with in
    Transit mode, and a Transit-sealed backup would depend on
    Transit availability and would not be a rollback.

## 12. Required implementation and acceptance gates

No implementation PR may be merged until **all** of the following
are satisfied, and the ADR itself is accepted by the maintainer in
writing (a draft or open ADR is not acceptance; a green CI run on
a docs-only PR is not a security review):

1. **Interface and classifier first.** `EnvelopeKeyProvider` (§4)
   and the §6 classifier land with tests, before any Transit
   HTTP client code, and the raw `TransformConfig` /
   `secrets.Key()` call-site audit (§6 rule 4) is in the PR
   description.
2. **Envelope tests (§3, §5, §6).** Round-trip for every
   secret-bearing field type; tamper of each envelope component
   independently; AAD swap (provider, field, credential index,
   header name); wrapped-DEK swap between envelopes; unknown
   version, malformed base64, non-strict base64, unknown JSON
   field, duplicate JSON key, oversized envelope, wrong nonce
   length, empty wrapped DEK, empty data ciphertext; `nxs1`
   byte-for-byte golden compatibility; mixed-format refusal at
   every entry point (§6); gate-off refusal with no Vault call;
   conflicting-configuration refusal before any Vault call;
   no `<config>.key` creation/read/write in Transit mode;
   `secrets rotate` refusal in Transit mode.
3. **Fake Transit API contract tests (§8).** An `httptest`
   fake implementing the §8.1 contract, pinning: URL construction
   (mount/key joining, no traversal), `X-Vault-Token` header only,
   request/response JSON shapes, timeout (slow fake), body cap
   (oversized fake), redirect rejection with token-non-forwarding
   proof (second server records headers), HTTP error mapping for
   400/403/404/429/500/503 to §8.4 classes, malformed-response
   handling, and canary absence (fake body/token/DEK/wrapped-DEK
   canaries appear in no gateway error, log, or report). Fake-API
   tests prove the gateway's client contract; they do **not**
   prove Vault compatibility.
4. **Negative, version, rotation, outage, and recovery tests.**
   Wrong key (key-ref mismatch), wrong/disabled/destroyed key
   version, denied policy on decrypt only and on encrypt only,
   missing/empty token, malformed Vault URL, Vault down at
   startup / mid-load / during save / during migrate / during
   rewrap, token revoked mid-operation, Vault key rotation
   (old ciphertext still decrypts, new encryptions use the new
   version), rewrap happy path / already-latest no-op / failure
   on field k (file unchanged, re-run succeeds) / field that no
   longer decrypts (abort before any rewrap call), rewrap
   endpoint assertion (rewrap called, decrypt/encrypt not called
   for the rewrap itself), and every migration crash state in
   §7.2 (via test seams that fail a chosen step, following the
   existing restore-journal test pattern), plus concurrent
   migration/rewrap lock behaviour.
5. **Leak / canary tests.** Canary secret, DEK, token, and wrapped
   DEK asserted absent from logs, CLI output (except the explicit
   `secrets decrypt` path, tested separately with its own flags),
   doctor output, backup manifests, restore reports, migration
   reports, Admin read responses, and error strings — for success,
   failure, timeout, and malformed-response cases. `secrets status`
   per-version counts and doctor/privacy-audit encrypted (not
   plaintext) reporting for `nxs2`.
6. **Fuzz / parser hardening.** Fuzz the `nxs2` parser and the
   Vault response parser (malformed base64, truncated JSON, huge
   lengths, duplicate keys, unknown fields): no panic, no
   acceptance, bounded memory. Fuzz envelope classification
   (random `nxs*` prefixes): never classified as plaintext.
7. **Documentation in the same PR that ungates Transit** (not a
   follow-up): SECURITY.md, `docs/SECURITY.md`,
   `docs/EXTERNAL_VERIFICATION.md`, CONFIGURATION.md,
   OPERATIONS.md, and KNOWN_GAPS.md, keeping the NOT IMPLEMENTED /
   UNVERIFIED wording until gate 8 passes.
8. **Real Vault Transit acceptance, separately authorised, staging
   only.** Disposable staging Vault, disposable key and token,
   report recording: Vault version, mount/key type and parameters
   (§9), policy capabilities granted (names only, never tokens),
   token type/TTL, encrypt/decrypt round-trip via `secrets verify`,
   denied-policy failure, key rotation then decrypt of old
   ciphertext, rewrap then decrypt, `secrets verify` after each
   step, sealed/outage fail-closed behaviour, token revocation
   after the run, and the audit-log check (§9) recorded explicitly.
   A historical-version test (raise `min_decryption_version` past
   an un-rewrapped version) runs on a separate disposable key only.
   Until this report exists, no production, HA, HSM, or other-KMS
   claim may be made, and the docs keep saying NOT IMPLEMENTED /
   UNVERIFIED for those.
9. **No silent fallback, anywhere.** Any test, code path, or doc
   that falls back to a local or environment key when Transit
   fails is a defect, not a convenience. This rule is tested by
   the outage tests in gate 4, not merely stated.

## 13. Risks and guardrails (summary)

- Transit outage blocks decrypt of any affected configuration;
  startup and CLI operations must fail closed and must not fall
  back to a local key (§8.4).
- Vault policy must grant only encrypt/decrypt/rewrap on the one
  configured Transit key (§9). Keep Vault URL and token in
  environment/secret injection, never in durable config, backups,
  or error output.
- Rewrap and migration must use same-directory temporary files,
  mode `0600`, sync, atomic rename, and a pre-operation backup
  before replacement (§7.1).
- Plaintext secret values and DEKs must never appear in logs,
  metrics, argv, reports, or committed fixtures. Tests use canary
  values and verify their absence at each surface (§5.4, gate 5).
- The feature must remain marked **NOT IMPLEMENTED** until all
  code, CI, and at least one real Vault Transit acceptance run pass
  (gate 8).

## 14. Owner decisions required

Acceptance of this ADR requires the maintainer's explicit written
decision on each of the following. Silence, a draft PR, or a green
CI run is not acceptance for any of them:

1. **Accept or reject this revised ADR as the basis for
   implementation**, including the normative values in §8.3
   (5 s per-request, 30 s load/save, 120 s migrate/rewrap, at most
   2 retries on 429/503 only).
2. **Approve the selection surface** (§10.2): env var names,
   `NEXAROUTE_KEY_PROVIDER=transit` plus `NEXAROUTE_TRANSIT_ENABLED`
   gate, and the conflict-is-error rules — or rename them in the
   acceptance decision.
3. **Approve the CLI surface** (§7): `secrets migrate-to-transit`
   and `secrets rewrap-transit`, each with `--dry-run`, plus
   `secrets rotate` refusal in Transit mode — or rename them in
   the acceptance decision.
4. **Confirm the "what Transit does not do" statement** (§1.2,
   §12 gate 7) for SECURITY.md and operator docs, in particular
   that DEKs are returned to the gateway and that a leaked token
   is effectively a master key.
5. **Authorise (separately, later) the real Vault acceptance run**
   in §12 gate 8, on a staging Vault with disposable credentials.
   Acceptance of this ADR alone does not authorise that run, and
   does not authorise any implementation PR merge.
