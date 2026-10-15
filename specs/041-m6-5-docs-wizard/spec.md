# M6-5 — Docs, tested examples, and the <2-minute first-run wizard

The GUI first-run exists (install -> UI -> Providers/Routing
pages). This unit adds the scripted fast path and proves the
examples:

- `nexaroute init`: a first-run wizard that produces a VALID,
  ready-to-serve config with one provider + one model in four
  questions (base URL, API-key env var NAME, model, listen
  address — every question has a default, so Enter-through takes
  seconds). The wizard NEVER asks for a key value; the config
  references the env var, as everywhere in NexaRoute. Fully
  flag-driven for scripts (`nexaroute init --base-url ... --model
  ...` asks nothing). Refuses to overwrite an existing config
  without --force; writes atomically after full validation.
- Tested examples: examples/*.json (OpenAI, Anthropic,
  two-provider failover, local Ollama) — each one is loaded and
  validated by an automated test (internal/config), so the
  examples can never rot silently.
- QUICKSTART gains the wizard path next to the GUI path, with
  the <2-minute budget stated as: install (one command) + init
  (four defaulted questions) + start + a smoke request.
