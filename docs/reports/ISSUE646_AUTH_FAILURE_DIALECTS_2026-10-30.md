# Issue #646 — auth_failure per dialect (FIXTURE evidence, 2026-10-30)

Base main at work start: `7a2623a9252d14943ebad64b8945bbebafd4eef2`
(merge PR #778). No live provider call was made by this work.

## What was missing, demonstrated first-hand

The auth_failure status rule (400/401/403 pass, 200 fails) was pinned
only through the `openai_compatible` adapter. PR #773 (draft, open at
this check) pinned error-envelope *bodies* per dialect through that
same adapter, and its own test comments state that envelope parsing
does not branch on dialect. Neither pinned the auth_failure path per
dialect: each dialect sends its credential under a different header,
hits a different path, and is rejected with a different native status.
This PR closes exactly that gap, with loopback fakes only.

## What was added (test-only, no production change)

`internal/providers/livecanary/issue646_auth_failure_dialects_test.go`:

| Dialect | Credential on the wire (bogus) | Path pinned | Native rejection pinned |
|---|---|---|---|
| openai_compatible | `Authorization: Bearer <bogus>` | `/v1/chat/completions` (default) | 401, OpenAI envelope |
| anthropic_compatible | `x-api-key: <bogus>` | `/v1/messages` | 401, Anthropic envelope |
| gemini | `x-goog-api-key: <bogus>` | `/v1beta/models/canary-model` | 400, Gemini `INVALID_ARGUMENT` |
| vertex | `Authorization: Bearer <bogus>` | `/v1/projects/canary-project/locations/us-central1/publishers/google/models/canary-model` | 401, Vertex `UNAUTHENTICATED` |
| azure_openai | `api-key: <bogus>` | `/openai/deployments/canary-model/chat/completions?api-version=…` | 401, OpenAI envelope |
| bedrock | SigV4 `Credential=<bogus>/…` | `/model/anthropic.claude-3-sonnet-20240229-v1%3A0/converse` | 403, Bedrock `AccessDeniedException` |

For Anthropic/Gemini/Azure the provider's `AuthMode` is left empty so
the adapter's dialect default is what the test pins (the shared
testProvider default is bearer, which would have sent the wrong header
for those three dialects; the first run of this file failed exactly
that way, with 200s, and the fix was the empty AuthMode, not a
weakened assertion).

Every case asserts: scenario `auth_failure` PASS on the native status,
exactly 1 upstream call (a rejected credential is never retried), the
fake never saw a real test credential, and the report summary contains
no credential material. A second test pins that a 200 answer to the
bogus credential FAILS auth_failure for Gemini, whose native rejection
is 400: acceptance of the bogus credential is never rejection evidence.

## Honest scope (FIXTURE, not LIVE)

- The loopback fake does not verify a SigV4 signature and does not
  verify any provider's real rejection behaviour. A PASS here means
  only that the Runner sent the bogus credential in the dialect's
  shape, the fake rejected it with the dialect's native status, and
  the Runner recorded PASS with exactly one call and no credential
  material in the report.
- This does not verify any dialect's live auth rejection, live
  discovery (discovery is by design NOT_RUN for azure_openai, vertex
  and bedrock in `internal/providers/discovery.go`), live streaming
  usage, live tool round-trips, or live rate-limit behaviour. Those
  remain BLOCKED/NOT_RUN per provider below.
- No provider's result is extrapolated to another provider, model,
  version, or endpoint.

## Live status per provider (unchanged by this PR)

| Provider | Live auth_failure | Live text/stream/tools/usage | Live rate-limit / failover |
|---|---|---|---|
| OpenAI-compatible | BLOCKED | BLOCKED | NOT_RUN (never induced) |
| Anthropic-compatible | BLOCKED | BLOCKED | NOT_RUN |
| Gemini | BLOCKED | BLOCKED | NOT_RUN |
| AWS Bedrock | BLOCKED | BLOCKED | NOT_RUN |
| Google Vertex AI | BLOCKED | BLOCKED | NOT_RUN |
| Azure OpenAI | BLOCKED | BLOCKED | NOT_RUN |

## Authorization gate (still FAIL)

No owner comment on #646 names providers, models, tenant/account,
endpoint, credential secret names, request/token budget, or spend.
Credential check by env-var name only (values never read, printed, or
used): 0 of 10 standard names set in this shell
(`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`,
`GOOGLE_API_KEY`, `AZURE_OPENAI_API_KEY`, `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `VERTEX_API_KEY`,
`NEXAROUTE_PROVIDER_API_KEY`).

## Exact owner authorization still required

For each provider the owner wishes to verify, a written comment on
#646 must name, before any paid/live call:

1. Provider dialect and endpoint/base URL.
2. Exact model (and, for Azure, deployment name; for Vertex, project
   and location; for Bedrock, model id and AWS region).
3. Tenant/account identifier (operator-owned; no credentials in the
   comment).
4. Credential secret names only (`api_key_env`; for Bedrock also
   `secret_key_env` and `session_token_env` if used) — never values.
5. Scenario set (the acceptance set is 7 calls per provider:
   text, stream, stream_usage, tools, tool_result, auth_failure,
   error_envelope), token limits, and `--max-cost-usd` spend ceiling.
6. Maximum request count.

Until that comment exists, zero live calls are made, no provider is
marked PASS, and #646 stays OPEN. #646 closes only when each required
provider is verified on its own evidence, or the owner explicitly
excludes a provider in a reviewed release decision.

## Verification (fixture only)

Commands and outcomes are recorded in the PR body; the evidence
itself is the test run, not this document. The full `livecanary`
package, the providers quota/rate-limit/cooldown subset, and the
gateway accept/conformance subset were run plain and (for livecanary)
with `-race`; `go vet` and `gofmt` are clean for the new file.
