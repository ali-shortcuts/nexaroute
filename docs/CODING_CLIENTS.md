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

## OpenAI-compatible clients

Any client that accepts an OpenAI base URL can use
`https://gateway.example/v1` with a client key as the API key
and the route's public model name as the model.
