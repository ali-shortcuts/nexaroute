# Frontend Data Model

The frontend owns view state only. Backend state is read through one snapshot/provider fetch and updated through backend mutations.

## Authoritative state

`Snapshot` contains deployments, health, health counts, node telemetry, provider health/pressure, safe events, request total, version, client-auth state, route primitives, runtime config and usage/cache metrics. `ProviderSummary` is the safe provider list row. `ProviderEditor` contains only human-editable fields plus masked-secret state and selected model metadata. `SimpleRoute` is `{id,name,public_model,mode,deployments,enabled}` and maps directly to `/admin/api/simple-routes`.

## Ephemeral view state

`activePage`, `modal`, `selectedProvider`, `selectedRoute`, `activityFilter`, `eventCursor`, `sseStatus`, `notice`, `reducedMotion`, `expertMode`, and form drafts are disposable browser state. No routing winner, health score, credential, or sample production record is stored locally.

## Event projection

Activity groups privacy-safe events by `request_id`. Overview uses only events received from `/admin/api/events/stream` and the authoritative snapshot. Missing/duplicate sequence numbers trigger a snapshot refresh; idle pages do not animate.
