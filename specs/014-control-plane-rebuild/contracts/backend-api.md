# Backend API Contract Inventory

All `/admin/api/*` requests use the existing admin authentication middleware (`x-admin-key` / supported bearer form where configured). Successful mutations persist configuration atomically and the UI must refresh authoritative state.

## Provider APIs

| Endpoint | Methods | Contract |
|---|---|---|
| `/admin/api/providers` | GET | `{providers:[summary]}`; summary includes id, name, type, base_url, auth_mode, enabled, model_count, has_secret; no literal secrets |
| `/admin/api/providers` | POST | Body `{provider, preserve_secret?, preserve_headers?, preserve_proxy?, replace_key?, test_models?, mode?}`; returns 201 `{saved:true,provider}` |
| `/admin/api/providers/{id}` | GET | Masked provider detail, secret source/state, header names and proxy structure only |
| `/admin/api/providers/{id}` | PUT | Same body; `preserve_secret`/`replace_key` control write-only credential lifecycle |
| `/admin/api/providers/{id}` | DELETE | Returns `{deleted:true,id}` |
| `/admin/api/provider-presets` | GET | `{presets:[...]}` server-owned provider catalog |
| `/admin/api/provider-discover` | POST | Provider form; returns `{ok,status_code,models:[string]}` or safe error |
| `/admin/api/provider-check` | POST | Provider form; returns reachability/auth/latency/status, never credential values |
| `/admin/api/provider-test` | POST | Provider form plus `test_models` and optional mode `full`/`claude_code`; returns bounded per-model results |

## General rules

The normal UI sends only human fields and selected models. Technical overrides are Advanced. Stored upstream secrets are never read back. Provider API key and NexaRoute client API key are separate credentials.
