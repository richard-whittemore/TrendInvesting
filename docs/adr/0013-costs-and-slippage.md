# ADR 0013: Slippage is volatility-scaled and never zero; commissions use the Interactive Brokers model

- Status: Accepted
- Date: 2026-09-08

## Context

ADR 0005 requires a declared slippage model on every fill. A fixed percentage of price under-penalises volatile instruments and over-penalises quiet ones; a fixed dollar amount does the same across price levels. Faith's own worked example of slippage is denominated in N [T p.19], and every other quantity in the Baseline is expressed in N.

## Decision

**Slippage is 0.05 N per fill**, applied against the trader on every entry, Add, stop, and exit, and sensitivity-tested across 0 to 0.25 N. **Commissions use LEAN's Interactive Brokers fee model.** Both are parameters recorded in every run's configuration and journal.

## Consequences

- Costs scale with the instrument's volatility, consistently with sizing, stops, and ladders.
- Sublime's "enter above the high of the breakout bar" is, in effect, a large deliberate slippage; expressing slippage in N makes that a measurable Variant rather than a hidden assumption.
- A backtest run with zero slippage is invalid by construction and must be rejected by the run registry.

## Amendment: slippage and commission are part of a hold (2026-09-24)

Under the owner's decision of 2026-09-24 (ADR 0005's and ADR 0020's amendments of that date), the reducer's affordability check and the cash hold it places at proposal use the Unit's **worst-case** cost. That cost is the proposal's price cap plus this ADR's slippage, `SlippageN × N` per share, times the quantity and dollars per point, plus this ADR's commission charged on that quantity at that price. The same parameters are used, so the check and the fill model agree:

- The reducer computes the commission with the same arithmetic `internal/fills` charges a fill with (`sizing.Commission`: rate, then floor, then ceiling).
- The charge never falls as the price rises, so the commission at the worst-case price bounds the commission on any fill below it.

Slippage is still applied to every fill and is never zero. The cap bounds the execution price before slippage (ADR 0005's amendment), so the hold covers the cap and the slippage together.
