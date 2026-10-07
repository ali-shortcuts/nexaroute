# 015 — Claude Code client onboarding hardening

## Problem

The Anthropic Messages routes already exist, but Connect derived its client URL from `location.origin` and did not distinguish a browser fallback from an address reachable by the process running Claude Code. Route editing also did not restore ordered-chain state in the UI.

## Requirements

- `Config.ClientBaseURL` is additive, persisted, environment-overridable with `NEXAROUTE_CLIENT_BASE_URL`, canonicalized without a trailing slash, and validated server-side.
- Empty `ClientBaseURL` remains backward-compatible and is shown as a browser-origin fallback only.
- Client URLs are absolute HTTP(S), have no userinfo/query/fragment, allow HTTP only for loopback development, and never contain credentials.
- Admin snapshot and settings expose only the public URL and source label; no secret is returned.
- Connect shows `POST {base}/v1/messages`, token counting, public alias, auth mapping, shell-safe setup, `/status`, and custom-gateway limitations.
- Settings saves preserve the existing backend retry policy rather than forcing `max_attempts` to 1 or 2.
- Existing route mode and ordered deployment sequence are restored in the editor.

## Out of scope

A new Anthropic endpoint, provider API changes, reverse-proxy implementation, Remote Control, MCP tool search, or real-provider calls.

## Compatibility and rollout

The field is optional. Existing config files load unchanged. Environment precedence is `NEXAROUTE_CLIENT_BASE_URL > persisted client_base_url > browser-origin fallback`. Roll back by clearing the field and reverting the branch; existing `/v1/messages` behavior is untouched.

## Threat model

The URL is operator-visible but not a secret. Userinfo, query, fragment, and remote plaintext HTTP are rejected. Provider credentials and client keys remain write-only and are not included in Connect, snapshot, or docs examples.
