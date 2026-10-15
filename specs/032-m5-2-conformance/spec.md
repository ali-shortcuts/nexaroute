# M5-2 — Provider conformance suite + translation-limit documentation

## Suite

`internal/httpapi/conformance_test.go` + `conformance_fakes_test.go`:
every client ingress (Anthropic /v1/messages, OpenAI
/v1/chat/completions, OpenAI /v1/responses) × every upstream kind
(anthropic, openai_chat, gemini, openai_responses) runs five
scenarios against a dialect-faithful fake upstream that VALIDATES
request shape and addressing for its dialect (role vocabularies,
tool-result forms, model-in-path for Gemini):

S1 text round trip · S2 tool call in the client's dialect ·
S3 tool-result turn (upstream must see the result in ITS dialect,
content intact) · S4 streaming (assembled text + client-dialect
terminal) · S5 upstream 429 → client error envelope, never a 200.

## Defects the suite found on first run (all fixed in this PR)

1. **Canonical Anthropic encoder emitted role "tool"** for
   tool-result turns decoded from OpenAI-family inputs — a
   conversation the real Anthropic API rejects with 400. The IR
   keeps tool results in dedicated RoleTool messages; Anthropic
   carries them as tool_result blocks in user turns. Encoder now
   folds non-assistant roles to user.
2. **Canonical Responses + OpenAI Chat encoders dropped tool
   results** embedded in user-role messages (the shape the
   Anthropic decoder produces): both now emit them as sibling
   function_call_output / role:"tool" items.
3. **Responses ingress lost bundle.path**: doUpstreamWithRepair
   called Adapter.Do (provider chat path) and ignored the bundle
   path, so model-in-path upstreams (Gemini) received a Gemini body
   at /v1/chat/completions for /v1/responses clients. The repair
   dispatcher now takes the path and uses DoPath when set (the
   hedging executor already did).

Each defect has a focused regression (canonical unit tests +
the matrix cell), proven red pre-fix by git stash.

## Documentation

docs/COMPATIBILITY.md gains the full 3×4 conformance matrix, the
per-field translation-limit table (what each encoder maps, drops
or cannot express), and the two-IR-shape note for tool results.
