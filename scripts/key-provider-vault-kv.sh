#!/bin/sh
# NexaRoute master-key provider helper backed by HashiCorp Vault KV v2.
#
# Implements the gateway's key-provider command protocol
# (internal/secrets.CommandProvider): the gateway feeds
#   get <ref>
# or
#   put <ref>
#   <base64 key>
# on stdin; "get" prints only the base64 key on stdout. All diagnostics
# go to stderr; failures exit nonzero (the gateway fails closed).
#
# Configuration (environment):
#   VAULT_ADDR   e.g. https://vault.example:8200   (required)
#   VAULT_TOKEN  Vault token with read/write on the ref path (required)
#   VAULT_HELPER_TIMEOUT  per-request timeout in seconds (optional, default 30)
#   <ref>        a KV v2 data path, e.g. secret/data/nexaroute/master-key
#
# Bounded requests: curl has no total timeout by default, so without an
# explicit bound a hung Vault would hang the caller indefinitely (the
# gateway loads its master key with context.Background, so no caller
# deadline protects this path). Every Vault request here is therefore
# bounded by --connect-timeout 10 and --max-time VAULT_HELPER_TIMEOUT.
# Redirects are deliberately NOT followed (no --location): curl -sf
# returns success for 3xx, so a standby Vault answering a PUT with 307
# used to make this helper report success without writing anything, and
# following the redirect would forward X-Vault-Token to the redirect
# target. Every response status is therefore checked explicitly and only
# 2xx counts as success; a 307 is an explicit failure, and the helper
# must point at the active Vault node (or a balancer that routes to it).
# GET responses are additionally capped at 1 MiB (--max-filesize).
#
# The key lives in the KV entry's "key_b64" field. NOTE: this helper
# implements the raw-key nxs1 custody pattern; the provider contract
# loads raw key material into the gateway process. Vault Transit
# envelope mode (nxs2, key material never leaves Vault) is a separate
# opt-in implemented by the gateway itself with
# NEXAROUTE_KEY_PROVIDER=vault-transit; see docs/EXTERNAL_VERIFICATION.md.
set -eu

if [ -z "${VAULT_ADDR:-}" ] || [ -z "${VAULT_TOKEN:-}" ]; then
  echo "key-provider-vault-kv: VAULT_ADDR and VAULT_TOKEN must be set" >&2
  exit 1
fi

vault_timeout="${VAULT_HELPER_TIMEOUT:-30}"
case "$vault_timeout" in
  ''|*[!0-9]*)
    echo "key-provider-vault-kv: VAULT_HELPER_TIMEOUT must be a positive integer (seconds)" >&2
    exit 1
    ;;
esac
if [ "$vault_timeout" -le 0 ]; then
  echo "key-provider-vault-kv: VAULT_HELPER_TIMEOUT must be a positive integer (seconds)" >&2
  exit 1
fi

read -r op ref || { echo "key-provider-vault-kv: missing operation" >&2; exit 1; }
# The ref is interpolated into the Vault URL, so it must be a plain KV v2
# data path: a ref without /data/ would silently read/write a KV v1 logical
# path (or a version-pinned path via a "?" query), and traversal or
# whitespace would change which entry the request addresses. Reject it
# before any Vault request, with the same shape vault-accept enforces.
case "$ref" in
  ''|/*|*/|*..*|*//*|*' '*|*'?'*|*'#'*)
    echo "key-provider-vault-kv: ref must be a relative KV v2 data path without traversal, whitespace, query, or fragment characters" >&2
    exit 1
    ;;
esac
case "$ref" in
  */data/*) ;;
  *)
    echo "key-provider-vault-kv: ref must be a KV v2 data path containing /data/ (for example secret/data/nexaroute/master-key)" >&2
    exit 1
    ;;
esac
url="${VAULT_ADDR%/}/v1/${ref}"

case "$op" in
  get)
    response=$(curl -sf --connect-timeout 10 --max-time "$vault_timeout" --max-filesize 1048576 -H "X-Vault-Token: ${VAULT_TOKEN}" -w '\n%{http_code}' "$url") || {
      echo "key-provider-vault-kv: vault read failed for ref $ref" >&2
      exit 1
    }
    code=$(printf '%s' "$response" | tail -n 1)
    case "$code" in
      2*) ;;
      *)
        echo "key-provider-vault-kv: vault read returned unexpected HTTP status $code for ref $ref" >&2
        exit 1
        ;;
    esac
    body=$(printf '%s' "$response" | sed '$d')
    key=$(printf '%s' "$body" | grep -o '"key_b64": *"[^"]*"' | head -n 1 | sed 's/.*: *"//; s/"$//')
    if [ -z "$key" ]; then
      echo "key-provider-vault-kv: vault entry at $ref has no key_b64 field" >&2
      exit 1
    fi
    printf '%s\n' "$key"
    ;;
  put)
    read -r keyb64 || { echo "key-provider-vault-kv: missing key line" >&2; exit 1; }
    payload="{\"data\":{\"key_b64\":\"$keyb64\"}}"
    code=$(curl -sf --connect-timeout 10 --max-time "$vault_timeout" --max-filesize 1048576 -X POST -H "X-Vault-Token: ${VAULT_TOKEN}" -H "Content-Type: application/json" \
      --data "$payload" -o /dev/null -w '%{http_code}' "$url") || {
      echo "key-provider-vault-kv: vault write failed for ref $ref" >&2
      exit 1
    }
    case "$code" in
      2*) ;;
      *)
        echo "key-provider-vault-kv: vault write returned unexpected HTTP status $code for ref $ref" >&2
        exit 1
        ;;
    esac
    ;;
  *)
    echo "key-provider-vault-kv: unknown operation '$op'" >&2
    exit 1
    ;;
esac
