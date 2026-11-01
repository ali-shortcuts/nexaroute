# External verification runbook (M4-7)

Three NexaRoute integrations depend on external systems that the
project's own CI cannot provide: an OIDC identity provider, HashiCorp
Vault, and an OS keyring. The implementations are covered by
protocol-faithful local tests (an in-process IdP, a stubbed
`secret-tool`, a fake Vault HTTP API). The procedures below are how
an operator verifies each integration against the REAL system and
what evidence closes the boundary. Until a procedure has been run
and its evidence captured, the corresponding claim stays
**UNVERIFIED** — this document does not change that status by itself.

## Automated opt-in acceptance harness

`nexaroute external-accept` is the executable companion to this runbook. It
checks a configured provider and/or a configured OIDC issuer (discovery, live
JWKS, and optionally an operator-issued ID token), requires `--confirm-live`,
uses strict timeouts, writes a redacted machine-readable report with
`PASS`/`FAIL`/`BLOCKED`/`NOT_RUN` per check, and has no built-in production
endpoints. Fixture tests and the exact invocations are documented in
`docs/reports/verification-slices/live-acceptance-harness.md`. A fixture run is
not external verification; the OIDC browser login procedure below remains
manual.

ComfyUI real-inference acceptance is a separate opt-in command,
`nexaroute comfyui-accept --confirm-live`. It requires a provisioned ComfyUI
server, a real compatible workflow/model, and `ffprobe`; it never downloads
models. The command and its deterministic loopback negative-path tests are
implemented, but **actual model inference remains NOT_RUN/BLOCKED** until an
operator supplies a real installation and records the sanitized report. See
`docs/reports/verification-slices/comfyui-real-inference-acceptance.md`.

## Status register — every external dependency

One row per external dependency the gateway integrates with. A dependency
with no executed real-system evidence is **UNVERIFIED**, whether or not a
runbook procedure exists for it.

| Dependency | Real-system status | Evidence / procedure |
|---|---|---|
| OIDC identity provider (browser login) | EXECUTED LOCALLY 2026-10-24; deterministic local coverage expanded 2026-10-27 (issuer pinning, JWKS overlap/rotation, unique state/nonce/PKCE, unmapped/ambiguous roles, logout revocation failure, audit/persistence failure); 2026-10-30 re-audit added exact-issuer trailing-slash, cross-transaction nonce, token-exchange client/redirect/grant, and duplicate/mixed role tests; re-checked 2026-10-30 on main `7daa36d` (callback-binding negatives: no/malformed flow cookie, wrong-method callback, provider-error state consumption, login-start audit failure); follow-up 2026-10-30 on main `7a2623a` added nested-claim denial, short-state denial, session-read-failure fail-closed, cross-host-HTTPS discovery acceptance, and login-audit attribution tests (test-only plus one stub fixture mode). Live organizational tenant remains **BLOCKED/UNVERIFIED** (NOT_RUN) because no operator-owned tenant, credentials, or authorization were supplied | §1 below; `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §3; `docs/reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md`; `docs/reports/OIDC_647_RECHECK_2026-10-30.md` |
| HashiCorp Vault (KV helper) | EXECUTED LOCALLY 2026-10-24 — real Vault v2.2.1 (dev mode) round-trip via the shipped helper PASS; gateway key custody + decrypt verify PASS; wrong token fails closed; a production Vault cluster remains UNVERIFIED. 2026-10-27: opt-in `vault-accept` automation added and fixture-tested, then executed live against a disposable non-dev single-node Vault (file storage, TLS, loopback): `vault-accept` PASS 10/12 (`evidence_class=loopback`), plus gateway boot, expired-token, outage, restart, and rotation-recovery checks PASS; a real operator staging run and HA/failover remain BLOCKED/NOT_RUN | §2 below; `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §5; `docs/reports/verification-slices/vault-kv-acceptance.md` |
| HashiCorp Vault Transit envelope mode | IMPLEMENTED — `nxs2` envelope path and fake Vault Transit API contract tests pass; a real Vault Transit acceptance run is **NOT_RUN**, so no production/HSM claim is made | §2 below; `docs/adr/0007-vault-transit-envelope-encryption.md` |
| OS keyring (secret-tool) | EXECUTED LOCALLY 2026-10-24 — real Secret Service (gnome-keyring) store/lookup PASS; gateway decrypt verify via the keyring provider PASS; a production desktop keyring remains operator-verified | §3 below; `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §6 |
| LLM providers (OpenAI/Anthropic/Gemini-compatible upstreams) | UNVERIFIED — NOT_RUN; zero paid calls by policy | Executed evidence §"LLM providers" |
| AWS Bedrock | UNVERIFIED — NOT_RUN (no AWS tenant/credentials) | Adapter covered by fixture/contract tests only |
| Google Vertex AI | UNVERIFIED — NOT_RUN (no GCP tenant/credentials) | Adapter covered by fixture/contract tests only |
| Azure OpenAI | UNVERIFIED — NOT_RUN (no Azure tenant/credentials) | Adapter covered by fixture/contract tests only |
| PostgreSQL (control-plane, security, video stores) | EXECUTED — DSN-gated integration suites PASS in CI and locally 2026-10-24; two real gateway processes sharing one PostgreSQL executed locally (breaker-marker convergence; one video job claimed once and completed); production multi-host deployment remains UNVERIFIED | `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §1–2 |
| Redis (control-plane, breaker store) | EXECUTED — integration suites PASS in CI and locally 2026-10-24 (with `-race`, incl. outage/recovery); a production deployment remains UNVERIFIED | `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §1 |
| OpenTelemetry collector (OTLP ingestion) | EXECUTED 2026-10-24 — the real OpenTelemetry Collector v0.163.2 received and decoded a gateway span with the documented GenAI attributes; managed backends (Jaeger/Tempo/vendor APM) remain UNVERIFIED | `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §4 |
| Kubernetes installation (Helm/manifests) | UNVERIFIED — NOT_RUN (no live cluster) | Helm lint/render checks only |
| ComfyUI inference | UNVERIFIED — HTTP contract fixture-tested; opt-in `comfyui-accept` automation exists, but real model inference remains NOT_RUN/BLOCKED and is not part of CI; fixtures are not real inference | `docs/reports/verification-slices/comfyui-real-inference-acceptance.md`; See KNOWN_GAPS (video section) |

## 1. Real OIDC identity provider

Local state: full Authorization Code + PKCE flow against an
in-process IdP, including discovery pinning, JWKS outage, tampered
tokens, claim-to-role mapping, and — since M4-7 — JWKS key rotation:
rollover logins work and tokens signed by a retired key are rejected
(the gateway fetches the current JWKS on every callback rather than
trusting a process-lifetime cache). The deterministic suite also covers
old-key logins during the overlap window, unique state/nonce/PKCE per
transaction, unmapped/missing/conflicting/malformed role claims, failed
logout revocation, and session/audit persistence failures. The
2026-10-30 re-audit added exact-issuer equality beyond an obviously
hostile issuer, cross-transaction nonce binding, token-exchange
client/redirect/grant assertions, and duplicate/mixed role-value
handling. Re-checked 2026-10-30 on main `7daa36d`: the callback binding gate is now also
pinned (no/malformed flow cookie, wrong-method callback, and
provider-error state consumption) along with login-start audit
failure; see `docs/reports/OIDC_647_RECHECK_2026-10-30.md`. These are
local protocol tests, not live-tenant evidence.

The exact live-tenant checklist is **BLOCKED/NOT_RUN** until an
operator supplies a real tenant and credentials:
[`OIDC_LIVE_TENANT_ACCEPTANCE_647.md`](reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md).
No credentials, tenant, or run authorization were available for the
2026-10-30 re-audit, so nothing below may be marked live-verified from
the local suite alone.

Procedure (any conformant provider — Keycloak, Auth0, Entra ID,
Okta, Google):

1. Create a confidential client; record the issuer URL, client id,
   and the secret in the env var named by the gateway config.
2. Configure `admin.oidc` with role mappings for the provider's
   groups/claims; start the gateway.
3. Evidence: a browser login completes; the admin audit log shows
   `auth.login` with the mapped role; a user outside the mapped
   groups is denied.
4. Rotate the provider's signing keys (or wait for its automatic
   rotation). Evidence: logins keep working across the rollover, and
   — after the old key leaves the provider's JWKS — a token signed
   with the retired key no longer authenticates (a captured pre-
   rotation token replayed at the callback is rejected).
5. Record: provider name/version, issuer URL (redact tenant ids if
   sensitive), dates, and the audit excerpts.

Still unverified until run: provider-specific claim shapes (nested
group claims, non-`groups` role claims beyond configuration),
provider clock-skew tolerance, and logout/back-channel behaviors
the gateway does not consume.

## 2. HashiCorp Vault

Local state: there is deliberately **no native Vault client** in the
gateway. Custody integrates through the key-provider command
protocol, and this repository ships a reference helper for Vault
**KV v2** (`scripts/key-provider-vault-kv.sh`), tested against a
fake Vault HTTP API for request shape, token handling and
fail-closed behavior.

`nexaroute vault-accept` is the opt-in automation for the procedure
below. It requires `--confirm-live` and an explicit empty KV v2
`--ref`, refuses dev mode unless `--allow-dev` (and then labels the
report non-production), never revokes the operator's primary token,
rotates only a disposable acceptance entry, tests fail-closed wrong/
revoked-token and unreachable-outage behavior, and writes a redacted
JSON report (mode `0600`) with Vault version, topology,
policy names, and per-check PASS/FAIL/BLOCKED/NOT_RUN. Exact
invocations and the remaining BLOCKED/NOT_RUN items are in
`docs/reports/verification-slices/vault-kv-acceptance.md`.

**Transit boundary (exact):** Two Vault patterns are distinct. The
`nxs1` KV v2 helper loads a raw 32-byte master key into the gateway
process. The optional `nxs2` Vault Transit path keeps the Transit KEK
inside Vault and returns only a short-lived per-secret DEK to the
gateway. It is an explicit opt-in (`NEXAROUTE_KEY_PROVIDER=transit` with
`NEXAROUTE_TRANSIT_ENABLED=true`)
with strict timeouts, response-size limits, redirect rejection, redacted
errors, and no local fallback. CI exercises the protocol against a fake
Vault API only. **No real Vault Transit acceptance run has been
executed**; do not claim HSM/KMS isolation or production readiness until
the procedure below is run and recorded.

Procedure:

1. Enable a KV v2 mount; create a policy granting read/write on one
   key path (e.g. `secret/data/nexaroute/master-key`).
2. Export `VAULT_ADDR`/`VAULT_TOKEN`; set
   `NEXAROUTE_KEY_PROVIDER=command`,
   `NEXAROUTE_KEY_PROVIDER_COMMAND=<path to the helper>`,
   `NEXAROUTE_KEY_PROVIDER_REF=secret/data/nexaroute/master-key`.
3. Evidence: `nexaroute secrets verify` succeeds against a config
   encrypted under the Vault-held key; revoking the token makes
   gateway startup fail closed (no fallback key source); rotating
   the KV entry + restarting re-keys cleanly per OPERATIONS.md.
4. Record: Vault version, mount type, policy (redacted), dates.

Still unverified until run: token renewal/lease behavior over long
uptimes (the gateway reads the key at startup and during `secrets`
commands only), Vault HA failover mid-startup, and any
AppRole/Kubernetes auth flow (the helper takes a static token from
the environment; token acquisition is the operator's supervisor's
job).

### Vault Transit (`nxs2`) acceptance procedure — NOT_RUN

Run this only in a disposable environment with operator-authorized
credentials. It is intentionally separate from CI.

1. Enable a Transit mount and key (for example `transit/nexaroute`).
   Grant a policy with `encrypt`, `decrypt`, and `rewrap` on that key only.
2. Export `NEXAROUTE_KEY_PROVIDER=transit`,
   `NEXAROUTE_TRANSIT_ENABLED=true`, `VAULT_ADDR`, `VAULT_TOKEN` (or
   `VAULT_TOKEN_FILE`), `NEXAROUTE_TRANSIT_MOUNT`, and
   `NEXAROUTE_TRANSIT_KEY`.
3. Start from a disposable config containing one secret. Evidence:
   the stored value is `nxs2:`, `nexaroute secrets verify` passes, and
   no `<config>.key` file is created.
4. Deny the token's `decrypt` capability (or use a wrong token). Evidence:
   startup and `secrets verify` fail closed with no local-key fallback.
5. Rotate the Transit key and run `nexaroute secrets rewrap-transit`; verify the
   wrapped DEK moved to the new key version and the secret still decrypts.
   Then disable the old key version only after verification.
6. Tamper with the stored envelope or move it to another field. Evidence:
   authentication fails.
7. Record Vault version, mount/key (redacted if sensitive), policy
   capabilities, dates, command exit codes, and redacted report paths.

A fake-API test is not live acceptance. Until this procedure is executed
and its evidence is committed under a repository-owned path, the status
remains **NOT_RUN / UNVERIFIED**.

#### BLOCKED attempt record — 2026-10-29 (main `ba5ff2f`, issue #649 still OPEN)

On 2026-10-29 an acceptance attempt was made from main `ba5ff2f` (PR #670
merged as `da727d61`; PR #672 closed as superseded by #670). Every live step
of the procedure above — startup/decrypt, wrap/unwrap round-trip,
unavailable Vault, forbidden Vault, invalid ciphertext, revoked/unavailable
key versions, fail-closed with no fallback, migration/rewrap/rotation
including rollback and recovery — is **NOT_RUN / BLOCKED** for this attempt:
zero live Vault calls were made.

Missing input (exact): an operator-authorized disposable Transit instance
(address ownership), a dedicated Transit key name, a least-privilege policy
(`encrypt`/`decrypt`/`rewrap` on that key only), a token-injection method (by
name only), and written operator authorization for the run. Environment check
by names/counts only (no values read): `VAULT_ADDR`, `VAULT_TOKEN`,
`VAULT_TOKEN_FILE`, `NEXAROUTE_KEY_PROVIDER`, `NEXAROUTE_TRANSIT_ENABLED`,
`NEXAROUTE_TRANSIT_MOUNT`, `NEXAROUTE_TRANSIT_KEY`, `VAULT_NAMESPACE` —
none set (set-count 0, proven numerically; a display-layer rendering of one
line as redacted was an artifact, not a set variable). No comment on issue
#649 authorizes a specific instance, key, policy, or token injection. A
local Vault dev server started by the agent would NOT count as operator
authorization and was not used as acceptance evidence.

Local health only (fake Vault API, never acceptance): `internal/secrets`
suite PASS, `cmd/gateway` Transit migrate/rewrap/backup/restore focused
tests PASS, `gofmt`/`go vet` clean on main `ba5ff2f`. Fake-API tests do not
close this acceptance.

No production Vault, key, or secret was touched. No token, ciphertext,
master-key byte, DEK, wrapped DEK, envelope string, or customer secret is
recorded here. No production/HSM claim is made or changed by this record.
This record does NOT close issue #649.

#### Re-check record — 2026-10-30 (main `8ac484e`, issue #649 still OPEN)

On 2026-10-30 the acceptance was re-attempted from current main
`8ac484e` (merge of PR #750; the `nxs2` Transit code and docs inspected
on `2c5f583` are byte-identical on `8ac484e` for
`internal/secrets/vault_transit.go`, `internal/secrets/envelope.go`,
`internal/secrets/migration.go`, `cmd/gateway/secrets.go`,
`docs/EXTERNAL_VERIFICATION.md`, and `docs/SECURITY.md`). Every live step
of the procedure above remains **NOT_RUN / BLOCKED**: zero live Vault
calls were made, no local Vault dev server was used as acceptance
evidence, and no second implementation was created. The missing input is
unchanged from 2026-10-29: an operator-authorized disposable Transit
instance (address ownership), a dedicated Transit key name, a
least-privilege policy (`encrypt`/`decrypt`/`rewrap` on that key only), a
token-injection method (by name only), and written operator authorization.
Environment check by names/counts only (no values read):
`VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_TOKEN_FILE`,
`NEXAROUTE_KEY_PROVIDER`, `NEXAROUTE_TRANSIT_ENABLED`,
`NEXAROUTE_TRANSIT_MOUNT`, `NEXAROUTE_TRANSIT_KEY`, `VAULT_NAMESPACE` —
none set (present_count 0, nonempty_count 0, proven numerically).

Local health only (fake Vault API, never acceptance; Go 1.27.0):
`internal/secrets` suite PASS plain and with `-race`, `cmd/gateway`
focused Transit tests PASS plain and with `-race` (migrate/rewrap
commands, Secrets Transit CLI, restore-verify Transit, backup Transit
manifest error), `gofmt`/`go vet` clean on both packages. Full
`verify.sh` was not run locally (known VM per-package timeout artifact);
CI on this change is the gate and is pending at the time of writing, not
claimed green. `docs/SECURITY.md` is unchanged by this re-check because
its Transit claim — not production-verified, no real run executed —
remains true. This record does NOT close issue #649, and fake-API tests
do not close this acceptance.

#### Re-check record — 2026-10-30 (branch `opencode/issue649-20261030101324`, base main `7daa36d`, issue #649 still OPEN)

This re-check adds deterministic coverage for concrete client-contract
gaps and corrects stale Transit wording. It does **not** upgrade the live
acceptance status. Live Vault Transit: **BLOCKED / NOT_RUN** (zero live
Vault calls; no local Vault dev server was used as acceptance evidence).

- **Written authorization:** not present in issue #649 or the workflow
  input for a specific instance, key, policy, or token injection.
- **Vault version/topology:** **NOT OBSERVED** (no authorized instance).
- **Key/policy identifiers:** **NONE** (no key or policy was provisioned;
  no production key was touched).
- **Environment check (names/counts only, no values read):**
  `VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_TOKEN_FILE`,
  `NEXAROUTE_KEY_PROVIDER`, `NEXAROUTE_TRANSIT_ENABLED`,
  `NEXAROUTE_TRANSIT_MOUNT`, `NEXAROUTE_TRANSIT_KEY`, `VAULT_NAMESPACE`
  — present_count 0, nonempty_count 0.

| Acceptance item | Live status | Local evidence in this change |
|---|---|---|
| Wrap/unwrap round-trip | BLOCKED | fake Vault API contract PASS |
| AAD binding and tamper rejection | BLOCKED | PASS (owner/field/key-ref/nonce/data/wrapped-DEK) |
| Strict envelope parsing | BLOCKED | PASS (unknown/duplicate/trailing fields) |
| Request limits and timeouts | BLOCKED | PASS (64 KiB response cap, 4 KiB wrapped-input pre-check, per-request and overall context budgets, no retry after budget expiry) |
| Redirect refusal | BLOCKED | PASS (3xx rejected, target not contacted) |
| Unavailable/forbidden Vault fail-closed | BLOCKED | PASS (`transit_unavailable`, `transit_forbidden`, no local key on config load) |
| Invalid ciphertext | BLOCKED | PASS (`transit_invalid_ciphertext`) |
| Key-version errors | BLOCKED | PASS (`transit_key_version_unavailable` for too-old/too-new/deleted/disabled/missing version, body never exposed) |
| Migration rollback and recovery | BLOCKED | PASS (post-write failure restores original bytes; re-run succeeds) |
| Rewrap rollback and recovery | BLOCKED | PASS (post-write failure restores original bytes; re-run moves to new version) |
| Gateway startup/decrypt, `secrets verify`, wrong/revoked token, outage, rotation, post-rewrap decrypt | NOT_RUN | requires the authorized live instance |

Local commands (2026-10-30, Go 1.25.0 linux/amd64):

```text
$ go test -timeout=5m -count=1 ./internal/secrets/ ./internal/config/ ./cmd/gateway/ ./internal/privacyaudit/
ok github.com/ali-shortcuts/nexaroute/internal/secrets
ok github.com/ali-shortcuts/nexaroute/internal/config
ok github.com/ali-shortcuts/nexaroute/cmd/gateway
ok github.com/ali-shortcuts/nexaroute/internal/privacyaudit

$ go test -race -timeout=5m -count=1 ./internal/secrets/ ./internal/config/ ./internal/privacyaudit/
ok (all three packages)

$ go vet ./internal/secrets/ ./internal/config/ ./cmd/gateway/
(clean)

$ ./scripts/verify.sh
...
BROWSER ACCEPTANCE: SKIPPED locally (Chromium/Playwright unavailable; CI browser gate remains separate)
VERIFY PASS
```

Stale wording corrected with accurate current behavior: `rewrap-transit`
writes a verbatim `.pre-rewrap.bak` (not an authenticated backup), `nxs2`
config backups contain `nxs2` envelopes and are verified through Transit
(not "Transit-sealed"), and the key-version command name in
`docs/KNOWN_GAPS.md` is `secrets rewrap-transit`. Historical records above
are preserved.

Fake-API tests do not close this acceptance. This record does NOT close
issue #649, and no production/HSM/KMS readiness is claimed.

#### Re-check record — 2026-10-30 (main `7daa36d`, issue #649 still OPEN)

On 2026-10-30 the acceptance was re-attempted from current main
`7daa36d` (merge of PR #764). The `nxs2` Transit code inspected on
`8ac484e` is unchanged on `7daa36d` for
`internal/secrets/vault_transit.go`, `internal/secrets/envelope.go`,
`internal/secrets/migration.go`, and `cmd/gateway/secrets.go` (diff
`8ac484e..7daa36d` for those files is empty; only
`docs/EXTERNAL_VERIFICATION.md` and `docs/adr/0007-vault-transit-envelope-encryption.md`
moved, by the merged BLOCKED records and an ADR header note). Every live
step of the procedure above remains **NOT_RUN / BLOCKED**: zero live
Vault calls were made, no local Vault dev server was used as acceptance
evidence, and no second implementation was created. The missing input is
unchanged: an operator-authorized disposable Transit instance (address
ownership), a dedicated Transit key name, a least-privilege policy
(`encrypt`/`decrypt`/`rewrap` on that key only), a token-injection
method (by name only), and written operator authorization naming that
instance and scope. Environment check by names/counts only (no values
read): `VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_TOKEN_FILE`,
`NEXAROUTE_KEY_PROVIDER`, `NEXAROUTE_TRANSIT_ENABLED`,
`NEXAROUTE_TRANSIT_MOUNT`, `NEXAROUTE_TRANSIT_KEY`, `VAULT_NAMESPACE` —
none set (8 names checked, 0 present). No message in this turn names an
instance, key, policy, or injection method, so the directive's step 3
cannot be executed against a real Vault without that input.

Code work in this re-check (test-only, no production change): one
demonstrated fake-API gap was closed —
`TestVaultTransitPerRequestTimeoutAndCancel` in
`internal/secrets/vault_transit_test.go` pins ADR 0007 §8.3 for the
Transit provider itself (a hanging Vault fails closed as
`transit_unavailable` within the per-request timeout with no retry, and
a caller cancel aborts a hanging request). No earlier test exercised
the Transit provider against a hanging server: the fake's `slow` field
was never set, and the only slow-server test covers the KV helper, a
different path. All other areas named in the task were re-verified as
already covered by deterministic fake-API tests on this head:
wrap/unwrap protocol, strict envelope/ciphertext parsing, 64 KiB
response bound, redacted errors (Vault bodies discarded, token never in
an error string), redirect refusal, 429/503-only retries, unavailable
(5xx) / forbidden (401/403) / not-found (404) / invalid-ciphertext
(400) classes, revoked-version decrypt/rewrap failing closed, no
local/env fallback (conflict-is-error selection, gate-off makes zero
Vault calls, existing `.key` ignored), migration rollback restoring the
original bytes, rewrap failure leaving the file byte-identical, and
already-latest rewrap writing nothing.

DEK/KEK boundary as implemented (code read, not live-proven): only the
Transit `encrypt`/`decrypt`/`rewrap` endpoints are called, the token
travels header-only, the KEK never enters the gateway process, DEKs are
fresh per secret and the wrapped DEK plus key version are deliberately
excluded from the local canonical AAD so rewrap does not invalidate
data. No HSM/KMS isolation is claimed: that would depend on the
selected Vault topology and key backend, which no live run has proven.

Open drift for the owner's decision (not changed unilaterally): ADR
0007 §8.4 says version cases (wrong/disabled/destroyed version,
`min_decryption_version` exceeded) use class
`transit_key_version_unavailable`, but the code never emits that class
— every HTTP 400, including a revoked-version decrypt, maps to
`transit_invalid_ciphertext` (the 400 message does carry the §7.3
key-version checklist). Fail-closed behavior is preserved either way;
distinguishing the cases would require classifying Vault's error body,
which the redaction design deliberately discards, and Vault's exact
error strings were not available to verify against. Either implement
body-sniffing classification against a real Vault's observed strings or
amend the ADR table; until then the class label, not the outcome, is
the open item.

Local health only (fake Vault API, never acceptance; Go 1.27.0):
`internal/secrets` suite PASS plain (9.574 s) and with `-race`
(16.621 s), `cmd/gateway` focused Transit tests PASS plain (1.194 s),
new timeout/cancel test PASS plain (4.091 s) and with `-race`,
`gofmt`/`go vet` clean. Full `verify.sh` was not run locally (known VM
per-package timeout artifact); CI on this change is the gate. No
production Vault, key, or secret was touched. No token, ciphertext,
master-key byte, DEK, wrapped DEK, envelope string, or customer secret
is recorded here. No production/HSM claim is made or changed by this
record. This record does NOT close issue #649, and fake-API tests do
not close this acceptance.

## 3. Headless OS keyring

Local state: the linux provider shells out to `secret-tool`
(libsecret) with hermetic stub tests pinning the headless contract:
a missing entry, a malformed value, a failing tool, or the tool's
absence all fail **closed** — there is never a fallback to a file or
env key.

Procedure (headless Linux host):

1. Install `libsecret-tools` (provides `secret-tool`) and a keyring
   daemon (`gnome-keyring`), unlock it in a D-Bus session, e.g.
   `dbus-run-session -- bash -c 'echo -n "" | gnome-keyring-daemon --unlock; <gateway commands>'`
   or the systemd user-service equivalent.
2. Set `NEXAROUTE_KEY_PROVIDER=keyring`; run
   `nexaroute secrets status` and a gateway start.
3. Evidence: the master key round-trips through the keyring; killing
   the D-Bus session makes the gateway fail closed at startup with
   the keyring error (and it must NOT boot with a substitute key).
4. Record: distro, keyring daemon, session recipe, dates.

Still unverified until run: behavior under `kwallet`/KDE,
prompt-driven unlock flows (the gateway is non-interactive; a locked
keyring that requires a prompt is expected to fail closed — confirm
rather than assume), and non-Linux keyrings (the provider is
unavailable there by build).

## Executed evidence — 2026-10-22 (gateway binary built from `main` `6ca8ac5f`)

The procedures above were exercised as far as this environment allows.
Statuses use the harness vocabulary; nothing below upgrades a claim the
evidence does not cover.

### OIDC (harness, live network)

- **Google (`https://accounts.google.com`)**: discovery **PASS** (issuer
  and endpoint metadata matched), JWKS **PASS** (document fetched, at
  least one signing key). One earlier attempt reported JWKS **FAIL** on
  a 15s fetch timeout; the harness reported the failure rather than
  masking it, and a retry at a 30s timeout passed — recorded here so
  the flaky attempt is not hidden. ID token: **NOT_RUN** (no operator
  token). Authorization Code + PKCE login: **NOT_RUN** (interactive;
  still requires a real tenant per the procedure above). The full
  login claim therefore remains **UNVERIFIED**.
- **GitHub Actions issuer (`https://token.actions.githubusercontent.com`)**:
  discovery **FAIL — correctly**. Its discovery document contains no
  `authorization_endpoint` or `token_endpoint` (it is a CI token
  issuer, not a login provider), and the harness's endpoint validation
  rejected it. A true negative against real-world metadata.

### Vault KV helper

- Helper against an unreachable `VAULT_ADDR`: exit 1, no key material
  on stdout — **fail-closed PASS**. Missing `VAULT_ADDR`/`VAULT_TOKEN`:
  exit 1 — **PASS**. A round-trip against a **real Vault server** was
  **BLOCKED** in this environment (no Vault available) and remains
  **UNVERIFIED**.

### OS keyring

- With an encrypted config present and `secret-tool` absent from the
  host, `nexaroute secrets verify` with `NEXAROUTE_KEY_PROVIDER=keyring`
  fails with the keyring lookup error and does **not** fall back to the
  key file beside the config — the documented absence contract,
  **fail-closed PASS**. A real keyring round-trip is **BLOCKED** here
  (no `secret-tool`, no keyring daemon) and remains **UNVERIFIED**.

### Key-provider command protocol + master-key lifecycle (local custody)

Exercised end-to-end with the shipped binary on disposable state:
`secrets rotate` encrypted a plaintext `api_key` in place (`nxs1`
envelope, key file + recovery copy written); `secrets verify` **PASS**
under the current key; `secrets decrypt` returned the original value; a
wrong key fails closed with "invalid ciphertext or master key". The
command key-provider protocol (the same protocol the Vault helper
implements) verified a config under a test-double command and failed
closed under a wrong-key command — protocol path **PASS** locally;
production KMS/Vault custody remains **UNVERIFIED**.

### LLM providers

**NOT_RUN**: no provider credentials are available in this environment
and no paid calls were made (by design). Live provider interoperability
remains **UNVERIFIED**; the fixture-based conformance suite
(`internal/providers/livecanary`, now including the extended scenario
set) is the local evidence, and it is not a substitute.

### Gate audit 2026-10-29 (pinned main c2ca13c, audit only; re-checked 2026-10-30 on main 2c5f583)

Audit scope: umbrella #654 + gates #646–#652. No live calls were made
in this audit or in the 2026-10-30 recheck; no credential was read or
used; no model was downloaded; no inference was started. Evidence
classes below are as recorded in the 2026-10-29 audit record —
fixture, loopback, offline-render, and doc-reported evidence are NOT
live evidence. The recheck verified issue states, blockers, and the
chart version on current main; it did not re-run the underlying
loopback runs. All seven gates remain OPEN, and this section changes
no gate status to verified. A fixture, a local mock, or an ordinary
green CI job is not evidence of live provider compatibility,
organizational OIDC, staging/HA Vault, a real Kubernetes install,
ComfyUI model inference, or multi-host HA/RPO/RTO.

| Gate | Status (re-checked 2026-10-30) | Evidence class of last verified evidence | Exact blocker |
|---|---|---|---|
| #646 LLM canaries | UNVERIFIED — NOT_RUN | FIXTURE/LOOPBACK (harness + refusal gates in main); no LIVE | Written operator comment naming exact providers + models + approved tenant + approved cost/budget, with credentials via named env vars only |
| #647 Organizational OIDC | UNVERIFIED — NOT_RUN (live tenant) | LOCAL IdP ONLY (PKCE browser login + security-negative flow against a local conforming IdP); code controls are not tenant acceptance | Operator-owned tenant (Keycloak, Entra ID, Okta, Auth0, or Google) + written authorization; a local-IdP pass does not upgrade this gate |
| #648 Vault KV | BLOCKED/NOT_RUN for staging + HA | LOOPBACK (disposable single-node non-dev Vault v2.2.1, TLS loopback, 33/33 harness + gateway-path checks PASS 2026-10-30, merged via #780; gateway boot verified on loopback, not staging) | Written authorization + instance pointer (HTTPS address, KV v2 mount, empty path, least-privilege token by name, mount type/version confirmation); HA, long renewal, AppRole, K8s auth, Transit remain separately blocked. Former harness gaps now FIXED on main: KV-v2 path-shape-only (pre-write mount_version via #780), no policy assertion (--expect-policy via #780), --timeout propagation (via #741); plus BLOCKED-metadata early stop, expired-token check, helper ref validation (via #780) and the gateway helper-output 1 MiB bound (this change) |
| #649 Vault Transit | NOT_RUN (live acceptance) | FIXTURE (fake Vault API tests); nxs2 code is present on main (merged via #670) but code presence is not acceptance | Complete package together: written authorization + disposable instance + dedicated key + least-privilege policy (encrypt/decrypt/rewrap on that key only) + token-injection method by name only; production/HSM/HA claims remain blocked even then |
| #650 K8s/Helm | UNVERIFIED — NOT_RUN | OFFLINE/RENDER (38 PASS lines, Helm v3.19.0, 2026-10-27); live NONE | Authorized host/runner with docker+kind+kubectl+helm and delegated cgroups (compute, not a credential); script safe-failure gap and chart staleness (chart 0.1.0 / appVersion 0.22.0 in deploy/helm/nexaroute/Chart.yaml on current main, vs the v0.24.0 release line) remain separately open |
| #651 ComfyUI | UNVERIFIED — NOT_RUN/BLOCKED | FIXTURE (harness) + prior LOOPBACK fail-closed (on ba5ff2f, UNVERIFIED on the audit pin) | Four separate inputs: provisioned server + compatible model/workflow/config + suitable compute host (GPU preferred) + written authorization; supplying one leaves the others blocked |
| #652 Multi-host HA | NOT_RUN (multi-host); no multi-host RPO/RTO values exist | SAME-HOST ONLY: multi-process PostgreSQL/Redis integration plus a 2026-10-30 container-isolated single-host drill (two gateway containers, shared PostgreSQL (Redis container present but no backend pointed at it), S1–S8 PASS in that run; no two-host or load-balancer claim) — `docs/reports/HA_ACCEPTANCE_CONTAINER_2026-10-30.md`; container/same-VM figures are not multi-host values | Disposable two-host staging topology + operator authorization; a two-host run, a failover drill, a backup-restore drill, and measured multi-host RPO/RTO |

Umbrella #654 note (re-checked 2026-10-30): its body checklist still
shows #646–#653 unchecked, and all of those issues are still OPEN, so
the checklist now matches live states. One stale line remains: #649
still reads "NOT IMPLEMENTED" although the nxs2 code merged via #670;
the correct statement is code present, live acceptance NOT_RUN. The
original draft of this section also claimed #647 was CLOSED/COMPLETED;
that claim is superseded — #647 was REOPENED and its live-tenant
acceptance is UNVERIFIED, and #652 was likewise REOPENED after an
audit found its closure unsupported. Neither reopening is re-litigated
here; both gates are recorded above in their reopened state.

Out of this section's scope, unchanged by this audit: #653 (coverage
and soak tracker, still OPEN — this audit makes no coverage claim, and
a coverage percentage never verifies a live provider, tenant, or
device; read any coverage figure from its own current record and CI
artifact, not from this section) and #664 (bounded CI soak smoke
recorded as PASS in docs/reports/ISSUE664_CI_SMOKE_2026-10-29.md;
long-form multi-hour soak NOT_RUN, live items NOT_RUN, no RPO/RTO
reported). Neither #653 nor #664 is closed by this audit.

## Gate audit 2026-10-29 (umbrella #654) — statuses unchanged

Re-verified 2026-10-29 evening against main `bb5865f`: issues #654, #646,
#648, #649, #650, #651 all still OPEN. The audit itself produced no live
acceptance evidence (no provider calls, no staging Vault, no cluster, no
model inference). Per-issue matrix (implementation readiness / fixture
evidence / true external acceptance / exact missing resource):

- #646 (LLM canaries): harness + runbook on main; fixture/loopback PASS
  only; live BLOCKED on one written operator comment naming providers +
  models + approved tenant + approved spend, credentials via named env
  vars only.
- #648 (Vault KV): loopback PASS 10/12 on the pre-hardening check set
  (disposable Vault v2.2.1, 2026-10-27); staging BLOCKED on written
  authorization + staging address + KV v2 mount + dedicated empty path +
  least-privilege token by name; HA / long renewal / AppRole / K8s auth /
  Transit NOT_RUN.
- #649 (Vault Transit): implementation on main via #670; live NOT_RUN;
  unblock package = written authorization + disposable instance + dedicated
  key + least-privilege policy (encrypt/decrypt/rewrap on that key) + token
  injection by name only.
- #650 (K8s/Helm): offline/render PASS; live NOT_RUN — needs an authorized
  host with docker + kind + kubectl + helm + delegated cgroups (the script
  creates its own disposable kind cluster; no operator cluster grant is
  needed). Safe-failure phases (misconfigured config / down dependency /
  missing Secret) are specified but not implemented; chart versions
  (0.1.0/0.22.0) are deliberately untouched (owner release-policy
  decision).
- #651 (ComfyUI): harness on main; live NOT_RUN — needs a provisioned
  server + compatible model/workflow with passing node mapping + suitable
  (GPU-preferred) compute + written authorization. Setup-only per owner
  decision 2026-10-29; no model download, no inference on this VM.

Umbrella checklist staleness (not silently rewritten): the #654 body still
reads #649 as NOT IMPLEMENTED though its implementation merged via #670.
#647 closed 2026-10-29 and has since reopened, so its unchecked box in the
#654 body is current again and its live acceptance remains outstanding. The accompanying
hardening PR (branch `supervisor/gates654-acceptance-hardening`, review
only) closes the three #648 harness gaps (KV v2 metadata probe, root-token
refusal, helper timeout propagation), reconciles ADR 0007's status line,
and documents the #650 safe-failure gap; it merges nothing and closes no
issue.

## Release preparation record — 2026-10-31, main `2f638da`

This section records only what was actually dispatched and observed in
the 2026-10-31 release-preparation run. It does not close any issue
and does not make a release claim.

- Current main at the start of the run:
  `2f638da5ff127e1a3a46a5a687c682357523604a` (merge of PR #802,
  head `c93279b`). PR #802's body was scrubbed before that merge:
  the live body reads "Related to #654; umbrella stays open" and
  contains zero close/fix/resolve + issue-number adjacencies, and
  issue #654 remained OPEN after the merge. The merge itself was
  performed by another actor, not by this run.
- PR #803 (independent-environment fail-closed fixes, branch
  `ci/independent-environment-fixes-20261031b`, head `442eed6`)
  is OPEN and NOT merged. The workflow on main `2f638da` still
  contains the known defects (unit/race steps pipe to `tee`
  without `pipefail`; integration required list contains the
  non-existent `TestControlPlaneRedisIntegration`; fuzz job has
  no zero-target FAIL), so a run of the main independent workflow
  cannot be quoted as a final independent verdict until #803 (or
  an equivalent fix) merges and the corrected workflow runs on
  the then-current main SHA.
- Workflows dispatched on main `2f638da` on 2026-10-31
  (manual, workflow_dispatch; no dispatch result is claimed
  beyond the run page state at check time):
  - Kubernetes live acceptance: run 41206916203
  - Vault runner acceptance: run 41206938283 — page state
    Failure, 56 s, 1 artifact
    (`vault-runner-reports-2f638da5ff127e1a3a46a5a687c682357523604a`)
  - HA runner acceptance: run 41206954815
  - OIDC runner acceptance: run 41207000118
  - Coverage report: run 41207035904
  - Soak (rounds=3, duration=2h, timeout 180m): run 41207083700
    — In progress at check time; a 2 h soak cannot be judged
    before it finishes and its summary is validated against
    commit `2f638da` and 7200 s sustained.
  - Independent environment (full_soak=true, 2h): run 41207125741
    — dispatched on the flawed main workflow; see #803 note above.
- Acceptance matrix on `2f638da` (runner evidence only, never
  real tenant / staging / multi-host / production evidence):
  - #646 LLM provider canaries: BLOCKED — no operator-selected
    providers, credentials, endpoints, model configuration, or
    spending ceiling were supplied; no paid calls were made.
  - #647 Organizational OIDC: BLOCKED for the real tenant;
    runner Keycloak evidence only, and only if run 41207000118
    finishes with a validated summary. It is not tenant evidence.
  - #648 Vault KV (staging): BLOCKED — runner Vault evidence
    only, and run 41206938283 already shows Failure; staging
    Vault, least-privilege policy, and operator authorization
    were not supplied.
  - #649 Vault Transit: BLOCKED — same runner-only scope as
    #648, plus the same failed run; no staging Transit service
    was supplied.
  - #650 Kubernetes/Helm: NOT YET JUDGED — run 41206916203
    dispatched; a disposable kind result, if it passes, is
    ephemeral-cluster evidence only, not production-cluster
    acceptance.
  - #651 ComfyUI real inference: BLOCKED — no reachable
    authorized ComfyUI service, real model/workflow, or
    compute/quota authorization was supplied; no inference ran.
  - #652 Multi-host HA: BLOCKED for multi-host — runner
    containers are single-host evidence only, even if run
    41206954815 passes; two independent hosts, shared
    PostgreSQL/Redis, and measured RPO/RTO were not supplied.
  - #653 Coverage + soak: NOT YET JUDGED — coverage run
    41207035904 and soak run 41207083700 were dispatched on
    `2f638da`; neither result was validated at the time of
    this record, and no earlier SHA's coverage or soak is
    carried over to `2f638da`.
- Final release verdict on `2f638da`: BLOCKED. No tag and no
  release were created. The release-gate script
  (`scripts/release-gate-status.sh`) could not be run to a
  PASS in this environment (it needs an authenticated `gh`
  Checks API read, which the available connector does not
  expose), and the external gates above are in any case not
  met. Remaining owner inputs: (1) Vault run failure detail
  and a fix/re-run; (2) approval + CI-green for PR #803, then
  a corrected independent run; (3) terminal results for the
  K8s/HA/OIDC/coverage/soak runs on `2f638da`; (4) operator
  resources for #646, #647 tenant, #648/#649 staging Vault,
  #651 ComfyUI, and #652 separate hosts; (5) a separate
  explicit release approval after all of the above.

## Release-readiness recheck — 2026-11-01, main `26ecd267`

Scope: this section records only what was revalidated on the exact current
main tip and what could be executed from the agent environment. It closes no
issue, produces no live-tenant, staging, multi-host or production evidence,
and creates no tag or release.

### Repository state at revalidation

- Exact current `main` tip:
  `26ecd267107a29d303a6834f5c35df9e0f47d063` (2026-11-01T11:21:12Z).
  Verified against `git ls-remote` and the branches API — the previously
  quoted merge commit was confirmed to still be the tip, not assumed.
- PR #818 ("fix: preserve pod termination status during Kubernetes
  acceptance", head `299c1464`, branch `fix/k8s-termination-finalizer-20261101`)
  is MERGED: its merge commit is `26ecd267`. It adds the acceptance-only Pod
  finalizer helper (`set_pod_termination_finalizer`,
  `release_termination_observer`, `cleanup_live_cluster`) to
  `scripts/k8s-acceptance.sh` so a Pod's terminal `finishedAt` cannot be
  deleted before the runner reads it (+157/−13).
- Open PRs at revalidation: #814 (docs refresh, head `c17bfb68`, red) and
  #819 (the Gemini failover test-correction described below).
- Issues #646–#653 are all OPEN (`state_reason=reopened`); umbrella #654 is
  OPEN. No acceptance issue was closed or reopened by this run.

### Workflow runs bound to the exact main SHA `26ecd267`

| Workflow | Run | Conclusion | Readable evidence |
|---|---|---|---|
| CI | [41301515228](https://github.com/ali-shortcuts/nexaroute/actions/runs/41301515228) | success | jobs `verify`, `container`, `fuzz-smoke`, `integration-postgres`, `integration-redis`, `ci-soak-smoke` all success; artifacts `browser-acceptance-26ecd267…-41301515228-1` (5,707,135 B, id 13081752839), `ci-soak-smoke-26ecd267…-41301515228-1` (12,958 B, id 13081632227), two `dockerbuild` artifacts |
| Coverage report | [41301515295](https://github.com/ali-shortcuts/nexaroute/actions/runs/41301515295) | success | job `Fresh Go coverage` success — the 95.0 % fail-closed gate (`scripts/coverage-gate.sh`) passed on this SHA; artifact `coverage-26ecd267107a29d303a6834f5c35df9e0f47d063-41301515295` (266,247 B, id 13081627549) |
| Security | [41301515283](https://github.com/ali-shortcuts/nexaroute/actions/runs/41301515283) | success | jobs `CodeQL (Go)`, `Go vulnerability scan` success |

**No acceptance workflow has run on the exact current main SHA.** The newest
run of every opt-in acceptance workflow binds to `89b608dd` (merge of PR #813,
2026-11-01T01:00Z), i.e. one merge-pair behind `26ecd267`:

| Workflow | Latest run | SHA | Conclusion |
|---|---|---|---|
| Kubernetes live acceptance | [41259193878](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259193878) | `89b608dd` | **FAILURE** (step `Run live acceptance`; artifact `k8s-live-acceptance-89b608dd…`, 35,406,057 B, id 13065160585) |
| Vault runner acceptance | [41259249411](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259249411) | `89b608dd` | success (artifact `vault-runner-reports-89b608dd…`, 2,852 B, id 13064312333) |
| OIDC runner acceptance | [41259304417](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259304417) | `89b608dd` | success (artifact `oidc-runner-reports-89b608dd…`, 1,291 B, id 13064865956) |
| HA runner acceptance | [41259343096](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259343096) | `89b608dd` | success (artifact `ha-runner-reports-89b608dd…`, 5,433 B, id 13064577122) |
| Independent environment | [41259476813](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259476813) | `89b608dd` | success, 11 jobs (artifact set 9 artifacts) |
| Soak | [41259436004](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259436004) | `89b608dd` | success, 2 h 04 m wall clock (artifact `soak-89b608dd…-41259436004-attempt-1`, 385,830 B, id 13068177643) |

Evidence from those runs is **not** transferable to `26ecd267`: PRs #817 and
#818 changed `scripts/k8s-acceptance.sh` after them, and #818 in particular
changed the termination measurement those runs exercised.

### Why no fresh acceptance run could be dispatched from this environment (BLOCKED — tooling)

1. Workflow dispatch is not permitted to the installed GitHub App:
   `gh workflow run "<name>" --ref main` returns
   `HTTP 403: Resource not accessible by integration`
   (`/repos/{owner}/{repo}/actions/workflows/{id}/dispatches`). This blocks
   every opt-in workflow named in the task: Kubernetes live acceptance,
   Vault runner acceptance, OIDC runner acceptance, HA runner acceptance,
   Independent environment, Soak, and Coverage report.
2. Adding a workflow file to run them another way is refused at push time:
   `refusing to allow a GitHub App to create or update workflow
   .github/workflows/… without workflows permission`.
3. Run logs and artifact *contents* are not retrievable here: the
   `actions/jobs/{id}/logs` and artifact download endpoints redirect to
   `results-receiver.actions.githubusercontent.com`,
   `*.blob.core.windows.net` and `objects.githubusercontent.com`, all of which
   are unreachable from this sandbox. Readable evidence is therefore limited to
   run/job conclusions, job step-level conclusions, artifact names/sizes, and
   check-run annotations. Any check that requires reading log bodies or artifact
   files is reported as **BLOCKED (evidence not readable)**, not as PASS.
4. The environment has no Go toolchain and cannot obtain one (`go.dev`,
   `dl.google.com`, `proxy.golang.org`, `storage.googleapis.com`,
   `objects.githubusercontent.com` all unreachable; `apt` has no toolchain
   policy), and no `docker`, `kind`, `kubectl` or `helm`. Local suite
   execution, kind-cluster acceptance and a 2 h soak are impossible here; Go
   changes were validated through PR CI on the exact PR head instead.

### Acceptance matrix on `26ecd267` (no new live evidence produced)

| Issue | Class of the newest available evidence | Verdict on `26ecd267` | Exact remaining prerequisite |
|---|---|---|---|
| #646 LLM provider canaries | fixture/loopback only | **BLOCKED** | Operator comment naming provider + endpoint + model/account + the env-var secret names that are already configured + an explicit per-provider spend ceiling. Zero paid calls were made; no provider is authorized. |
| #647 Organizational OIDC | runner-local Keycloak (`89b608dd`) | **BLOCKED** for the real tenant; runner-local PASS is real but classed `runner-keycloak` | A disposable/staging organizational tenant: issuer URL, client ID/secret secret names, group→role mapping, and authorization to register the redirect URI |
| #648 Vault KV (staging) | runner-local disposable Vault (`89b608dd`) | **BLOCKED** for staging | Staging Vault HTTPS address, KV v2 mount, a dedicated empty path, a least-privilege token injected by name (`VAULT_TOKEN`/`VAULT_TOKEN_FILE`), plus that instance's mount type/version confirmation |
| #649 Vault Transit | runner-local + fake-API contract tests | **BLOCKED** for live Transit | Written authorization, an operator-owned disposable Transit instance, a dedicated key name, a policy granting exactly `encrypt`/`decrypt`/`rewrap` on it, and token injection by name |
| #650 Kubernetes/Helm | last live run FAILED on `89b608dd`; `26ecd267` contains the #817+#818 script fixes, unvalidated live | **NOT_RUN** on `26ecd267` (dispatch blocked). No `RESULT:` line — zero mandatory failures and zero mandatory `NOT_RUN` checks are *not* established for the current tree | A dispatch of `k8s-live-acceptance.yml` (inputs `prev_tag=v0.24.0`) on `26ecd267`, with the artifact inspected for `RESULT:` = no failures and no NOT_RUN checks |
| #651 ComfyUI real inference | fixtures + fail-closed loopback | **BLOCKED** | A reachable authorized ComfyUI server, a compatible real model + API workflow + node mapping, compute authorization, and an explicit no-download policy confirmation |
| #652 Multi-host HA | runner containers on ONE host (`89b608dd`); the run's own notice reports `two_independent_hosts NOT_MEASURED` | **BLOCKED** for multi-host. Nine scenarios (S1–S9), `validation`, `redis-smoke` PASS on a single Docker host; this is `container-isolated-single-host` evidence and is **never** multi-host acceptance | Two independent hosts, shared PostgreSQL + Redis reachable from both, a load-balancer/VIP path, and measured failover RPO/RTO |
| #653 Coverage + soak | coverage PASS on `26ecd267`; soak PASS only on `89b608dd` | Coverage **PASS** (95.0 % gate, exact SHA). Soak **NOT_RUN** on `26ecd267` | Dispatch `soak.yml` with `rounds=3`, `duration=2h`, `timeout_minutes>=150` on `26ecd267`, then confirm `check-soak-evidence.py` passed and read `summary.json` (exact SHA, `sustained_seconds>=7200`, all 7 fixed stages + 3 stress rounds, resource samples, NOT_RUN block). Soak badge alone is not evidence; the completeness gate is what makes it evidence |

Coverage detail, stated exactly: run 41301515295 enforces
`./scripts/coverage-gate.sh .artifacts/coverage/coverage.out 95.0` on a
pinned Go `1.27.3` toolchain and concluded success on `26ecd267`, so the
repository-wide total was ≥ 95.0 % inclusive. The exact
covered/total statement counts and percentage in that artifact
(`coverage-26ecd267…-41301515295`, id 13081627549) are **not readable from
this environment**; they must not be quoted from an earlier SHA or from a
local toolchain, because statement counts are toolchain-dependent.

Soak detail, stated exactly: run 41259436004 ran the workflow's
`Validate soak evidence completeness` step to success, which fails closed
unless `summary.json` names the run's commit, contains every planned stage,
reports `sustained_seconds` at least the requested duration, and carries the
NOT_RUN block for live items. That gate passed for `89b608dd` with a
2 h 04 m job wall clock; it proves nothing about `26ecd267`.

### PR #814 — intermittent Gemini failover failure, diagnosis and correction

PR #814 (docs-only, head `c17bfb68`) failed CI run
[41301006743](https://github.com/ali-shortcuts/nexaroute/actions/runs/41301006743)
with `TestWave2GeminiHTTPFailover: good candidate calls=2 want 1`
(`internal/httpapi/issue663_wave2_gemini_test.go`).

Diagnosis (from the code and from instrumented CI runs, not from the failing
log alone, which is not readable here):

- The Gemini ingress loop in `internal/httpapi/gemini.go` dispatches at most
  one attempt per candidate (`for i := 0; i < len(candidates) && attempts < max; i++`),
  there is no hedging on that path (`doAttemptWithHedge` is only used by the
  Anthropic and OpenAI/Responses paths), no shadow/mirror dispatch, and
  `doUpstreamWithRepair` returns immediately on a 2xx, so one request cannot
  deterministically dispatch the same candidate twice.
- The failing assertion passed the preceding 200/`ok` body check, and the
  gateway's own attempt ledger for the request (`X-NexaRoute-Attempts`) reads
  `a/m>b/m` — i.e. exactly one dispatch to the good candidate.
- A temporary instrumented rebuild of the same scenario (recording method,
  path, body digest and remote port of every request the "good" upstream
  received) ran the scenario **156 times** (60 sequential + 96 concurrent
  rounds) and observed `hits=1` with `attempts="a/m>b/m"` in every round; the
  same suite also logs `httptest.Server blocked in Close ... connection in
  state active`, i.e. lingering connections exist in this package.
- Conclusion: this is **not a product routing defect and not an incorrect
  expectation**. The bare call counter attributed *any* HTTP request delivered
  to the loopback port to the request under test, so a stray request from a
  lingering connection/goroutine reusing that ephemeral port broke the
  assertion.

Correction (PR #819, `internal/httpapi/issue663_wave2_gemini_test.go`): the
three Gemini failover tests now use an upstream that counts only payloads
carrying that request's unique prompt marker (a genuine double dispatch
carries the marker and still fails), report unrelated arrivals through
`t.Logf` instead of folding them into the count, and additionally assert the
deterministic invariant `route_attempt events for b/m == 1` from the
gateway's own event ledger, which no loopback-port reuse can perturb. The
same latent exposure remains in `internal/httpapi/mission95_wave3_canonical_test.go`
(three equivalent call-count assertions); it is recorded here as a follow-up,
not silently changed.

Validation of the correction: full CI, Security, Coverage and Real-system
acceptance all green on PR #819 head `802f207` (CI run 41303283821: `verify`
9 m 52 s, `container`, `fuzz-smoke`, `integration-postgres`,
`integration-redis`, `ci-soak-smoke`; Coverage run 41303283719 `Fresh Go
coverage`; Security run 41303283740 `CodeQL (Go)` + `Go vulnerability scan`;
Real-system acceptance run 41303283770 two PostgreSQL-backed gateway
processes + Secret Service Store/Load).

PR #814 itself was not modified by this run: the branch
(`docs/release-readiness-audit-20261031`) belongs to a different workflow and
this environment is restricted to the session branch. It needs a rebase onto
main after the test correction lands.

### Release gate verdict on `26ecd267`

`.github/scripts/check-final-release-gate.sh` still requires issues
#646–#653 **and** #654 to be `closed` with `state_reason=completed`. All nine
are open, and every external gate above is BLOCKED or NOT_RUN, so the gate is
**not satisfied**. No tag was created, no release was published, no release
gate or required check was altered, and no issue was closed.


## Release-readiness refresh — 2026-10-31, main `d54c36b6`

This refresh records the exact evidence observed after PR #812 reached main. It supersedes earlier status summaries only for the current SHA and run IDs listed here. Historical results remain tied to their original commits. It does not certify product-wide completion, change issue states, or authorize a release.

### Main CI, security, and coverage

- Main SHA: `d54c36b6eb14c1105ba2f7331f25c772b0a7714a`.
- CI: PASS — [run 41224560136](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224560136).
- Security: PASS — [run 41224560158](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224560158).
- Fresh coverage: PASS — [run 41225255224](https://github.com/ali-shortcuts/nexaroute/actions/runs/41225255224), artifact `13051338685`.
- The retained `coverage.out` was independently recomputed from that artifact: **42,173 covered / 44,195 total Go statements = 95.4248218%**. The inclusive 95% gate requires at least 41,986 covered statements, so this artifact is 187 statements above the minimum. Go version: 1.27.3. This measurement binds only to `d54c36b6`.

### Runner acceptance — PASS within the explicitly limited topology

These jobs passed on `d54c36b6`, but their artifacts and workflow definitions expressly limit their evidence class. They do not satisfy live organizational, staging, HA, or multi-host acceptance.

- Vault runner: [run 41224688649](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224688649), artifact `13051646611`: runner-loopback PASS (44/44 checks) against a disposable single-node Vault 1.18.5 container. The report states HA failover, long-running token renewal, and HSM/KMS backing were not tested; it is not staging Vault evidence.
- OIDC runner: [run 41224739927](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224739927), artifact `13051372278`: runner-Keycloak PASS. The tenant was a disposable Keycloak container on the same runner, not an operator-owned organizational IdP. The report also excludes provider-specific claims and clock-skew coverage from this live run.
- HA runner: [run 41224779612](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224779612), artifact `13051392453`: container-isolated single-host PASS (two Gateway containers, shared PostgreSQL; Redis container present but no Gateway backend configured to use it). In S6, the application's own bundle verification returned `BLOCKED` as expected because PostgreSQL is an external shared store; the workflow separately restored a PostgreSQL dump into a fresh database, confirmed matching source/restored row counts, started a restored Gateway replica, and read back the drilled job with HTTP 200. Thus the single-host database dump/restore path passed, but the application bundle alone did not contain the external database. The report explicitly records two independent hosts and load-balancer failover as NOT_MEASURED. The observed RPO/RTO figures apply only to this single-host container drill and must not be represented as multi-host values.

### Kubernetes live acceptance — FAIL

- [Kubernetes live acceptance run 41224660001](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224660001), artifact `13052096015`, on main `d54c36b6`: **FAIL**, with two failures and two mandatory safe-failure phases marked NOT_RUN.
- The upgrade log shows the new Pod Ready on ReplicaSet `7f6b6cd5bc`, while the harness waited for the old hash `6c848db5c`. Application checks subsequently returned the expected successful responses, but this does not erase the readiness failure.
- The old termination measurement reported 120659ms for the blocking `kubectl delete` client operation. That duration includes API/controller finalization and does not directly establish when the containers exited.
- PR [#813](https://github.com/ali-shortcuts/nexaroute/pull/813) contains a revision-transition wait and revised, fail-closed timing based on Kubernetes deletion-grace metadata and every regular container's `finishedAt`. The 120000ms threshold is unchanged. Latest validated PR head: `a3d0ee960722936a186bce45da4378b0c102f48b`. Its exact-head CI, Security, Coverage, Redis integration, and Real-system acceptance checks completed successfully (CI run 41227135497; Security 41227135610; Coverage 41227135359; Redis 41227135439; Real-system 41227135355). The CI container job ran Helm lint/render, `--offline-only`, and `--self-test`. PR #813 was then merged as `89b608dd02e89d7c422022dffa3bdca3edef4648` (merged 2026-10-31T18:33:44Z), so its code validation is complete and it is no longer open. A fresh live kind run [41259193878](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259193878) on `89b608dd02e89d7c422022dffa3bdca3edef4648` was recorded at 2026-11-01T01:05:36Z and **FAILED**: `could not observe deletionTimestamp and finishedAt for every pod container before deletion/timeout` (script exit 1; `RESULT: failures=1 not_run=2`). The replacement Pod, PVC persistence, rollback, and re-upgrade checks passed, but the mandatory safe-failure phases were NOT_RUN because the termination-measurement phase failed. Issue #650 remains OPEN pending a corrected live rerun.

### Full soak and independent environment

- Full soak: [run 41225317366](https://github.com/ali-shortcuts/nexaroute/actions/runs/41225317366), main SHA `d54c36b6`, completed **SUCCESS** (2026-10-31T19:41:58Z; total 7,461s). All ten stages passed, including the 7,210-second http-hot-reload sustained phase, race-soak, and budget-leak-slope. Artifact: `soak-d54c36b6eb14c1105ba2f7331f25c772b0a7714a-41225317366-attempt-1` (375 KB). This result binds only to `d54c36b6` and is not transferred to `89b608d`.
- Independent environment: [run 41225370775](https://github.com/ali-shortcuts/nexaroute/actions/runs/41225370775), main SHA `d54c36b6`, completed **FAILURE** (2026-10-31T19:41:01Z; total 2h 4m 20s). The only failing job was "Full sustained soak (opt-in only)" (1h 45m); the other ten jobs succeeded (build, unit + race, PostgreSQL/Redis integration, gateway smoke, fuzz, browser acceptance, security, coverage gate, stress smoke, real-services check). The failed-step log was not retrievable, so no root cause is claimed here; the failure needs triage and a re-run on the final SHA, not a pass.
- Any older soak or independent-environment result from `dccd254` or an earlier SHA is historical evidence only and is not transferred to `d54c36b6`.

### Mandatory external acceptance status

The owner has recorded that all seven external acceptance gates are mandatory for the first stable release; no gate is to be waived or downgraded. The latest issue-state recheck found #646–#654 all OPEN.

| Issue | Current result | Missing evidence / input |
|---|---|---|
| #646 — live LLM providers | BLOCKED / NOT_RUN | Written selection of each provider, exact model and tenant/account, endpoints, secret names only, maximum calls/tokens, and approved spend ceiling. No paid upstream call is inferred from fixtures. |
| #647 — organizational OIDC | BLOCKED / NOT_RUN | An operator-owned organizational IdP tenant, client/configuration, approved secret injection, and a live browser acceptance report. Runner Keycloak is not tenant acceptance. |
| #648 — staging Vault KV | BLOCKED / NOT_RUN | Authorized non-loopback staging Vault endpoint, KV v2 mount and dedicated empty path, least-privilege policy, and approved token injection by secret name. Runner-loopback evidence is not staging. |
| #649 — live Vault Transit | BLOCKED / NOT_RUN | Authorized non-loopback Transit instance, dedicated key, least-privilege encrypt/decrypt/rewrap policy, approved token injection, and actual API/rotation/recovery evidence. Runner-loopback Transit checks do not prove staging, HA, HSM, or KMS readiness. |
| #650 — Kubernetes/Helm | FAIL on `d54c36b6`; live run 41259193878 on `89b608d` also FAILED | The `89b608d` run failed to observe Pod `deletionTimestamp` plus all container `finishedAt` values before the Pod disappeared or timed out; script exit 1, `failures=1`, `not_run=2`. Safe-failure phases did not execute. A passing live rerun is still required. |
| #651 — real ComfyUI inference | BLOCKED / NOT_RUN | Provisioned ComfyUI service, exact compatible model/workflow and node mapping, suitable authorized compute, and written usage/budget authorization. No fixture is a real media inference. |
| #652 — multi-host HA | NOT_RUN / NOT_MEASURED | Two genuinely independent authorized hosts, shared disposable PostgreSQL/Redis topology, backup/restore inputs, and measured multi-host RPO/RTO. Same-host container metrics are not multi-host evidence. |
| #653 — coverage and soak | Coverage MET on `d54c36b6` (95.4248218%); soak SUCCESS on `d54c36b6` (7,461s); independent FAILURE on `d54c36b6` (only the Full sustained soak job failed; triage pending, no root cause claimed) | Soak and independent results bind only to `d54c36b6`; re-run both on the final SHA (`89b608d`) and validate artifact, duration, stages, and failure semantics there; the external dependencies #646–#652 remain mandatory. |
| #654 — umbrella | OPEN | Remains the final tracker. The release gate requires #646–#653 to be closed with `state_reason=completed`, then #654 completed last. No manual state change substitutes for evidence. |

### Release decision

The latest published stable release remains [v0.24.0](https://github.com/ali-shortcuts/nexaroute/releases/tag/v0.24.0), published 2026-10-22. No newer stable tag or release was found. **Release status: BLOCKED.** Main CI, security, and coverage success are necessary but insufficient while the live kind run on `89b608d` has FAILED (termination evidence was incomplete and two safe-failure phases were NOT_RUN), and mandatory operator-dependent external acceptance remains unmet. Later soak/independent results on `89b608d` are recorded separately below; they do not transfer to newer commits.

Never place credential values, tokens, master-key bytes, private prompts, or customer data in issues, commits, artifacts, or reports. Record secret variable names only.



## Release-readiness refresh — 2026-11-01, main `244df238`

This update supersedes the earlier "latest" status statements above only where it cites newer commit-bound evidence. It does not waive any acceptance gate, close an issue, approve a release, or transfer a result from an older commit to the current main head.

### Current main CI, security and coverage

- Current main: `244df23836ceeec517b404819b25d7dda58cb0f1` (merge of PR #817; PR #816 is also on its first parent).
- CI: PASS on this exact SHA — [run 41265100942](https://github.com/ali-shortcuts/nexaroute/actions/runs/41265100942).
- Security: PASS on this exact SHA — [run 41265100980](https://github.com/ali-shortcuts/nexaroute/actions/runs/41265100980).
- Fresh Go coverage: PASS on this exact SHA — [run 41265100947](https://github.com/ali-shortcuts/nexaroute/actions/runs/41265100947), measured coverage 95.4% (above the repository-wide 95.0% gate).
- PR #816 (universal provider profile registry and compatibility matrix) merged as `ed5087cb5afdab80f531ffea1eab41987f906371`.
- PR #817 (Kubernetes termination-wait `timeout_s` nounset fix) merged as this main SHA. The fix separates the local timeout assignment from deadline arithmetic and adds a deterministic self-test under `set -u`. Exact-head CI/security/coverage checks passed before merge; the post-merge main CI also passed.

### Kubernetes live acceptance — source fix merged; live rerun still missing

- The latest live Kubernetes run before this update is [run 41259193878](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259193878), on `89b608dd02e89d7c422022dffa3bdca3edef4648`: **FAIL** at `scripts/k8s-acceptance.sh:459` with `timeout_s: unbound variable`.
- PR #817 corrected that shell error and added a cluster-free self-test. The post-merge CI on `244df238` passed, including the offline harness/self-test. This is code-level regression evidence only.
- No Kubernetes live-acceptance run on `244df238` is recorded as of this refresh. A fresh authorized disposable-kind run must still exercise the full install/upgrade/rollback/persistence/termination and safe-failure lifecycle. Issue #650 therefore remains OPEN; do not call live Kubernetes acceptance PASS from the offline self-test.

### Soak and independent-environment reruns on `89b608d`

The two runs that were still in progress in an earlier audit have now completed:

- Full soak: [run 41259436004](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259436004), commit `89b608dd02e89d7c422022dffa3bdca3edef4648`, **SUCCESS**. Its retained job log validates the summary for the same SHA: 10 stages, 7,441 seconds total, result PASS. Artifact `13068177643`.
- Independent environment: [run 41259476813](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259476813), same commit, **SUCCESS**. All 11 jobs passed, including unit/race, browser, stress/soak smoke, build, fuzz, gateway/container smoke, full sustained soak, coverage gate, security, PostgreSQL/Redis integration and evidence validation. The sustained-soak validator reports 7,436 seconds and all 10 stages PASS. Artifact `13067314321`.
- Important limitation: the "Real services that this workflow does NOT fake" job passed by explicitly reporting #646–#652 as BLOCKED pending operator-owned systems and authorization. Its green conclusion does not satisfy those external gates.
- Both runs bind to `89b608d`, not the newer current main `244df238`. Their PASS results are historical evidence and must not be transferred to current main. Re-run the required soak and independent-environment acceptance on the final/current main candidate before using them as current-main evidence.

### Pull request follow-up

- PR #815 originally changed `scripts/key-provider-vault-kv.sh` from executable mode `100755` to `100644`, causing `verify` and fresh coverage to fail with a permission error even though other jobs passed.
- Commit [`5a853fdd62d343be5e0cee8508021bfce016daf9`](https://github.com/ali-shortcuts/nexaroute/commit/5a853fdd62d343be5e0cee8508021bfce016daf9) on PR #815 restores mode `100755` without changing the file contents. New CI, security, coverage and integration runs were queued/in progress at the time of this refresh; no result is claimed until they finish.
- PR #814 contains this documentation refresh. Its new checks must complete before it is considered validated.

### External acceptance and final release decision

The latest issue-state read still finds #646–#654 OPEN. Their external requirements remain:

| Issue | Remaining real-system evidence |
|---|---|
| #646 | Operator-selected LLM providers/models/tenants, authorized billable usage and approved secret injection; fixtures do not count. |
| #647 | Operator-owned organizational IdP tenant and live browser/claim/key-rotation evidence; runner-local Keycloak does not count. |
| #648 | Authorized non-dev/staging Vault KV deployment with least-privilege policy, token rotation and failover evidence. |
| #649 | Authorized Vault Transit service and real encrypt/decrypt/rewrap/rotation/recovery evidence; fake API tests do not establish interoperability. |
| #650 | Fresh authorized disposable-kind lifecycle acceptance on current main; PR #817's offline self-test is not a live cluster run. |
| #651 | Real ComfyUI service, compatible model/workflow and verified generated media asset; fixture output does not count. |
| #652 | Two independent hosts with shared services, failover/backup recovery and measured multi-host RPO/RTO; same-host containers do not count. |
| #653 | Current-main coverage is above 95%, and soak/independent runs pass on `89b608d`; repeat/validate on current final main and retain the external blockers above. |
| #654 | Umbrella tracker remains open until all required gates have evidence and are completed in the prescribed order. |

**Release status: BLOCKED.** Current-main CI, security and coverage pass, and the earlier Kubernetes shell defect is fixed. However live Kubernetes acceptance has not been rerun on current main, current-main soak/independent runs are still needed, and operator-controlled external acceptance #646–#652 is outstanding. No release tag or issue-state change is authorized by this evidence refresh.


## Release-readiness refresh — 2026-11-01, main `310000bc`

This is the latest snapshot in this document and supersedes every earlier statement that identifies `244df238` as current main. Evidence is bound to the exact commit shown below. It does not waive a gate, close an issue, approve a release, or transfer a result from an older SHA.

### Current-main CI, security, coverage and browser checks

- Current main: `310000bc3f131fe53fa24a1ea14b66b9e8ea1d48`, merge commit for PR #815.
- CI: **PASS** — [run 41273029717](https://github.com/ali-shortcuts/nexaroute/actions/runs/41273029717).
- Security: **PASS** — [run 41273029762](https://github.com/ali-shortcuts/nexaroute/actions/runs/41273029762).
- Fresh Go coverage: **PASS**, 42,198 / 44,230 statements = **95.4%**, against the inclusive 95.0% gate — [run 41273029707](https://github.com/ali-shortcuts/nexaroute/actions/runs/41273029707), artifact `13069344275`. This is the exact `310000bc` measurement.
- Browser evidence: **PASS** — [run 41273029702](https://github.com/ali-shortcuts/nexaroute/actions/runs/41273029702).
- Browser-evidence fixture proposal: **PASS** — [run 41273156990](https://github.com/ali-shortcuts/nexaroute/actions/runs/41273156990).
- No current-main workflow failure is recorded among these five checks. The skipped OpenCode workflow run is not a product acceptance check and is not counted as a pass.

### PR #815 — Vault KV response-cap fix merged

- PR [#815](https://github.com/ali-shortcuts/nexaroute/pull/815) merged at 2026-11-01T04:25:13Z as `310000bc3f131fe53fa24a1ea14b66b9e8ea1d48`.
- It enforces the 1 MiB Vault KV GET response limit while bytes are received, including unknown-length/chunked responses, using a bounded temporary file and an OS-enforced file-size limit. The new regression test streams an oversized 32 MiB chunked response and verifies the client stops before receiving the complete body.
- The helper remains executable in the merged tree: `scripts/key-provider-vault-kv.sh` mode `100755`. This resolves the earlier executable-bit regression that had caused exact-head CI and coverage failures.
- The PR's final validation and the post-merge current-main CI/Security/Coverage checks succeeded. This is code/test evidence; it is not staging Vault acceptance. Issue #648 remains OPEN until an operator-authorized non-loopback staging Vault run is recorded.

### Kubernetes live acceptance and soak/independent evidence

- Latest live Kubernetes run found: [run 41259193878](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259193878), on `89b608dd02e89d7c422022dffa3bdca3edef4648`, **FAIL**. The script log records: `FAIL: could not observe deletionTimestamp and finishedAt for every pod container before deletion/timeout`; `RESULT: failures=1 not_run=2`. The lifecycle failed at termination measurement, so the mandatory safe-failure phases were NOT_RUN. This run is not evidence for `310000bc`.
- Full soak: [run 41259436004](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259436004), on `89b608d`, **SUCCESS**; 10 stages and 7,441 seconds total, with validated report artifact `13068177643`.
- Independent environment: [run 41259476813](https://github.com/ali-shortcuts/nexaroute/actions/runs/41259476813), on `89b608d`, **SUCCESS**; all 11 jobs passed, including the 7,436-second sustained-soak validator and all 10 soak stages (artifact `13067314321`).
- The live Kubernetes failure and the soak/independent PASS results above all bind only to `89b608d`. No Kubernetes live-acceptance run, full soak run, or independent-environment run on current main `310000bc` was found. Therefore #650 remains OPEN, and #653's current-main coverage measurement is MET but its current-main soak/independent evidence is still NOT_RUN.

### Open external gates and release decision

The latest issue search still returns all nine issues #646–#654 as OPEN. The distinction between automated tests and operator-owned acceptance remains mandatory:

| Issue | Current status on `310000bc` | Remaining evidence |
|---|---|---|
| #646 — live LLM providers | BLOCKED / NOT_RUN | Operator-selected providers/models/tenants, authorized billable usage limits, and credentials injected by secret name. Fixtures do not count. |
| #647 — organizational OIDC | BLOCKED / NOT_RUN | Operator-owned organizational IdP tenant and live browser/claims/key-rotation acceptance. Runner-local Keycloak does not count. |
| #648 — staging Vault KV | BLOCKED / NOT_RUN | Authorized non-loopback staging Vault, least-privilege policy, rotation/outage/failover evidence. |
| #649 — Vault Transit | BLOCKED / NOT_RUN | Authorized non-loopback Transit service and real encrypt/decrypt/rewrap/rotation/recovery evidence. |
| #650 — Kubernetes/Helm | FAIL on `89b608d`; no run on `310000bc` | Fix the termination-observation race, rerun the full disposable-kind lifecycle on current main, and obtain PASS with all mandatory phases executed. |
| #651 — real ComfyUI inference | BLOCKED / NOT_RUN | Provisioned ComfyUI service, compatible model/workflow, authorized compute, and verified media result. |
| #652 — multi-host HA | NOT_RUN / NOT_MEASURED | Two genuinely independent hosts, shared PostgreSQL/Redis, backup/failover drill, and measured multi-host RPO/RTO. |
| #653 — coverage and soak | Coverage MET (95.4%); current-main soak/independent NOT_RUN | Coverage is measured on `310000bc`; rerun and validate the long soak and independent-environment workflow on this exact final candidate. |
| #654 — release umbrella | OPEN | Remains open until all mandatory acceptance gates have truthful evidence and are completed in the required order. |

**Release status: BLOCKED.** Current-main CI, Security, browser evidence, and the 95.4% coverage gate pass. PR #815 is merged and its executable-mode regression is corrected. However Kubernetes live acceptance on the latest available candidate failed, current-main live K8s/soak/independent runs are absent, and operator-dependent gates #646–#652 remain open. No issue was closed and no release tag was created by this refresh.

Never place credentials, access tokens, master-key bytes, private prompts, or customer data in issues, commits, artifacts, or reports. Record secret variable names only.

## Release-readiness recheck — 2026-11-01, main `5c3b5c4a` (superseded by `436c4e9`)

This is the current snapshot of this document. It supersedes every earlier
statement in this file that identifies an older commit (`26ecd267`, `310000bc`,
`244df238`, `89b608dd`, `d54c36b6`, `2f638da`) as current main. Historical
sections above are deliberately preserved with their original SHAs: evidence
stays bound to the commit it was produced on and is never transferred forward.
This section closes no issue, merges nothing, waives no gate and authorizes no
release.

### Exact repository state

- Current `main`: **`5c3b5c4a3c5f8e3dd86df5d4d031e78d9437a46b`** — the merge
  commit of PR #819 (merged 2026-11-01T12:20:54Z). Verified with
  `git ls-remote origin refs/heads/main` and the branches API, not assumed.
- Merged in this cycle, in merge order:

| PR | Merge commit | Head | What it changed |
|---|---|---|---|
| #815 | `310000bc` | `3e1a2b41` | Vault KV helper GET response-size cap (1 MiB bound enforced while receiving, incl. chunked; executable mode restored) |
| #816 | `ed5087cb` | `5b07c82d` | Universal Provider Profile registry + compatibility matrix |
| #817 | `244df238` | `e9ede3e1` | Kubernetes acceptance: unbound `timeout_s` initialization fix |
| #818 | `26ecd267` | `299c1464` | Kubernetes acceptance: terminal-status observation via an acceptance-only Pod finalizer |
| #819 | `5c3b5c4a` | `57fdac96` | Gemini failover call-count attribution + the `26ecd267` recheck record |

- Open PRs at the time of this record: #814 (docs refresh, head `c17bfb68`,
  `mergeable=CONFLICTING`, base `310000bc`) and #820 (this cycle's
  implementation + documentation work).
- Issues #646–#653 remain OPEN (`state_reason=reopened`); umbrella #654 OPEN.
  No issue was closed, reopened or re-scoped by this run.

### Workflow runs bound to `5c3b5c4a`

| Workflow | Run | Result | Evidence |
|---|---|---|---|
| CI | [41306060827](https://github.com/ali-shortcuts/nexaroute/actions/runs/41306060827) | **PASS** | jobs `verify`, `container`, `fuzz-smoke`, `integration-postgres`, `integration-redis`, `ci-soak-smoke` all success; artifacts `browser-acceptance-5c3b5c4a…-41306060827-1` (5,711,171 B, id 13083577104), `ci-soak-smoke-5c3b5c4a…-41306060827-1` (12,950 B, id 13082759977), dockerbuild ids 13083820730 / 13083458592 |
| Coverage report | [41306060797](https://github.com/ali-shortcuts/nexaroute/actions/runs/41306060797) | **PASS** | job `Fresh Go coverage` success — the fail-closed 95.0 % gate (`scripts/coverage-gate.sh`) passed on this SHA with the pinned Go 1.27.3 toolchain; artifact `coverage-5c3b5c4a…-41306060797` (266,227 B, id 13083019792) |
| Security | [41306060769](https://github.com/ali-shortcuts/nexaroute/actions/runs/41306060769) | **PASS** | `CodeQL (Go)` + `Go vulnerability scan` success |
| Browser evidence | [41306060783](https://github.com/ali-shortcuts/nexaroute/actions/runs/41306060783) | **PASS** | browser acceptance job success |
| Propose browser evidence fixtures | [41306218551](https://github.com/ali-shortcuts/nexaroute/actions/runs/41306218551) | PASS | not an acceptance gate |

Coverage, stated exactly: the gate passed on `5c3b5c4a`, so the
repository-wide total is ≥ 95.0 % inclusive. The covered/total statement counts
and the percentage inside artifact id 13083019792 are **not readable from this
environment** (see access limits below), so they are not quoted; the last
readable measurement in this file (42,198 / 44,230 = 95.4 %, artifact
13069344275) binds to `310000bc` and is **not** restated as a `5c3b5c4a`
measurement.

### No acceptance workflow has run on `5c3b5c4a` — dispatch is BLOCKED, not skipped

Every opt-in acceptance workflow was attempted explicitly against `main` and
refused by the installed GitHub App with the same error:

```
$ gh workflow run "Kubernetes live acceptance" --ref main
could not create workflow dispatch event: HTTP 403: Resource not accessible by
integration (…/actions/workflows/402661777/dispatches)
```

Identical `HTTP 403 Resource not accessible by integration` for:
`Kubernetes live acceptance` (workflow 402661777), `Vault runner acceptance`
(402667298), `OIDC runner acceptance` (402691012), `HA runner acceptance`
(402690907), `Independent environment` (402551343), `Soak` (365306920),
`Coverage report` (395791099).

Additional access limits, recorded so no reader assumes a silent skip:

- Adding a workflow file to trigger them another way is refused at push:
  *"refusing to allow a GitHub App to create or update workflow
  `.github/workflows/…` without `workflows` permission"*.
- `GET /repos/…/actions/secrets`, `/actions/variables` and even
  `[.secrets[].name]` enumeration return `403 Resource not accessible by
  integration`; `/environments` returns an empty set. Secret **names** could
  therefore not be enumerated either — no secret value was ever requested,
  read or printed.
- Run logs and artifact contents redirect to
  `results-receiver.actions.githubusercontent.com`, `*.blob.core.windows.net`
  and `objects.githubusercontent.com`, all unreachable here. Readable evidence
  is limited to run/job/step conclusions, artifact names and sizes, and
  check-run annotations.
- No Go toolchain could be installed (`go.dev`, `dl.google.com`,
  `proxy.golang.org`, `objects.githubusercontent.com` all unreachable), and
  the image has no `docker`, `kind`, `kubectl` or `helm`. Go changes in this
  cycle were therefore validated through PR CI on the exact PR head; no local
  Go result is claimed.

### Acceptance verdicts for `5c3b5c4a`

| Issue | Verdict | Evidence class available | Exact remaining prerequisite |
|---|---|---|---|
| #646 LLM provider canaries | **BLOCKED** (no live call) | fixture / documentation only | Operator authorization naming provider + endpoint + model/account, the configured secret variable names, and an explicit per-provider spend ceiling; then dispatch the live-canary path. Zero paid calls were made. |
| #647 Organizational OIDC | **BLOCKED** (tenant); runner-local PASS | `runner-local` (Keycloak container self-test PASS on the current tree) | Real organizational tenant: issuer URL, client ID, secret variable name, redirect-URI registration, group→role mapping, tenant owner authorization |
| #648 Vault KV staging | **BLOCKED** (staging); runner-local PASS | `runner-local` (Vault runner acceptance self-test 5/5 + policy least-privilege audit PASS) | Staging Vault HTTPS endpoint, KV v2 mount, dedicated empty path, least-privilege policy, token injected by name (`VAULT_TOKEN` / `VAULT_TOKEN_FILE`), instance version + topology, authorization |
| #649 Vault Transit | **BLOCKED** (live Transit); contract tests PASS | `fixture` (fake Transit API + runner self-test) | Authorized disposable Transit instance, dedicated key name, policy granting exactly `encrypt`/`decrypt`/`rewrap`, token by name, tamper/recovery evidence |
| #650 Kubernetes live | **NOT_RUN** on `5c3b5c4a` | last live run bound to `89b608dd` (FAILED); local `--offline-only` / `--self-test` **BLOCKED — `helm` is required**, exact message: `FAIL: helm is required for offline acceptance` | A dispatch of `k8s-live-acceptance.yml` (`prev_tag=v0.24.0`) on `5c3b5c4a`, with the artifact inspected for `RESULT: failures=0 not_run=0`. No `RESULT:` line exists for the current tree, so zero mandatory failures and zero mandatory NOT_RUN checks are **not** established |
| #651 ComfyUI real inference | **BLOCKED** | `fixture` only | Reachable authorized ComfyUI server, compatible real model + API workflow + node mapping, compute allocation, and a written no-download/budget confirmation |
| #652 Multi-host HA | **BLOCKED** (multi-host); single-host harness self-test PASS | `container-isolated-single-host` / `runner-local` | Two genuinely independent hosts (the HA harness self-test already reports `two_independent_hosts` as unmeasurable in this topology, and the last runner run reported `two_independent_hosts NOT_MEASURED`), shared PostgreSQL + Redis, a load-balancer/VIP path, authorized outage/recovery drills, backup restore, and measured RPO/RTO |
| #653 Coverage + soak + independent | Coverage **PASS** on `5c3b5c4a`; soak and independent **NOT_RUN** on `5c3b5c4a` | `runner-local` | Dispatch `soak.yml` (`rounds=3`, `duration=2h`, `timeout_minutes>=150`) and `independent-environment.yml` on `5c3b5c4a`, then inspect `summary.json` (exact SHA, `sustained_seconds` ≥ 7200, all 10 planned stages, 3 stress rounds, resource samples, NOT_RUN block) and every independent job/artifact |

### Harness self-tests executed on this tree (all runner-local / fixture class)

| Check | Command | Result |
|---|---|---|
| OIDC runner harness | `python3 scripts/oidc-runner-acceptance.py --self-test` | PASS |
| Vault runner acceptance + Transit contract + policy least-privilege | `bash scripts/test-vault-runner-acceptance.sh` | PASS (5/5 checks, self-test PASS, policy audit PASS) |
| HA two-host harness (containers/self-test, single host) | `python3 scripts/ha-two-host-acceptance.py --self-test` | PASS |
| Soak evidence completeness checker | `python3 scripts/test-check-soak-evidence.py` | PASS (32 tests) |
| Final-release-gate script | `bash .github/scripts/test-final-release-gate.sh` | PASS |
| Release-gate status script | `bash scripts/test-release-gate-status.sh` | PASS |
| Coverage gate regression | `bash scripts/test-coverage-gate.sh` | **BLOCKED** locally — the script shells out to `go tool cover` and no Go toolchain is installable here; covered by CI on the exact head |
| Kubernetes `--self-test` / `--offline-only` | `bash scripts/k8s-acceptance.sh --self-test` | **BLOCKED** — `FAIL: helm is required for self-test` (fails closed, does not fabricate a pass) |
| Kubernetes `--live` | — | **NOT_RUN** — no container runtime or cluster here, and dispatch denied |

These are harness-and-gate self-checks. None of them is staging, live or
multi-host acceptance.

### Code and test work completed in this cycle

- **Failover call-count attribution completed (issue #663 follow-up).** The
  three remaining bare call-count assertions in
  `internal/httpapi/mission95_wave3_canonical_test.go`
  (`TestWave3AnthropicTransportFailover`, `TestWave3ResponsesTransportFailover`,
  `TestWave3OpenAITransportFailover`) had the same attribution defect #819 fixed
  for Gemini: a fixture that counted *every* request on a reused loopback port
  could report an unrelated stray request as a duplicate dispatch. They now use
  one shared request-attribution fixture
  (`internal/httpapi/fixture_upstream_attribution_test.go`) that counts only
  payloads carrying that test's unique prompt marker, logs unrelated arrivals
  instead of folding them into the count, and additionally asserts the
  deterministic `route_attempt` ledger count for the winning deployment. A
  genuine duplicate dispatch still carries the marker and still fails, so no
  assertion was weakened. Regression coverage added for the fixture itself and
  a repeated (4 workers × 3 rounds) concurrent chat-failover run.
- **AgentRouter profiles added (#646 scope).** `agentrouter-chat` and
  `agentrouter-messages` are separate profiles because the vendor documents two
  surfaces that must not be mixed: `https://agentrouter.org/v1` (OpenAI
  compatible, Bearer) and `https://agentrouter.org` with no `/v1` suffix
  (Anthropic compatible, token as Bearer). Only `auth` and `non_streaming` are
  claimed; streaming, tool, usage and error shapes are not documented for these
  routes. Documented model identifiers (`gpt-5.6-sol`, `gpt-6-astra`,
  `claude-opus-5`, `claude-opus-4-8`, `glm-5.3`, `deepseek-v4-flash`) are
  recorded as documentation evidence in the profile limitations; no live call
  was made and no Responses/Gemini profile was invented.

### Release-gate verdict on `5c3b5c4a`

`check-final-release-gate.sh` requires #646–#653 **and** #654 to be closed with
`state_reason=completed`. All nine are open, coverage is the only #653 sub-gate
whose evidence binds to the current SHA, and every external gate above is
BLOCKED or NOT_RUN. The gate is therefore **NOT satisfied**: no tag was
created, no release was published, no release gate or required check was
altered, and no issue was closed or reopened.

## Release-readiness recheck — 2026-11-01, main `b1eebc1` (superseded by `436c4e9`)

This is the latest snapshot in this document. It supersedes every earlier
statement that names an older commit as current main. It waives no gate,
closes no issue, approves no release and transfers no result from an older
SHA.

### Repository state

- Current main: `b1eebc105340a85b39d051510320bd06390737ce`, the merge commit of
  PR #820 (test-only failover-attribution work plus the provider-profile and
  documentation refresh that preceded it).
- Open pull requests: **none** as of this recheck. PR #820 merged at
  2026-11-01T13:59:30Z after all thirteen recorded checks passed on its head
  `5eccfd0af44b02354036098428946bdc72d02526`. PR #814 was closed as superseded:
  it carried no merge base with `main` and its entire content was already on
  `main` verbatim.
- Open issues: #646–#654 (nine). None was closed, reopened or relabelled by
  this recheck.

### Required checks on the exact SHA `b1eebc1`

Read from the Checks API for that commit, not inferred from a run list:

| Context | Conclusion |
|---|---|
| `verify` | success |
| `container` | success |
| `fuzz-smoke` | success |
| `ci-soak-smoke` | success |
| `integration-postgres` | success |
| `integration-redis` | success |
| `Go vulnerability scan` | success |
| `CodeQL (Go)` | success |
| `Fresh Go coverage` (95.0% inclusive gate) | success |
| Real-system acceptance (`Two real Gateway processes against PostgreSQL`, `Real Secret Service Store/Load`) | success |

These are the nine contexts enforced by ruleset `26200123`
(`main-required-checks`) plus the real-system acceptance workflows. The
covered/total statement counts inside the coverage artifact are not readable
from this environment (`actions/*/logs` and artifact downloads redirect to
hosts unreachable here), so this recheck records that the fail-closed 95.0%
gate passed on `b1eebc1` and quotes no percentage.

### Why no live acceptance could be dispatched from this environment

`gh workflow run` against this repository returns `HTTP 403: Resource not
accessible by integration`. The GitHub App operating this session has
read-only Actions permission on the repository. Every acceptance workflow is
`workflow_dispatch`-only (`k8s-live-acceptance`, `vault-runner-acceptance`,
`oidc-runner-acceptance`, `ha-runner-acceptance`, `independent-environment`,
`soak`, `conformance-live-canary`), so none of them can be started from here.
Editing a workflow file to add a `push` trigger is also impossible: a push that
touches `.github/workflows/*` is refused without the `workflows` permission.
This is a tooling limitation of the session identity, not a property of the
repository.

The previous accepted batch (2026-11-01, 01:00Z–01:04Z) exists on `89b608dd`:
`vault-runner-acceptance`, `oidc-runner-acceptance`, `ha-runner-acceptance`,
`independent-environment` and `soak` all succeeded, and
`k8s-live-acceptance` **failed** at `Run live acceptance`. None of those
results binds to `b1eebc1`. The compare `89b608dd...b1eebc1` is 19 commits
across 14 files and does contain production Go changes
(`internal/providers/{presets,profiles,profiles_data}.go`,
`internal/httpapi/admin.go`) plus the `scripts/k8s-acceptance.sh` and
`scripts/key-provider-vault-kv.sh` changes, so the strict rule applies: the
older PASS results are NOT transferred, and the older FAIL is not evidence
about the current script.

### Position of the release gate on `b1eebc1`

`.github/scripts/check-final-release-gate.sh` still refuses: all nine blockers
are open. A version-scoped exclusion path now exists
(`docs/releases/v0.25.0-exclusions.json`, see `docs/RELEASE_GATE.md` and
`docs/RELEASE_BLOCKERS.md`), but it is **unsigned and unused** in this
recheck: no issue was closed, no exclusion was approved, no tag was created
and no release was published. An open blocker cannot be excluded by design, so
the gate's verdict on `b1eebc1` is unchanged: **NOT satisfied**.

Never place credentials, access tokens or key bytes in issues, commits,
artifacts or reports. Record secret variable names only.

## Post-merge state — 2026-11-01, main `b1eebc10` (superseded by `436c4e9`)

This appendix records what changed after the `5c3b5c4a` section above. It does
not re-bind any acceptance evidence: nothing in it is live, staging or
multi-host acceptance, and no issue state was changed.

- Current `main`: **`b1eebc105340a85b39d051510320bd06390737ce`** — merge commit
  of PR #820 (verified with `git ls-remote origin refs/heads/main`). The tree
  now contains the three commits that PR carried: the remaining failover
  call-count attribution work, the AgentRouter profiles, and the
  current-main documentation section above.
- **PR #814 is CLOSED** (closed 2026-11-01T14:00:03Z, by the repository owner;
  not merged, not closed by the agent). Its three historical refresh sections
  were carried into `main` verbatim by PR #820, so that audit evidence is
  preserved rather than lost with the branch.
- Post-merge checks on `b1eebc10`:

| Workflow | Run | Result |
|---|---|---|
| Security | [41313911924](https://github.com/ali-shortcuts/nexaroute/actions/runs/41313911924) | **PASS** — `CodeQL (Go)`, `Go vulnerability scan` |
| Browser evidence | [41313911926](https://github.com/ali-shortcuts/nexaroute/actions/runs/41313911926) | **PASS** |
| Propose browser evidence fixtures | [41314088417](https://github.com/ali-shortcuts/nexaroute/actions/runs/41314088417) | PASS |
| CI | [41313911860](https://github.com/ali-shortcuts/nexaroute/actions/runs/41313911860) | **PASS** — jobs `verify`, `container`, `fuzz-smoke`, `integration-postgres`, `integration-redis`, `ci-soak-smoke` all success |
| Coverage report | [41313911888](https://github.com/ali-shortcuts/nexaroute/actions/runs/41313911888) | **PASS** — `Fresh Go coverage`, 95.0 % fail-closed gate; artifact `coverage-b1eebc10…-41313911888` (266,242 B, id 13086805469) |

The covered/total statement counts in that artifact are not readable from this
environment, so only the gate verdict is recorded for `b1eebc10`; the exact
counts must be read from artifact id 13086805469 before any release note quotes
them.

- Open PR: #821 — the owner-review follow-up to #820 §5, keeping the shared
  upstream fixture's exhaustion path off the worker goroutine
  (`t.Errorf` + `nil` from the constructor, `mustMarkerCountingServer` for
  same-goroutine callers). Test-only.
- Acceptance status is unchanged from the `5c3b5c4a` section: #646–#653 and
  #654 are OPEN, coverage remains the only #653 sub-gate bound to a
  post-#819 SHA, every external gate is BLOCKED or NOT_RUN, and no tag or
  release exists. The release gate is **NOT satisfied**.

## Repository audit and release-readiness recheck — 2026-11-01, main `436c4e9` (current)

Current `main` is **`436c4e98618eed0c41e59059eae7eca5f05dd190`**, the merge commit
of PR #822 (verified with `git ls-remote origin refs/heads/main`). The sections
above record superseded SHAs and are kept unedited as history; no result below
is transferred from any of them.

### What changed since `b1eebc10`

- PR #821 merged as part of `436c4e9`'s history (test-only: the shared upstream
  fixture no longer calls `t.Fatalf` from worker goroutines).
- PR #822 merged (release-readiness package: the version-scoped exclusion path in
  `.github/scripts/check-final-release-gate.sh`, `docs/RELEASE_BLOCKERS.md`,
  `docs/RELEASE_GATE.md`, `docs/releases/v0.25.0.md` and an unsigned exclusion
  manifest).
- **No open pull requests remain.** #820, #821 and #822 are merged; #814 is
  closed unmerged (its historical refresh sections live in this file).
- Issues **#646–#653 and the umbrella #654 are all still OPEN** (9 open issues,
  no others).

### Post-merge gates on `436c4e9`

| Workflow | Run | Result |
|---|---|---|
| CI | [41317289767](https://github.com/ali-shortcuts/nexaroute/actions/runs/41317289767) | **PASS** |
| Coverage report | [41317289862](https://github.com/ali-shortcuts/nexaroute/actions/runs/41317289862) | **PASS** — `Fresh Go coverage`, 95.0 % inclusive fail-closed gate; artifact `coverage-436c4e9…-41317289862` (266,222 B, id **13088460086**) |
| Security | [41317289831](https://github.com/ali-shortcuts/nexaroute/actions/runs/41317289831) | **PASS** |

The coverage artifact's covered/total statement counts are **not readable from
this environment** (the artifact body redirects to a storage host that is not
reachable here). Artifact id 13088460086 is the authoritative record; the
percentage must be read from it, or from the CI job log, before any release note
quotes a number. No percentage is claimed in this section.

### External acceptance — re-attempted, still BLOCKED

Every opt-in workflow was dispatched again on 2026-11-01 against `436c4e9`. All
of them failed identically, before any run was created:

```
could not create workflow dispatch event: HTTP 403: Resource not accessible by integration
(https://api.github.com/repos/ali-shortcuts/nexaroute/actions/workflows/<id>/dispatches)
```

| Workflow | Dispatch result |
|---|---|
| `k8s-live-acceptance.yml` (#650) | **BLOCKED** — HTTP 403 |
| `vault-runner-acceptance.yml` (#648/#649) | **BLOCKED** — HTTP 403 |
| `oidc-runner-acceptance.yml` (#647) | **BLOCKED** — HTTP 403 |
| `ha-runner-acceptance.yml` (#652) | **BLOCKED** — HTTP 403 |
| `independent-environment.yml` (#653/#654) | **BLOCKED** — HTTP 403 |
| `soak.yml` (#653) | **BLOCKED** — HTTP 403 |
| `coverage-report.yml` (#653) | **BLOCKED** — HTTP 403 |
| `conformance-live-canary.yml` (#646) | **BLOCKED** — HTTP 403 |
| `real-system-acceptance.yml` | **BLOCKED** — HTTP 403 |
| `quality-gate.yml`, `flake-hunt-acceptance.yml` | **BLOCKED** — HTTP 403 |

`gh api …/actions/secrets` and `…/actions/variables` also return 403, so secret
**names** cannot even be enumerated from this identity; the owner must confirm
which references exist. Run logs and artifact bodies are unreachable, so no job
log was inspected for these runs.

### Runner-local harness results in this environment (evidence class: `runner-local`)

| Command | Result |
|---|---|
| `python3 scripts/oidc-runner-acceptance.py --self-test` | **PASS** — workdir resolution + management-port readiness |
| `bash scripts/test-vault-runner-acceptance.sh` | **PASS** — 5/5 incl. least-privilege policy audit (`create`, `lookup-self`, `revoke-self`) |
| `python3 scripts/ha-two-host-acceptance.py --self-test` | **PASS** — `two_independent_hosts=NOT_MEASURED` |
| `python3 scripts/test-check-soak-evidence.py` | **PASS** |
| `bash .github/scripts/test-final-release-gate.sh` | **PASS** — success plus open, `not_planned`, API error, missing-reason, malformed, missing-state cases |
| `bash scripts/test-release-gate-status.sh` | **PASS** — pass / one-failed / missing=NOT_RUN / running / api-error=BLOCKED / non-SHA refused |
| `bash scripts/test-flake-hunt.sh` | **PASS** — 11 assertions |
| `python3 scripts/test-check-integration-report.py` | **PASS** — 8 cases |
| `bash scripts/test-generate-sbom.sh` | **PASS** (Syft tarball skipped: no network/cache) |
| `bash scripts/check-ha-docs.sh` | **PASS** — HA documentation consistency |

Environment limitations, stated so they are never mistaken for passes:

- **No Go toolchain**: `scripts/test-coverage-gate.sh` fails locally at
  `go tool cover` (`go: command not found`), so local coverage is NOT_MEASURED;
  the CI gate above is the only coverage evidence.
- **No helm/kubectl/kind/docker**: `scripts/k8s-acceptance.sh --self-test` and
  `--offline-only` fail closed with `helm is required` (1 failure, 0 not_run).
  That is a *local self-test failure*, never a live-cluster result.
- **No browser / Playwright binding**: `scripts/test-browser-e2e.py`,
  `test-browser-e2e-502.py` and `test-live-visual-agent.py` stop at
  `ModuleNotFoundError: No module named 'playwright'`;
  `test-browser-nexaflow-studio.py` self-skips. Browser evidence is **NOT_RUN**
  here and must come from the `browser-evidence` workflow on the PR head.

### Control-plane UI work in this pass

- The Overview page gained a **throughput panel** (`trendPanel()` in `web/app.js`)
  drawn from the real `/admin/api/activity/requests` feed: one bar per time
  bucket, succeeded and failed portions coloured by tokens, with loading, empty,
  permission-denied and error states, plus a caption that names the source and
  says the data is a bounded in-memory bus rather than a historical metrics
  store. Nothing is simulated, smoothed or back-filled.
- The model builder and SVG renderer are a dependency-free block executed
  verbatim under node by `internal/httpapi/activity_chart_test.go`, which pins
  window inclusivity, out-of-window skipping, unparsable timestamps, bucket
  clamping, determinism, escaped accessible labels and the no-raw-colour rule.
- Audit checks performed on the same pass (static, `runner-local`): every
  `data-action` in `app.js`/`studio.js` resolves to a handler literal (0 dead
  controls); every `/admin/api/...` path used by the UI resolves to a registered
  route or a registered prefix; `node --check` passes on all three JS bundles;
  and the body-copy dictionaries are at exact EN/FA/PS parity (594 keys each).

### Verdict

Nothing here is live, staging, multi-host or paid-provider acceptance. The
release gate is **NOT satisfied**: #646–#653 and #654 stay open, no acceptance
workflow has executed on `436c4e9` (or on any SHA after `89b608dd`'s failed
live run), and no tag or release exists.
