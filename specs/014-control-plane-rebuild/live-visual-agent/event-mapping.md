# Live Visual Agent Event Mapping

| Backend kind | Current emitter | Normalized semantic | Request ID | Deployment | Latency | Terminal | Visual use |
|---|---|---|---|---|---|---|---|
| `route_fail` | `internal/httpapi/openai.go`, `anthropic.go`, `canonical_path.go` | `failed` | yes on request path | yes | usually | no if failover remains | mark actual attempted target failed |
| `failover` | request handlers | `failover` | yes | failed deployment | no/optional | no | show backend-declared transition only |
| `request_failover` | production lifecycle adapter | `failover` | request ID passed | failed deployment | optional | no | diagnostic corroboration; never compute next target |
| `route_ok` | request handlers | `success` | yes | yes | yes | yes | mark actual target success and settle |
| `candidate_exhausted` | request handlers | `failed` | yes in normal request path | no | no | yes | terminal failure without inventing a target |
| `route_timeout` | request handlers | `failed` | yes | yes | yes | yes | timeout only because backend classifies it |
| `stream_fail` / `stream_fail_precommit` | request handlers | `failed` | yes | yes | yes | yes | terminal/precommit failure as emitted |
| `client_disconnect` | request handlers | `cancelled` | yes | yes | yes | yes | terminal cancellation |
| unknown/malformed | any future emitter | ignored/diagnostic | n/a | n/a | n/a | n/a | never crash or animate |

`model_failed` and other deployment lifecycle events are not sufficient alone to claim a request journey because probes can emit them without a request. They may be used only when a request ID is present and the tracker has an existing execution.

No event is mapped to `attempt_started` until the backend emits that evidence. Fallback order is never calculated in the browser.
