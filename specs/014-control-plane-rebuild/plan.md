# v0.14.0 Implementation Plan

## Phase 0 — Contracts and baseline

Freeze R1 API/event/auth contracts, run stable backend/frontend tests, and record environment blockers. Preserve Go routing and event behavior.

## Phase 1 — New frontend foundation

Replace the current multi-layer presentation with one componentized static frontend architecture: centralized API client, centralized SSE client, page state, tokens, shell, dialogs, focus management and teardown. Keep the Go embed contract unchanged.

## Phase 2 — Providers and Routing

Implement Providers first, then the simple route builder. Every mutation refreshes authoritative snapshot/provider APIs. Reuse only backend-compatible routing semantics from PR #207.

## Phase 3 — Connect, Overview and Activity

Add contextual Connect with actual values, event-backed Overview route visualization, and Activity request journeys. No synthetic events or animations.

## Phase 4 — Settings, Advanced, accessibility

Move low-level controls behind explicit Advanced/Expert surfaces. Add keyboard navigation, ARIA, reduced-motion handling and narrow-layout behavior.

## Phase 5 — Convergence

Run unit/integration/browser E2E, screenshots from the running application, security/performance checks, requirement matrix, and PR update. Do not release.

## Phase 6 — Live Visual Agent continuation

The bounded LVA sub-spec under `live-visual-agent/` governs event correlation, static execution topology, presentation-only motion, accessibility, reduced motion, and performance evidence. Attempt-start and epoch semantics remain explicit backend capability gates rather than inferred UI behavior.
