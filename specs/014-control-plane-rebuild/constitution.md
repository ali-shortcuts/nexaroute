# v0.14.0 Control Plane Rebuild Constitution

**Status:** Drafted from the product brief; governs `rebuild/v0.14.0-greenfield-control-plane`.

## Non-negotiable principles

1. **Backend is the source of truth.** The browser composes verified admin APIs and never invents routing, health, telemetry, credentials, or API-key values.
2. **Simple routing is primary.** Public model → selected deployments → routing behavior → NexaRoute backend primitives. Candidate pools, profiles, virtual endpoints, fallback, failover, capability filtering, affinity, health, circuit state, priority, weight, latency, and supported cost behavior remain authoritative in Go.
3. **Advanced objects are contextual.** Pools, profiles, endpoints, compatibility, health internals, and raw tuning are not top-level destinations.
4. **Simple by default; powerful when needed.** Normal users see intent and outcomes; expert controls are explicit and safe.
5. **One frontend architecture.** No competing legacy dashboard, overlay CSS, inline handlers, duplicate pollers, or client-side routing authority. Static assets remain embedded in one Go executable.
6. **Secrets are write-only.** Provider credentials never return to the browser. Upstream keys and NexaRoute client keys are distinct. Never render placeholder text as a usable credential.
7. **Evidence is part of done.** A requirement is not complete without implementation, automated verification, a real running UI connected to the backend, and screenshot evidence.
8. **No release automation.** A PR may be opened or updated, but v0.14.0 is not published. Human visual acceptance is required.

## Quality gates

Run or explicitly mark BLOCKED with evidence: `gofmt`, `git diff --check`, `go vet ./...`, `go test ./...`, race tests, frontend syntax/tests, browser E2E, provider/routing/Connect/SSE tests, security checks, build, Docker, and installer checks.

## Decision record

Stable `main` is the implementation base. The correction branch is a source of routing/API-compatible ideas only; it is not merged as-is because its UI violates this constitution.
