# Real LLM provider live acceptance (#646)

This runbook is the repeatable, operator-authorized process for accepting
**actual provider endpoints**. It does not replace the fixture suite, and it
does not promote any provider claim on its own: a live `PASS` is evidence for
one provider, one model, and one date only. One provider's success never
proves compatibility with another provider, even one that shares its dialect.

Public CI never runs any command in this document with paid credentials.
Every live step below requires an explicit operator decision first.

## Commands used (existing harnesses, preferred in this order)

| Step | Command | Report schema | What it proves |
|---|---|---|---|
| 1 | `nexaroute provider-accept` | `nexaroute.provider-acceptance.v1` | Bounded discovery + one-token generation probe per selected model |
| 2 | `nexaroute external-accept` | `nexaroute.external-acceptance.v1` | Step 1 wrapped in `PASS`/`FAIL`/`BLOCKED`/`NOT_RUN` checks, plus OIDC when selected |
| 3 | `nexaroute conformance canary --scenarios acceptance` | `conformance.live-canary.v1` | Text, streaming, stream usage, tool round trips, expected auth failure, error envelope |

Step 3 is new in this change: the `acceptance` scenario set is the core set
plus `stream_usage` and `auth_failure` (see `internal/providers/livecanary`).
Steps 1–2 already existed; this document sequences them instead of inventing
a fourth harness or a new report format.

## Operator authorization gate

No live request happens until the operator has, in writing on the issue or
the run record:

1. Named the exact providers and models to test (never "all providers" by
   accident — `--provider` is mandatory for probing, and `--all-providers`
   is a deliberate second choice).
2. Confirmed the target is a staging tenant or an explicitly approved
   production tenant, and that the spend is acceptable.
3. Set the cost and call budgets below, and confirmed the selected models
   are the cheapest adequate test models for that tenant.
4. Passed `--confirm-live` (steps 1–2) and `--acknowledge-cost` (step 3).
   Without those flags each command exits before any network call and
   writes no report.

A refused run (missing flag) is itself a preflight check: run every command
once *without* the flag first and confirm it exits `2` with no report.

## Credentials: environment or secret store only

- Credentials reach the gateway only through environment variables named in
  the config (`api_key_env`, credential-pool `api_key_env`, Bedrock
  `secret_key_env` / `session_token_env`), or through the operator's own
  secret store exporting those variables into the operator shell.
- Never put a credential in source code, in a command-line argument, in a
  report, or in a log. For a live run, the config must use `api_key_env`,
  not a literal `api_key`. Check before running:
  `grep -n '"api_key"' <config>` must return nothing for the providers
  under test.
- The reports record only that a credential was configured and which
  environment variable name supplied it — never its value. Error text is
  sanitized and bounded before it reaches a report, and reports are written
  atomically with mode `0600` (parent directories `0700`). Review a report
  before attaching it to an issue or PR: hostnames, tenant ids, and model
  ids can themselves be sensitive metadata.
- `auth_failure` (step 3) uses a deliberately invalid credential, never the
  operator credential. Bedrock is the documented exception: SigV4 reads the
  secret only from the environment variable named in config at request
  time, so that probe replaces only the access-key id. The signature is an
  HMAC over the request and does not disclose the secret; AWS rejects the
  unknown key id with `403`.

## Providers and configured test models

The model under test is always a model the operator configured and enabled
for that provider (`--model`), or the provider's single enabled model.
Discovery never invents a model id.

| Provider type | Discovery | Auth | Model identifier | Notes |
|---|---|---|---|---|
| `openai_compatible` | Yes (`/v1/models`) | bearer | upstream model id | Stream usage via `stream_options.include_usage` |
| `anthropic_compatible` | Yes (`/v1/models`) | `x-api-key` | upstream model id | Stream usage from `message_start`/`message_delta` events |
| `gemini` | Yes (`/v1/models`) | `x-goog-api-key` | upstream model id | Invalid-key rejection is `400`, not only `401`/`403`; the canary accepts `400`/`401`/`403` for `auth_failure` |
| `azure_openai` | No | `api-key` | deployment name | No plain inventory endpoint; manual model entry is the only path. `api_version` defaults to `2024-10-21` |
| `vertex` | No | bearer (operator-supplied token via `api_key_env`) | `publishers/google/models/<model>` | `vertex_project` required; `vertex_location` defaults to `us-central1` |
| `bedrock` | No | SigV4 (`api_key_env` = access-key id, `secret_key_env`, optional `session_token_env`) | Converse model id | No plain inventory endpoint; manual model entry is the only path |
| `openai_responses` | Via configured `models_path` | bearer | upstream model id | Accepted by the same harnesses; it is not one of the six #646 dialects, so its result is recorded separately and never folded into another dialect's claim |

A `discovery.supported=false` row is an honest observation, not a failure:
for Azure, Vertex, and Bedrock it must be recorded as `NOT_RUN` (discovery)
with the manual-model path noted, not as a failed provider.

## Case matrix: what is live, what is observed-only, what is hermetic

Status vocabulary for the run record: `PASS` (ran, succeeded),
`FAIL` (ran, failed), `BLOCKED` (could not run: credential/endpoint/operator
authorization absent), `NOT_RUN` (not selected, not applicable to that
provider, or not automatable).

| Case | How it is tested | Evidence class |
|---|---|---|
| Authentication success | `text` scenario + one-token probe return `200` with a real credential | LIVE |
| Expected authentication failure | `auth_failure`: deliberately invalid credential must be rejected (`400`/`401`/`403`); a `200` is a `FAIL` | LIVE |
| Model discovery | `provider-accept` / `external-accept` discovery, where supported | LIVE, or NOT_RUN where unsupported (see table above) |
| Minimal non-streaming completion | `text` scenario + one-token probe | LIVE |
| Streaming | `stream` scenario: SSE data events plus the dialect's terminal event | LIVE |
| Streaming usage extraction | `stream_usage` scenario: the completed stream must report input *and* output tokens; a stream without usage is a `FAIL`, not a pass | LIVE |
| Tool-call round trip | `tools` (model emits the requested tool call) + `tool_result` (model answers after a tool-result turn) | LIVE |
| Rate limit (`429`) | **Never induced.** If a live run observes `429`, record the status, the `Retry-After` value if present, and the credential-cooldown behavior; do not re-run to "confirm" it. The cooldown mapping itself is proven by adapter unit tests | OBSERVED_ONLY live; HERMETIC for the mapping |
| Timeout | Client timeout is honored by the harness (per-call timeout, overall timeout, context cancellation) and proven against blocking loopback fakes. A live call that exceeds `--call-timeout` is recorded as a timeout observation, not silently retried beyond the call budget | HERMETIC + OBSERVED_ONLY live |
| Cancellation | Operator `SIGINT`/`SIGTERM` between calls stops `provider-accept`/`external-accept`; the canary's overall timeout and context cancellation stop a run and record `CANCELED`. Client-side abort is not provider-side cancellation and is never reported as such | HERMETIC + operator procedure live |
| Provider outage (`5xx`, unreachable) | **Never induced.** Record any `5xx`/transport failure observed during a run with its sanitized error; do not manufacture an outage against a real tenant | OBSERVED_ONLY live |
| Failover | Live failover needs two live tenants and a gateway route, and is `NOT_RUN` by default in this runbook. When an operator does stage it, failover is valid only *before* the first response byte: after bytes are committed to a client, mid-stream failover is not possible and must never be claimed. The routing/fallback logic itself is covered by orchestrator unit tests | NOT_RUN live by default; HERMETIC for routing |
| Failed / interrupted stream | A truncated stream (data events, no terminal event) must be reported `FAIL` with exactly one upstream call — no silent pass and no retry-as-failover. Proven against a truncating loopback fake; a live truncation, if observed, is recorded the same way | HERMETIC + OBSERVED_ONLY live |

## Bounds (hard ceilings, not suggestions)

| Bound | `provider-accept` / `external-accept` | `conformance canary` (acceptance) |
|---|---|---|
| Upstream calls | default `20`, max `100` total | `7` calls for the acceptance set; pass `--max-calls 7` (CLI default is `5`, which is too small for this set); hard ceiling `12` |
| Output tokens | `1` per generation probe (fixed) | per-request ceiling, default `512` in the CLI; keep it low for acceptance |
| Per-call timeout | default `15s`, max `60s` | default `30s`, max `2m` |
| Overall timeout | `external-accept` max `5m` | default `2m`, max `10m` |
| Cost | call/token budgets are the guard when pricing is unknown | `--max-cost-usd` (CLI default `1.00`); if pricing is configured and the preflight estimate exceeds the cap, the run refuses before any call |
| Concurrency | one provider at a time, in selection order | one provider, one model, sequential scenarios |

No step in this process makes a production-changing request: no fine-tuning,
no file upload, no model or deployment creation, no configuration save.
`provider-accept` does not persist observed test state to the config, and
the canary never edits the conformance manifest. Promoting any dimension to
live-verified is a separate operator review decision after reading the
report, never an automatic effect of a `PASS`.

## Operator sequence

```bash
# 0. Preflight: refusal without the live flag, config validation, no literals.
nexaroute provider-accept --config /etc/nexaroute/config.json --provider <provider-id>
# expect: exit 2, "refusing to run without --confirm-live", no report written
nexaroute config validate --config /etc/nexaroute/config.json
grep -n '"api_key"' /etc/nexaroute/config.json   # expect: no output for tested providers

# 1. Discovery + one-token probes, redacted report.
nexaroute provider-accept \
  --config /etc/nexaroute/config.json \
  --provider <provider-id> \
  --model <test-model> \
  --confirm-live \
  --out provider-acceptance-<provider-id>-$(date -u +%Y%m%dT%H%M%SZ).json

# 2. Same checks in PASS/FAIL/BLOCKED/NOT_RUN form (add --oidc for the OIDC
#    check when that tenant is also under test; otherwise OIDC is NOT_RUN).
nexaroute external-accept \
  --config /etc/nexaroute/config.json \
  --provider <provider-id> \
  --model <test-model> \
  --confirm-live \
  --out external-acceptance-<provider-id>-$(date -u +%Y%m%dT%H%M%SZ).json

# 3. Full acceptance scenario set for one provider + one model.
#    Credentials come from the environment variables named in the config
#    (export them from the operator secret store first; never paste values
#    into this command line or the report).
nexaroute conformance canary \
  --config /etc/nexaroute/config.json \
  --provider <provider-id> \
  --model <test-model> \
  --scenarios acceptance \
  --max-calls 7 \
  --acknowledge-cost \
  --out canary-<provider-id>-$(date -u +%Y%m%dT%H%M%SZ).json
```

Repeat steps 1–3 per provider and per test model, one at a time. Record for
each run: provider, model, UTC timestamp, gateway commit SHA, the exact
command (without secrets), the report path, the per-case status, and any
sanitized error. Then fill the run record below; do not copy a status from
one provider's row into another's.

## Validating a report

Before attaching any report to issue #646, run the report validator
(`scripts/check-acceptance-report.py`, stdlib only). It checks the
report's own schema, dates, scenario/check statuses, outcome and
totals arithmetic against the rules the writing code enforces, plus
the contract rules a report must not violate (a `stream_usage` PASS
needs input and output tokens, an `auth_failure` PASS needs HTTP
400/401/403, an `error_envelope` PASS needs a 4xx status, observed
cost must not exceed the cost ceiling), and it fails the report if
it contains credential-shaped material. A valid report is still not
live evidence by itself: a loopback fixture produces a canary report
with the same schema as a live run, and none of the three report
schemas carries a commit SHA. Use `--require-live` to reject
loopback-class reports outright, and check the SHA in the signed run
record against the report's `generated_at` date before quoting any
status.

```sh
python3 scripts/check-acceptance-report.py --report canary-<provider-id>-<ts>.json
python3 scripts/check-acceptance-report.py --report canary-<provider-id>-<ts>.json --require-live
python3 scripts/check-acceptance-report.py --report external-acceptance-<ts>.json --require-live
```

## Run record template

| Provider | Model | Case | Status | Evidence (report path, HTTP status, tokens) |
|---|---|---|---|---|
| _provider-id_ | _model_ | auth success / auth failure / discovery / non-streaming / streaming / stream usage / tools / tool_result / rate limit / timeout / cancellation / outage / failover / interrupted stream | PASS / FAIL / BLOCKED / NOT_RUN | _report file, observed values only_ |

## This run (2026-10-27, main `e6c3cdf3`)

No operator authorization and no live provider credentials were supplied
for this change, and no billable or live request was made: **zero upstream
calls**. The harness work below is therefore the deliverable, and every live
case is honestly `BLOCKED` or `NOT_RUN`. Nothing in this section is live
evidence, and no provider is marked verified on the basis of another
provider or of a fixture.

| Provider | Case | Status | Reason |
|---|---|---|---|
| openai_compatible | all live cases | BLOCKED | No staging tenant, credential, or operator authorization supplied |
| anthropic_compatible | all live cases | BLOCKED | No staging tenant, credential, or operator authorization supplied |
| gemini | all live cases | BLOCKED | No staging tenant, credential, or operator authorization supplied |
| azure_openai | all live cases | BLOCKED | No staging tenant, credential, or operator authorization supplied |
| vertex | all live cases | BLOCKED | No staging tenant, credential, or operator authorization supplied |
| bedrock | all live cases | BLOCKED | No staging tenant, credential, or operator authorization supplied |
| all providers | rate limit / outage | NOT_RUN | Not induced against real tenants; recorded only if observed during an authorized run |
| all providers | live failover | NOT_RUN | Requires two live tenants + a gateway route; not staged for this change |
| all providers | harness behavior (acceptance scenarios, auth discrimination, stream usage, truncated stream, timeout/cancellation handling) | PASS (fixtures only) | Loopback fake upstreams; see test evidence below. Fixture passes are **not** interoperability evidence |

### Fixture and regression evidence for this change

```bash
go test ./internal/providers/livecanary/ -count=1
go test ./cmd/gateway/ -run 'TestExternalAccept|TestProviderAccept' -count=1
go vet ./internal/providers/livecanary/ ./cmd/gateway/
```

Observed locally for this change (main `e6c3cdf3` + this branch, loopback
fixtures only, zero live provider calls):

- `go test ./internal/providers/livecanary/ -count=1` — PASS, including
  the new acceptance tests; `go test -race` on the same package — PASS.
- `go test ./cmd/gateway/ -run 'TestExternalAccept|TestProviderAccept|TestConformance' -count=1` — PASS.
- `go vet ./internal/providers/livecanary/ ./cmd/gateway/` — clean;
  `gofmt -l` on both packages — clean.
- Binary end-to-end against a loopback OpenAI-shaped fake: refusal without
  `--acknowledge-cost` exits `2` with no report; the acceptance set with
  the CLI default `--max-calls 5` is refused as a setup error ("need 7
  calls"); with `--max-calls 7` the run is `PASS`, 7/7 scenarios, and the
  report contains zero occurrences of either the fixture credential or
  the bogus credential. `provider-accept --discover=false` probe against
  the same fake passes with 1 upstream call.
- A truncated-stream regression found during this work is fixed in the
  same change: the old terminal check matched a `finish_reason` key with a
  null value and reported a cut-off stream as `PASS`; it now requires
  `[DONE]` or a non-empty finish-reason value.

Full `./scripts/verify.sh` and GitHub Actions CI results are recorded on
the pull request; this document does not claim CI results that were not
observed. The one-token probe, the acceptance canary, and the external
harness remain operator-only commands: they are never wired into public
CI.

## This run (2026-10-28, PR #674 head `04f2bd7`, base main `e6c3cdf3`)

A second attempt to complete the live acceptance, on the exact PR head.
The outcome is unchanged: **zero upstream calls, zero paid requests**.
No credential value was read, printed, stored, or transmitted anywhere
in this run; only names and availability were inspected.

Credential and authorization check (names only, 2026-10-28):

- Standard provider credential names checked for non-empty values in the
  operator shell: `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`,
  `GOOGLE_API_KEY`, `AZURE_OPENAI_API_KEY`, `AWS_ACCESS_KEY_ID`,
  `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `VERTEX_API_KEY` —
  **0 of 9 non-empty**. No preconfigured provider credential exists in
  this environment.
- Repository secret list: empty (exit 0). No CI secret supplies a
  provider credential for this run.
- Issue #646 has no comments: no operator authorization naming providers
  and models, no staging/production tenant confirmation, and no spend
  approval. The GitHub token used for repository operations was never
  used as a provider credential, and no token value appears in any
  report, log, or file produced here.

Because the credential and cost scope are not established, no live
request was initiated for any provider, per the gate in this runbook.
Every live case below is therefore `BLOCKED` or `NOT_RUN`, exactly as
in the 2026-10-27 run. Nothing in this section is live evidence.

| Provider | Case | Status | Reason |
|---|---|---|---|
| openai_compatible | all live cases | BLOCKED | No credential, tenant, or operator authorization supplied |
| anthropic_compatible | all live cases | BLOCKED | No credential, tenant, or operator authorization supplied |
| gemini | all live cases | BLOCKED | No credential, tenant, or operator authorization supplied |
| azure_openai | all live cases | BLOCKED | No credential, tenant, or operator authorization supplied |
| vertex | all live cases | BLOCKED | No credential, tenant, or operator authorization supplied |
| bedrock | all live cases | BLOCKED | No credential, tenant, or operator authorization supplied |
| openai_responses | all live cases | NOT_RUN | Not one of the six #646 dialects; recorded separately when tested |
| all providers | rate limit / outage | NOT_RUN | Not induced against real tenants; recorded only if observed during an authorized run |
| all providers | live failover | NOT_RUN | Requires two live tenants + a gateway route; not staged |
| all providers | harness behavior (acceptance scenarios, auth discrimination, stream usage, truncated stream, quota mapping) | PASS (fixtures only) | Loopback fakes; see test evidence below. Fixture passes are **not** interoperability evidence |

### Fixture and regression evidence for this run

```bash
go test ./internal/providers/livecanary/ -count=1
go test ./internal/providers/livecanary/ -count=1 -race
go test ./cmd/gateway/ -run 'TestExternalAccept|TestProviderAccept|TestConformance' -count=1
go test ./internal/providers/ -run 'TestQuota|TestRateLimit|TestCooldown' -count=1
go vet ./internal/providers/livecanary/ ./cmd/gateway/
```

Observed locally for this run (PR #674 head `04f2bd7`, loopback
fixtures only, zero live provider calls):

- `go test ./internal/providers/livecanary/ -count=1` — PASS (0.539s),
  including the 7-scenario acceptance set (`text`, `stream`,
  `stream_usage`, `tools`, `tool_result`, `auth_failure`,
  `error_envelope`) and the truncated-stream / null-finish-reason /
  stream-usage-dialect negative paths; `-race` — PASS (1.731s).
- `go test ./cmd/gateway/ -run 'TestExternalAccept|TestProviderAccept|TestConformance' -count=1` — PASS (3.780s).
- `go test ./internal/providers/ -run 'TestQuota|TestRateLimit|TestCooldown' -count=1` — PASS (0.090s); the
  rate-limit cooldown mapping is hermetic, as the matrix requires.
- `go vet` on both packages — clean; `gofmt -l` on both packages —
  clean.
- Binary end-to-end against a loopback fake was NOT re-run this time;
  the same scenarios are covered by the acceptance tests above through
  loopback fakes, and no binary behavior changed on this branch since
  the 2026-10-27 run.
- No capability or conformance-manifest claim was changed: with no live
  evidence, no provider gains a live-verified dimension.

### Exact prerequisites to unblock the live run

1. An operator comment on issue #646 naming the exact providers and
   test models, the staging (or explicitly approved production) tenant,
   and the approved spend.
2. Credentials supplied only through the environment variables named in
   the config (`api_key_env`, credential-pool `api_key_env`, Bedrock
   `secret_key_env` / `session_token_env`) or the operator's secret
   store exporting those variables — never pasted, committed, or placed
   in a URL.
3. Re-run steps 1–3 of the operator sequence per provider, one at a time,
   with `--confirm-live` / `--acknowledge-cost`, and attach the sanitized
   per-provider reports. Issue #646 stays open until then.

## Limitations (carried with every report)

- A `PASS` proves the selected deployment answered bounded probes on the
  run date. It is not full protocol conformance; the conformance manifest
  remains the source of truth for dimension-level claims.
- Discovery parses common inventory shapes only; nonstandard listings
  require manual model entry.
- `retry_after` and `idempotency` are not induced by the canary, and
  cache-token behavior is claimed only when the provider reports cache
  token counts.
- Provider-side cancellation semantics are not proven by a client-side
  abort; cancellation remains partial until a provider-specific
  observation exists.
- A live pass does not survive provider or version changes: re-run after
  either changes, and keep fixture evidence in its own `FIXTURE-VERIFIED`
  state.
