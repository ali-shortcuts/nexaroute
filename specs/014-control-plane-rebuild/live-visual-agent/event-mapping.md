# Live Visual Agent Event Mapping

| Backend kind | Current emitter | Normalized semantic | Request ID | Deployment | Latency | Terminal | Visual use |
|---|---|---|---|---|---|---|---|
| `route_attempt` | OpenAI, Anthropic, canonical request loops; hedge launch | `active` attempt | yes | yes | no | no | show one real target transition |
| `route_fail` | OpenAI, Anthropic, canonical request loops | `failed` | yes | yes | usually | no if failover remains | mark actual attempted target failed |
| `failover` | request handlers | `failover` | yes | failed deployment | no/optional | no | show backend-declared transition only |
| `request_failover` | production lifecycle adapter | `failover` | yes | failed deployment | optional | no | diagnostic corroboration; never compute next target |
| `route_ok` | request handlers | `success` | yes | yes | yes | yes | mark actual target success and settle |
| `candidate_exhausted` | request handlers | `failed` | yes in normal request path | no | no | yes | terminal failure without inventing a target |
| `route_timeout` | request handlers | `failed` | yes | yes | yes | yes | timeout only because backend classifies it |
| `stream_fail` / `stream_fail_precommit` | request handlers | `failed` | yes | yes | yes | yes | terminal/precommit failure as emitted |
| `client_disconnect` | request handlers | `cancelled` | yes | yes | yes | yes | terminal cancellation |
| unknown/malformed | any future emitter | ignored/diagnostic | n/a | n/a | n/a | n/a | never crash or animate |

Every bounded event carries a process `epoch` and monotonic `seq`. The frontend resumes with `since=<last seq>`, ignores duplicates, detects sequence gaps, and refreshes the authoritative snapshot. A changed epoch triggers the same snapshot resynchronization path.

`model_failed` and other deployment lifecycle events are not sufficient alone to claim a request journey because probes can emit them without a request. They may be used only when a request ID is present and the tracker has an existing execution. Fallback order is never calculated in the browser.
