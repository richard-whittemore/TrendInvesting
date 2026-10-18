# D7-L and the held name one lot short of its target (2026-10)

Research only; not investment advice. Part of #437.

## Question

ADR 0026's D7-L rule (point 2, and its 2026-10-17 amendment) skips a pick
whose one share, at the previous session's raw close, costs more than its
target value (weight x momentum scale x net liquidation). The rule does not
look at what the account holds. A stock that is held, is still ranked into
the book, but whose target has fallen below one share's price is therefore
skipped, gets no target, and is sold in full at the rebalance, where the
25% rebalance band would otherwise have left it alone.

The owner chose option 2 on 2026-10-18, on condition that it is measured
once first (AGENTS.md rule 6): **a held name whose target is within one lot
of affordability keeps its current holding (no buy, no sell) for that
rebalance.** This note measures option 2 against the rule as written. It
does not change the rule; the implementation decision follows the result.

## Plan (written 2026-10-18, before any run)

### Reading of "within one lot of affordability"

A lot is one share (D7-L, "whole shares"). A pick is unaffordable when its
one-share price p exceeds its target value t. The shortfall, p - t, is less
than one lot (p) whenever t > 0, and D7-L only compares picks with t > 0. So
every held pick that D7-L would skip qualifies. Option 2 as measured is:

> When D7-L would skip a pick that the account holds (one share or more),
> the pick is not skipped. It keeps its place in the book and its sector
> slot, its weight stays in the book's weights, and no order is placed in it
> at that rebalance (no buy, no sell, no band resize). A pick that is not
> held is skipped exactly as D7-L is written.

The substitution stays a fixed point: each pass weights the picks, skips
every unaffordable pick that is not held, and keeps every unaffordable pick
that is held; passes end when a pass skips nothing new. The all-unaffordable
withhold (amendment case b) is unchanged: if no pick is left and at least
one was skipped, the decision is withheld and the book kept.

### Instrument, and why

The measurement must compare the Go rule as written with the Go rule plus
the held-name exception, at $30,000 under Interactive Brokers' fees.

- **The QuantConnect template does not express D7-L.**
  `momentum/fresh_universe.py` (the FU_COMBO_30K and OOS_FU_COMBO_30K runs)
  never skips an unaffordable pick. It sizes with `SetHoldings`, which rounds
  to whole shares of the *split- and dividend-adjusted* price (the
  universe's default `DataNormalizationMode`, Adjusted, is never changed),
  so a target below one adjusted share simply places no order and its
  weight stays in cash. Neither the rule's test (one share at the raw close)
  nor its consequence (substitution, and the full sale of a held name) is
  in the recorded runs.
- **The Go module alone cannot give a path.** The recorded inputs (the
  parity exports, 61 months, and the golden months, 9) are single months'
  snapshots. Replaying them through `SelectBookWithMinimumLot` would show
  which picks change in those months, but not turnover, fees, the account
  value the next month is valued against, or CAGR and drawdown; and the
  exports hold adjusted closes, not the raw closes D7-L tests.
- **Chosen: a QuantConnect template that adds D7-L, ported from the Go
  rule, with and without the exception.** `momentum/minimum_lot.py` is
  `fresh_universe.py` with additions only (a test checks this; with
  `MIN_LOT = None` it trades as `fresh_universe.py` does). It records each
  month's raw closes from the coarse selection that chooses the month (the
  last session before it, as `FRESH_SELECTION` does), which are the
  previous session's raw closes at the 8:00 rebalance, and values each pick
  at weight x 1 x `TotalPortfolioValue` (FU_COMBO has no bond sleeve, so the
  momentum scale is 1). `minimum_lot_book` repeats `defensive_book` over the
  names not yet skipped until no pick is unaffordable, as
  `selectBook` does in `internal/targetweight/momentum/book.go`. Its tests
  transcribe the Go tests' cases (`TestMinimumLotSkipsAPickWhoseOneShareExceedsItsTarget`,
  `TestMinimumLotFreesTheSectorSlot`,
  `TestMinimumLotIteratesUntilEveryPickIsAffordable`,
  `TestMinimumLotWithNoAffordablePickPicksNothing`) and must give the same
  books.

Known limits of the instrument, the same in both variants of each pair:

- Selection is `defensive_book`'s (the backtest's): a name with no
  Morningstar sector is uncapped and a zero volatility weighs 1. The live
  rule now picks no-sector names the same way (RulesVersion 1.13.0, PR
  #472); the zero-volatility difference is unchanged and did not occur in
  the parity months.
- Orders are still sized and filled in adjusted shares by `SetHoldings`, and
  LEAN's `InteractiveBrokersFeeModel` charges per adjusted share. Only the
  D7-L test uses raw closes. The share counts, so the per-share part of the
  fee, are therefore not the live ones; most of these orders pay IB's $1
  minimum either way.
- A kept name holds its actual holding, which is worth more than its
  target (it is at least one share above a target below one share), so the
  exception runs slightly more invested in that name than the book says.

### Variants

Each is FU_COMBO_30K or OOS_FU_COMBO_30K (`CASH = 30000`, top 25, sector
cap 3, inverse volatility, half risk-off, fresh selection, IB brokerage
model) on `momentum/minimum_lot.py`, differing only in `MIN_LOT`:

| Variant | Window | MIN_LOT | Role |
| --- | --- | --- | --- |
| D7L_NONE_30K | 1999-01 to 2015-12 | `None` | control: no D7-L; must reproduce FU_COMBO_30K's trading on the day's data |
| D7L_LIT_30K | 1999-01 to 2015-12 | `"literal"` | D7-L as written |
| D7L_HELD_30K | 1999-01 to 2015-12 | `"held"` | D7-L plus the held-name exception |
| OOS_D7L_LIT_30K | 2016-01 to 2026-06-30 | `"literal"` | held-out, run once |
| OOS_D7L_HELD_30K | 2016-01 to 2026-06-30 | `"held"` | held-out, run once |

All five run on the same day, on one QuantConnect project, with the
`Fundamentals` statistic recorded as the data-state fingerprint
(`docs/research/r4-rerun-discrepancy-2026-10.md`). The in-sample pair runs
first. **The held-out pair is run once, as a paired same-day comparison of
the exception with the unchanged rule, and is flagged as a held-out
reading:** it is not used to tune anything, and no variant of the exception
is run on it afterwards.

### Metrics

Per run: CAGR and maximum drawdown over the measured window (the template's
`OVERALL` statistic, and its SPY line), orders (`Transactions.OrdersCount`),
total fees (the backtest's `Total Fees`), and the `MinLot` statistic:

- `skipped`: name-months D7-L skipped as unaffordable;
- `heldSold`: of those, names the account held, so sold in full (the
  marginal-name full sells the exception is meant to avoid);
- `kept`: name-months the exception kept (only in `"held"` runs): the full
  sells avoided;
- `withheld`: months withheld as all-unaffordable;
- `noPrice`: picks with no raw close recorded (treated as affordable;
  expected 0).

### Verdict rule (fixed before the runs)

Compare D7L_HELD_30K with D7L_LIT_30K, then the held-out pair:

- **Harm** on a window: the exception's CAGR is more than 0.10 pp/yr below
  the literal rule's, or its maximum drawdown more than 1.0 pp deeper, or
  its fees higher.
- **Adopt** if the exception fires (`kept` > 0) and shows no harm in the
  in-sample pair and no harm in the held-out pair.
- **Do not adopt** (report to the owner before implementing) if either pair
  shows harm.
- **Inconclusive** if the exception never fires in either window (no
  effect at $30k to measure), or a run fails its checks (the control does
  not reproduce FU_COMBO_30K's order count on the day's data, `noPrice` is
  not 0, or the rebalance count differs within a pair).

The 0.10 pp and 1.0 pp margins treat smaller differences as path noise: a
one-name change in one month moves every later month's sizes.

## Runs

(To be filled in after the runs.)

## Result

(To be filled in after the runs.)
