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
