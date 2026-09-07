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
