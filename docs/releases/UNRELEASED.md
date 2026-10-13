# Unreleased

## Features

- Added `nexaroute privacy audit` (M3-2): a read-only CLI that inspects configuration and runtime privacy policy surfaces locally — plaintext credentials at rest, credential-bearing or cleartext provider URLs, unsatisfiable `no_training` requirements for route profiles and virtual keys, egress deny-policy, cache and access-log settings — with deterministic exit codes (0/1/2) and text or JSON output. It never prints secret values and never modifies the config file.
- Admin-configurable egress allow/deny policy: a new top-level `egress` config section (`deny_hosts`, `deny_cidrs`, `allow_hosts`, `allow_cidrs`) lets operators restrict which hosts and networks providers may connect to, beyond the built-in special-purpose IP blocklist. Deny lists take precedence and are enforced at URL validation and again for every DNS-resolved address at dial time (PR #283). See `docs/CONFIGURATION.md` ("Global egress policy").

## Security

- Provider egress now blocks the full IANA special-purpose space, including carrier-grade NAT (`100.64.0.0/10`, which contains the Alibaba metadata address `100.100.100.200`), TEST-NETs, benchmarking, reserved, Teredo, site-local and `::/96`. NAT64 (`64:ff9b::/96`) and 6to4 (`2002::/16`) addresses are judged by the IPv4 address they embed, so legitimate IPv6-only deployments keep working.
- Loopback is reachable only when the provider host is spelled as loopback (`127.0.0.1`, `::1`, `localhost`). A public DNS name that resolves to loopback is rejected at dial time.

## Breaking

- Plain `http://` provider base URLs are now accepted only for explicit loopback hosts. Previously the loopback opt-in was derived from the URL scheme, so `http://<any host>` was accepted and API keys could be sent without TLS. Use `https://`, or a loopback address for local development (for example Chat2API on `http://127.0.0.1:5000/v1`).

## Tests

- Table-driven SSRF/egress matrix (`internal/egress/ssrf_matrix_test.go`) and `docs/THREAT_MODEL.md` mapping each mitigation to its test (M3-4, in progress).
