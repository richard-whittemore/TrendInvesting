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

## Amendment: entries and Adds rest as stop-limit orders capped at level + k·N (2026-09-24)

Decided by the owner (Richard, 2026-09-24, on #220: "only the available amount should dictate what gets purchased"). Rule 1 above lets a long gap fill at any open, however far above the level, so no affordability check made before the bar can bound what the fill costs. That is how #212's 2011-07-20 gap fill overspent a Unit that was affordable at its level. Entries and Adds therefore carry a **price cap**.

**The order.** Every entry and Add rests as a **stop-limit** buy: its stop is the proposal's level, and its limit, the price cap, is **level + k·N**. N is the N the Unit's slippage is measured in: the decision N for an entry, the Campaign's frozen N for an Add (ADR 0006). k is the configured `GapBufferN`, finite and at least zero. **The Baseline declares k = 1**, a Baseline-declared adaptation under ADR 0012, and other values (0.5, 2) are testable as Variants. The proposal carries the cap (`price_cap`, with `order_type` and `gap_buffer_n`, so the cap is re-derivable from the payload), and the reducer's affordability check and hold use it (ADR 0020's amendment of the same date).

**How a bar fills it.** For a buy with stop `level`, cap `C`, reference `R` (the bar's open, or the creating fill's price for an order created inside the bar; see `internal/fills`' `Range`), high `H` and low `L`:

1. `H < level`: not triggered, no fill. Unchanged.
2. Triggered and `R ≤ C`: rule 1 unchanged. It executes at `max(level, R)`, which is at most the cap.
3. Triggered with `R > C`, a gap above the cap: the order is triggered at the reference and then works as a limit buy at `C`. It fills **at `C`** if the bar trades back down to the cap (`L ≤ C`), and pessimistically at the cap itself rather than at any better price the bar may have offered. Otherwise **it does not fill** and the entry or Add is skipped: the proposal expires with its bar (ADR 0011), and no fill, decline or new event records it.

A fill at the cap after a gap above it did not execute at the open, so it is not an at-the-open execution for rule 3's ordering. An order that rested from the previous Session and opens above its cap does not fill in the open-instant pass, and the reducer expires it when that bar arrives, so it is never evaluated against the rest of that bar.

**Slippage and the cap.** Rule 2 is unchanged: ADR 0013's `SlippageN × N` is added to every buy's execution price. The cap bounds the **execution price before slippage**, which is the market price at which the order trades. So a recorded fill price is at most `C + SlippageN × N`, and that is exactly the per-share price the hold reserved. Two other readings were rejected:

- **Requiring the slipped price to be at most `C`.** A cap below `level + SlippageN × N` could then never fill, so k = 0 could never trade. It would also treat a modelled cost as though it were a market price.
- **Clamping the slipped price at `C`.** That charges less than ADR 0013's slippage on exactly the fills that gapped furthest, which is optimistic, the one direction this ADR forbids.

With this reading, a fill never costs more than its hold. Rounding is monotone, so `max(level, R) + s ≤ C + s` holds in float64 as well, and ADR 0013's commission never falls as the price rises. Stops are unaffected: a Unit's Protective Stop is measured from its actual fill (ADR 0006).

**The declared Variant `uncapped`** keeps Faith's stop-market entry [T p.18], with `order_type` `stop-market` and `gap_buffer_n` 0, and fills under rule 1 alone. It relies on the account refusing an unfundable fill, so the effect of the cap is measured head to head (ADR 0012) rather than argued. The fill simulator (`internal/fills`) implements both. The LEAN adapter places a LEAN stop-limit order when the proposal carries a cap, and a stop-market order otherwise. LEAN's own stop-limit fill model differs from this one, as `adapter/lean/README.md` records.
