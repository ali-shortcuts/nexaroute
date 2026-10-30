# Issue #646 — acceptance report validator + dialect error-envelope fixtures (2026-10-30)

Status: **fixture work only; every live case stays BLOCKED.** No live
provider call was made in this work, no provider is marked PASS, and
issue #646 stays OPEN. This report records what the harness already
covers per provider, what this change adds, and exactly what is still
missing for a live run. It does not claim any live interoperability.

Base: main `7daa36da962bf3031b353487f760506cd60aedb2` (PR #764 merge).
Branch: `supervisor/issue646-acceptance-validator-20261030`.
Harness reused, not duplicated: `nexaroute provider-accept`,
`nexaroute external-accept`, `conformance canary`
(`internal/providers/livecanary`), runbook
`docs/PROVIDER_LIVE_ACCEPTANCE.md`. No competing framework was created.

## Authorization gate (re-checked this session, counts only)

- Issue #646 comments at check time: 5, none naming providers, models,
  tenant, endpoint, or spend. The 2026-10-30 comment carrying pasted
  agent task text was inspected and is NOT an authorization: it names
  no provider, model, tenant, or spend cap, and a pasted prompt in a
  shared-account comment is not the owner's approval.
- Credential presence by env-var name only (values never read, printed,
  or used): **0 / 9** standard provider credential names set in this
  shell. Repository Actions secrets: **0**.
- Consequence: zero upstream provider calls in this session (only
  GitHub API reads and loopback fixture tests). The consolidated
  6-field live-acceptance owner request delivered to the owner today
  already asks for the #646 items (providers/models/tenant, credential
  env names, written billable-call authorization, max spend plus max
  requests/tokens, operations, cleanup); this work does not duplicate
  that request, it only restates that the live run is still waiting
  for its reply. No call runs until that reply names the providers,
  models, tenant, spend cap, and request/token caps.

## Per-provider audit (code first-hand, fixture evidence only)

"Fixture" means loopback/fake tests in the repo, never a live tenant.
A SKIP below is a designed applicability rule in `scenarios.go`, not
a failure; a NOT_RUN is a live-only status.

| Dialect | Discovery | Auth | Stream usage source | structured_output | parallel_tools | reasoning | Live |
|---|---|---|---|---|---|---|---|
| openai_compatible | /v1/models | bearer | stream_options.include_usage | yes | yes | yes | BLOCKED |
| anthropic_compatible | /v1/models | x-api-key | message_start / message_delta usage | SKIP by design | yes | yes | BLOCKED |
| gemini | /v1/models | x-goog-api-key; invalid key is 400, accepted for auth_failure alongside 401/403 | usageMetadata promptTokenCount / candidatesTokenCount; finishReason terminal | yes | SKIP by design | yes | BLOCKED |
| azure_openai | NOT_RUN (no inventory endpoint; manual model entry only) | api-key; deployment name; api_version default 2024-10-21 | OpenAI usage shape | yes | yes | yes | BLOCKED |
| vertex | NOT_RUN (manual model entry only) | bearer token via api_key_env; publishers/google/models/<model>; vertex_project required; location default us-central1 | usageMetadata; finishReason terminal | yes | SKIP by design | yes | BLOCKED |
| bedrock | NOT_RUN (manual model entry only) | SigV4 (access-key id + secret + optional session token); auth_failure replaces only the access-key id | Converse metadata usage inputTokens / outputTokens; messageStop terminal | SKIP by design | SKIP by design | SKIP by design | BLOCKED |

`openai_responses` is accepted by the same harnesses but is NOT one
of the six #646 dialects; its result is recorded separately and never
folded into another dialect's claim.

Negative paths already pinned by fixtures before this change (verified
by reading the tests, not assumed): timeout FAIL + exactly 1 call and
429 FAIL + HTTP 429 + exactly 1 call (PR #743, loopback); truncated
stream FAIL + exactly 1 call; auth_failure PASS on 400/401/403 and
FAIL on 200, plus skip for auth_mode none; stream_usage extraction
for OpenAI, Anthropic, Gemini, and Bedrock envelope shapes; 429
cooldown mapping with bounded Retry-After parsing in the adapter;
pre-canceled and overall-timeout cancellation; failover only before
stream commit (routing unit tests — the canary itself never retries
a call). Rate limit, outage, and live failover are OBSERVED_ONLY /
NOT_RUN live by runbook design and were never induced here.

## What this change adds

1. `scripts/check-acceptance-report.py` (stdlib only): validates a
   canary, provider-accept, or external-accept report against the
   rules the writing code enforces — schema, RFC3339 date, scenario /
   check status sets, outcome derivation, totals arithmetic,
   dimension consistency, budget ceilings, the stream_usage
   both-tokens rule, the auth_failure 400/401/403 rule, the
   error_envelope 4xx rule — and fails any report containing
   credential-shaped material (AWS key ids, GitHub tokens, PEM keys,
   live/project secret keys, raw Bearer tokens). Two deliberate
   limits, documented in the script: it does NOT prove a run was
   live (a loopback fixture produces the same canary schema, so
   `--require-live` rejects loopback-class reports instead of
   certifying them), and it does NOT check a commit SHA (no report
   schema carries one; the SHA belongs in the signed run record
   next to the report).
2. `internal/providers/livecanary/issue646_error_envelope_dialects_test.go`:
   the error_envelope check is dialect-independent code, but only
   the OpenAI spellings were pinned through the real Runner path.
   This pins the native error bodies of OpenAI/Azure (OpenAI
   envelope), Anthropic (`type` + nested `error`), Gemini/Vertex
   (`error` with numeric code + status), and Bedrock (`__type` +
   `message`) through that path with loopback fakes, plus the
   negative case (a 400 body with no envelope key must FAIL). The
   test comments state plainly that this pins envelope parsing,
   not dialect adapters.
3. Runbook section "Validating a report" in
   `docs/PROVIDER_LIVE_ACCEPTANCE.md` (usage + the two limits above).

What was deliberately NOT changed: no production code, no adapter
behavior, no scenario set, no cost/timeout defaults, no conformance
manifest, no `docs/KNOWN_GAPS.md` (a sibling lane owns that file
this mission), and no claim about any provider's live behavior.

## Evidence (local, fixture only — not live acceptance)

- New tests: `TestIssue646ErrorEnvelopeDialectBodies` (4 sub-tests)
  PASS, `TestIssue646ErrorEnvelopeBareBodyFails` PASS
  (livecanary package, 0.115s for the new tests).
- Full `internal/providers/livecanary`: PASS (4.000s); `-race`: PASS
  (7.728s). Providers quota/rate-limit/cooldown subset: PASS
  (0.163s). Gateway accept/conformance subset
  (`TestExternalAccept|TestProviderAccept|TestConformance`): PASS
  (5.079s). `go vet` on livecanary: clean. `gofmt`: clean on both
  new files. Python compile of the validator: clean.
- Validator fixtures (synthetic reports under `~/workspace/.tmp`,
  not committed): canary good → VALID (0 warnings, class loopback);
  canary with a FAIL scenario but outcome PASS → INVALID (outcome
  + verified-dimensions errors); stream_usage PASS with output 0 →
  INVALID; credential-shaped text → INVALID; calls_used over
  max_calls → INVALID (plus the soft sum warning); provider-accept
  good → VALID; provider-accept with a miscounted total → INVALID;
  external-accept good → VALID; external-accept with a FAIL check
  but outcome PASS → INVALID (outcome + totals errors);
  `--require-live` on a loopback canary and on a loopback
  external-accept report → INVALID in both cases; missing file →
  exit 2. Exit codes: 0 valid, 1 invalid, 2 usage.
- Full `scripts/verify.sh` locally: NOT run (this change touches one
  test file, one script, and docs; the PR's CI jobs are the wider
  gate). No live binary run was performed against any provider.

## Live matrix (this session — all BLOCKED, none extrapolated)

For every dialect (OpenAI-compatible, Anthropic-compatible, Gemini,
Bedrock, Vertex, Azure) and every live case (auth success, auth
failure, discovery where supported, non-streaming, streaming, stream
usage, tools, tool_result, rate limit, timeout, cancellation, outage,
failover, interrupted stream): **BLOCKED** (no authorization, no
credentials) except rate limit / outage / live failover, which are
**NOT_RUN** by runbook design (never induced; observed-only if a
live run ever sees them). No RPO/RTO, SLO, or spend figure is
reported. A future live run must fill one row per provider and per
model from its own sanitized report, validated with the new script
plus `--require-live`, with the SHA in its run record.

## Reproduce

```sh
go test -count=1 ./internal/providers/livecanary/ -run 'TestIssue646ErrorEnvelope'
python3 scripts/check-acceptance-report.py --report <report.json>
python3 scripts/check-acceptance-report.py --report <report.json> --require-live
```
