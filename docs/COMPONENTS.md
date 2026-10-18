# Control-plane component set (#416 / #489 item 2)

The small, deliberate component vocabulary of the NexaRoute admin
UI. Every component consumes design tokens only — enforced for
colors by `TestDesignTokenSingleSourceOfTruth` (no raw color
literal may exist outside the token fence in
`internal/httpapi/web/styles.css`) and for this catalog by
`TestComponentCatalogContract`. New rules use CSS logical
properties (`padding-inline`, `inset-inline-start`, …) so EN/FA/PS
layouts share one source. Motion is functional only and the
global `prefers-reduced-motion` rule neutralizes animations.

## Components and their states

| Component | Classes | States | Notes |
|---|---|---|---|
| Status pill | `.status` + one state class | *(default)* muted/unknown · `.good` ok · `.warn` degraded · `.bad` down · `.live` live data · `.stale` stale data · `.disconnected` not connected (dashed border, deliberately un-alarmed) · `.estimated` value is an estimate | One pill, one state at a time. State meaning comes from the semantic tokens (`--ok/--degraded/--down/--live/--stale/--estimated`), never from a raw color. |
| Cost markers | `.cost-estimated`, `.cost-actual` | estimated (violet, dashed underline) / actual | Estimated and actual money must never look identical; pairs with the usage API's `cost_basis` labels (#435). |
| Metric tile | `.metric-card` (+ `.accent`) | default / accent | Overview metrics. Accent border uses the accent token wash. |
| Provider / model rows | `.model-row`, tables + `.status` pills | per-row status via the pill states | Rows compose existing primitives; there is no separate row state language. |
| Route topology | `.topology`, `.topology-node` (`.target`, `.failover`), `.topology-line` (`.active`) | primary / failover target / active path | The route-chain visualization: primary → fallback is drawn by the topology component (see #489 item 4 for RTL mirroring of direction-carrying chains). |
| Modal | `.modal`, `.modal-head`, `.modal-foot` | open / closed | **Decision:** the product's overlay pattern is the modal. No drawer component exists and none is planned; this catalog records that choice so it is not re-litigated per screen. |
| Advanced section | `.advanced` | collapsed / expanded | Progressive disclosure for expert settings (#411 pattern). |
| Empty state | `.empty-state` | — | Dashed border, centered, calm. Pairs with `.state-error` geometry. |
| Loading state | `.state-loading` | pulsing skeleton block | Token-colored (`--muted` wash), `state-pulse` keyframes; the global reduced-motion rule reduces it to a static block. |
| Error state | `.state-error` | — | Same geometry as `.empty-state`, `--bad` dashed border + wash; for panel-level load failures, not form validation. |

## Rules for new components

1. Tokens only. Need a new color? Add a token inside the fence in
   both themes (parity is contract-tested) — never a literal.
2. Reuse the state vocabulary above before inventing a new one;
   a new state class lands with its catalog row in the same PR.
3. Logical properties for anything direction-sensitive.
4. Wiring status is per screen and tracked in #489 item 3
   (live-data visual grammar); this catalog documents the
   primitive layer, and a class listed here is not a claim that
   every screen uses it yet.
