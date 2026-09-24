# Upstream error detection — NexaRoute v0.3

LLM providers do not fail in one uniform way. NexaRoute therefore classifies
upstream failures with one canonical implementation
(`internal/providers/upstream_error.go`) shared by the adapter, probes,
provider tests, and both data-plane ingress paths. HTTP status alone is
never trusted as success.

## Detected failure shapes

| Shape | Example | Verdict |
|---|---|---|
| Non-2xx + OpenAI error envelope | 429 `{"error":{"code":"insufficient_quota",...}}` | error (class from status + message) |
| Non-2xx + Anthropic error envelope | 529 `{"type":"error","error":{"type":"overloaded_error",...}}` | error |
| Non-2xx + OpenRouter/DeepSeek envelope | 402 `{"error":{"message":"Insufficient Balance",...}}` | error (quota) |
| Non-2xx + proxy detail shape | 422 `{"detail":"..."}` | error |
| 200 + error envelope (object or string) | `{"error":"Unexpected endpoint..."}` (LM Studio style) | error |
| 200 + `success:false` / `ok:false` / `status:"error"` | proxy failure markers | error |
| 200 + valid envelope, paywall text in content | Pollinations: content starts with "The account behind this API key doesn't have enough credits..." | error (quota) |
| 200 + `finish_reason:"error"` / `stop_reason:"error"` | OpenRouter-style terminal failure | error |
| 200 + `null` / empty / malformed / HTML body | broken proxy output | error |
| In-stream error chunk | `error` member, Anthropic `type:"error"` event | error (post-commit accounting) |
| In-stream `finish_reason:"error"` | failed generation tail | error (post-commit accounting) |
| Paywall text delivered as stream deltas | same signatures as the content rule below | error (post-commit accounting) |

## Failure classes and behavior

| Class | Meaning | Credential | Deployment | Failover |
|---|---|---|---|---|
| `quota` | credits/balance/quota/billing exhausted | cool 1h | quarantine/failure | yes |
| `auth` | bad/revoked/disabled key, permission denied | cool 15m | quarantine/failure | yes |
| `rate_limit` | RPM/TPM/concurrency throttle | cool Retry-After (capped) | quarantine/failure | yes |
| `overloaded` | provider saturated (incl. Anthropic 529) | failure noted | quarantine/failure | yes |
| `server` | provider 5xx / error finish | failure noted | quarantine/failure | yes |
| `not_found` | unknown model/endpoint | failure noted | quarantine/failure | yes |
| `invalid` | caller must fix the request (400/422/...) | failure noted | failure (no quarantine under ready strategies) | no |
| `bad_payload` | malformed/empty/non-JSON body | failure noted | quarantine/failure | yes |

Key-scoped classes (`quota`, `auth`, `rate_limit`) cool only the serving
credential and rotate to a sibling key when one is available, exactly like
their non-2xx equivalents. A 429 carrying `insufficient_quota` is treated
as account-state quota (long cooldown), never as a transient throttle.

Events reuse the existing vocabulary: quota → `provider_billing`, auth →
`provider_auth_failed`, throttle → `provider_rate_limited`, overload →
`provider_overloaded`, server → `provider_server_error`, payload →
`provider_invalid_response`, not-found → `provider_request_rejected`,
invalid caller input → `caller_invalid_request`.

## Content-sniffing rule (anti-paywall)

Injected paywall/quota text always *starts* the reply, so only the first
120 characters of completion text are inspected, against distinctive
phrases (`doesn't have enough credits`, `insufficient balance`,
`quota exceeded`, `out of credits`, `requires more credits`,
`needs paid`, `invalid api key`, `rate limit`, `overloaded`, ...).
Short contextual phrases such as `top up` additionally require a
billing-context word (`credit`, `balance`, `quota`, `billing`, ...) in
the same window, so ordinary text like "top up your coffee" never
matches. A genuine completion that merely *discusses* quota wording
later in the text is not flagged.

## Honest limitations

- Probes use `max_tokens=1`: a provider that truncates injected text to a
  single token defeats probe-time content sniffing. The data plane still
  catches the full text on the first real request and quarantines the
  deployment, and recovery re-probes it.
- Committed stream bytes cannot be un-sent: mid-stream detection corrects
  health accounting (failure, no session pin) but cannot fail over.
- Adapter credential peeking inspects the first 64 KB of non-streaming
  bodies; a failure signaled only past that point is still caught by the
  data plane for deployment accounting, but the credential is not cooled.
- Classification matches documented, well-evidenced signals. Unknown
  shapes are never flagged, so a provider inventing a new success field
  cannot be misclassified; the residual direction on ambiguity is a
  failover, never silent success.
