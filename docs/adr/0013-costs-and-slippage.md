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
