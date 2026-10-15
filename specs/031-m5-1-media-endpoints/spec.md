# M5-1c — Media endpoints (embeddings / images / audio / rerank)

M5-1's endpoint half. Until now the data plane served only chat,
messages, responses, count_tokens and video jobs. This unit adds a
model-routed **media passthrough** for the OpenAI-wire media APIs:

- POST /v1/embeddings         (JSON in/out, usage recorded)
- POST /v1/images/generations (JSON in/out; large base64 payloads pass through)
- POST /v1/audio/speech       (JSON in, BINARY audio out — bytes and
                               content type preserved exactly)
- POST /v1/audio/transcriptions (multipart/form-data in — the model
                               form field is rewritten to the deployment
                               model, file parts pass through byte-exact;
                               JSON out)
- POST /v1/rerank              (JSON in/out, Cohere/Jina wire shape)

Semantics mirror the chat data plane where they apply: client-key
auth, model → deployment routing with ordered failover across
candidates, health success/failure recording, X-Gateway-* response
headers, upstream error passthrough on final failure (bounded),
gateway error envelope for transport failures, global data-plane
admission (the five paths join isDataPlaneRequest).

Scope boundary (explicit): the passthrough targets
**openai_compatible** providers — the wire shape every media
endpoint above is defined by. Deployments of other provider types
are skipped, and when none is eligible the handler answers 400
naming the boundary instead of mangling the payload. Per-cloud
media addressing (Azure deployment URLs once M5-1a lands, Bedrock
InvokeModel translation, Gemini embedContent) are follow-ups at the
path-resolver seam (mediaUpstreamPath), not silent omissions.

Sizes: request bodies up to 32 MiB (audio uploads), JSON responses
bounded by the existing upstream cap, binary (speech) responses
streamed with a 64 MiB cap.
