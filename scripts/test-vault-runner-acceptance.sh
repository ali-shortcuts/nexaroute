#!/usr/bin/env bash
# Deterministic regression checks for scripts/vault-runner-acceptance.sh.
# No Docker, Vault, Go, network, or credentials are required: the script's
# --self-test mode exercises the real summary writer and EXIT trap with
# child processes, and this wrapper also pins the observed Vault 1.18.5
# Transit-key status contract.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=".artifacts/vault-runner-selftest"
rm -rf "$OUT"

bash scripts/vault-runner-acceptance.sh --self-test --out-dir "$OUT"

python3 - "$OUT/vault-runner-summary.json" <<'PY'
import json
import sys

summary = json.load(open(sys.argv[1]))
assert summary["outcome"] == "PASS", summary
assert summary["exit_code"] == 0, summary
ids = {c["id"] for c in summary["checks"]}
for required in (
    "selftest.success_summary",
    "selftest.failure_summary",
    "selftest.transit_key_contract",
):
    assert required in ids, required
assert all(set(c) == {"id", "status"} for c in summary["checks"])
PY

if grep -q 'require_status "vault.transit_key" 204' scripts/vault-runner-acceptance.sh; then
  echo "vault-runner-acceptance self-test: FAIL transit key still expects 204" >&2
  exit 1
fi
if ! grep -q 'require_status "vault.transit_key" 200 "$code"' scripts/vault-runner-acceptance.sh; then
  echo "vault-runner-acceptance self-test: FAIL transit key does not expect 200" >&2
  exit 1
fi
if ! grep -q 'require_status "vault.transit_key_readback" 200 "$code"' scripts/vault-runner-acceptance.sh; then
  echo "vault-runner-acceptance self-test: FAIL transit key readback check missing" >&2
  exit 1
fi

echo "vault-runner-acceptance self-test: PASS"
