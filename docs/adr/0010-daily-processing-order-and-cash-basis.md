# ADR 0010: Exits are processed before entries, but entries only ever see the previous close's cash

- Status: Accepted
- Date: 2026-09-08

## Context

Under the intraday fill model (ADR 0005) a daily bar cannot say whether a stop in one instrument occurred before an entry in another. If entries could spend cash freed by same-day exits, or use cap headroom freed by same-day exits, the model would be quietly optimistic and the daily decision would depend on an unknowable ordering.

## Decision

Within a trading day, events are processed in the order **Protective Stops and Exit-Channel exits → Adds → new entries**. However, **the cash and Unit-cap headroom available to every Add and entry are those known at the previous close**. Exits in bar *t* free capital and headroom for bar *t+1*, never for bar *t*.

When the next Unit costs more than the available cash, the Unit is **skipped** and the rejection is journaled with reason `insufficient-cash`; there are no partial Units, no borrowing, and no deferred queue.

Simultaneous signals are ranked by Faith's mechanical strength measure — **(close − close 63 bars ago) / N**, highest first [T p.29] — with ties broken by 20-day median dollar volume and then by symbol.

## Consequences

- The day's decisions are reproducible from a single snapshot at the previous close.
- The model is never optimistic about cash or caps.
- A partial Unit would break the Unit-as-risk-measure invariant the caps depend on; skipping preserves it.
- Sublime's sector-strength ranking is a declared Variant of the ranking rule.
