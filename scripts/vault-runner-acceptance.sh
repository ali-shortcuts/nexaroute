#!/bin/bash
# NexaRoute Vault runner acceptance (#648 KV + #649 Transit).
#
# What this is, exactly:
#   A disposable, single-node, non-dev Vault in a container on the
#   SAME GitHub Actions runner that runs this script, reached over
#   loopback HTTP. It is NOT a staging Vault, NOT an HA cluster,
#   NOT an organizational tenant, and NOT a multi-host topology.
#   Every report it writes says evidence_class=runner-loopback and
#   lists those limitations, so a PASS here must never be cited as
#   staging, HA, production, HSM, or tenant acceptance.
#
# What it runs, in order:
#   #648 KV: vault-accept (put/get, rotation, wrong/revoked/expired
#     token, outage, cleanup) plus a real gateway boot with a
#     Vault-held key, wrong-token boot fail-closed, KV rotation
#     recovery, and a Vault stop/start + unseal + boot recovery.
#   #649 Transit: migrate an nxs1 config to nxs2 with the real
#     Transit API, verify, tamper fail-closed, wrong-token
#     fail-closed, Vault-side key rotation + rewrap-transit,
#     rollback to the pre-rewrap backup, and unavailable-Vault
#     fail-closed.
#
# Safety:
#   No token, key, ciphertext, or wrapped DEK is ever printed or
#   written to a report. Tokens are created inside the run, used
#   only from files with mode 0600 in a temporary directory, and
#   that directory (plus the Vault container and its data) is
#   deleted by the EXIT trap. The only files that survive are the
#   wrong-token reports in --out-dir. Do NOT run this script with
#   set -x, and do NOT add the temporary directory to an artifact.
#   On GitHub Actions, created tokens are also registered with
#   ::add-mask:: so the runner redacts them from logs.
#
# Usage (from the repository root; the workflow invokes it with bash):
#   bash scripts/vault-runner-acceptance.sh \
#     --out-dir vault-runner-reports
set -euo pipefail

OUT_DIR="vault-runner-reports"
VAULT_IMAGE="hashicorp/vault:1.18.5"
VAULT_PORT="18200"
GATEWAY_PORT="18080"
CONTAINER_NAME="nexaroute-vault-acceptance"

while [ $# -gt 0 ]; do
  case "$1" in
    --out-dir) OUT_DIR="$2"; shift 2 ;;
    --vault-image) VAULT_IMAGE="$2"; shift 2 ;;
    --vault-port) VAULT_PORT="$2"; shift 2 ;;
    --gateway-port) GATEWAY_PORT="$2"; shift 2 ;;
    *) echo "vault-runner-acceptance: unknown argument: $1" >&2; exit 2 ;;
  esac
done

VAULT_ADDR="http://127.0.0.1:${VAULT_PORT}"
KV_REF="secret/data/nexaroute/gateway/master-key"
ACCEPT_REF="secret/data/nexaroute/acceptance/master-key"
HELPER="scripts/key-provider-vault-kv.sh"

mkdir -p "$OUT_DIR"
WORK="$(mktemp -d)"
chmod 700 "$WORK"
CHECKS_FILE="$WORK/checks.tsv"
: > "$CHECKS_FILE"
VAULT_VERSION="unknown"
VAULT_STORAGE="unknown"
VAULT_HA_ENABLED="false"

cleanup() {
  if [ -n "${GATEWAY_PID:-}" ] && kill -0 "$GATEWAY_PID" 2>/dev/null; then
    kill "$GATEWAY_PID" 2>/dev/null || true
    wait "$GATEWAY_PID" 2>/dev/null || true
  fi
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

log() { echo "vault-runner-acceptance: $*"; }
pass() { printf '%s\tPASS\n' "$1" >> "$CHECKS_FILE"; log "PASS $1"; }
fail() { printf '%s\tFAIL\t%s\n' "$1" "$2" >> "$CHECKS_FILE"; log "FAIL $1: $2"; exit 1; }

# mask <value>: register a secret with the Actions runner only. Outside
# Actions this prints nothing, so the value never reaches a log.
mask() {
  if [ "${GITHUB_ACTIONS:-}" = "true" ] && [ -n "${1:-}" ]; then
    echo "::add-mask::$1"
  fi
}

token_from_file() { cat "$1"; }

# vault_api <token-file|-> <method> <path> [body-file]
# Prints the response body to stdout and the HTTP status to stderr as
# "HTTP <code>" on the last line of stderr. Never prints the token.
vault_api() {
  local token_file="$1" method="$2" path="$3" body_file="${4:-}"
  local args=(-s -w '\nHTTP %{http_code}' --max-time 30)
  if [ "$token_file" != "-" ]; then
    args+=(-H "X-Vault-Token: $(token_from_file "$token_file")")
  fi
  if [ -n "$body_file" ]; then
    args+=(-H "Content-Type: application/json" --data @"$body_file")
  fi
  curl "${args[@]}" -X "$method" "${VAULT_ADDR}/v1/${path}"
}

# vault_api_status <token-file> <method> <path> [body-file] [out-file]
# Returns the HTTP status code; writes the body to out-file (or /dev/null).
vault_api_status() {
  local token_file="$1" method="$2" path="$3" body_file="${4:-}" out_file="${5:-/dev/null}"
  local raw code
  raw="$(vault_api "$token_file" "$method" "$path" "$body_file" 2>/dev/null || true)"
  code="$(printf '%s' "$raw" | tail -n 1 | sed 's/^HTTP //')"
  printf '%s' "$raw" | sed '$d' > "$out_file"
  printf '%s' "$code"
}

require_status() {
  # require_status <check> <expected> <actual> <detail>
  if [ "$3" != "$2" ]; then
    fail "$1" "expected HTTP $2, got $3 ($4)"
  fi
}

json_field() { # json_field <file> <jq-filter>
  jq -r "$2" "$1" 2>/dev/null || echo ""
}

for tool in docker curl jq python3 go; do
  command -v "$tool" >/dev/null 2>&1 || fail "prereq" "required tool $tool is missing"
done
[ -x "$HELPER" ] || [ -f "$HELPER" ] || fail "prereq" "helper $HELPER not found"

# ---------------------------------------------------------------- build
log "building gateway binary"
go build -o "$WORK/nexaroute" ./cmd/gateway
pass "gateway.build"

# ------------------------------------------------------- start Vault
log "starting disposable non-dev Vault container ($VAULT_IMAGE)"
docker run -d --name "$CONTAINER_NAME" \
  -p "127.0.0.1:${VAULT_PORT}:8200" \
  -e "VAULT_LOCAL_CONFIG={\"storage\":{\"file\":{\"path\":\"/vault/data\"}},\"listener\":{\"tcp\":{\"address\":\"0.0.0.0:8200\",\"tls_disable\":true}},\"disable_mlock\":true,\"ui\":false}" \
  "$VAULT_IMAGE" server >/dev/null
for _ in $(seq 1 60); do
  if curl -s --max-time 2 "${VAULT_ADDR}/v1/sys/seal-status" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -s --max-time 5 "${VAULT_ADDR}/v1/sys/seal-status" > "$WORK/seal.json" || fail "vault.boot" "seal-status unreachable"
[ "$(json_field "$WORK/seal.json" '.sealed')" = "true" ] || fail "vault.boot" "fresh Vault is not sealed as expected"
VAULT_VERSION="$(json_field "$WORK/seal.json" '.version')"
VAULT_STORAGE="$(json_field "$WORK/seal.json" '.storage_type')"
pass "vault.boot"

# ------------------------------------------------------- init + unseal
printf '{"secret_shares":3,"secret_threshold":2}' > "$WORK/init-body.json"
code="$(vault_api_status "-" POST sys/init "$WORK/init-body.json" "$WORK/init.json")"
require_status "vault.init" 200 "$code" "sys/init"
ROOT_TOKEN="$(json_field "$WORK/init.json" '.root_token')"
mask "$ROOT_TOKEN"
printf '%s' "$ROOT_TOKEN" > "$WORK/root.token"; chmod 600 "$WORK/root.token"
for i in 0 1; do
  share="$(json_field "$WORK/init.json" ".keys[$i]")"
  printf '{"key":%s}' "$(printf '%s' "$share" | jq -Rs .)" > "$WORK/unseal-body.json"
  code="$(vault_api_status "-" POST sys/unseal "$WORK/unseal-body.json" "$WORK/unseal.json")"
  require_status "vault.unseal" 200 "$code" "sys/unseal share $i"
done
curl -s --max-time 5 "${VAULT_ADDR}/v1/sys/seal-status" > "$WORK/seal.json"
[ "$(json_field "$WORK/seal.json" '.sealed')" = "false" ] || fail "vault.unseal" "Vault is still sealed after 2 shares"
[ "$(json_field "$WORK/seal.json" '.storage_type')" != "inmem" ] || fail "vault.unseal" "Vault reports in-memory storage; this is dev mode, not the non-dev run"
pass "vault.init"
pass "vault.unseal"

# ------------------------------------------------------- mounts
printf '{"type":"kv","options":{"version":"2"}}' > "$WORK/mount-kv.json"
code="$(vault_api_status "$WORK/root.token" POST sys/mounts/secret "$WORK/mount-kv.json")"
require_status "vault.mount_kv" 204 "$code" "sys/mounts/secret"
printf '{"type":"transit"}' > "$WORK/mount-transit.json"
code="$(vault_api_status "$WORK/root.token" POST sys/mounts/transit "$WORK/mount-transit.json")"
require_status "vault.mount_transit" 204 "$code" "sys/mounts/transit"
code="$(vault_api_status "$WORK/root.token" GET sys/mounts/secret "" "$WORK/mount-check.json")"
require_status "vault.mount_version" 200 "$code" "sys/mounts/secret read"
[ "$(json_field "$WORK/mount-check.json" '.data.type')" = "kv" ] || fail "vault.mount_version" "secret mount is not type kv"
[ "$(json_field "$WORK/mount-check.json" '.data.options.version')" = "2" ] || fail "vault.mount_version" "secret mount is not KV version 2"
pass "vault.mount_kv"
pass "vault.mount_transit"
pass "vault.mount_version"

# ------------------------------------------------------- policies + tokens
cat > "$WORK/policy-accept.hcl" <<'EOF'
path "secret/data/nexaroute/acceptance/*" { capabilities = ["create", "read", "update"] }
path "secret/metadata/nexaroute/acceptance/*" { capabilities = ["read", "delete"] }
path "secret/data/nexaroute/gateway/*" { capabilities = ["create", "read", "update"] }
path "secret/metadata/nexaroute/gateway/*" { capabilities = ["read", "delete"] }
path "auth/token/create" { capabilities = ["update"] }
path "auth/token/revoke-self" { capabilities = ["update"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
path "sys/mounts/secret" { capabilities = ["read"] }
EOF
cat > "$WORK/policy-transit.hcl" <<'EOF'
path "transit/encrypt/nexaroute" { capabilities = ["update"] }
path "transit/decrypt/nexaroute" { capabilities = ["update"] }
path "transit/rewrap/nexaroute" { capabilities = ["update"] }
EOF
cat > "$WORK/policy-rotate.hcl" <<'EOF'
path "transit/keys/nexaroute/rotate" { capabilities = ["update"] }
EOF
for policy in accept transit rotate; do
  jq -Rs '{policy: .}' "$WORK/policy-$policy.hcl" > "$WORK/policy-$policy.json"
  code="$(vault_api_status "$WORK/root.token" PUT "sys/policies/acl/nexaroute-$policy" "$WORK/policy-$policy.json")"
  require_status "vault.policy_$policy" 204 "$code" "policy nexaroute-$policy"
  pass "vault.policy_$policy"
done

# Transit key: the pinned non-derived aes256-gcm96 type (ADR 0007).
printf '{"type":"aes256-gcm96"}' > "$WORK/transit-key.json"
code="$(vault_api_status "$WORK/root.token" POST transit/keys/nexaroute "$WORK/transit-key.json")"
require_status "vault.transit_key" 204 "$code" "transit/keys/nexaroute"
pass "vault.transit_key"

create_token() { # create_token <policies-csv> <out-token-file>
  local policies="$1" out="$2"
  printf '{"policies":%s,"ttl":"1h","renewable":false,"type":"service"}' \
    "$(printf '%s' "$policies" | jq -R 'split(",")')" > "$WORK/token-body.json"
  local code
  code="$(vault_api_status "$WORK/root.token" POST auth/token/create "$WORK/token-body.json" "$WORK/token.json")"
  require_status "vault.token_create" 200 "$code" "auth/token/create for $policies"
  local tok
  tok="$(json_field "$WORK/token.json" '.auth.client_token')"
  [ -n "$tok" ] || fail "vault.token_create" "no client_token for $policies"
  mask "$tok"
  printf '%s' "$tok" > "$out"; chmod 600 "$out"
}
create_token "nexaroute-accept" "$WORK/kv.token"
create_token "nexaroute-transit" "$WORK/transit.token"
create_token "nexaroute-rotate" "$WORK/rotate.token"
pass "vault.tokens"

# Root is no longer needed: setup is done, and the restart test unseals
# with shares, not with root. Revoking it narrows the rest of the run.
code="$(vault_api_status "$WORK/root.token" POST auth/token/revoke-self)"
require_status "vault.root_revoke" 204 "$code" "revoke root token"
rm -f "$WORK/root.token"
pass "vault.root_revoke"

# ------------------------------------------------------- #648: vault-accept
log "running nexaroute vault-accept (KV harness)"
VAULT_ADDR="$VAULT_ADDR" VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
  "$WORK/nexaroute" vault-accept \
    --ref "$ACCEPT_REF" \
    --helper "$HELPER" \
    --expect-policy nexaroute-accept \
    --timeout 10s \
    --confirm-live \
    --out "$OUT_DIR/vault-kv-acceptance.json" || fail "kv.vault_accept" "vault-accept exited nonzero"
KV_OUTCOME="$(jq -r '.outcome // "unknown"' "$OUT_DIR/vault-kv-acceptance.json")"
KV_FAILED="$(jq -r '.totals.failed // -1' "$OUT_DIR/vault-kv-acceptance.json")"
KV_BLOCKED="$(jq -r '.totals.blocked // -1' "$OUT_DIR/vault-kv-acceptance.json")"
KV_CLASS="$(jq -r '.evidence_class // "unknown"' "$OUT_DIR/vault-kv-acceptance.json")"
if [ "$KV_OUTCOME" != "PASS" ] || [ "$KV_FAILED" != "0" ] || [ "$KV_BLOCKED" != "0" ] || [ "$KV_CLASS" != "loopback" ]; then
  fail "kv.vault_accept" "outcome=$KV_OUTCOME failed=$KV_FAILED blocked=$KV_BLOCKED evidence_class=$KV_CLASS"
fi
pass "kv.vault_accept"

# ------------------------------------------------------- #648: gateway KV boot
helper_run() { # helper_run <op> <ref> [key-b64] <token-file>
  local op="$1" ref="$2" key="${3:-}" token_file="$4"
  if [ "$op" = "put" ]; then
    printf '%s %s\n%s\n' "$op" "$ref" "$key" |
      VAULT_ADDR="$VAULT_ADDR" VAULT_TOKEN="$(token_from_file "$token_file")" VAULT_HELPER_TIMEOUT=10 \
      bash "$HELPER"
  else
    printf '%s %s\n' "$op" "$ref" |
      VAULT_ADDR="$VAULT_ADDR" VAULT_TOKEN="$(token_from_file "$token_file")" VAULT_HELPER_TIMEOUT=10 \
      bash "$HELPER"
  fi
}

python3 -c "import base64,os; print(base64.b64encode(os.urandom(32)).decode())" > "$WORK/gw-key1.b64"
helper_run put "$KV_REF" "$(cat "$WORK/gw-key1.b64")" "$WORK/kv.token" || fail "kv.gateway_put" "helper put failed"
GOT_KEY="$(helper_run get "$KV_REF" "" "$WORK/kv.token" || fail "kv.gateway_get" "helper get failed")"
[ "$GOT_KEY" = "$(cat "$WORK/gw-key1.b64")" ] || fail "kv.gateway_get" "helper get did not return the put key byte-identically"
pass "kv.gateway_put_get"

cat > "$WORK/config-plaintext.json" <<EOF
{"listen":"127.0.0.1:${GATEWAY_PORT}","providers":[{"id":"acceptance","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"acceptance-plaintext","enabled":false}]}
EOF
cp "$WORK/config-plaintext.json" "$WORK/config.json"

boot_gateway() { # boot_gateway <config> <log> ; sets GATEWAY_PID
  local cfg="$1" logfile="$2"
  env NEXAROUTE_KEY_PROVIDER=command \
      NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
      NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" \
      VAULT_ADDR="$VAULT_ADDR" VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
      "$WORK/nexaroute" -no-browser -config "$cfg" > "$logfile" 2>&1 &
  GATEWAY_PID=$!
  local ok=0
  for _ in $(seq 1 60); do
    if curl -s --max-time 2 "http://127.0.0.1:${GATEWAY_PORT}/healthz" >/dev/null 2>&1; then ok=1; break; fi
    if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then break; fi
    sleep 1
  done
  [ "$ok" = "1" ]
}
stop_gateway() {
  if [ -n "${GATEWAY_PID:-}" ] && kill -0 "$GATEWAY_PID" 2>/dev/null; then
    kill "$GATEWAY_PID" 2>/dev/null || true
    wait "$GATEWAY_PID" 2>/dev/null || true
  fi
  GATEWAY_PID=""
}

boot_gateway "$WORK/config.json" "$WORK/gateway1.log" || fail "kv.gateway_boot" "gateway did not reach /healthz (see gateway log in the run, no secrets are logged there)"
pass "kv.gateway_boot"
env NEXAROUTE_KEY_PROVIDER=command NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
    NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" VAULT_ADDR="$VAULT_ADDR" \
    VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
    "$WORK/nexaroute" secrets status --config "$WORK/config.json" > "$WORK/status1.txt" 2>&1 \
  || fail "kv.gateway_status" "secrets status failed after boot"
grep -q "nxs1_fields=1" "$WORK/status1.txt" || fail "kv.gateway_status" "boot did not encrypt the config to nxs1_fields=1"
[ ! -e "$WORK/config.json.key" ] || fail "kv.gateway_status" "a local .key file was created despite the Vault provider"
env NEXAROUTE_KEY_PROVIDER=command NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
    NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" VAULT_ADDR="$VAULT_ADDR" \
    VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
    "$WORK/nexaroute" secrets verify --config "$WORK/config.json" > "$WORK/verify1.txt" 2>&1 \
  || fail "kv.gateway_verify" "secrets verify failed after boot"
grep -q "verified_fields=1" "$WORK/verify1.txt" || fail "kv.gateway_verify" "verified_fields is not 1"
pass "kv.gateway_status"
pass "kv.gateway_verify"
stop_gateway

# Wrong-token boot must fail closed, quickly, with no health endpoint.
# (Wrapped in if so set -e does not exit on the expected nonzero code.)
if env NEXAROUTE_KEY_PROVIDER=command NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
    NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" VAULT_ADDR="$VAULT_ADDR" VAULT_TOKEN="invalid-acceptance-token" \
    timeout 15 "$WORK/nexaroute" -no-browser -config "$WORK/config.json" > "$WORK/gateway-wrong.log" 2>&1; then
  fail "kv.gateway_wrong_token" "gateway booted with an invalid Vault token"
fi
pass "kv.gateway_wrong_token"

# KV rotation: replace the entry, prove the old config fails, then
# re-encrypt from the plaintext copy and prove the new key boots.
python3 -c "import base64,os; print(base64.b64encode(os.urandom(32)).decode())" > "$WORK/gw-key2.b64"
helper_run put "$KV_REF" "$(cat "$WORK/gw-key2.b64")" "$WORK/kv.token" || fail "kv.rotation_put" "helper put of the replacement key failed"
if env NEXAROUTE_KEY_PROVIDER=command NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
    NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" VAULT_ADDR="$VAULT_ADDR" \
    VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
    "$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1; then
  fail "kv.rotation_old_fails" "old config verified under the rotated key"
fi
pass "kv.rotation_old_fails"
cp "$WORK/config-plaintext.json" "$WORK/config.json"
boot_gateway "$WORK/config.json" "$WORK/gateway2.log" || fail "kv.gateway_boot_rotated" "gateway did not boot after KV rotation re-encryption"
env NEXAROUTE_KEY_PROVIDER=command NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
    NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" VAULT_ADDR="$VAULT_ADDR" \
    VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
    "$WORK/nexaroute" secrets verify --config "$WORK/config.json" > "$WORK/verify2.txt" 2>&1 \
  || fail "kv.gateway_verify_rotated" "secrets verify failed after rotation"
grep -q "verified_fields=1" "$WORK/verify2.txt" || fail "kv.gateway_verify_rotated" "verified_fields is not 1 after rotation"
pass "kv.rotation_recovery"
stop_gateway

# Vault stop/start + unseal + boot: single-node restart recovery.
docker stop "$CONTAINER_NAME" >/dev/null
if env NEXAROUTE_KEY_PROVIDER=command NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
    NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" VAULT_ADDR="$VAULT_ADDR" \
    VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
    timeout 15 "$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1; then
  fail "kv.vault_down_fails" "secrets verify succeeded while Vault was stopped"
fi
pass "kv.vault_down_fails"
docker start "$CONTAINER_NAME" >/dev/null
for _ in $(seq 1 60); do
  if curl -s --max-time 2 "${VAULT_ADDR}/v1/sys/seal-status" >/dev/null 2>&1; then break; fi
  sleep 1
done
for i in 0 1; do
  share="$(json_field "$WORK/init.json" ".keys[$i]")"
  printf '{"key":%s}' "$(printf '%s' "$share" | jq -Rs .)" > "$WORK/unseal-body.json"
  code="$(vault_api_status "-" POST sys/unseal "$WORK/unseal-body.json" "$WORK/unseal.json")"
  require_status "kv.vault_restart_unseal" 200 "$code" "sys/unseal after restart"
done
pass "kv.vault_restart_unseal"
boot_gateway "$WORK/config.json" "$WORK/gateway3.log" || fail "kv.gateway_boot_restart" "gateway did not boot after Vault restart + unseal"
env NEXAROUTE_KEY_PROVIDER=command NEXAROUTE_KEY_PROVIDER_COMMAND="$(pwd)/$HELPER" \
    NEXAROUTE_KEY_PROVIDER_REF="$KV_REF" VAULT_ADDR="$VAULT_ADDR" \
    VAULT_TOKEN="$(token_from_file "$WORK/kv.token")" \
    "$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1 \
  || fail "kv.gateway_verify_restart" "secrets verify failed after Vault restart"
pass "kv.gateway_verify_restart"
stop_gateway

# Current KV key becomes the nxs1 legacy key for the Transit migration.
helper_run get "$KV_REF" "" "$WORK/kv.token" > "$WORK/legacy-key.b64" || fail "transit.legacy_key" "helper get for the legacy key failed"
python3 - "$WORK/legacy-key.b64" "$WORK/legacy.key" <<'PYEOF'
import base64, sys
raw = open(sys.argv[1]).read().strip()
open(sys.argv[2], "wb").write(base64.b64decode(raw))
PYEOF
chmod 600 "$WORK/legacy.key"
[ "$(stat -c %s "$WORK/legacy.key")" = "32" ] || fail "transit.legacy_key" "legacy key file is not 32 bytes"
pass "transit.legacy_key"

# ------------------------------------------------------- #649: Transit
export NEXAROUTE_KEY_PROVIDER=transit
export NEXAROUTE_TRANSIT_ENABLED=true
export VAULT_TOKEN="$(token_from_file "$WORK/transit.token")"
export NEXAROUTE_TRANSIT_MOUNT=transit
export NEXAROUTE_TRANSIT_KEY=nexaroute

"$WORK/nexaroute" secrets status --config "$WORK/config.json" > "$WORK/status-nxs1.txt" 2>&1 \
  || fail "transit.status_nxs1" "secrets status failed on the nxs1 config"
grep -q "nxs1_fields=1" "$WORK/status-nxs1.txt" || fail "transit.status_nxs1" "config is not nxs1_fields=1 before migration"
grep -q "nxs2_fields=0" "$WORK/status-nxs1.txt" || fail "transit.status_nxs1" "config already contains nxs2 fields before migration"
pass "transit.status_nxs1"

"$WORK/nexaroute" secrets migrate-to-transit --config "$WORK/config.json" --dry-run > "$WORK/migrate-dry.txt" 2>&1 \
  || fail "transit.migrate_dry_run" "migrate-to-transit --dry-run failed"
grep -q "would_migrate_fields=1" "$WORK/migrate-dry.txt" || fail "transit.migrate_dry_run" "dry run did not report would_migrate_fields=1"
pass "transit.migrate_dry_run"

"$WORK/nexaroute" secrets migrate-to-transit --config "$WORK/config.json" --legacy-key-file "$WORK/legacy.key" > "$WORK/migrate.txt" 2>&1 \
  || fail "transit.migrate" "migrate-to-transit failed"
grep -q "migrated_fields=1" "$WORK/migrate.txt" || fail "transit.migrate" "migrate did not report migrated_fields=1"
BACKUP_COUNT="$(ls "$WORK"/config.json.*.pre-transit.bak 2>/dev/null | wc -l)"
[ "$BACKUP_COUNT" = "1" ] || fail "transit.migrate" "expected exactly one .pre-transit.bak backup, found $BACKUP_COUNT"
[ ! -e "$WORK/config.json.key" ] || fail "transit.migrate" "a local .key file exists after Transit migration"
pass "transit.migrate"

"$WORK/nexaroute" secrets status --config "$WORK/config.json" > "$WORK/status-nxs2.txt" 2>&1 \
  || fail "transit.status_nxs2" "secrets status failed on the nxs2 config"
grep -q "nxs2_fields=1" "$WORK/status-nxs2.txt" || fail "transit.status_nxs2" "config is not nxs2_fields=1 after migration"
grep -q "transit_key_versions=1" "$WORK/status-nxs2.txt" || fail "transit.status_nxs2" "transit_key_versions is not 1 after migration"
"$WORK/nexaroute" secrets verify --config "$WORK/config.json" > "$WORK/verify-nxs2.txt" 2>&1 \
  || fail "transit.verify" "secrets verify failed after migration"
grep -q "verified_fields=1" "$WORK/verify-nxs2.txt" || fail "transit.verify" "verified_fields is not 1 after migration"
pass "transit.status_nxs2"
pass "transit.verify"

# Tamper: flip one character inside the nxs2 envelope; verify must fail.
cp "$WORK/config.json" "$WORK/config-tampered.json"
python3 - "$WORK/config-tampered.json" <<'PYEOF'
import sys
p = sys.argv[1]
s = open(p).read()
i = s.index("nxs2:")
j = i + 5
c = s[j]
s = s[:j] + ("A" if c != "A" else "B") + s[j+1:]
open(p, "w").write(s)
PYEOF
if "$WORK/nexaroute" secrets verify --config "$WORK/config-tampered.json" >/dev/null 2>&1; then
  fail "transit.tamper_fails" "tampered nxs2 config verified"
fi
pass "transit.tamper_fails"

# Wrong-token verify must fail closed with no local fallback.
if VAULT_TOKEN="invalid-acceptance-token" "$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1; then
  fail "transit.wrong_token" "nxs2 config verified with an invalid Vault token"
fi
[ ! -e "$WORK/config.json.key" ] || fail "transit.wrong_token" "a local .key file appeared after a failed Transit verify"
pass "transit.wrong_token"

# Vault-side rotation + rewrap-transit, then rollback to the backup.
printf '{}' > "$WORK/rotate-body.json"
code="$(vault_api_status "$WORK/rotate.token" POST transit/keys/nexaroute/rotate "$WORK/rotate-body.json")"
require_status "transit.key_rotate" 204 "$code" "transit/keys/nexaroute/rotate"
pass "transit.key_rotate"
"$WORK/nexaroute" secrets rewrap-transit --config "$WORK/config.json" --dry-run > "$WORK/rewrap-dry.txt" 2>&1 \
  || fail "transit.rewrap_dry_run" "rewrap-transit --dry-run failed"
grep -q "transit_key_versions=1" "$WORK/rewrap-dry.txt" || fail "transit.rewrap_dry_run" "dry run did not report the pre-rewrap version 1"
pass "transit.rewrap_dry_run"
"$WORK/nexaroute" secrets rewrap-transit --config "$WORK/config.json" > "$WORK/rewrap.txt" 2>&1 \
  || fail "transit.rewrap" "rewrap-transit failed"
grep -q "rewrapped_fields=1" "$WORK/rewrap.txt" || fail "transit.rewrap" "rewrap did not report rewrapped_fields=1"
REWRAP_BACKUP="$(ls "$WORK"/config.json.*.pre-rewrap.bak 2>/dev/null | head -n 1)"
[ -n "$REWRAP_BACKUP" ] || fail "transit.rewrap" "no .pre-rewrap.bak backup was written"
"$WORK/nexaroute" secrets status --config "$WORK/config.json" > "$WORK/status-rewrapped.txt" 2>&1 \
  || fail "transit.status_rewrapped" "secrets status failed after rewrap"
grep -q "transit_key_versions=2" "$WORK/status-rewrapped.txt" || fail "transit.status_rewrapped" "transit_key_versions is not 2 after rewrap"
"$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1 \
  || fail "transit.verify_rewrapped" "secrets verify failed after rewrap"
pass "transit.rewrap"
pass "transit.status_rewrapped"
pass "transit.verify_rewrapped"

cp "$WORK/config.json" "$WORK/config-rewrapped.json"
cp "$REWRAP_BACKUP" "$WORK/config.json"
"$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1 \
  || fail "transit.rollback_backup" "restored pre-rewrap backup did not verify (old key version must remain recoverable)"
pass "transit.rollback_backup"
cp "$WORK/config-rewrapped.json" "$WORK/config.json"
"$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1 \
  || fail "transit.verify_after_rollback" "rewrapped config did not verify after the rollback check"
pass "transit.verify_after_rollback"

# Unavailable Vault must fail closed (wrong loopback port, no container stop).
if VAULT_ADDR="http://127.0.0.1:1" "$WORK/nexaroute" secrets verify --config "$WORK/config.json" >/dev/null 2>&1; then
  fail "transit.unavailable_fails" "nxs2 config verified against an unreachable Vault address"
fi
pass "transit.unavailable_fails"

# ------------------------------------------------------- summary + leak scan
python3 - "$CHECKS_FILE" "$OUT_DIR/vault-runner-summary.json" "$VAULT_VERSION" "$VAULT_STORAGE" "$VAULT_HA_ENABLED" "$VAULT_IMAGE" <<'PYEOF'
import json, sys
checks_path, out_path, version, storage, ha, image = sys.argv[1:7]
checks = []
for line in open(checks_path):
    parts = line.rstrip("\n").split("\t")
    checks.append({"id": parts[0], "status": parts[1], "detail": parts[2] if len(parts) > 2 else ""})
passed = sum(1 for c in checks if c["status"] == "PASS")
failed = sum(1 for c in checks if c["status"] == "FAIL")
summary = {
    "schema": "nexaroute.vault-runner-acceptance.v1",
    "evidence_class": "runner-loopback",
    "outcome": "PASS" if failed == 0 and passed > 0 else "FAIL",
    "vault": {"version": version, "storage_type": storage, "ha_enabled": ha == "true",
              "topology": "single-node container on the same GitHub Actions runner",
              "image": image, "address": "http://127.0.0.1 (loopback, http allowed for loopback only)"},
    "issues": ["648", "649"],
    "totals": {"checks": len(checks), "passed": passed, "failed": failed},
    "checks": checks,
    "limitations": [
        "This run used a disposable Vault container on the same runner, over loopback HTTP. It is NOT a staging Vault.",
        "HA failover was NOT tested: the Vault was single-node, and the restart test is a container stop/start + unseal, not an HA failover event.",
        "Long-running token renewal, AppRole/Kubernetes auth, HSM/KMS backing, and multi-host topology were NOT tested.",
        "The KV gateway boot and the Transit migration used a dummy provider key (acceptance-plaintext), never a real provider credential.",
        "No token, key, ciphertext, or wrapped DEK is included in any report.",
    ],
}
with open(out_path, "w") as f:
    json.dump(summary, f, indent=2)
    f.write("\n")
print(f"vault-runner-acceptance: {passed}/{len(checks)} checks PASS")
PYEOF
chmod 600 "$OUT_DIR/vault-runner-summary.json" 2>/dev/null || true

# Leak scan: no created token, key, or wrapped value may appear in a report.
for secret_file in "$WORK/kv.token" "$WORK/transit.token" "$WORK/rotate.token" "$WORK/legacy-key.b64"; do
  if [ -f "$secret_file" ] && grep -F -q "$(cat "$secret_file")" "$OUT_DIR"/* 2>/dev/null; then
    fail "hygiene.leak_scan" "a secret value was found in an output report"
  fi
done
if grep -q "key_b64" "$OUT_DIR"/* 2>/dev/null; then
  fail "hygiene.leak_scan" "a key_b64 field was found in an output report"
fi
pass "hygiene.leak_scan"

log "DONE: reports in $OUT_DIR (vault-kv-acceptance.json, vault-runner-summary.json)"
log "Evidence class is runner-loopback only. Do NOT cite this run as staging, HA, production, HSM, or tenant acceptance."
