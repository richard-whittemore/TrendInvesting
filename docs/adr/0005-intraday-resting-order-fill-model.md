# ADR 0005: Daily-bar backtests model fills as intraday resting orders, resolved pessimistically

- Status: Accepted
- Date: 2026-09-07

## Context

Faith's Turtles executed intraday, the moment price traded through a level, entering on the open if the market gapped through it [T p.18]. The Baseline runs on daily bars, so it must choose a fill model. The obvious "conservative" choice — signal on the completed bar, fill at the next open — is not conservative at all: it systematically enters late on exactly the fast breakout days that produce the strategy's large winners, taxing the right tail the whole approach depends on, and it makes results incomparable with the source.

Modelling a resting order at the level is faithful and matches how the strategy would run live (a resting buy-stop *is* the implementation), but every intrabar fill asserts something about the order in which prices were touched that daily bars cannot verify.

## Decision

Entries, Adds, Protective Stops, and Exit-Channel exits all use **one fill model**: a resting order at the level, filled in the bar whose range first covers it. Three rules prevent the model from ever flattering the equity curve:

1. **Gaps fill at the open.** A long fills at `max(level, open)`; a stop fills at `min(level, open)`. A level is never assumed reachable if the bar opened beyond it.
2. **A declared slippage model applies to every fill.** It is a named parameter and is sensitivity-tested, never zero by default. Add Ladders are measured from the *slipped* fill, as Faith specifies [T p.19].
3. **Same-bar ambiguity resolves pessimistically.** When one bar's range covers both an entry (or Add) and its Protective Stop, the Campaign is assumed to have entered *and then* been stopped. The favourable ordering is never assumed.

Next-open execution is retained as a declared Variant so the effect of the fill model is measured rather than argued.

## Consequences

- Backtest results are comparable with published Turtle results and with live behaviour.
- Any bar that fills both an entry and a stop is recorded as a loss; this understates performance in whipsaw conditions, deliberately.
- The slippage parameter must appear in every run's configuration and in the decision journal.
- The adapter must expose enough of each bar (open, high, low, close) for the domain to apply these rules; a close-only feed is insufficient.

## Amendment: a Protective Stop and an Exit-Channel exit rest as one order per Unit (2026-09-24)

The Decision above lists Protective Stops and Exit-Channel exits as resting orders under one fill model. It does not say they are *separate* orders, and they are not. Each held Unit rests exactly one sell order for its own shares — its **Exit Order** (CONTEXT.md) — at the higher of its own Protective Stop and, while an Exit-Channel exit is proposed for its Campaign, the Exit Channel level; a tie names the stop. A long Unit sold at the higher of two sell levels is sold at the first one price reaches, and one order per Unit can never sell the same shares twice. The reducer records that level with `strategy.exit-order.set`, and the backtest's fill simulator fills that order rather than the two levels separately, so a backtest holds the same single order per Unit a live account would.

The three rules apply to the Exit Order unchanged: a gap fills at `min(level, open)`, slippage applies to every fill, and a bar covering both an entry or Add and a Unit's Exit Order enters first and then sells. What changes is that a bar reaching both a stop and the exit level no longer poses an ordering question between two orders for the same Campaign: each Unit sells at its own order's level. Before this amendment the simulator held the stops and the exit as competing orders and filled the lower-priced one first, which sold every Unit at its stop even when the exit level above it had been reached first.

A Unit resting at its own stop fills as a stop naming that Unit. The Units resting at the Exit Channel all rest at the one proposed level and fill together as the exit, after any stop fills in that bar, because an exit closes whatever the Campaign still holds. Any bar that reaches the exit level has also reached every stop above it, so by then the Units still held are exactly those resting at the exit.
