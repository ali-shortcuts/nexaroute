# OIDC runner acceptance, issue 647 — status: NOT_RUN

This document describes a runner workflow, not a result. Nothing
in it is OIDC acceptance evidence yet. The run itself is
NOT_RUN until an owner merges the workflow PR (separate
approval), dispatches it on GitHub Actions, and sends the run
URL and the artifact back. Only then can this document be
completed from the observed result, and only within the limits
stated here.

## Why a runner workflow

This environment has no organizational identity provider, and
a local in-process IdP test must never be labelled tenant
acceptance. The agreed path is therefore: a workflow in a PR,
run by the owner on a GitHub-hosted runner (which has Docker),
result sent back, and the report completed from that result.
The workflow runs a new script,
`scripts/oidc-runner-acceptance.py`, against a disposable
Keycloak container; it does not reuse any tenant credential.

## What the workflow does

On a GitHub-hosted runner, `ubuntu-24.04`, `workflow_dispatch`
only, `contents: read`:

1. Build the gateway binary (`CGO_ENABLED=0 go build
   -trimpath -o oidc-runner-gateway ./cmd/gateway`).
2. Run the script, which:
   - Starts Keycloak (`quay.io/keycloak/keycloak:26.0`,
     `start-dev`, health enabled) in a container on loopback,
     and records its real version from the Keycloak Admin
     `/admin/serverinfo` endpoint, never from the image tag.
   - Creates realm `nexaroute`, confidential client
     `nexaroute-control-plane` with redirect URI
     `http://127.0.0.1:<gateway-port>/admin/auth/oidc/callback`,
     groups `nexaroute-admins` and `nexaroute-viewers`, and
     three test users: `oidc-admin-user` (in the admin group),
     `oidc-viewer-user` (in the viewer group), and
     `oidc-unmapped-user` (in no group).
   - Creates a group-membership protocol mapper that puts a
     top-level `groups` claim into ID tokens. This matters:
     the gateway reads exactly one claim name
     (`admin.oidc.role_claim`); a nested claim alone, such as
     `realm_access.roles` with no top-level claim, is denied
     by the gateway, so the mapper shape is part of the
     evidence.
   - Starts the real gateway with `admin.oidc` set to that
     Keycloak issuer, `role_claim` `groups`, and role
     mappings `nexaroute-admins -> admin`,
     `nexaroute-viewers -> viewer`, over loopback HTTP.
   - Drives the real Authorization Code + PKCE login flow:
     gateway `/admin/auth/login`, Keycloak login form, POST
     credentials, gateway `/admin/auth/oidc/callback`,
     session cookie. Cookies are handled manually, because
     the gateway sets Secure cookies that a cookie jar would
     not send back over loopback HTTP; the script sends the
     session cookie explicitly.
   - Checks, with status codes only:
     - admin login succeeds, audit API 200, snapshot API 200
     - viewer login succeeds, snapshot API 200, audit API 403
     - unmapped login ends 401 with no session cookie
     - logout (with a CSRF token) revokes the session, and
       the audit API afterwards is 401
3. Validate the summary: schema, evidence class
   `runner-keycloak`, result PASS, a recorded Keycloak
   version, and every check PASS.
4. Upload, with `if: always()`, exactly one file:
   `oidc-runner-reports/oidc-runner-summary.json`, artifact
   name `oidc-runner-reports-<sha>`, retention 90 days.

## What the run does NOT prove

- NOT a real organizational tenant. The Keycloak instance is
  a disposable container on the same runner, started in dev
  mode, with disposable test users. It must never be cited as
  tenant acceptance.
- NOT Entra ID, Okta, Auth0, or Google. Provider-specific
  claim shapes, clock-skew policy, and key rotation on those
  providers are NOT tested here.
- NOT JWKS rotation, session expiry, or audit-chain integrity
  in a live setting. Those are covered by gateway unit tests,
  not by this runner run, and the summary says so.
- NOT production. No real user, group, or credential is
  involved.

After a successful run, issue 647 stays open. The live
organizational tenant step remains BLOCKED until an operator
runs the checklist on a real tenant and records it.

## How the owner runs it

1. Merge the workflow PR (separate owner approval; opening
   the PR is not a merge, and this document is not a merge
   request).
2. GitHub Actions → "OIDC runner acceptance" → Run workflow.
3. Download the `oidc-runner-reports-<sha>` artifact.

## What to send back

- The run URL.
- The artifact, or at least `oidc-runner-summary.json`.
- The job-log excerpt with the PASS/FAIL lines.

The summary alone is not enough to complete anything. On
failure, send the FAIL line and the step log, and do NOT
silently re-run, because a re-run overwrites the first
failure's evidence. If a check fails, the failure is the
result, not a reason to weaken a check.

## Safety

No real secret is used. Every password and the client secret
are generated inside the run, kept only in a temporary
directory with mode 0700 (files 0600), and destroyed with
the container when the script exits. The summary contains
test usernames and group names only, plus status codes and
callback endpoint paths (query strings are dropped); never a
password, a client secret, an authorization code, a token, or
a session value. The working directory (config, security database,
gateway log) is deliberately NOT uploaded. The workflow reads
no repository secret.

## Update 2026-10-31: first runner dispatch failed; harness defects fixed

The first dispatch (run
[41207000118](https://github.com/ali-shortcuts/nexaroute/actions/runs/41207000118)
on main `2f638da`) failed with `run.aborted` and no script summary. The
workflow trap wrote a minimal FAIL summary. Investigating the harness
found four deterministic defects, all fixed here:

1. **Workspace deletion bug.** `work = pathlib.Path(os.environ.get(
   "OIDC_RUNNER_WORKDIR", "")) or None` treated `Path(".")` as truthy,
   so with the variable unset the script used the repository root as its
   work directory and removed it in `finally` — deleting the summary the
   workflow then tried to upload. It now falls back to a fresh temp dir,
   refuses to remove the cwd, and has a self-test for both.
2. **Wrong readiness port.** Keycloak 25+ serves `/health/*` on its
   management port (9000 in the container), not on 8080. The script now
   publishes and polls 9000; a run that would otherwise time out now
   reaches Keycloak 26.0.8.
3. **Admin API sent as form data.** `kc_admin` passed a dict to a helper
   that form-encodes dicts, so Keycloak answered `415 Unsupported Media
   Type`. Admin requests are now JSON.
4. **Automatic redirects hid the flow.** `urllib` followed redirects
   internally, so the form POST jumped straight to the gateway callback
   without capturing intermediate cookies, and mapped users got 401.
   The client now uses a no-redirect opener and drives every hop
   explicitly. Also: test users now carry the profile fields Keycloak 26
   requires (`email`, `firstName`, `lastName`, `emailVerified`), which
   otherwise stop the flow at `VERIFY_PROFILE`.

Local validation (not a GitHub runner, not an organizational tenant):
disposable Keycloak 26.0.8 and the real gateway on loopback, checkout at
the branch head. Result: **exit 0, 15/15 checks PASS**, including admin
login/audit/snapshot, viewer login/snapshot/audit-403, unmapped-user
401, and logout revocation. `python3 scripts/oidc-runner-acceptance.py
--self-test` passes and is wired into `scripts/verify.sh`.

This proves the harness now completes the Authorization Code + PKCE
flow. It is **not** #647 acceptance: the evidence class remains
`runner-keycloak`, not a real organizational IdP tenant, and a fresh
GitHub Actions dispatch on the new main SHA is required. #647 stays
OPEN.
