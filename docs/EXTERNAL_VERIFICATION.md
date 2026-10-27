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
| OIDC identity provider (browser login) | EXECUTED LOCALLY 2026-10-24 — full login flow PASS against a conforming local IdP (discovery, PKCE S256, JWKS verify, role mapping, session, fail-closed negatives); a live organizational tenant remains UNVERIFIED | §1 below; `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §3 |
| HashiCorp Vault (KV helper) | EXECUTED LOCALLY 2026-10-24 — real Vault v2.2.1 (dev mode) round-trip via the shipped helper PASS; gateway key custody + decrypt verify PASS; wrong token fails closed; a production Vault cluster remains UNVERIFIED | §2 below; `docs/reports/LOCAL_ACCEPTANCE_2026-10-24.md` §5 |
| HashiCorp Vault Transit envelope mode | NOT IMPLEMENTED — the helper loads a raw 32-byte master key into the gateway process; key material is never claimed to stay inside Vault | §2 below |
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
trusting a process-lifetime cache).

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

**Transit boundary (exact):** Vault Transit envelope mode — where
key material never leaves Vault — is **not implemented** and cannot
be used with the current provider contract, which loads a raw
32-byte master key into the gateway process at startup. Deployments
that require key material to never leave the HSM/KMS boundary must
not treat NexaRoute as satisfying that requirement today.

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
