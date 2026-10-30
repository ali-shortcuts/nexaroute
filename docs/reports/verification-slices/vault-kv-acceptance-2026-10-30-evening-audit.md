# Vault KV acceptance — 2026-10-30 evening audit (code audit only, no live Vault)

**Status: staging acceptance remains BLOCKED. #648 stays OPEN.
This audit ran zero non-loopback Vault calls and makes no staging
claim. It verifies the merged harness state on main and closes one
remaining gateway-path gap.**

## Scope and authorization gate

- Base: main `7a2623a` (PR #778 merge; includes #780 merge
  `ad0c665`).
- Date: 2026-10-30 (evening +0430).
- Authorization gate (names/counts only, no values read or
  printed): no written operator authorization, instance pointer,
  mount/path, least-privilege policy, or approved token injection
  exists in the task, in #648 comments, or in the environment
  (VAULT_ADDR unset; no Vault binary on this box). A shell
  environment variable for a token name was observed set, but a
  stray value without an address, a mount, a policy, or written
  authorization is not an authorization and was not used, read,
  or passed to any Vault call. **Zero staging Vault calls were
  made by this audit.**
- Transit (#649) is separate throughout: nothing here claims
  Transit acceptance, and a KV result never proves Transit.

## What was verified first-hand on main (code, not re-executed live)

| Directive item | Status on main `7a2623a` | Evidence |
|---|---|---|
| KV v2 metadata/data path handling | FIXED (merged #780) | `vault.mount_version` pre-write check (`sys/mounts/<mount>`, mount = everything before `/data/`, nested mounts correct); post-round-trip `vault.kv_version` metadata probe; helper + accept ref validation (rejects missing `/data/`, traversal, whitespace, query, fragment) |
| Least-privilege policy assertion | FIXED (merged #780) | `--expect-policy` + `vault.policy_assertion` before preflight/write; missing expected policy FAILS; without the flag the report records manual review required |
| Refusal of unsafe/root credentials | FIXED (merged #741) | Root policy FAILS at `vault.token_metadata` before any write; BLOCKED metadata result now stops before preflight/put (merged #780; previously it fell through and wrote) |
| Helper timeout propagation | FIXED (merged #741) | Explicit `--timeout` wins (1 s floor via ceil); else exported `VAULT_HELPER_TIMEOUT`; else 30 s script default; invalid exported value is a usage error; helper bounds every request (`--connect-timeout 10`, `--max-time`) |
| Response-size limits | FIXED in harness (merged); GAP in gateway path, FIXED by this change | Harness: 1 MiB cap on Vault responses (`vaultHTTP`); helper: `--max-filesize 1048576`. Gateway path (`internal/secrets` `CommandProvider`) read helper stdout unbounded — see below |
| Redirect handling | FIXED (merged #750 + harness) | Helper: no `--location`, explicit 2xx-only status check (a 307 fails); accept client: `http.ErrUseLastResponse`, 3xx surfaces as failed check; token never forwarded to a redirect target |
| Token expiry / revocation, fail-closed | FIXED (merged #780 + earlier) | `vault.expired_token` (2 s child polled to expiry; create failure BLOCKED; still-valid past 10 s FAIL); `vault.revoked_token` (60 s child revoked, then fails closed); `vault.wrong_token`; `vault.outage_fail_closed` (unreachable endpoint); gateway boot with wrong token / Vault down exits 1 (loopback report) |
| Write/read round trip, rotation, restart (gateway path) | Verified on loopback only (merged #780 report) | 33/33 checks: helper put/get byte-identical, rotation read-back, gateway boot `/healthz` 200 with Vault-held key, wrong-token/vault-down exit 1, old config fails after rotation, re-encrypt + boot, restart key re-read — all `docs/reports/verification-slices/vault-kv-acceptance-2026-10-30.md`, evidence_class=loopback |
| Redaction | PASS (code) | Reports are 0600 + atomic rename; token env value redacted from details/reasons; addresses sanitized (scheme + host + path, no userinfo/query/fragment); helper prints only the key on stdout, diagnostics to stderr, no token/key bytes in logs |
| Gateway startup/decrypt verification | Verified on loopback only | `vault.gateway_decrypt` (nxs1 envelope encrypt/decrypt with the Vault-held key + wrong-key fail-closed) in the harness; full gateway boot path in the loopback report above |

## The one remaining gap found by this audit (fixed in this change)

**Gateway helper-output size bound (new).** `runProviderCommand`
used `cmd.Output()`, which buffers a helper's entire stdout before
the 32-byte key check rejects it. The shipped Vault helper caps
its own Vault response at 1 MiB, so the exposure through that
helper was ~1 MiB — but the gateway path itself had no bound for
an arbitrary command provider, unlike the harness (1 MiB) and the
helper (1 MiB). Fix: `internal/secrets/key_provider.go` now
reads helper stdout through a 1 MiB + 1 limit, kills a flooding
helper instead of waiting on an undrained pipe, and fails closed
with a size-bound error; stderr is capped at 64 KiB (content
never enters returned errors). The bound applies to get and put.

- RED proof (before the fix): new test
  `TestCommandProviderRejectsOversizedOutput` FAILED with
  "key provider returned an invalid 32-byte key" — output was
  buffered, then rejected on length, with no size error.
- GREEN proof (after the fix): the same test PASSES
  (1 MiB + 1 byte → size-bound error; exactly 1 MiB → no size
  error, invalid-key error instead, proving the boundary).
- No regression: existing `TestCommandProviderProtocolAndInvalidOutput`
  (valid key round trip, malformed output, short store key)
  still passes; `gofmt`/`go vet` clean; `internal/secrets`
  plain + `-race` PASS; `cmd/gateway` `TestVaultAccept*` PASS;
  gateway binary builds.

## What this audit does NOT prove (still BLOCKED / NOT_RUN)

- External staging (non-loopback, operator-owned, TLS, dedicated
  empty KV v2 mount/path, least-privilege token incl.
  `sys/mounts` read): **BLOCKED** — no authorization or instance
  supplied. Exact unblock package: written authorization +
  instance pointer (HTTPS address) + KV v2 mount + dedicated
  empty path + least-privilege token by name + token-injection
  method by name + `--expect-policy` names. Then run
  `nexaroute vault-accept --confirm-live` from this change (or
  main, once it lands) plus the gateway boot/rotation/restart
  path against that instance.
- HA failover, long token renewal, AppRole/Kubernetes auth:
  **NOT_RUN** (harness records `vault.ha_failover` NOT_RUN by
  design; auth method is static token only; gateway re-resolves
  on restart only and never renews tokens).
- Vault Transit envelope mode: **NOT_RUN** by this harness and
  not claimed by this KV result (see #649, separate).
- A loopback PASS is harness evidence only and must never be
  cited as production, staging, or HA acceptance.

## Files changed by this audit's PR

- `internal/secrets/key_provider.go` — gateway helper-output
  1 MiB bound + capped stderr (behavior tightening, fail-closed).
- `internal/secrets/key_provider_test.go` — red/green bound test.
- `docs/EXTERNAL_VERIFICATION.md` — #648 register row refreshed
  (#741/#780 merged state; verdict still BLOCKED/NOT_RUN).
- This file — the audit record.

No token, key byte, DSN, ciphertext, or customer secret appears
in this report, in the PR, or in any #648 comment from this
audit. Secret names only.
