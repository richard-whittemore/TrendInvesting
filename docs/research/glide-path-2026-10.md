# Glide path as one account (2026-10-16)

Research only; not investment advice.

## Question

The IRAs run the combination with a bond sleeve whose share *b* follows the
owner's age (decision D2, `docs/live-trading/plan.md`, section 2). The
earlier study (`docs/research/retirement-bonds-2026-10.md`) measured only
fixed mixes, blended outside QuantConnect. The plan therefore requires the
schedule to be backtested as a path, in one account with the 25% band, before
any IRA trades, and declares the test in advance: a path run **fails** if its
return per year is below both of its bracketing fixed mixes' or its worst
drop is deeper than both.

## Method

- Template `research/edge-search/momentum/glide_path.py` (PR #304): the
  combination (`F_COMBO` settings) with a half-IEF, half-SHY bond sleeve in
  one QuantConnect account. GLIDE `"path"` sets *b* each month from the
  schedule; GLIDE `"fixed"` holds it constant (the comparison mixes).
- Variants `G_IS_*` and `G_OOS_*` in `research/edge-search/build_variants.py`,
  declared in the plan before any run and each run once:
  - tuning window: warm-up from 2001-08, measured 2002-08 to 2015-12 (the bond
    funds start in July 2002);
  - held-out window: measured 2016-01 to 2026-07-18 (set to run to 2026-09;
    see the correction below).
- In a path run the owner's age on the first measured day is the run's
  starting age (38, 53 or 65), and age advances with the calendar from there.
- Return per year and worst drop are measured on daily equity over the
  measured window only (the run's `OVERALL` statistic), not over the
  warm-up. QuantConnect's own headline statistics include the warm-up and
  are not used.
- QuantConnect project 37096083.

| Run | Backtest ID |
| --- | --- |
| G_IS_P38 | 9de544e8109e6babbb9e0a64ee838863 |
| G_IS_P53 | 4d59f6edebc913b36e18b66441384e97 |
| G_IS_P65 | b63a49beacb85b38c1225e337d5994b1 |
| G_IS_F00 | 054979d73973006d98d767e08e283279 |
| G_IS_F30 | 1c867ed7c38b567115709e9e9eeaf465 |
| G_IS_F50 | 80b29526cb04f53f07c27cfd79ab7efc |
| G_IS_F60 | 424babffc064b744fa809735dfa7a2bc |
| G_IS_F70 | 60156031ea9925a5bf6d59ed49e7fc70 |
| G_OOS_P38 | a2cece4559cde3ed5253f63376bccca5 |
| G_OOS_P53 | dd8e3cee8e6df87b088639cb74c878c7 |
| G_OOS_P65 | 882d9c6ad1bd49fdeab44992c7c6037c |
| G_OOS_F00 | 7d9b8411482e015535beed72b66a27a3 |
| G_OOS_F30 | e1f3f4f00c39bf91944eac756e724048 |
| G_OOS_F50 | 743eafb0432ab3e7fa2f9ec198d87d3d |
| G_OOS_F70 | d4f277e8171bd8b583abf53e04c04fae |

## Results

**Tuning window, 2002-08 to 2015-12** (SPY: 8.55% a year, worst drop 55.2%)

| Run | *b* over the run (mean) | Return per year | Worst drop |
| --- | --- | --- | --- |
| Fixed 0% | 0% | 13.64% | 29.2% |
| **Path from 38** | 0% → 27.4% (13.7%) | **12.32%** | **24.1%** |
| Fixed 30% | 30% | 10.96% | 18.0% |
| **Path from 53** | 30.8% → 53.8% (41.6%) | **9.62%** | **16.3%** |
| Fixed 50% | 50% | 8.85% | 12.9% |
| Fixed 60% | 60% | 7.80% | 10.2% |
| **Path from 65** | 50% → 70% (64.7%) | **7.35%** | **10.9%** |
| Fixed 70% | 70% | 6.65% | 7.9% |

**Held-out window, 2016-01 to 2026-07-18** (SPY: 15.07% a year, worst drop 33.7%)

| Run | *b* over the run (mean) | Return per year | Worst drop |
| --- | --- | --- | --- |
| Fixed 0% | 0% | 20.10% | 31.0% |
| **Path from 38** | 0% → 21.6% (10.8%) | **18.28%** | **28.5%** |
| Fixed 30% | 30% | 14.71% | 25.2% |
| **Path from 53** | 30.8% → 47.6% (39.2%) | **13.03%** | **23.0%** |
| Fixed 50% | 50% | 11.05% | 21.1% |
| **Path from 65** | 50% → 70% (63.3%) | **8.36%** | **17.6%** |
| Fixed 70% | 70% | 7.22% | 16.9% |

**The declared test**

| Window | Path from | Lower mix | Upper mix | Return between? | Worst drop between? | Result |
| --- | --- | --- | --- | --- | --- | --- |
| Tuning | 38 | 0% | 30% | yes | yes | pass |
| Tuning | 53 | 30% | 60% | yes | yes | pass |
| Tuning | 65 | 50% | 70% | yes | yes | pass |
| Held out | 38 | 0% | 30% | yes | yes | pass |
| Held out | 53 | 30% | 50% | yes | yes | pass |
| Held out | 65 | 50% | 70% | yes | yes | pass |

All six runs pass: in every case both the return and the worst drop lie
between the two mixes, never below or deeper than both.

## What it means

- **The path behaves like the mixes it passes through.** Running the
  schedule in one account, with the band and monthly changes in *b*, adds
  no penalty the test can detect. The plan's gate for the glide path (section
  2, and Phase 2 in section 6) is met.
- **The bond sleeve trades return for a smaller drop, as in the earlier
  study.** From 38, over the held-out years, the path gave up about 1.8 points
  a year against no bonds and its worst drop was 2.5 points shallower.
  In the tuning years the drop was 5.1 points shallower for 1.3 points a
  year. That trade is the schedule's purpose, not a finding about whether it
  is worth it, which is the owner's call.
- **The held-out years favoured stocks.** With SPY at 15% a year, bonds
  cost more return there than in the tuning years and saved less drawdown:
  from 0% to 30% bonds, 5.4 points a year for 5.8 points of drop held out,
  against 2.7 points a year for 11.3 points in the tuning years.

## Notes

- **Correction (2026-10-16):** QuantConnect's US equity data ends on 2026-07-18, so these runs, set to end in September 2026, ended on 2026-07-17 or 2026-07-18 (`docs/research/universe-massive-2026-10.md`). The figures are unchanged; only the label was wrong.

- **Ending shares differ slightly from the plan's table.** The plan's
  table takes each window's full length (14 years for the tuning window);
  the runs count age from the first measured day (August 2002 for the
  tuning window) and report the share at the last monthly rebalance. The
  largest difference is 1.9 points, for the tuning path from 53 (55.7% in
  the plan, 53.8% in the run). None changes which mixes bracket the path.
- **The 60% and 70% mixes are now measured** in one account. The earlier
  study stopped at 50%.
- **The held-out window is not blind for the combination**
  (`docs/live-trading/plan.md`, section 2). The schedule and the test were
  fixed before these runs, so the comparison between path and mixes is
  clean, but the absolute returns carry that caveat.
