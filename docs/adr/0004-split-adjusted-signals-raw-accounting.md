# ADR 0004: Signals see split-adjusted prices; accounting uses raw prices

- Status: Accepted
- Date: 2026-09-06

## Context

Historical equity prices can be delivered raw, split-adjusted, or adjusted for both splits and dividends. Splits must be neutralised or a 2-for-1 reads as a 50 % crash and every channel and N is corrupted. Dividend adjustment, however, rewrites history: the adjustment factor changes each time a dividend is paid, so a 55-day high computed today from a dividend-adjusted series is not the level that actually existed on the day, and breakout levels drift retroactively as dividends accrue. That is a form of look-ahead, and it makes replayed decisions differ from live ones.

## Decision

All signal computation — Entry and Exit Channels, N, pivots, any Sublime feature — runs on **split-adjusted** prices. Order pricing, fills, and portfolio accounting use **raw** prices, with dividends credited as cash events when they occur.

## Consequences

- Signals see the price levels that genuinely existed; a level computed in a backtest is the same level a live system would have seen on that day.
- The equity curve still earns dividends, through cash, without double-counting them in price.
- Two price views must be maintained and never mixed. The adapter is responsible for delivering both; a domain calculation that received the wrong view would produce wrong levels silently, so the event contract must label which view a price belongs to.

## Amendment: what "raw" is actually required for, and the one-view rule (2026-09-17)

### Context

The Decision above reads "Order pricing, fills, and portfolio accounting use raw prices." None of the three does, deliberately. `internal/fills` decides and reports every fill price on the split-adjusted view, because that is the view every level it fills against was computed on, and comparing a level against a price in the other view would fill orders at levels that never existed. The Delisting Exit — the one exit that reaches a bar price directly rather than through a fill — takes the split-adjusted close for the same reason: its exit price is subtracted from entry prices that came from those fills. The disagreement stayed invisible because every slice-1 fixture gives a bar identical views.

The Context above argues for only one of the two things the word "raw" bundles together. Its whole case is that *dividend* adjustment rewrites history: the factor changes each time a dividend is paid, so a level computed today is not the level that existed on the day. It makes no case against *split* adjustment; it requires it.

That asymmetry has a reason worth stating. A split adjustment divides price and multiplies share count by the same factor, so `quantity × price` is invariant under it: every cash amount and every realised result is the same number in either view. A dividend has no compensating change in quantity, which is exactly why it cannot be neutralised in the price series and must be credited as cash instead.

What is not invariant under the factor is anything computed per share or against an absolute price level: a per-share commission and its per-order minimum and cap (ADR 0013), the universe's $5 price floor (ADR 0009), Sublime's $20 floor, a nominal tick. Those need the raw view, and none of them reads a price view today.

Raw accounting also presupposes a mechanism that does not exist: a split has to change a held position's share count. `event.CorporateActionPayload` recognises only a delisting, so a Campaign's quantity is whatever its entry fill recorded. Pricing an exit from the raw view against a quantity in the adjusted view would report a real loss as a profit.

> **Note (2026-09-25):** [ADR 0023](0023-a-split-carries-its-cash-in-lieu-as-a-corporate-action.md) adds a split kind to `event.CorporateActionPayload`. It carries only a split's cash in lieu, the whole raw shares a broker could not deliver: the split-adjusted view needs no other change at a split. A Campaign's money stays in that view.

### Decision

Signal computation on split-adjusted prices is unchanged, as is crediting dividends as cash rather than folding them into either series.

**A Campaign's money is computed entirely within one price view — the view its own fills were priced in — and prices from two views are never subtracted from one another.** Until the quantities below are implemented, that single view is split-adjusted throughout: fill prices, and the Delisting Exit's last available price.

**Raw prices are required for the quantities that are not invariant under a split adjustment factor**: per-share commission and its per-order minimum and cap, absolute price floors in universe eligibility, and any nominal tick.

Moving a Campaign's money to the raw view is a change of view for the whole Campaign, not for one price within it, and cannot be done before a split corporate action adjusts a held position's quantity.

### Consequences

- Both views must still be delivered and never mixed. What "never mixed" means is now a rule about a single calculation, rather than a convention each call site restates in its own words.
- A Delisting Exit's exit price is the split-adjusted close of the last completed bar it has, consistently with the entry prices it is subtracted from.
- `event.CompletedBarPayload` labels both of its views, but `event.FillPayload` carries no view label at all, so the rule above is upheld by construction and cannot be checked at the event seam. Labelling a fill's price is a schema change and is tracked separately.
- A realised result is invariant under split adjustment provided the held quantity moves with the price, so adopting the raw view later changes no P&L figure — only the per-share and threshold quantities named above.
- The first per-share cost or absolute price threshold to be implemented is where the raw view starts being read, and it must state which view each of its inputs is in.
