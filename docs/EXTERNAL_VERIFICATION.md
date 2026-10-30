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
| OIDC identity provider (browser login) | EXECUTED LOCALLY 2026-10-24; deterministic local coverage expanded 2026-10-27 (issuer pinning, JWKS overlap/rotation, unique state/nonce/PKCE, unmapped/ambiguous roles, logout revocation failure, audit/persistence failure) — live organizational tenant remains **BLOCKED/UNVERIFIED** because no operator-owned credentials/config were supplied | §1 below; `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §3; `docs/reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md` |
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
logout revocation, and session/audit persistence failures. These are
local protocol tests, not live-tenant evidence.

The exact live-tenant checklist is **BLOCKED** until an operator supplies
a real tenant and credentials:
[`OIDC_LIVE_TENANT_ACCEPTANCE_647.md`](reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md).
No credentials or configuration were available for this pass, so nothing
below may be marked live-verified from the local suite alone.

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
