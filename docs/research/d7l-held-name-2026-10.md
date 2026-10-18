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
  *Correction, 2026-10-18 (wording only; the runs are unchanged):* "slightly"
  was not measured and is not supported. The kept holding is at least one
  share, and its size relative to its target was not recorded: it can exceed
  the target by a multiple bounded only by one share's price, so a kept
  four-figure share against a target of a few hundred dollars could be a
  large fraction of a $30,000 account, outside the sector-cap and weight
  bound the book states, and it lowers the equity that `SetHoldings` sizes
  the other names from. Paper trading should record the kept holding's value
  over net liquidation value for every kept name.

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

All five ran on 2026-10-18 on QuantConnect project 38022068 (Free plan, LEAN
IB brokerage model, $30,000 starting cash), one after another, in the order
below. The in-sample runs came first; the held-out pair ran once, after
them. Each script is its variant rendered by `build_variants.render` from
`momentum/minimum_lot.py`.

| Run | Backtest ID | Created (UTC) | Window | CAGR | MaxDD | Ratio | Orders | Fees | MinLot | Fundamentals |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| D7L_LIT_30K | bd2e8ad19d01fa1d8aba39b6ceb008cb | 10:21 | 1999-01 to 2015-12 | 14.51% | 41.58% | 0.3490 | 5533 | $8,259.50 | skipped=8 heldSold=0 kept=0 withheld=0 noPrice=0 | reads=107500 hits=194735 |
| D7L_HELD_30K | f105daf10f4d1f525a5de8f9af912fae | 10:55 | same | 14.51% | 41.58% | 0.3490 | 5533 | $8,259.50 | skipped=8 heldSold=0 kept=0 withheld=0 noPrice=0 | reads=107500 hits=194735 |
| D7L_NONE_30K | eaf06cdbcd1e47e79d2c44431b441f4a | 11:30 | same | 14.50% | 43.06% | 0.3367 | 5528 | $8,257.05 | (rule off) | reads=107500 hits=194735 |
| OOS_D7L_LIT_30K (held-out, once) | cdbc58fb9dec63a9741cada858555bb7 | 11:58 | 2016-01 to 2026-06-30 | 18.93% | 36.83% | 0.5141 | 3801 | $3,919.63 | skipped=18 heldSold=2 kept=0 withheld=0 noPrice=0 | reads=69000 hits=126722 |
| OOS_D7L_HELD_30K (held-out, once) | e1dcf24f4496f7ff30b3c00acf8934aa | 12:18 | same | 18.95% | 36.94% | 0.5132 | 3789 | $3,908.13 | skipped=15 heldSold=0 kept=3 withheld=0 noPrice=0 | reads=69000 hits=126722 |

SPY over the same windows (the runs' own SPY line): 1999-2015 4.91% /
55.17%; 2016-2026-06 15.19% / 33.72%. Orders are `Transactions.OrdersCount`;
fees are the backtest's `Total Fees`.

### Checks

- **Control.** D7L_NONE_30K reproduces FU_COMBO_30K
  (28188ee1f1c4776a6f68080005c9d974, 2026-10-17) statistic for statistic:
  14.50% / 43.06%, ratio 0.3367, 5,528 orders, $8,257.05 in fees, 572
  resizes, the same Fundamentals counter. So the added code is inert with
  the rule off, and the day's data state is the one the $30k runs used.
- **Raw closes.** `RawCheck` is an instrument check, not a verdict metric.
  It was added to the template after the plan was written (the plan's
  metrics list `MinLot` only), because D7-L's test is only meaningful on
  raw closes and nothing in the plan showed that the coarse `Price` is one;
  it changes no order and no verdict. It counted 89,124 (in-sample) and 50,082
  (held-out) month-choosing rows whose coarse `Price` is more than 1% above
  their `AdjustedPrice`, so `Price` is the raw close D7-L tests, not the
  adjusted one.
- `noPrice` is 0 in every run, nothing was withheld as all-unaffordable,
  and each pair has the same rebalance count (203 in-sample, 138 held-out)
  and risk-off months (59, 25).

## Result

### The comparison the owner asked for (exception minus literal rule)

| Window | CAGR (pp) | MaxDD (pp) | Orders | Fees | Full sells of a held marginal name |
| --- | --- | --- | --- | --- | --- |
| 1999-2015 | 0.00 | 0.00 | 0 | $0.00 | 0 under the literal rule; the exception never fired |
| 2016-2026-06 (held-out) | +0.02 | +0.11 (deeper) | -12 | -$11.50 | 2 under the literal rule; the exception kept 3 name-months |

A positive MaxDD delta is a deeper drawdown.

In the tuning years, D7-L skipped 8 name-months, and none of them was a
stock the account held. The exception therefore never fired, and the two
runs are identical. Over the held-out years D7-L skipped 18 name-months,
and 2 of those were held stocks that the literal rule sold in full. With the
exception, the account kept 3 name-months it held, skipped 15, and placed 12
fewer orders. Once one held stock is kept, the later months' books and
values differ, which is why the counts do not pair one for one (2 sold
against 3 kept). The return, drawdown and fee differences are all well
inside the margins the plan treats as path noise (0.10 pp of CAGR, 1.0 pp
of drawdown), and the fees are lower.

### Verdict: adopt

The rule fixed before the runs gives **adopt**. The exception fired
(kept = 3), and neither pair shows harm: CAGR was not lower, the drawdown
was deeper by 0.11 pp against a 1.0 pp margin, and fees were not higher
($11.50 lower held-out, equal in-sample).

What the verdict rests on, stated plainly:

- **The evidence is small.** At $30,000 the situation option 2 addresses is
  rare. It never arose in 17 tuning years (1999-2015) and arose 2 or 3
  times in 10.5 held-out years. A likely reason, not a finding (the runs did
  not record share prices against targets): few stocks traded above about
  $1,000 a share before 2016, the size of a typical target here, and
  four-figure share prices were more common afterwards. The measurement shows no harm by the plan's margins. The
  order, fee and drawdown differences are within path noise (one kept name
  moves every later month's sizes), so they are not evidence of savings, and
  there is no return benefit shown either; none was expected at this size.
  *Wording corrected 2026-10-18; the original read "does no harm and saves a
  few orders and dollars". The numbers in the tables are unchanged.*
- **The only window where it fired is the held-out one.** That pair ran
  once, on the same day as the unchanged rule, and nothing was tuned on it.
  There is no setting to tune: option 2 has no parameter.
- **What the instrument can and cannot show.** These runs measure the
  rule's choice of names on raw closes, as the Go rule makes it. Sizes and
  fees come from LEAN's adjusted shares (see "Known limits"), so the fee
  difference is indicative, not exact.

### A side observation, not part of the verdict

D7-L itself (literal rule against no rule) moved the in-sample drawdown
from 43.06% to 41.58% and CAGR from 14.50% to 14.51%, at 5 more orders.
Held out, against the recorded OOS_FU_COMBO_30K (94dc8977, 2026-10-17;
18.86% / 37.22%, 3,778 orders, $3,895.99), it moved CAGR to 18.93%,
drawdown to 36.83% and orders to 3,801. That earlier run was recorded on
another day, not as a pair from today, so it gives context only. Either
way, at $30k D7-L substitutes a handful of names and does not change the
strategy's character.

### Next step (not done here)

As the owner decided on 2026-10-18 for a no-harm result: implement option 2
as an ADR 0026 D7-L amendment, with a RulesVersion bump and a golden-case
update. That work is a separate change. This note only measures.
