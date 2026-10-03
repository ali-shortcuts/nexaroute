# Client Authentication Contract

`GET /admin/api/snapshot` exposes safe client-auth state only: `{enabled,keys,rpm}`. It does not return client key literals. Provider API keys are upstream credentials and must never be presented as client credentials.

Connect must show the actual gateway base URL derived from the running application, actual public model, and actual client-auth state. If enabled keys are write-only, the UI must say so and offer only a real backend-supported create/rotate action. It must never render `local-placeholder` or token-like explanatory text as a usable key.

Gateway endpoints remain `/v1/messages`, `/v1/chat/completions`, `/v1/responses`, `/v1/messages/count_tokens`, and `/v1/models`. A Connect test must use the actual gateway and requested public model; it must not simulate a response in the browser.
