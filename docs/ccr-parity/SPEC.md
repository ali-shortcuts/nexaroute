# NexaRoute — CCR-Parity Control Plane (Super Prompt for the builder agent)

## 0. Role and hard rules
You are the sole builder for ONE pull request on `ali-shortcuts/nexaroute` (Go gateway, embedded vanilla-JS UI in `internal/httpapi/web/`).
- Work from `main`. Branch: `feat/ccr-parity-control-plane`. Open ONE PR. Do not split into many PRs.
- All code, identifiers, comments, commit messages, UI strings: English only.
- No heavy new dependencies. Keep the UI embedded in the Go binary (no separate web server, no Node build step required to run).
- Do not break existing behavior: routing, failover, hedging, cache, hashed client keys, admin auth/CSRF, privacy/data-handling fields, video page.
- Remove dead/old leftovers you replace. README is stale (says v0.6 / v0.5.2, repo is at M4): refresh it.
- Honesty rule: if something cannot be verified in your environment, say so in the PR body. Do not claim "zero bugs".
- Finish with: `go vet ./...`, `go test -race ./...`, `node --check internal/httpapi/web/app.js`, plus new tests below.

## 1. Verified current state (from code audit)
Already exists (reuse, do not rewrite):
- Provider types: openai_compatible, anthropic_compatible, gemini, openai_responses (`/v1/responses` exists).
- Backend credential pool, per-key cooldown, client auth keys (`internal/httpapi/client_keys.go`), `preserve_secret`, quota-header intelligence, `internal/providers/presets.go`.
- UI pages only: overview, providers, routing, activity, settings, video.

MISSING compared to the CCR-style reference screens (these are the work):
1. Providers page is a card grid, not a searchable table (no Models pill, no Account-usage column, no inline enable toggle, no delete, no expandable row).
2. Edit Provider modal: no preset dropdown (only a protocol select), no "API key vs Credential pool" selector, no "Pick models" section with connectivity gating, no Loading state.
3. API key field is write-only: after save the field is empty ("Credentials are write-only"). Required behavior: after Save and re-opening Edit, the key is shown (masked, with eye toggle to reveal), exactly like the reference.
4. No API Keys page (Name / Key / Expires / Limits, copy button, add/edit/delete, search).
5. No Agent profiles page (Claude Code + Codex cards).
6. No global Endpoint pill (green dot + "Endpoint" + URL).
7. No Account-usage column (quota/usage status per provider).

## 2. Feature specs

### 2.1 Global Endpoint pill
Top-center pill: status dot (green = `/readyz` ok, amber = degraded, red = down), label "Endpoint", monospace URL (client base URL from config, fallback `location.origin`). Click copies URL. Polls every 5s.

### 2.2 Providers page (table)
- Search box "Search providers" + primary "+ Add" button.
- Columns: PROVIDER (icon/letter avatar, name, protocol badges e.g. "OpenAI Chat, OpenAI Responses, Anthropic Messages"), MODELS (count pill), ACCOUNT USAGE (text, truncated with tooltip), ENDPOINT (monospace base URL), then enable toggle, edit (pencil), delete (trash with confirm).
- Row expands (chevron) to list models with per-model health, latency, enable toggle.
- Toggle persists immediately via existing provider update API and triggers hot reload.
- Protocol badges are derived from what the provider actually supports (type + endpoint overrides), not hard-coded.

### 2.3 Edit / Add Provider modal (must match this order)
1. "Select preset provider": dropdown from `internal/providers/presets.go` + "Other / custom API endpoint". Choosing a preset prefills name, endpoint, protocol; custom leaves them empty.
2. Name.
3. API endpoint (+ helper text: "Enter API endpoint, API key, and at least one model to enable connectivity check.").
4. "Add credentials": two selectable tiles — "API key (use one key for every request)" and "Credential pool (multiple keys with optional limits)". Pool editor: rows of key + optional rpm/concurrency limit, add/remove row.
5. API key input (password type + eye toggle). On reopen it shows the saved key (masked by default, reveal via eye). See 2.4 for the secure mechanism.
6. "Pick models": Detect models button (existing discovery), checklist with search, manual add input.
7. Footer: Cancel, "Test connection" (enabled only when endpoint + key + >=1 model), Save with a spinner "Loading" state while saving.
Round-trip rule: Save -> close -> Edit again must show EXACTLY the same endpoint, preset, protocol, credential mode, key (masked), pool entries, selected models, proxy, headers, auth mode. Add a Go + JS test for this.

### 2.4 Showing saved keys safely (design decision, implement this)
Current design is write-only; the owner explicitly wants keys visible after save (single-operator local use). Implement a controlled reveal:
- Admin list/get responses still return only `has_secret` + masked preview (first 6 + "…" + last 4).
- New admin endpoint `POST /admin/api/providers/{id}/reveal-secret` (also for pool entries by index): requires admin auth + CSRF, rate-limited, writes an audit event (no secret in log), returns the secret once per call.
- Config switch `admin.allow_secret_reveal` default `true` only when the listener is loopback; default `false` otherwise, and the UI hides the eye toggle when disabled.
- Secrets at rest: keep existing secret store behavior; do not downgrade encryption/hashing already in place.
- Never put full secrets in: snapshot, SSE events, metrics, logs, error messages, or HTML data attributes.

### 2.5 Account usage column
Per provider, best-effort and clearly labeled:
- If provider has quota headers observed -> show "remaining requests/tokens" from existing quota intelligence.
- If provider declares an optional `usage_endpoint` (new optional field, GET with the provider's credential) -> poll every N minutes, parse common shapes (OpenAI-style billing/usage, generic `{balance|remaining|quota}`), show result.
- Otherwise show "Usage tracking not configured". On upstream error show "Account endpoint returned HTTP <code>: <short reason>" (truncate, tooltip for full).
- Never block the page on usage polling; cache, timeout 5s, jitter.

### 2.6 API Keys page (client keys)
- Table: NAME, KEY (monospace prefix like `sk-ccr-3Zao…` + copy icon), EXPIRES ("Never" or date), LIMITS ("Unlimited" or rpm/tpm), edit, delete. Search + "+ Add".
- Seed a default key named "Local Gateway" on first run if none exists.
- Add/Edit modal: name, optional expiry date, optional rpm limit, optional allowed profiles/routes.
- Copy button: copies the full key. Keys are hashed at rest today (M3-8): therefore (a) show full key once at creation, and (b) when `admin.allow_secret_reveal` is true store an encrypted revealable copy so Copy works later. If reveal is disabled, copy is only available at creation and the UI says so. Document this trade-off in docs/SECURITY.md.
- Enforce expiry and limits in the existing client-auth middleware; expired key -> protocol-shaped 401.

### 2.7 Agent profiles page
Cards in a 2-column grid + "+ Add profile". Built-in templates: **Claude Code** and **Codex**.
Each card: icon, name, enable toggle, badges (launch mode e.g. "Only opened from NexaRoute" / "System default"; surface e.g. "CLI only" / "CLI & APP"), CONFIGURATION block, and footer buttons: terminal (copy/open launch command), play (launch, only when supported), edit (pencil), delete.
- **Claude Code profile fields**: Model (default model/alias), Fable model, Opus model, Sonnet model, Haiku model (selectors listing real deployments/aliases/virtual routes), Routing (N routes -> link to routing page), Environment variables (count; editable key/value list). Generated env: `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` (the profile's own client key), `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`, plus custom vars. Provide a "Copy launch command" (`env ... claude`) and an optional "Write to ~/.claude/settings.json" action with a diff preview and a backup of the previous file.
- **Codex profile fields**: Model; generates `~/.codex/config.toml` snippet (model_provider -> NexaRoute `/v1`, wire_api = "responses") and env key. Same preview/backup/copy flow. Route through the existing `/v1/responses` ingress; if the chosen upstream cannot serve Responses, show a clear compatibility warning instead of failing silently.
- Each profile auto-creates its own client key named `Profile: <name>` (visible on the API Keys page); deleting the profile offers to delete the key.
- "Only opened from NexaRoute" means: the profile's env is injected only for processes launched through the play/terminal action, never written globally. "System default" means the config is written to the tool's global config file (with consent + backup).
- Backend: `GET/POST/PUT/DELETE /admin/api/agent-profiles`, persisted in config with validation (model refs must exist, env keys uppercase, no secrets echoed). Add RBAC consistent with existing admin_authz.

## 3. Acceptance criteria (all must pass)
1. Add provider "agentrouter" via Other/custom + endpoint + key + models; Save; reopen Edit -> every field identical, key masked and revealable.
2. Providers table shows Models count, Account usage text, Endpoint, toggle, edit, delete, search.
3. Credential pool mode saves multiple keys and reloads them.
4. API Keys page lists "Local Gateway" + profile keys with masked prefix and working copy; expiry/limit enforced (test with expired key -> 401).
5. Claude Code profile with Fable/Opus/Sonnet/Haiku mapping produces correct env; launching `claude` through NexaRoute works against a mock upstream in an integration test.
6. Codex profile produces valid config and a request through `/v1/responses` succeeds against a mock upstream.
7. Endpoint pill reflects real readiness.
8. No secret appears in snapshot/SSE/metrics/logs (add a leak test scanning those outputs for a canary key).
9. Existing test suite green; race detector green; UI works at 1280px and 390px widths.
10. README + docs updated; stale version text removed; CHANGELOG entry added.

## 4. PR body must include
Checklist of 2.1-2.7 with status, list of anything not verifiable here, screenshots of each page, and the security trade-off note for 2.4/2.6.