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

## RTL mirroring (#489 item 4)

Direction-carrying surfaces and how they mirror under
`[dir=rtl]` (set from the UI language):

- **Route topology (Overview):** the horizontal connector's
  gradient flips (`90deg` → `270deg` under `[dir=rtl]`); the
  ≤700px stacked variant keeps its vertical `180deg` fade via a
  restoring rule inside the media block. The pulsing dot is a
  scale animation — direction-agnostic.
- **Attempt waterfall & request steps:** the state accent bar is
  `border-inline-start-*` — it sits at the reading-start edge in
  both directions with zero LTR change.
- **Arrow glyphs:** directional arrows (`→` in the setup steps,
  `↗` on the activity metric) are wrapped in `.dir-arrow`, which
  flips via `transform: scaleX(-1)` under `[dir=rtl]`.
- **Verified by construction (no rule needed):** the `.score-bar`
  fill is a block box, so it grows from the inline-start edge in
  both directions.
- **Exempt:** `⇄` (symmetric), `↻` (rotation glyph), code and
  numeric content (tabular figures, `.mono`), and the language
  switcher's divider border (a symmetric separator between
  buttons, not a chain element).

## Motion audit (#489 item 5)

Motion is functional only. The complete loop inventory, each
justified in writing per the remove-or-justify rule:

| Loop | Used by | Why it exists (function) |
|---|---|---|
| `state-pulse` (opacity) | `.state-loading` | Signals "content is loading" on the skeleton primitive. Static under reduced motion. |
| `visual-pulse` (transform scale + shadow) | `.topology-line.active span` | Marks the route line that is carrying traffic *now* — a live-state signal, not decoration. |
| `visual-walk` (transform rotate ±3°) | `.visual-agent` | The live-execution marker drawn by `visual-agent.js` from real routing events; the element exists only while executions are in flight, and the rocking is its "currently executing" cue. Bounded by the visualizer's execution tracking and 4 s terminal settle; the visualizer has its own `reduced` state. |

Rules, enforced by `TestMotionContract`:

1. **No layout-property animation.** Keyframes animate
   `transform`/`opacity` only — never `width`, `height`, `top`,
   `left`, `right`, `bottom`, `margin` or `padding`.
2. **No all-property transitions.** The interactive cards
   (`.health-layer`, `.provider-tile`, `.route-tile`,
   `button.metric-card`) transition exactly the properties their
   hover/focus states change: `transform`, `border-color`,
   `box-shadow` (`.18s ease`). A bare `transition:` shorthand is
   a contract failure.
3. **`prefers-reduced-motion` is honored everywhere:** the
   global rule collapses all animation/transition durations, and
   a specific block disables the visualizer and topology
   animations outright.
4. A new loop lands with its justification row here in the same
   PR — unlisted animation is a contract failure (every
   `animation:` reference must resolve to a defined `@keyframes`,
   and the defined set is exactly the table above).

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
