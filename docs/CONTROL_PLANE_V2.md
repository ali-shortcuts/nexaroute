# NexaRoute Control Plane v2

## Product intent

Control Plane v2 makes NexaRoute's existing routing power understandable without weakening it.

The normal user flow is deliberately short:

1. Add provider.
2. Enter Base URL and credential.
3. Detect protocol/models.
4. Select models.
5. Save provider.
6. Create a public route such as `coding`.
7. Copy the Claude Code connection snippet.

The Go backend remains the sole configuration and routing authority. The browser does not implement a second router.

## Information architecture

Primary navigation:

- Overview
- Providers
- Routing
- Models
- Observability
- Health
- Connect
- Settings

The lower-level backend objects remain available from **Routing → Advanced**:

- Candidate Pools
- Route Profiles
- Virtual Endpoints
- Fallback Chains / compatibility views

## Provider workflow

The provider drawer is a three-step workflow:

### 1. Connection

- Custom Provider remains first.
- New providers receive a safe generated display name / internal ID (for example `Provider 1` / `provider-1`).
- Base URL and credential are first-class fields.
- Protocol defaults to automatic discovery.
- Existing providers reload their non-secret connection metadata.
- Saved credentials remain write-only; edit mode shows a saved-credential state and a **Replace** action instead of retrieving the literal key.

### 2. Models

Discovery tries the supported provider protocols without requiring the user to understand protocol details first.

The model picker includes:

- search
- Select all
- Select visible
- Clear all
- persistent selected state
- manual model entry
- selected count
- advanced per-model metadata on demand

Advanced routing metadata (priority, weight, costs, capabilities) remains available without dominating the basic flow.

### 3. Verify

Connection verification and selected-model execution tests remain separate operations so model testing does not run accidentally.

## Simple Route Builder

A simple route is compiled into existing NexaRoute backend primitives.

Automatic mode:

```text
Public model
  -> Virtual Endpoint
  -> Route Profile
  -> Candidate Pool
  -> existing router eligibility / health / capability filters
```

Ordered fallback mode creates one generated pool per selected deployment plus a Fallback Chain. The backend resolver continues to determine which candidate is actually eligible.

The frontend never chooses a deployment itself.

## Security invariants

Control Plane v2 preserves:

- write-only provider credentials
- secret-preserving provider edits
- admin key boundary
- SSRF/DNS pinning protections
- TLS verification
- proxy protections
- bounded provider error handling
- backend validation of every persisted object

Base URLs and other non-secret connection metadata can be displayed during edit. Literal provider credentials must not be returned to the browser.

## Native browser dialogs

User-facing `prompt()`, `confirm()`, and `alert()` flows are removed. NexaRoute uses product-owned modals, confirmation dialogs, inline errors and toasts.

## Localization

The control plane ships an English and Persian/Dari shell. Persian mode sets `dir=rtl` and keeps technical values such as URLs, model IDs, code and command snippets left-to-right where appropriate.

## Themes and accessibility

- Dark and light themes are first-class.
- Keyboard focus styles are preserved.
- Layouts collapse for tablets and small screens.
- `prefers-reduced-motion` disables non-essential animation.
- Dialogs use product-owned semantic controls.

## Competitor patterns intentionally adopted

The design follows useful current patterns seen in established AI gateway/router tools:

- provider presets plus custom endpoint
- endpoint/credential first
- model discovery
- searchable multi-select model catalogs
- separate connectivity verification
- advanced routing hidden behind a simpler route abstraction
- observability as a first-class control-plane surface

No third-party UI code or proprietary assets are copied.

## Backend contracts used

Control Plane v2 composes the existing admin API:

- `/admin/api/providers`
- `/admin/api/provider-discover`
- `/admin/api/provider-check`
- `/admin/api/provider-test`
- `/admin/api/candidate-pools`
- `/admin/api/route-profiles`
- `/admin/api/fallback-chains`
- `/admin/api/virtual-endpoints`
- `/admin/api/snapshot`

Every successful mutation is followed by an authoritative refresh from the backend.

## Runtime architecture

The frontend remains static embedded assets under `internal/httpapi/web`.

There is no Node.js runtime requirement and no change to NexaRoute's one-binary installation model.
