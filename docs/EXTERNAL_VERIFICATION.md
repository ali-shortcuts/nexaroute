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
- HA runner: [run 41224779612](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224779612), artifact `13051392453`: container-isolated single-host PASS. The report explicitly records two independent hosts and load-balancer failover as NOT_MEASURED. The observed RPO/RTO figures apply only to this single-host container drill and must not be represented as multi-host values.

### Kubernetes live acceptance — FAIL

- [Kubernetes live acceptance run 41224660001](https://github.com/ali-shortcuts/nexaroute/actions/runs/41224660001), artifact `13052096015`, on main `d54c36b6`: **FAIL**, with two failures and two mandatory safe-failure phases marked NOT_RUN.
- The upgrade log shows the new Pod Ready on ReplicaSet `7f6b6cd5bc`, while the harness waited for the old hash `6c848db5c`. Application checks subsequently returned the expected successful responses, but this does not erase the readiness failure.
- The old termination measurement reported 120659ms for the blocking `kubectl delete` client operation. That duration includes API/controller finalization and does not directly establish when the containers exited.
- PR [#813](https://github.com/ali-shortcuts/nexaroute/pull/813) contains a revision-transition wait and revised, fail-closed timing based on Kubernetes deletion-grace metadata and every regular container's `finishedAt`. The 120000ms threshold is unchanged. Latest recorded PR head at this refresh: `a3d0ee960722936a186bce45da4378b0c102f48b`. Its required CI is still running, and no fresh live kind run on that PR head has been recorded. The issue remains open.

### Full soak and independent environment

- Full soak: [run 41225317366](https://github.com/ali-shortcuts/nexaroute/actions/runs/41225317366), main SHA `d54c36b6`, was IN_PROGRESS at the time of this refresh. No PASS is claimed until the 7,200-second sustained phase, all required stages, artifact, and exact commit binding are independently validated.
- Independent environment: [run 41225370775](https://github.com/ali-shortcuts/nexaroute/actions/runs/41225370775), main SHA `d54c36b6`, was IN_PROGRESS at the time of this refresh. Its terminal job results and artifacts still need validation.
- Any older soak or independent-environment result from `dccd254` or an earlier SHA is historical evidence only and is not transferred to `d54c36b6`.

### Mandatory external acceptance status

The owner has recorded that all seven external acceptance gates are mandatory for the first stable release; no gate is to be waived or downgraded. The latest issue-state recheck found #646–#654 all OPEN.

| Issue | Current result | Missing evidence / input |
|---|---|---|
| #646 — live LLM providers | BLOCKED / NOT_RUN | Written selection of each provider, exact model and tenant/account, endpoints, secret names only, maximum calls/tokens, and approved spend ceiling. No paid upstream call is inferred from fixtures. |
| #647 — organizational OIDC | BLOCKED / NOT_RUN | An operator-owned organizational IdP tenant, client/configuration, approved secret injection, and a live browser acceptance report. Runner Keycloak is not tenant acceptance. |
| #648 — staging Vault KV | BLOCKED / NOT_RUN | Authorized non-loopback staging Vault endpoint, KV v2 mount and dedicated empty path, least-privilege policy, and approved token injection by secret name. Runner-loopback evidence is not staging. |
| #649 — live Vault Transit | BLOCKED / NOT_RUN | Authorized non-loopback Transit instance, dedicated key, least-privilege encrypt/decrypt/rewrap policy, approved token injection, and actual API/rotation/recovery evidence. Runner-loopback Transit checks do not prove staging, HA, HSM, or KMS readiness. |
| #650 — Kubernetes/Helm | FAIL on `d54c36b6`; PR validation pending | Review and validate PR #813, then run the complete disposable-kind lifecycle on the reviewed merged candidate. The previous run's mandatory safe-failure phases remain NOT_RUN. |
| #651 — real ComfyUI inference | BLOCKED / NOT_RUN | Provisioned ComfyUI service, exact compatible model/workflow and node mapping, suitable authorized compute, and written usage/budget authorization. No fixture is a real media inference. |
| #652 — multi-host HA | NOT_RUN / NOT_MEASURED | Two genuinely independent authorized hosts, shared disposable PostgreSQL/Redis topology, backup/restore inputs, and measured multi-host RPO/RTO. Same-host container metrics are not multi-host evidence. |
| #653 — coverage and soak | Coverage MET on `d54c36b6`; soak and independent workflow IN_PROGRESS | Validate the current-main soak and independent run on their exact SHA; the external dependencies #646–#652 remain mandatory. |
| #654 — umbrella | OPEN | Remains the final tracker. The release gate requires #646–#653 to be closed with `state_reason=completed`, then #654 completed last. No manual state change substitutes for evidence. |

### Release decision

The latest published stable release remains [v0.24.0](https://github.com/ali-shortcuts/nexaroute/releases/tag/v0.24.0), published 2026-10-22. No newer stable tag or release was found. **Release status: BLOCKED.** Main CI, security, and coverage success are necessary but insufficient while Kubernetes is failing, soak/independent validation is pending, and mandatory live external acceptance remains unmet.

Never place credential values, tokens, master-key bytes, private prompts, or customer data in issues, commits, artifacts, or reports. Record secret variable names only.
