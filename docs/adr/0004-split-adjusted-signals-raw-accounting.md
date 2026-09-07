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
