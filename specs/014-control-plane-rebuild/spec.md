# v0.14.0 Product Specification

## IA

Top-level navigation is exactly: **Overview, Providers, Routing, Activity, Settings**. Models, pools, profiles, endpoints, compatibility, health and Connect are contextual surfaces owned by Providers, Routing, Activity or Settings.

## Product flow

A new user can start NexaRoute → add a provider (Provider, Name, Base URL, API Key, Detect Models, test/select models, Test Connection, Save) → create public route `coding` (deployments + Automatic or Ordered fallback) → open Connect → copy actual gateway URL, client-auth state and public model → send a real request → see the exact route in Overview and Activity.

## Screen requirements

- **Overview:** minimal status strip and static idle live-routing view; animate only real SSE-backed request paths.
- **Providers:** empty state, add-provider workflow, searchable model list, per-model Test, health and credential state; never reveal upstream secrets.
- **Routing:** simple public-model route cards and builder. Backend remains authoritative. Advanced primitives are behind an explicit Advanced action.
- **Connect:** contextual route connection details for Claude Code/compatible clients. Provider secrets are never shown; client auth is represented by actual backend state only.
- **Activity:** Requests, Incidents, History, Diagnostics. Request journey is derived only from privacy-safe backend events.
- **Settings:** user intent first; raw thresholds and internals only under Expert Controls.

## Hard prohibitions

No fake data, fake movement, fake health, fake routing, fake credentials, sample production state, raw JSON in normal UI, native browser dialogs, client-side routing decisions, or duplicate frontend architectures.

## Acceptance IDs

- IA-01 five-item navigation.
- PROVIDER-01 through PROVIDER-08 onboarding and secret safety.
- ROUTE-01 through ROUTE-06 simple routing and Advanced boundary.
- CONNECT-01 through CONNECT-05 real gateway/client-auth/model state.
- OVERVIEW-01 through OVERVIEW-04 calm idle and real event path.
- ACTIVITY-01 through ACTIVITY-04 event-backed operational workspace.
- SETTINGS-01 through SETTINGS-03 intent/expert separation.
- A11Y-01 through A11Y-06 keyboard, ARIA, focus, reduced motion, contrast, responsive.
- E2E-01 through E2E-08 deterministic provider/route/connect/request/activity flow.

## Live Visual Agent additions

The Overview topology is the primary Live Visual Agent surface. See `live-visual-agent/spec.md` for immutable LVA requirements. The agent is idle without real request events, consumes the same normalized execution state as Activity, and never selects or computes routes.
