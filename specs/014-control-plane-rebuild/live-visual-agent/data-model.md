# Live Visual Agent Data Model

```text
Execution {
  requestId: string
  startedAt: string
  publicModel: string | empty
  status: active | failover | failed | success | cancelled
  deployment: string | empty
  attempts: [{ deployment, state, latency }]
  latency: number | 0
  terminal: boolean
  updatedAt: number
}
```

The browser only populates fields supported by event/snapshot evidence. `requestId` is required for visual correlation. The tracker deduplicates non-zero `seq` values, ignores events without request identity, retains at most 64 executions, and evicts terminal executions after a four-second bounded settle interval. These are presentation limits and do not alter routing.

Historical snapshot events initialize state without motion. Live events update the same normalized execution object consumed by Activity and Overview. A later backend event cannot rewind a terminal state through an older sequence.
