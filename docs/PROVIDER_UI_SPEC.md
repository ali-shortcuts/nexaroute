# Provider Web UI specification — v0.4

## Add flow

`Providers -> Add provider`

Visible fields, in order:
1. Name
2. Provider ID
3. Endpoint type: OpenAI Compatible / Anthropic Compatible
4. Base URL
5. API Key / Token
6. API key environment-variable reference
7. Authentication mode: Auto / Bearer / x-api-key / None
8. Extra headers JSON
9. Models, one ID per line
10. Enabled toggle

Actions:
- Detect models
- Test connection
- Save provider

## Edit flow

Clicking a provider card reopens the same editor with the saved configuration populated.

Required semantics:
- Base URL is fully visible and editable.
- Provider type is visible.
- Saved models are visible.
- Credential is loaded for an authorized local/admin edit request and placed in a password input.
- Show/Hide changes only browser display, not the stored value.
- If the secret field was not changed, `preserve_secret=true` is sent. This keeps the exact previous `api_key` / `api_key_env` source.
- Provider ID is locked in the current UI after creation to avoid accidental identity changes.
- Delete is explicit and requires confirmation.

## Runtime behavior after save

A successful save:
1. takes a fresh config snapshot under the control-plane mutation lock,
2. validates and prepares the proposed runtime before touching disk,
3. writes the config atomically,
4. reuses unchanged provider adapters so live HTTP pools and credential cooldown state survive unrelated edits,
5. rebuilds only providers whose transport/auth/credential identity actually changed, including rotated environment-backed credentials,
6. reloads the router deployment indexes and invalidates only stale health proofs,
7. triggers the selective probe/recovery engine.

A gateway process restart is not required for provider CRUD.

## Secret handling boundary

The edit endpoint can reveal a resolved secret only through the admin boundary. With the default configuration, admin API access is loopback-only. If an admin API key is configured, that key is required; `bind_local_only=true` still blocks remote admin access.

This UI is intentionally local-first. Exposing it to a network requires additional CSRF/session hardening planned for a later release.
