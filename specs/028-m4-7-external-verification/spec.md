# M4-7 — Real IdP, Vault Transit and headless keyring verification

## Audit (2026-10-15)

- OIDC: full implementation (discovery pinned to a fixed trusted
  issuer, PKCE, JWKS verification via go-oidc) with 7 tests against
  an in-process IdP. The classic real-IdP failure mode NOT covered:
  signing-key rotation (JWKS rollover) mid-operation.
- Key custody: KeyProvider contract returns raw 32-byte master-key
  material. Providers: file (default), env, command (operator helper
  over a stdin protocol), keyring. There is NO native Vault client:
  Vault integrates through the command provider. Vault Transit in
  envelope mode (key material never leaves Vault) is architecturally
  incompatible with the raw-key provider contract and is NOT
  implemented — the supported Vault pattern is KV v2 + a helper.
  No reference helper ships today and no doc admits this boundary.
- Headless keyring: the linux provider shells out to `secret-tool`
  (libsecret). On headless hosts (no D-Bus session/keyring daemon)
  every call fails; the failure mode (fail-closed, no fallback) has
  no test.

## This PR (local scope)

- OIDC JWKS rotation test: rollover (both keys published, tokens
  signed by the new key) and completed rotation (old key unpublished;
  tokens signed by the retired key are rejected).
- Keyring provider tests with a hermetic `secret-tool` stub on PATH:
  store/load round trip, missing entry fails closed, garbage value
  rejected — the headless behavior contract.
- Reference Vault KV command helper
  (scripts/key-provider-vault-kv.sh) implementing the repo's provider
  protocol over Vault's HTTP API, tested against a fake Vault
  (request shape, token header, fail-closed errors). Docs state the
  Transit boundary exactly.
- docs/EXTERNAL_VERIFICATION.md: the operator runbook for verifying
  against a REAL IdP / Vault / headless keyring, with evidence
  criteria — the exact claims that stay UNVERIFIED until it is run.

## Boundary

Live external verification (real IdP tenant, real Vault cluster,
real OS keyring daemon) remains UNVERIFIED. M4-7 stays IN PROGRESS.
