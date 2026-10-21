# Opt-in live provider/OIDC acceptance harness (#611)

Parent audit: [#593](https://github.com/ali-shortcuts/nexaroute/issues/593), which remains open.
This slice adds and verifies the harness only. It does **not** change provider
protocol semantics and does **not** promote any provider or identity-provider
claim to live-verified.

## What exists

`nexaroute external-accept` composes the existing bounded provider acceptance
path (`provider-accept`) with a new OIDC issuer/JWKS/token check. Every
target must be supplied explicitly by the operator. There are no built-in
provider or identity-provider endpoints.

The command is dispatched from `cmd/gateway/main.go` and implemented in
`cmd/gateway/external_accept.go`. OIDC protocol use is the same
`go-oidc` verification path used by the admin login code; the harness adds no
new production authentication behavior.

## Exact invocations

Provider only, using a configured provider and its `api_key_env`:

```bash
export TENANT_STAGING_API_KEY='...'   # operator shell only; never commit it
nexaroute external-accept \
  --config /etc/nexaroute/config.json \
  --provider tenant-staging \
  --model test-model \
  --confirm-live \
  --out docs/reports/verification-slices/provider-staging-$(date -u +%Y%m%dT%H%M%SZ).json
```

OIDC discovery + JWKS only, using the configured `admin.oidc` issuer:

```bash
export NEXAROUTE_OIDC_CLIENT_SECRET='...'   # name must match admin.oidc.client_secret_env
nexaroute external-accept \
  --config /etc/nexaroute/config.json \
  --oidc \
  --confirm-live \
  --out docs/reports/verification-slices/oidc-staging-$(date -u +%Y%m%dT%H%M%SZ).json
```

OIDC discovery + JWKS + verification of an operator-issued test ID token:

```bash
export NEXAROUTE_ACCEPT_ID_TOKEN='...'      # an ID token from the test issuer
nexaroute external-accept \
  --config /etc/nexaroute/config.json \
  --oidc \
  --oidc-id-token-env NEXAROUTE_ACCEPT_ID_TOKEN \
  --confirm-live \
  --out docs/reports/verification-slices/oidc-staging-$(date -u +%Y%m%dT%H%M%SZ).json
```

OIDC issuer supplied only by environment (no config file target):

```bash
export NEXAROUTE_ACCEPT_OIDC_ISSUER='https://idp.staging.example'
nexaroute external-accept \
  --oidc-issuer-env NEXAROUTE_ACCEPT_OIDC_ISSUER \
  --oidc-client-id staging-client \
  --oidc-audience nexaroute-control-plane \
  --confirm-live \
  --out docs/reports/verification-slices/oidc-staging-$(date -u +%Y%m%dT%H%M%SZ).json
```

Both provider and OIDC in one report:

```bash
nexaroute external-accept \
  --config /etc/nexaroute/config.json \
  --provider tenant-staging \
  --oidc \
  --confirm-live \
  --out docs/reports/verification-slices/staging-$(date -u +%Y%m%dT%H%M%SZ).json
```

`--all-providers` is available instead of `--provider` when an operator
deliberately wants every enabled provider. `--provider` and `--all-providers`
are mutually exclusive. `--model` narrows the probe to configured model ids;
without it, enabled configured models are used up to `--max-models`.

## Opt-in, timeouts and report

- `--confirm-live` is mandatory. Without it the command exits `2` before it
  loads targets or makes any network request, and it writes no report.
- Selecting at least one of `--provider`, `--all-providers`, `--oidc`, or
  `--oidc-issuer-env` is mandatory.
- Provider requests use bounded discovery plus one-token probes. Defaults:
  `--timeout=15s` (hard max `60s`), `--max-models=10` (max `100`),
  `--max-calls=20` (max `100`). The overall run is capped at `5m`.
- OIDC discovery, JWKS fetch, and optional ID-token verification use the same
  per-request timeout and a `5m` overall cap.
- Reports are written atomically with mode `0600`; parent directories are
  created with mode `0700`.
- The report schema is `nexaroute.external-acceptance.v1`.

Exit codes:

| Exit | Outcome |
|---|---|
| `0` | `PASS` — every selected check produced a successful live observation. |
| `1` | `FAIL` — at least one selected check ran and failed. |
| `3` | `BLOCKED` — a selected check could not run because required credentials, service input, or environment configuration was absent. |
| `4` | `NOT_RUN` — no selected check produced an observation. |
| `2` | Usage/setup error. No report is written. |

Every check has one of:

- `PASS` — ran against the supplied target and passed.
- `FAIL` — ran and failed.
- `BLOCKED` — did not run because required input was missing; the reason names
  the missing environment variable name or configuration field, never its value.
- `NOT_RUN` — the check was not selected, or an interactive step was not
  automated. An unselected OIDC check is included as `NOT_RUN` so provider-only
  reports still state that OIDC was not tested.

The top-level `evidence_class` is `external`, `loopback`, `mixed`, or `none`.
Loopback targets are useful for harness fixtures, but their `PASS` results are
explicitly marked as harness evidence, not external interoperability evidence.

## Redaction rules

The report never contains secret values. It records only environment variable
names, booleans for "credential configured", sanitized endpoints, HTTP status,
latency, model ids, key counts, and redacted error text.

- Known values are redacted from provider credentials, credential pools,
  provider headers, AWS secret/session keys, proxy user info, the configured
  OIDC client secret, and the supplied ID token.
- Authorization-style values are redacted even when not previously known,
  including `Authorization: Bearer ...`, `Basic ...`, `Digest ...`,
  `Negotiate ...`, `x-api-key`, `api-key`, and `x-goog-api-key`.
- URLs are reduced to scheme, host, and path; user info, query strings, and
  fragments are removed.
- Provider error text is bounded and redacted before it reaches the report.
  OIDC bodies are never copied into the report; only status, counts, and
  redacted error summaries are recorded.
- Tokens, subjects, group values, and role-mapping claim values are never
  written to the report. ID-token validation records only pass/fail and a
  role count. ID-token verification does not check a nonce because the harness
  did not initiate the authorization request.

The operator should still review a report before attaching it to an issue.
Hostnames, tenant ids, and model ids can themselves be sensitive metadata.

## Mock-versus-live boundary

- **Fixture tests** in `cmd/gateway/external_accept_test.go` use loopback
  `httptest` servers for a fake OpenAI-compatible provider and a fake OIDC
  issuer/JWKS. They prove opt-in enforcement, request budgets, report schema,
  status mapping, URI/endpoint validation, ID-token verification, and
  redaction. They are **not** external interoperability evidence.
- **Live runs** require `--confirm-live` plus real operator-supplied staging
  endpoints and credentials. Only those runs can produce
  `evidence_class=external`.
- A provider `PASS` proves only that the selected configured endpoint answered
  bounded discovery/probes. It is not full protocol conformance.
- OIDC discovery/JWKS `PASS` does not exercise an interactive Authorization
  Code + PKCE browser login. Without `--oidc-id-token-env`, token issuance is
  reported as `NOT_RUN`; the interactive login step is always `NOT_RUN` in this
  harness. Use the procedure in `docs/EXTERNAL_VERIFICATION.md` for the browser
  flow.

## Current results (2026-10-21)

This work ran in a GitHub Actions runner with no staging provider credentials,
no staging OIDC issuer, and no staging client secret. No external service was
contacted by the authoring run.

| Target | Capability | Status | Reason |
|---|---|---|---|
| Provider tenant (staging) | discovery + one-token generation | **BLOCKED/UNVERIFIED** | No staging provider endpoint/credential was supplied to the runner. |
| OIDC issuer (staging) | discovery + JWKS | **BLOCKED/UNVERIFIED** | No staging issuer/client configuration was supplied to the runner. |
| OIDC issuer (staging) | operator-issued ID token verification | **BLOCKED/UNVERIFIED** | No staging ID token was supplied. |
| OIDC issuer (staging) | interactive Authorization Code + PKCE login | **NOT_RUN/UNVERIFIED** | Not automated by this harness; browser procedure remains in `docs/EXTERNAL_VERIFICATION.md`. |
| Harness itself | provider/OIDC/report/redaction behavior | **PASS (fixtures only)** | See test command and results below. |

The provider and OIDC integrations therefore remain **UNVERIFIED** against real
staging systems. This report does not close that external-verification gap; it
makes the gap executable and machine-readable for an operator who has the
required access.

## Fixture and regression evidence

Commands:

```bash
go test ./cmd/gateway -run '^TestExternalAccept' -count=1
go test ./internal/providers/livecanary -run 'TestSecretRedactor' -count=1
./scripts/verify.sh
```

Recorded result for this slice: both focused test commands passed. Full
`./scripts/verify.sh` result and exact PR SHA are recorded on the pull request;
this document does not claim CI results that were not observed.

## Remaining blockers

1. A staging provider endpoint, test model, and credential supplied through an
   environment variable named by the config.
2. A staging OIDC issuer, test client id/audience, and either a client secret or
   an operator-issued test ID token. The interactive browser login still needs
   the manual procedure in `docs/EXTERNAL_VERIFICATION.md`.
3. After a real run, review the generated JSON, record the UTC timestamp,
   gateway SHA, exact command (without secret values), report path, and per-check
   statuses on #611. Do not mark the parent #593 external checks complete from a
   fixture run.
