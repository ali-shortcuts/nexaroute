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
#   <ref>        a KV v2 data path, e.g. secret/data/nexaroute/master-key
#
# The key lives in the KV entry's "key_b64" field. NOTE: this is the
# supported Vault pattern for NexaRoute — the provider contract loads
# raw key material into the gateway process. Vault Transit envelope
# mode (key material never leaves Vault) would require a different
# provider contract and is NOT what this helper implements; see
# docs/EXTERNAL_VERIFICATION.md.
set -eu

if [ -z "${VAULT_ADDR:-}" ] || [ -z "${VAULT_TOKEN:-}" ]; then
  echo "key-provider-vault-kv: VAULT_ADDR and VAULT_TOKEN must be set" >&2
  exit 1
fi

read -r op ref || { echo "key-provider-vault-kv: missing operation" >&2; exit 1; }
url="${VAULT_ADDR%/}/v1/${ref}"

case "$op" in
  get)
    response=$(curl -sf -H "X-Vault-Token: ${VAULT_TOKEN}" "$url") || {
      echo "key-provider-vault-kv: vault read failed for ref $ref" >&2
      exit 1
    }
    key=$(printf '%s' "$response" | grep -o '"key_b64": *"[^"]*"' | head -n 1 | sed 's/.*: *"//; s/"$//')
    if [ -z "$key" ]; then
      echo "key-provider-vault-kv: vault entry at $ref has no key_b64 field" >&2
      exit 1
    fi
    printf '%s\n' "$key"
    ;;
  put)
    read -r keyb64 || { echo "key-provider-vault-kv: missing key line" >&2; exit 1; }
    payload="{\"data\":{\"key_b64\":\"$keyb64\"}}"
    curl -sf -X POST -H "X-Vault-Token: ${VAULT_TOKEN}" -H "Content-Type: application/json" \
      --data "$payload" "$url" > /dev/null || {
      echo "key-provider-vault-kv: vault write failed for ref $ref" >&2
      exit 1
    }
    ;;
  *)
    echo "key-provider-vault-kv: unknown operation '$op'" >&2
    exit 1
    ;;
esac
