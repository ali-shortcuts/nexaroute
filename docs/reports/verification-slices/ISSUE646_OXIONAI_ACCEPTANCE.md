# Issue #646: OxionAI live acceptance procedure

This procedure replaces direct OpenAI and Anthropic endpoint checks for the revised #646 scope. It tests one OpenAI-family model and one Claude-family model through OxionAI’s single OpenAI-compatible Chat Completions API. A pass is evidence for the selected models, OxionAI endpoint, account and test date only; it is not evidence for direct OpenAI, direct Anthropic Messages, other OxionAI models, or production failover.

## What is tested

The manual workflow runs the same seven bounded canary scenarios for each selected model: authenticated text, SSE streaming, streaming token usage, tool call, tool-result follow-up, invalid-credential rejection, and an error envelope. Each model run is capped at seven calls, 4,096 estimated input tokens, 512 output tokens, 30 seconds per call and three minutes overall. The total approved USD limit is divided equally between the two model runs; a run whose preflight estimate exceeds its half-budget stops before making a provider request. Observed token costs are checked as the run progresses and again in the combined report.

Both canaries use the NexaRoute `openai` preset with protocol `openai_compatible`, Bearer authentication and base URL `https://oxionai.com/v1`. The Claude-family test therefore exercises OxionAI’s documented OpenAI-compatible surface, not Anthropic Messages protocol.

## Prepare the protected credential

In the GitHub repository, create the `conformance-live` Environment if it does not already exist. Configure authorized reviewers and branch access for the candidate branch `feat/issue-646-oxionai-acceptance` and, after merge, `main`. Add the OxionAI key as the Environment secret `NEXAROUTE_OXIONAI_API_KEY`. Do not add it as a repository-wide secret, commit it, or paste it in chat, an Issue, or a workflow input.

The key needs OxionAI `inference` scope. To query the model catalog, it also needs `models:read`. The workflow itself uses Bearer auth and never prints the key.

## Select enabled models and prices

Choose one enabled OpenAI-family model and one enabled Claude-family model from the account’s `GET https://oxionai.com/v1/models` response. For both models, verify the `capabilities` list includes `tools` and `streaming`; otherwise the seven-case acceptance cannot pass. Copy each exact `id` and its `pricing.input_per_1k` / `pricing.output_per_1k` values.

The workflow accepts price per one million tokens, while the catalog reports price per one thousand tokens. Multiply each catalog price by 1,000 when entering the four price fields. Use the exact prices shown for the account and exact model IDs. Do not infer prices from a model-family name or another vendor’s pricing page.

To inspect the account-scoped catalog without putting the key in shell history or command-line arguments, run this locally; the key is requested with hidden input and only model IDs, capabilities and prices are printed:

```bash
python3 - <<'PY'
import getpass
import json
import urllib.request

key = getpass.getpass("OxionAI API key: ")
request = urllib.request.Request(
    "https://oxionai.com/v1/models",
    headers={"Authorization": "Bearer " + key},
)
with urllib.request.urlopen(request, timeout=30) as response:
    catalog = json.load(response)
for model in catalog.get("data", []):
    capabilities = model.get("capabilities", [])
    if "tools" in capabilities and "streaming" in capabilities:
        pricing = model.get("pricing", {})
        print(
            model.get("id"),
            "capabilities=" + ",".join(capabilities),
            "input_per_1k=" + str(pricing.get("input_per_1k")),
            "output_per_1k=" + str(pricing.get("output_per_1k")),
        )
PY
```

The command makes one non-inference catalog request. It does not run a model or spend inference tokens.

## Approve a total cost limit

Before any canary call, decide and record an explicit total USD ceiling for both runs. The workflow divides this ceiling by two and supplies the per-model halves to the harness. The harness estimates the maximum cost from the configured prices and token/call bounds before contacting OxionAI, then stops remaining scenarios if observed usage exceeds a model’s half-budget. Because reported cost depends on the accuracy of the entered prices and provider usage reporting, also review the OxionAI usage dashboard after the run.

No model inference call should be started until the exact model IDs, four prices and total ceiling have been recorded and approved on #646 or in the run record.

## Run and review

After the candidate branch is pushed and the Environment secret is configured, open **Actions → “v0.25.0 OxionAI OpenAI-family + Claude-family live acceptance” → Run workflow** and select `feat/issue-646-oxionai-acceptance`. Enter both model IDs, the four per-million-token prices, and the approved total cost ceiling. A run from `main` can be made later against the same final merged candidate.

A successful run uploads one combined artifact with `openai-family.json`, `claude-family.json`, and `summary.json`. Confirm the summary commit matches the candidate SHA, both reports have all seven scenarios `PASS`, both reports identify `https://oxionai.com/v1` and `openai_compatible`, and the combined estimated/observed costs do not exceed the approved total. Attach the run and artifact links to #646. Do not close #646 for fixture-only test results, missing reports, a provider failure, or incomplete scenarios.

## Source references

OxionAI documents its base URL and Chat Completions format at [API overview](https://oxionai.com/docs), [Chat Completions](https://oxionai.com/docs/chat-completions), [Models](https://oxionai.com/docs/models), [Authentication](https://oxionai.com/docs/authentication), [Streaming](https://oxionai.com/docs/streaming) and [Tool Calling](https://oxionai.com/docs/tool-calling). In particular, the model catalog exposes account-available IDs, tool/streaming capabilities and per-1K prices; the harness’s Claude-family run uses the same OpenAI-compatible endpoint documented by OxionAI.
