# Provider onboarding live acceptance (#394)

This runbook covers the remaining live gate for provider catalog and model
onboarding: running best-effort discovery and bounded generation probes against
**real provider tenants** and recording reproducible, redacted outcomes.

Public CI never runs this command with paid credentials. The hermetic test suite
uses loopback fake upstreams only.

For a combined provider + OIDC report with `PASS`/`FAIL`/`BLOCKED`/`NOT_RUN`
statuses, use `nexaroute external-accept` (see
`docs/reports/verification-slices/live-acceptance-harness.md`). This
`provider-accept` command remains the narrower provider-only tool.

## What the command does

```bash
nexaroute provider-accept \
  --config /etc/nexaroute/config.json \
  --provider <provider-id> \
  --confirm-live \
  --out docs/reports/provider-acceptance-$(date -u +%Y%m%dT%H%M%SZ).json
```

- Loads the existing config through the normal loader. It does not save
  provider/model or test-state changes, although the loader's standard legacy
  secret-at-rest migration can still run. Credentials referenced by
  `api_key_env`, the credential pool, or `secret_key_env` are resolved in the
  process and never printed or written to the report.
- Verifies that the selected provider's credentials are present, then best-effort
  lists models from its real inventory endpoint when the protocol supports one.
- Runs one bounded generation probe per selected model (`max_output_tokens=1`,
  one upstream request, per-call timeout).
- Separates discovery, manual-model provenance (`manual`/`discovered`) and the
  previous persisted test state from the new observed result.
- Writes a JSON report with mode `0600` containing provider ids, sanitized
  endpoints, model ids, HTTP status, latency, bounded token use and redacted
  errors.

## Safety bounds

| Flag | Default | Hard maximum |
|---|---|---|
| `--timeout` | `15s` | `60s` |
| `--max-models` | `10` per provider | `100` |
| `--max-calls` | `20` total upstream calls | `100` |
| generation output tokens | `1` per call | fixed (not configurable) |

`--confirm-live` is mandatory. Without it the command exits `2` before any
network call. `--provider` is mandatory when `--test=true`, so an operator
cannot accidentally probe every configured tenant. SIGINT/SIGTERM cancels the
run between calls and returns whatever was observed so far.

Each generation probe may still consume provider rate limits or incur real
cost. Confirm the selected provider and models before adding `--confirm-live`.

## Credentials

Do not pass secrets as command-line flags and do not paste them into the report.

Prefer an environment reference in the config:

```json
{
  "providers": [
    {
      "id": "tenant-prod",
      "type": "openai_compatible",
      "base_url": "https://provider.example/v1",
      "auth_mode": "bearer",
      "api_key_env": "TENANT_PROD_API_KEY",
      "models": [
        { "id": "model-manual", "model": "model-manual", "enabled": true, "source": "manual" }
      ]
    }
  ]
}
```

Export `TENANT_PROD_API_KEY` in the operator shell only. The report records that
the credential source is `env` and that a credential was configured, never its
value or the environment variable's value.

## Exit codes and interpretation

| Exit | Meaning |
|---|---|
| `0` | Every selected provider passed and every attempted discovery was observed. |
| `1` | At least one provider failed or was degraded. The report still contains every observed result. |
| `2` | Usage/config error, or `--confirm-live` was omitted. No report is written for an unconfirmed run. |

Per-provider status:

- `passed` — every selected model passed, and discovery succeeded or is not
  supported by that protocol.
- `degraded` — some models passed, or models passed while a supported discovery
  endpoint failed. A failed `/models` endpoint **never** prevents manually
  configured models from being tested; it is recorded as a degraded observation.
- `failed` — no selected model passed, or no enabled model was configured.
- `configured` — `--test=false` and discovery succeeded.
- `unsupported` — `--test=false` and the provider type has no plain inventory
  endpoint.

`discovery.supported=false` is honest for Azure OpenAI deployments, Vertex named
projects and Bedrock Converse. Discovery is best-effort and never a prerequisite
for saving a valid manually configured model.

## Recommended operator sequence

1. Configure the real provider and at least one model in the Admin UI, or add the
   provider to the config file. Manual model entry always works even if
   discovery fails.
2. Run `nexaroute config validate --config /etc/nexaroute/config.json`.
3. Run the acceptance command with explicit `--provider` and `--model` values.
4. Review the JSON report. Confirm it contains no secret values before attaching
   it to the issue or PR. Hostnames and model ids are tenant metadata; redact
   them too if the operator considers them sensitive.
5. Record the following in #394: UTC timestamp, gateway commit SHA, exact command
   (without secrets), report path, provider/model counts, per-model outcomes and
   any discovery error.
6. Do **not** mark live tenant acceptance complete if credentials were absent,
   the command was not run, or any selected model/provider failed. State the
   remaining operator steps and required evidence instead.

## Limitations

- A one-token `200` response proves the selected deployment answered, not full
  protocol parity. The conformance manifest remains the source of truth for
  dimension-level claims.
- The command does not persist the observed `passed`/`failed` test state to the
  config (the normal loader may still migrate legacy secrets at rest). Use the
  Admin UI's **Test selected models** then save the provider if the state should
  be persisted.
- Discovery parses common inventory shapes only. Nonstandard listings require
  manual model entry.
- This command does not replace the hermetic contract tests or `./scripts/verify.sh`.
