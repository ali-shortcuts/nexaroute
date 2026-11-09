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
assert summary["evidence_class"] == "self-test-no-vault", summary
assert summary["revision"]["git_sha"], summary
assert isinstance(summary["revision"]["worktree_clean"], bool), summary
ids = {c["id"] for c in summary["checks"]}
for required in (
    "selftest.success_summary",
    "selftest.failure_summary",
    "selftest.transit_key_contract",
    "selftest.transit_runner_checks_present",
):
    assert required in ids, required
assert all(set(c) == {"id", "status"} for c in summary["checks"])
script = open("scripts/vault-runner-acceptance.sh").read()
assert 'VAULT_EVIDENCE_CLASS="local-loopback"' in script
assert 'address = "127.0.0.1:${VAULT_PORT}"' in script
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

# --- Least-privilege policy audit (run 41206938283 follow-up) ---
# policy-accept may grant exactly these auth/token paths, and nothing
# broader: create (to mint acceptance tokens is done by root, not by
# this token), lookup-self (read, for vault-accept metadata), and
# revoke-self (update). No sudo, no wildcard, no token lookup/renew/
# revoke of other tokens.
python3 - <<'PY'
import re
hcl = open("scripts/vault-runner-acceptance.sh").read()
m = re.search(r'cat > "\$WORK/policy-accept\.hcl" <<\'EOF\'\n(.*?)\nEOF', hcl, re.S)
assert m, "policy-accept.hcl block not found"
body = m.group(1)
token_paths = sorted(re.findall(r'path "auth/token/([^"]+)"', body))
assert token_paths == ["create", "lookup-self", "revoke-self"], token_paths
assert "sudo" not in body, body
assert 'path "*"' not in body, body
assert "auth/token/renew" not in body, body
print("policy-accept least-privilege audit: PASS", token_paths)

transit_cfg = re.search(r'cat > "\$WORK/policy-transit-config\.hcl" <<\'EOF\'\n(.*?)\nEOF', hcl, re.S)
assert transit_cfg, "policy-transit-config.hcl block not found"
cfg_paths = sorted(re.findall(r'path "([^"]+)"', transit_cfg.group(1)))
assert cfg_paths == ["transit/keys/nexaroute", "transit/keys/nexaroute/config"], cfg_paths
assert 'path "*"' not in transit_cfg.group(1), transit_cfg.group(1)
print("policy-transit-config single-key audit: PASS", cfg_paths)
PY
if ! grep -q 'vault.token_survives_root_revoke" 200' scripts/vault-runner-acceptance.sh; then
  echo "vault-runner-acceptance self-test: FAIL no post-revocation lookup-self survival check" >&2
  exit 1
fi
echo "vault-runner-acceptance survival/policy regression: PASS"
