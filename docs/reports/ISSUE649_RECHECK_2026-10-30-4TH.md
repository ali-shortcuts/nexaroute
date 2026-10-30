# Issue #649 — fourth re-check (2026-10-30, evening): still BLOCKED, no new code gap

Status: **live Vault Transit acceptance remains BLOCKED / NOT_RUN.**
The `nxs2` implementation is on `main`; this record does not claim
live interoperability, production readiness, or HSM/KMS isolation,
and issue #649 stays OPEN. What changed in this attempt is narrow:
two earlier open items are now closed on `main`, and three stale
label sentences in ADR 0007 were corrected. No production code and
no test was changed, because no demonstrated fake-API gap remains.

Base: main `7a2623a9252d14943ebad64b8945bbebafd4eef2` (PR #778 merge).
Prior merges verified on this base: PR #775 (Transit per-request
timeout/cancel test + third BLOCKED record, merged 2026-10-30, all
13 checks pass on its head) and PR #769 (Transit key-version and
recovery coverage, merged before #772). Transit code, envelope,
migration, CLI, SECURITY.md, EXTERNAL_VERIFICATION.md, and ADR 0007
are unchanged between the #775 merge and this base (empty diff on
those paths), so the third re-check's audit stands, except for the
two corrections below.

## Correction 1: the key-version class drift is closed

The third re-check recorded as open drift that
`transit_key_version_unavailable` was defined but never emitted.
That is no longer true on this base. PR #769's commit `3d35849c`
added `transitVersionUnavailable`, which classifies an HTTP 400
body by a small set of version phrases ("version is too new",
"no such key version", "encryption key version N is not
available for decryption", "key version N has been deleted",
"key has been deleted", "key is disabled") as
`transit_key_version_unavailable` for both decrypt and rewrap,
while the test asserts the Vault body phrases and the token never
leak into the error. A tampered ciphertext
("ciphertext or signature is not valid") still maps to
`transit_invalid_ciphertext`, not to the version class. The ADR
§8.4 table (version cases use the version class) now matches the
code. Whether real Vault emits exactly those phrases is still a
live-run observation, not a fixture claim.

## Correction 2: the #654 body staleness is already fixed

The EXTERNAL_VERIFICATION.md audit notes (lines ~510, ~561)
record that the #654 body "still reads #649 as NOT IMPLEMENTED".
Checked live in this attempt: the #654 body contains no
"NOT IMPLEMENTED" line at all, and its #649 line (line 19) now
reads that the `nxs2` implementation and ADR 0007 are on `main`
via PR #670 and that real Vault Transit acceptance remains
NOT_RUN/BLOCKED and must not be claimed. Those audit notes are
themselves superseded and were not re-edited (historical records
are left as written). Likewise, `docs/SECURITY.md` already marks
Transit "not production-verified" (fake-API CI only, no real
Vault claim), the EXTERNAL_VERIFICATION.md #649 row already
reads NOT_RUN + FIXTURE + code present, and OPERATIONS.md /
KNOWN_GAPS.md contain no #649 "NOT IMPLEMENTED" line. The only
stale label sentences found were in ADR 0007 itself, fixed by
this PR (below).

## What this PR changes (docs only)

Three sentences in `docs/adr/0007-vault-transit-envelope-encryption.md`
used the label NOT IMPLEMENTED for the feature's current marking:
§2 (twice, in one paragraph) and the §12 rollout item and the
§13 guardrail bullet. They now read "implemented but not
production-verified" / "live acceptance NOT_RUN", matching
`docs/SECURITY.md` and `docs/EXTERNAL_VERIFICATION.md`. Nothing
else in the ADR changed: the normative envelope, AAD, bounds,
fail-closed rules, and the §11 / gate 8 production gate (no
production, HA, HSM, or other-KMS claim until a real acceptance
run passes) are untouched, the ADR header already reads
"Accepted (architecture decision; implementation merged via
PR #670). Live acceptance remains NOT_RUN", and no ADR status
line was adjudicated or rewritten.

## Fake-API coverage re-verified (no new gap)

Every task-named area was re-checked against the current tests,
not assumed from the prior audit: wrap/unwrap protocol and
version cross-checks, strict envelope/ciphertext parsing, 64 KiB
response bound, 5 s per-request timeout with no retry of timeouts
(#775 test), redirect refusal, 429/503-only bounded retries,
redacted errors (Vault bodies discarded, token header-only, no
Transit `context` parameter), 401/403 → forbidden, 404 →
key_not_found, 400 version phrases → version class, other 400 →
invalid ciphertext, revoked/disabled versions fail closed on
decrypt and rewrap, gate-off zero Vault calls, no local/env
fallback (conflicts are errors), migration rollback restoring
original bytes, rewrap failure leaving the file byte-identical,
already-latest rewrap writing nothing, and the migrate/rewrap/
backup/restore CLI paths. No area lacked a test on this base, so
no test was added.

## DEK/KEK boundary (as implemented, not extended)

The provider calls only the Transit `encrypt`, `decrypt`, and
`rewrap` endpoints. The Transit KEK never enters the gateway
process; only the short-lived DEK does, and it is wiped after
use. No HSM/KMS isolation is claimed: that would depend on the
selected Vault topology and key backend, neither of which exists
for this gate yet.

## Authorization gate (counts only, no values read)

- Issue #649 comments re-read: the owner's 2026-10-30 reopen
  comment confirms live acceptance is still undone; it names no
  instance, key, policy, or injection method and authorizes
  nothing. The pasted agent-prompt comments name no resource
  either and are not authorizations.
- Transit env names set: **0 / 8** (VAULT_ADDR, VAULT_TOKEN,
  VAULT_TOKEN_FILE, NEXAROUTE_KEY_PROVIDER,
  NEXAROUTE_TRANSIT_ENABLED, NEXAROUTE_TRANSIT_MOUNT,
  NEXAROUTE_TRANSIT_KEY, VAULT_NAMESPACE). No `vault` binary,
  no Vault process on this machine.
- Consequence: **zero live Vault calls** in this attempt, and no
  local Vault dev server was started as acceptance (that would
  not be operator authorization). A self-started dev server
  remains, at most, a local rehearsal, never acceptance evidence.

## Live matrix (this attempt — all BLOCKED / NOT_RUN)

Envelope creation, gateway startup/decrypt, `secrets verify`,
wrap/unwrap round trip, denied/wrong/revoked credentials
failing closed, unavailable Vault failing closed, ciphertext
tampering rejection, key rotation, `rewrap-transit`, post-rewrap
decrypt, rollback/recovery, and old-key retirement: **NOT_RUN**,
all blocked on the same missing input — written operator
authorization naming a disposable Transit instance, a dedicated
test key, a least-privilege encrypt/decrypt/rewrap-only policy,
and a token-injection method by name, plus explicit approval
before any destructive key-version action. No Vault version,
topology, policy capability, token, key material, DEK, or
ciphertext is recorded here, because no live run exists to
record.

## Evidence (local, fake only — not live acceptance)

Go 1.27.0, base `7a2623a9`: `internal/secrets` PASS plain
(15.492 s) and `-race` (17.566 s); `cmd/gateway` Transit /
migrate focused tests PASS (1.043 s); `go vet` clean
(unrelated GOPATH/GOROOT warning only); `gofmt` clean. Full
`scripts/verify.sh` locally NOT run (docs-only change; PR CI is
the wider gate). No live binary run was performed against any
Vault.
