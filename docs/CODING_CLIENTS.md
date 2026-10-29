# Coding clients

NexaRoute is built to sit in front of coding agents. Every client
below talks to the **gateway** — its public base URL and a
**client key** — never to an upstream provider directly. Upstream
provider URLs and provider keys stay inside the gateway and are
never shown to, or needed by, the client.

The Routing page's **Connect** action on any route generates
these snippets for that route's public model, with the correct
base URL for your deployment, plus a one-click connection test
and a **scoped key** creator that mints a client key allowed
only that route and public model (through the standard virtual
key system: allowed routes/models, RPM/TPM, budget).

## Claude Code

```sh
export ANTHROPIC_BASE_URL="https://gateway.example"
export ANTHROPIC_MODEL="<public-model>"
export ANTHROPIC_AUTH_TOKEN="<client-key>"
```

The gateway serves `POST /v1/messages` and
`POST /v1/messages/count_tokens` for this route. See
`docs/CLAUDE_CODE.md` for the full guide.

## Codex CLI

`~/.codex/config.toml`:

```toml
model = "<public-model>"
model_provider = "nexaroute"

[model_providers.nexaroute]
name = "NexaRoute"
base_url = "https://gateway.example/v1"
env_key = "NEXAROUTE_API_KEY"
wire_api = "responses"
```

Set `NEXAROUTE_API_KEY` to a NexaRoute client key. Codex uses
the gateway's OpenAI Responses endpoint.

Known limitation (REAL-BINARY-LOOPBACK, Codex 0.164.0-alpha.9):
Codex's default toolset is rejected fail-closed, because it includes
non-function Responses tools the gateway cannot represent canonically:
a `namespace` tool (`multi_agent_v1`) and a `web_search` tool
(`internal/protocol/canonical/openai_responses.go` rejects any tool
type other than `function`). With both disabled client-side the
loopback run succeeds (`web_search = "disabled"` plus
`[features] multi_agent = false` in `config.toml`; the model must also
advertise `reasoning` capability, since Codex always sends a reasoning
object). Default-toolset Codex against the gateway therefore remains a
gap until the Responses tool policy is deliberately revisited — the
gateway never silently drops tools.

**Compatibility statement (exact):** Codex works through NexaRoute
only with the documented reduced toolset above. The DEFAULT Codex
toolset is NOT compatible: it is rejected fail-closed. Do not read
"Codex works" in any matrix or report as default-toolset support.

**Product choice needed for full default-toolset compatibility**
(owner decision, not a silent engineering default): the gateway must
either (a) keep rejecting non-function tools (current behavior), or
(b) implement real backends for them — a configured web-search
provider behind `web_search` and an agent-delegation backend behind
the `namespace` tool — with canonical tool types for both. The
middle option, translating `web_search`/`namespace` into plain
function declarations, is explicitly NOT chosen: it would fabricate
capabilities the upstream cannot honor (a model calling a search
function that returns no real search results, or a delegation tool
that delegates nothing) and would silently change request semantics.
Until option (b) is designed and authorized, the reduced toolset is
the supported Codex configuration.

## OpenCode

`opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "nexaroute": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "NexaRoute",
      "options": {
        "baseURL": "https://gateway.example/v1",
        "apiKey": "{env:NEXAROUTE_API_KEY}"
      },
      "models": { "<public-model>": { "name": "<public-model>" } }
    }
  },
  "model": "nexaroute/<public-model>"
}
```

## Gemini CLI

```sh
export GOOGLE_GEMINI_BASE_URL="https://gateway.example/v1beta"
export GEMINI_API_KEY="<client-key>"
```

Point Gemini CLI at the gateway's `/v1beta` base and use the route's
public model as the model name (the SDK sends it in the URL path).
The gateway serves `POST /v1beta/models/{model}:generateContent`,
`POST /v1beta/models/{model}:streamGenerateContent` (`alt=sse`), and
`GET /v1beta/models`, which lists the gateway's public models as
`models/<public-model>`. The client key may arrive as
`Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, or the `?key=`
query parameter (the query form is stripped before logging).

Supported (FIXTURE: gateway-stack tests against loopback fake
upstreams, `internal/httpapi/gemini_ingress_test.go`): text and system
instruction, inline images, function declarations with
functionCall/functionResponse round trips, generationConfig
(temperature, topP, topK, maxOutputTokens, stopSequences, JSON
responseMimeType/responseSchema), streaming SSE with a terminal
finishReason frame, and usageMetadata (prompt/candidates/total tokens).

Fail-closed, never silently dropped: `candidateCount` above 1,
`safetySettings`, non-TEXT `responseModalities`, `fileData` and other
content parts the canonical IR cannot represent, and tuned-model paths
are rejected with a Gemini error envelope. A virtual endpoint whose
protocol list is restricted must include `gemini`, and a client key
with a route allow-list must include the `/v1beta/models/` path (or
`*`); model allow-lists match the path model. Opencode upstreams fail
closed for this ingress (no Gemini path mapping exists for them).
Live Gemini CLI runs against real tenants are LIVE-BLOCKED (no
operator authorization or credentials in this environment).

## OpenAI-compatible clients

Any client that accepts an OpenAI base URL can use
`https://gateway.example/v1` with a client key as the API key
and the route's public model name as the model.
