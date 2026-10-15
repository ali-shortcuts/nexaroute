# Example configs

Each file is a complete, loadable NexaRoute configuration
(defaults are applied at load). Copy one, adjust the env var
names if needed, and start the gateway with it:

- `openai.json` — a single OpenAI provider.
- `anthropic.json` — a single Anthropic provider.
- `failover.json` — the same public model on two providers;
  priority 0 serves first, priority 1 is the fallback.
- `local-ollama.json` — a local Ollama instance, no API key.

Every example is loaded and validated by an automated test
(`internal/config` examples test), so they cannot silently rot.
API keys are referenced by env var NAME only — never put key
values in these files.
