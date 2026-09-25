#!/usr/bin/env bash
# Optional manual smoke test for the real Jev model-route API.
#
#   JEV_API_KEY=... ./scripts/smoke-jev.sh
#
# - Disabled by default; never runs in CI; never required for Phase F PASS.
# - Sends synthetic metadata ONLY (no prompts, no transcripts, no credentials
#   other than the Jev key in the Authorization header).
# - Never commit a key. The key travels in Authorization, never in the URL.
set -euo pipefail

if [[ -z "${JEV_API_KEY:-}" ]]; then
  echo "JEV_API_KEY is not set; refusing to run." >&2
  echo "Usage: JEV_API_KEY=... ./scripts/smoke-jev.sh" >&2
  exit 2
fi

payload='{"task":"task_type=general;complexity=low;tools=false;vision=false;reasoning=false;structured_output=false;estimated_context_tokens=120","candidates":[{"id":"c0","description":"ctx=32000 tools=false vision=false reasoning=false streaming=true reliability=unknown latency=unknown headroom=high"},{"id":"c1","description":"ctx=200000 tools=true vision=false reasoning=true streaming=true reliability=higher latency=low headroom=high","latency":"low"}],"priorities":["reliability","latency","cost"],"constraints":["Candidates are pre-filtered eligible deployments; select exactly one of the supplied candidate ids."]}'

echo "payload bytes: ${#payload} (limit 32768)"
if (( ${#payload} > 32768 )); then
  echo "payload exceeds Jev 32 KiB cap; aborting." >&2
  exit 1
fi

curl -sS -m 20 https://www.jevai.org/api/v1/decisions/model-route \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${JEV_API_KEY}" \
  --data-binary "$payload"
echo
