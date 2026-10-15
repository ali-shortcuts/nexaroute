# M5-4 — Opt-in semantic cache with privacy provenance

The exact-match response cache (existing) stays as is. This unit
adds a SEPARATELY opt-in semantic layer beside it:

- `cache.semantic.enabled` (default off) + `embedding_deployment`
  ("provider/model", must be openai_compatible), `embedding_path`
  (default /v1/embeddings), `threshold` (cosine, default 0.95,
  valid (0.5, 1]), `max_entries` (default 1024), `ttl_seconds`
  (default: the exact cache's TTL).
- Eligibility mirrors the exact cache (non-streaming, deterministic
  sampling) plus: no tools, prompt text extractable, body within
  budget. Embeddings are computed through the gateway's own
  provider adapter for the configured deployment; any embedder
  failure bypasses the cache silently (never fails the request).
- Isolation: entries live in a scope hashed from the client-key
  digest, ingress path, requested model, and the same
  forwarded-headers context the exact cache isolates on. Similarity
  is only ever computed WITHIN a scope.
- Privacy provenance: every entry records its origin deployment,
  the origin provider's trains_on_data class, whether the storing
  request required no_training, its embedding deployment, and its
  creation time. A no_training request can only be served by an
  entry stored under no_training whose origin provider declares
  trains_on_data="no". Hits are observable (X-NexaRoute-Cache:
  SEMANTIC-HIT, X-Gateway-Cache-Provenance, event, metrics).
- Bounds: own LRU (entries + total bytes), TTL, vector dimension
  cap; wholesale invalidation on config swap, like the exact cache.
- Coverage: the same two ingresses the exact cache serves
  (/v1/chat/completions, /v1/messages), including translated
  responses stored at the existing store sites.
