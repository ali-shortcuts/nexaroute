# Vault Transit `nxs2` implementation verification

- Date: 2026-10-27
- Branch: `feat/vault-transit-envelope-nxs2`
- ADR: [0007](../adr/0007-vault-transit-envelope-encryption.md)
- Real Vault Transit acceptance: **NOT RUN** (no Vault instance or
  operator-authorized credentials in this environment)

This report records local verification only. It does not establish real
Vault Transit compatibility, HSM/KMS isolation, production readiness, HA
failover behavior, or token-renewal behavior.

## What was implemented

- Separate `EnvelopeKeyProvider` contract (`KeyID`, `WrapDEK`, `UnwrapDEK`,
  `RewrapDEK`) that never returns the Transit KEK.
- `nxs2` envelope: base64url-encoded JSON metadata containing envelope
  version, full SHA-256 of the Transit mount/key reference, local nonce,
  Transit-wrapped DEK, and local AES-256-GCM ciphertext.
- Vault Transit HTTP provider with strict configuration validation,
  bounded timeout (default 5000 ms, max 60000 ms), response-size cap,
  redirect rejection, token header only, and redacted errors.
- Explicit opt-in via `NEXAROUTE_KEY_PROVIDER=vault-transit`; no local
  file/env/raw-provider fallback.
- Explicit `nexaroute secrets migrate-envelope` with legacy-key access,
  authenticated encrypted backup, non-secret migration journal, atomic
  replacement, and interrupted-migration recovery.
- Explicit `nexaroute secrets rewrap` with authenticated backup and atomic
  replacement.
- Existing `nxs1` raw-key providers remain backward-compatible.

## Deterministic test coverage

`internal/secrets/envelope_test.go`:

- `TestEnvelopeRoundTripAndMetadata`
- `TestEnvelopeAADBindingAndTamper`
- `TestEnvelopeWrongKeyReference`
- `TestTransformConfigWithEnvelopeAndLegacyCompatibility`
- `TestMigrateConfigBytes`
- `TestMigrateToEnvelopeFileAndInterruptedRecovery`
- `TestRewrapConfigBytesAndRevokedVersion`
- `TestConfiguredEnvelopeFailsClosedWithoutFallback`
- `TestNXS2WithoutTransitDoesNotCreateFallbackKey`

`internal/secrets/vault_transit_test.go`:

- `TestVaultTransitContractAndRoundTrip` (URL, token header, AAD, rewrap)
- `TestVaultTransitKeyIDIsStableAndFullHash`
- `TestVaultTransitFailuresAreRedactedAndBounded` (HTTP error, timeout,
  body cap, redirect rejection, malformed response)
- `TestVaultTransitConfigurationValidation`
- `TestConfiguredEnvelopeTimeoutValidation`

`internal/config/envelope_integration_test.go`:

- `TestConfigLoadSaveWithVaultTransitEnvelope` (real config load/save path,
  no local key file, `nxs1` rejection, explicit migration)
- `TestConfigTransitMissingProviderFailsClosed`

`cmd/gateway/secrets_transit_test.go`:

- `TestSecretsMigrateAndRewrapCommands`
- `TestSecretsMigrateRequiresProviderAndLegacyKey`

## Exact commands and outcomes

```text
$ go test -timeout=5m -count=1 ./...
... all packages ok ...

$ go test -race -timeout=5m -count=1 ./internal/secrets/ ./internal/config/ ./internal/privacyaudit/
ok  github.com/ali-shortcuts/nexaroute/internal/secrets
ok  github.com/ali-shortcuts/nexaroute/internal/config
ok  github.com/ali-shortcuts/nexaroute/internal/privacyaudit

$ ./scripts/verify.sh
== go version ==
go version go1.25.0 linux/amd64
...
== mandatory clean unit/integration pass ==
... all packages ok ...
== go vet ==
== mandatory clean race pass ==
... all packages ok ...
== real browser control-plane acceptance ==
WARN: Chromium + Python Playwright unavailable; skipping browser acceptance
== short fuzz checks ==
PASS
PASS
== linux amd64 build ==
== linux arm64 build ==
VERIFY PASS
```

The browser acceptance scripts were skipped because Chromium and Python
Playwright are not installed in this environment. That is unrelated to the
secrets changes but is recorded so the evidence is not overstated.

## Remaining acceptance gate

The procedure in
[`docs/EXTERNAL_VERIFICATION.md`](../EXTERNAL_VERIFICATION.md#vault-transit-nxs2-acceptance-procedure--not_run)
must be executed against a disposable real Vault Transit instance with
operator-authorized credentials before any production/HSM claim is made.
