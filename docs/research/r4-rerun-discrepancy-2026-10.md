# Why the 2026-10-16 F_COMBO reruns differ from F_COMBO_R4 (#403)

Research only; not investment advice. No backtest was run for this note,
and no recorded result is changed.

## Question

The paired D8 timing reruns of 2026-10-16 (PR #402) report results for the
combination that differ from the recorded `F_COMBO_R4` / `OOS_F_COMBO_R4`
figures. Is the cause the template, the drawdown measure, a different
script, or QuantConnect?

| Run | Backtest ID | Window | CAGR | Worst drawdown | Orders | Fees |
| --- | --- | --- | --- | --- | --- | --- |
| `F_COMBO_R4` (recorded) | 24bdb0a9ff4d4fe089644db615c58804 | 1999–2015 | 11.25% | 34.03% | 5,209 | n/r |
| `F_COMBO` rerun | eee58c08c960a253047aab3b075c75f0 | 1999–2015 | 13.09% | 46.05% | 5,187 | $161,655 |
| `OOS_F_COMBO_R4` (recorded) | dd45f8ecd9385e34cd94bb39658634ae | 2016 to 2026-06-30 | 20.85% | 31.01% | 3,585 | n/r |
| `OOS_F_COMBO` rerun | a752a0ae4155142464abb218b826abc9 | 2016 to 2026-06-30 | 20.06% | 40.47% | 3,589 | $34,225 |

n/r: not read. R4 figures are the runs' own runtime statistics, read back
from QuantConnect on 2026-10-17 (created 2026-10-04 03:15 and 01:43). The
reruns were created 2026-10-16 20:56 and 21:45.

**Where the R4 figures are recorded.** `docs/research/boost-followup-2026-09.md`,
"The unpriced-buy fix", headline table, Combination row (11.25% / 34.0% and
20.85% / 31.0%). The commit that declared them is f6ddadd ("#276: fix
unpriced buys in the momentum backtests; tax study (#277)", 2026-10-04). It
added `momentum/defensive_v2.py` and the `F_*` variants. ac758d8 (#282)
repeats the same figures as the execution test's baseline. The run IDs
(prefixes only in the repository) are in `docs/live-trading/plan.md`
(Recorded runs), ADR 0026, `docs/research/retirement-bonds-2026-10.md` and
`research/edge-search/portfolio/bonds.json` / `strategies_together.json`.
`docs/research/edge-search-2026-09.md` does not mention R4.

## Finding

**Cause: QuantConnect's fundamental data changed between 2026-10-04 (the R4
runs) and 2026-10-16 (the reruns). The code and the requests did not
change.** This is the conclusion the evidence supports and the one the plan
adopts; it rests on counter output and exclusion of the other candidates, not
on a direct comparison of the underlying data (see "Still open").
- **Same requests.** The two runs read the same number of stock-months:
  107,500 in-sample and 69,000 held out, in both R4 and the reruns. Equal
  read counts show the same number of reads, not that the same stock-months
  were requested.
- **Different data.** The reruns found about 1,190 fewer fundamental
  fields in each window (below). This is changed counter output; by itself
  it does not prove which values changed.
- **Different path.** With the same bytes and the same measure, the
  1999–2002 span alone moves from 4.94% / 34.03% (R4) to 11.74% / 46.05%.

Candidates (a)–(c) are ruled out by byte-level or arithmetic evidence. The
engine version could not be read: the log endpoint returned no first line
for these runs. So an engine change alongside the data change is not
excluded, but no engine change is needed to explain the counters.

### (a) Template drift: ruled out

- `git log f6ddadd..origin/main -- research/edge-search/momentum/defensive_v2.py`
  is empty. The template has not changed since the R4 commit.
- The `F_COMBO` / `OOS_F_COMBO` entries and `_OOS` in `build_variants.py`
  are the same at f6ddadd and on main (2ba5b76). That covers the window
  (START 2015-01-01, END 2026-06-30, MEASURE_FROM 2016-01-01) and the
  settings: mom_trend, TOP_N 25, RISK_OFF_FRACTION 0.5, SECTOR_CAP 3,
  INV_VOL. The template's defaults are unchanged too: MEASURE_FROM
  1999-01-01, SLIPPAGE 0, REBALANCE_BAND 0.25, the pending/retry logic,
  the End statistics and the risk-off logic. Later commits only added
  variants.
- Each builder was run with the same TB3MS file
  (`build_variants.py TB3MS.csv OUT`). The builder at f6ddadd and the one
  on main both give `F_COMBO/main.py` md5 `5777e08167a672f6e80efbd569ef8fd7`
  and `OOS_F_COMBO/main.py` md5 `9c3c7b9d86caff63eb73cff0da8b539c`.
- The scripts actually queued for the 2026-10-16 reruns were kept (the
  owner's local scratch, `fu_queue.json`, written 16:15). They have exactly
  these md5s and are byte-identical to the main build.
- The local upload snapshots from 2026-10-03 21:22 (the last before the
  R4 declaration) carry the same two md5s.

So the R4 runs and the reruns ran the same bytes.

### (b) A different drawdown measure or curve: ruled out

- Both figures are the template's own `OVERALL` runtime statistic: `_stats`
  on the daily `equity_curve` (one point per SPY bar), from MEASURE_FROM to
  END_DATE. The rerun's own Ratio confirms it: 0.1309 / 0.4605 = 0.2843.
- The SPY figures, computed by the same function on the same span, are
  identical in both: 1999–2015 4.91% / 55.17%; 2016+ 15.19% / 33.72%. The
  span and the SPY data are the same.
- The R4 runs' saved equity charts (`eq_F_COMBO_R4.json`,
  `eq_OOS_F_COMBO_R4.json`, sampled about every 2.3 days) recompute to
  11.34% / 33.80% (peak 2000-03-09, trough 2002-07-23) and 20.36% / 30.25%
  (peak 2021-11-07, trough 2023-10-28). These match the recorded figures to
  within the sampling. The tax study's export of the same script's
  2026-10-03 runs (`data/qc_exports/tax_r2.json`, a denser curve) gives
  11.25% / 33.68% and 20.52% / 31.01%.

### (c) A different template or window: ruled out

- R4 is `defensive_v2.py` (the doc's "fixed template"; `F_*` in the
  builder), not `defensive.py`. The `D_COMBO` / `OOS_D_COMBO` runs on
  `defensive.py` are the "as originally run" column: 10.46% / 31.1% and
  18.80% / 27.7%.
- The windows match: the R4 curves end 2015-12-29 and 2026-06-28. The
  reruns report 203 and 138 rebalances, 59 and 25 risk-off months. The
  reruns' window label "2016-01 to 2026-07" in PR #402 is wrong: the script
  ends on 2026-06-30.

### (d) QuantConnect data or engine: supported

**1. Same-logic runs shortly before agree with R4.** Several runs on
2026-10-15 and on the morning of 2026-10-16 used templates that are
`defensive_v2.py` with additions only (a test keeps every code line):
`golden_log.py`, `universe_parity.py` (coarse source) and `glide_path.py`
(bond share 0). All of them reproduce the R4 drawdowns. The afternoon
reruns do not:

| Run (date) | Span | CAGR / worst drawdown |
| --- | --- | --- |
| `IS_GOLD_2008_11` 65c3df3b… (10-16, before 04:40) | 1999–2002 | 4.82% / 34.13% |
| `IS_GOLD_2009_06` 2fb0d2e1… (10-16, before 04:40) | 1999–2002 | 4.88% / 34.24% |
| `IS_GOLD_2013_05` e06aef39… (10-16, before 04:40) | 1999–2002 | 4.88% / 34.24% |
| **`F_COMBO_R4` 24bdb0a9 (10-04), own statistic** | 1999–2002 | **4.94% / 34.03%** |
| R4 script, 2026-10-03 (`tax_r2.json`, chart) | 1999–2002 | 4.94% / 33.68% |
| **`F_COMBO` rerun eee58c08 (10-16 afternoon)** | 1999–2002 | **11.74% / 46.05%** |
| `IS_GOLD_2013_05` (10-16 morning) | 2009 to 2013-05 | 15.99% / 25.48% |
| R4 script, 2026-10-03 (`tax_r2.json`) | 2009–2015 | 15.99% / 23.63% |
| `F_COMBO_R4` 24bdb0a9, own statistic | 2009–2015 | 16.04% / 25.48% |
| `F_COMBO` rerun eee58c08 | 2009–2015 | 13.58% / 25.81% |
| `OOS_P_COARSE` 33a58892… (10-15), R4's window from its chart | 2016 to 2026-06-30 | 20.76% / 30.97% |
| `G_OOS_F00` 7d9b8411… (10-15) | 2016 to 2026-07-17 | 20.10% / 30.97% |
| `OOS_P_GICS` b0f7eb65… (10-15, 20 sector labels changed) | 2016 to 2026-07-17 | 20.08% / 28.63% |
| **`OOS_F_COMBO` rerun a752a0ae (10-16 afternoon)** | 2016 to 2026-06-30 | **20.06% / 40.47%** |

The 1999–2015 worst drawdown is set in 2000–2002 in every run: R4's peak
is 2000-03, its trough 2002-07. The 1999–2002 rows therefore compare the
drawdown directly. The rerun made far more in 1999–2000 and lost far more
afterwards. That is a different set of holdings, not a different measure.
The morning golden runs' holdings also match the 2026-10-03 R4-script
fills name for name, once tickers are mapped to their QuantConnect
security IDs (for example FB→META, FD→M, AMB→PLD, DEAC→DKNG, GSAH→VRT).
This holds in 2008-11, 2009-06, 2013-05, 2016-02, 2020-04, 2022-06 and
2024-03.

**2. The reruns read different fundamental data.** Each run reports
`Fundamentals: reads=… hits=…`. A read is one stock in a month's fine
selection, so 500 a month. A read scores at most 2 hits: one for gross
profit and total assets together, one for earnings yield.

| Run | Selection months | reads | hits |
| --- | --- | --- | --- |
| **`F_COMBO_R4` 24bdb0a9 (10-04)** | 1998-01 to 2015-12 (215) | 107,500 | **196,051** |
| **`F_COMBO` rerun eee58c08 (10-16)** | 1998-01 to 2015-12 (215) | 107,500 | **194,862** |
| **`OOS_F_COMBO_R4` dd45f8ec (10-04)** | 2015-01 to 2026-06 (138) | 69,000 | **127,905** |
| `OOS_P_GICS` (10-15) | 2015-01 to 2026-07 (139) | 69,500 | 128,758 |
| `G_OOS_F00` (10-15) | 2015-01 to 2026-07 (139) | 69,500 | 128,758 |
| `OOS_F_COMBO` rerun (10-16 afternoon) | 2015-01 to 2026-06 (138) | 69,000 | 126,718 |
| `OOS_FU_COMBO` rerun (10-16 afternoon) | 2015-01 to 2026-06 (138) | 69,000 | 126,722 |

- **Same script, same window, same reads, fewer hits:** 196,051 − 194,862
  = 1,189 fewer in-sample; 127,905 − 126,718 = 1,187 fewer held out. The
  data the same requests returned changed between 2026-10-04 and
  2026-10-16.
- The 10-15 runs sit with R4. 128,758 − 127,905 = 853 hits for their
  extra month (2026-07). That is within the 1,000 a month can add, so
  10-15 is consistent with R4's data state.
- The 10-15 runs' extra month (2026-07) can add at most 500 × 2 = 1,000
  hits. So over the 138 months they share, the 10-15 runs found at least
  128,758 − 1,000 = 127,758 hits.
- The afternoon reruns found 126,718: at least 1,040 fewer. Each run's
  universe or fundamentals data in those months differed from what the
  10-15 runs saw.
- Within the earlier batch the counter repeats exactly: `OOS_P_GICS` =
  `G_OOS_F00`, and `OOS_GOLD_2021_03` = its `_B` rerun (69,646). So it is
  a stable fingerprint of the data, not noise.
- The four afternoon runs (`F_COMBO`, `OOS_F_COMBO`, `FU_COMBO`,
  `OOS_FU_COMBO`) all have deep drawdowns (46.05%, 40.47%, 43.48%,
  39.47%). They were queued together from one file at 16:15 on 2026-10-16.

**3. Small run-to-run variation exists, but is far smaller.** Runs that
trade identically still differ slightly:
- `IS_GOLD_2008_11` against `IS_GOLD_2009_06`: 4.82% against 4.88% on
  1999–2002;
- `IS_GOLD_2000_03` against `_B`: 380 against 377 orders;
- `OOS_GOLD_2021_03` against `_B`: 1,966 against 1,967 orders.

Their settings differ only in what is logged. This noise is about 0.1
point a year and 0.1 point of drawdown. It cannot by itself explain
12 points of drawdown, and the three morning in-sample runs agree with
one another and with R4.

**4. Fees follow the larger account.** Fees per order rose from about
$23.7 (2026-10-03 export: 5,209 fills, $123,290) to $31.2 in the rerun
(5,187 orders, $161,655). That ratio (1.31) matches the larger compounded
account, (1.1309 / 1.1125)^17 ≈ 1.32. It is a consequence of the
different path, not a separate cause.

### Side by side: R4 against the rerun (runtime statistics, 2026-10-17)

| Statistic | `F_COMBO_R4` | `F_COMBO` rerun | `OOS_F_COMBO_R4` | `OOS_F_COMBO` rerun |
| --- | --- | --- | --- | --- |
| OVERALL | 11.25% / 34.03% | 13.09% / 46.05% | 20.85% / 31.01% | 20.06% / 40.47% |
| 1999-02 | 4.94% / 34.03% | 11.74% / 46.05% | | |
| 2003-07 | 16.69% / 25.41% | 21.41% / 27.02% | | |
| 2008-09 | −5.08% / 28.23% | −7.22% / 31.66% | | |
| 2009-15 | 16.04% / 25.48% | 13.58% / 25.81% | | |
| Rebalances / risk-off | 203 / 59 | 203 / 59 | 138 / 25 | 138 / 25 |
| Orders | 5,209 | 5,187 | 3,585 | 3,589 |
| Resizes / retried | 582 / 875 | 624 / 894 | 648 / 445 | 691 / 506 |
| Average cash | 13.9% | 13.9% | 3.9% | 3.6% |
| Fundamentals reads / hits | 107,500 / 196,051 | 107,500 / 194,862 | 69,000 / 127,905 | 69,000 / 126,718 |

The trend filter's counts match (59 and 25 risk-off months), but these
counts do not show whether the same months were risk-off. The difference
appears to be in which stocks the momentum part held, which fits a change
in the universe or fundamentals data (membership, the Morningstar sector
codes the cap uses). SPY or IEF price changes are not ruled out without
comparing the monthly decisions and price series.

### Still open

- **Underlying inputs.** Matching `Fundamentals` totals are supporting
  evidence only: `fund_hits` counts readable nonzero fields, not their
  values, names, sectors or prices, and an engine change in how `_get` reads
  attributes could also move the totals. Settling the cause needs a
  comparison of the underlying inputs, not the counts.
- **Engine version.** The log endpoint returned no first line for these
  runs, so it was not read. The morning golden runs log LEAN
  v2.5.0.0.18184.
- **Where the paths part.** The order list of eee58c08, compared month by
  month with the R4-script fills held locally
  (`data/qc_exports/tax_r2.json`, which match R4: 5,209 fills against R4's
  5,209 orders), would name the first differing month. It would also show
  whether the change is universe membership or sector code. Not needed
  for the conclusion.

## What the plan should cite

- **Keep citing the recorded R4 figures, as R4 (QuantConnect data of
  2026-10-04)** (1999–2015 11.25% / 34.0%;
  2016 to mid-2026 20.85% / 31.0%) as the recorded result. They are
  consistent with at least six later same-logic runs up to the morning of
  2026-10-16. They are also what the golden cases (ADR 0026) and the
  retirement/blend studies were built on.
- **Add the 2026-10-16 reruns as the result on QuantConnect's revised
  data.** The rerun pair below confirmed this state persists. Quote its
  drawdowns (46.05% and 40.47%) as the risk planning figure. This issue carries the
  capital-safety label, and a later data state that deepens the drawdown
  may be the one live trading resembles.
- **PR #402's note is wrong as written.** "Differ a little … because
  QuantConnect's data is revised over time" understates a 9–12 point
  drawdown change. A data change is the cause, shown by the fundamentals
  counter, but its size is not little.
- **The D8 comparison itself is not invalidated.** Its four runs ran
  together on the same data state, so FU against F is paired. The absolute
  levels in that table belong to the new data state, not to R4.

## Rerun pair (2026-10-17): the revised data persists

Each was run once, on the identical bytes (md5 `5777e081…` and
`9c3c7b9d…`), in project 37096083:

| Run | Backtest ID | CAGR / worst drawdown | Fundamentals reads / hits | Orders | Fees |
| --- | --- | --- | --- | --- | --- |
| `F_COMBO_RR` | d3d11664ca37d9e61505e19769980ef3 | 13.09% / 46.05% | 107,500 / 194,862 | 5,187 | $161,655.36 |
| `OOS_F_COMBO_RR` | 28914125c2522b35e2c2a3d9098c7f72 | 20.06% / 40.47% | 69,000 / 126,718 | 3,589 | $34,221.86 |

- **They match the 2026-10-16 reruns statistic for statistic.** The hits
  are identical, as are the risk-off months (59 / 25) and the order
  counts. SPY is unchanged at 4.91% / 55.17%.
- The only differences are a few dollars of fees, and Ratio 0.4956 against
  0.4957 held out.

## Conclusion

- **The revised data state has persisted since at least 2026-10-16.** The
  2026-10-04 state that produced R4 can no longer be reproduced.
- **R4 is not wrong.** It is the result of the same script on
  QuantConnect's earlier data. The difference is in the data, not in the
  strategy's rules or the measurement.

## Consequences

- **Parity and commission baselines.** Parity (#398, #324) and the $30k
  commission runs (#399, #376) use baselines recorded on the current data:
  the 2026-10-16 `F_COMBO` / `OOS_F_COMBO` and `FU_COMBO` / `OOS_FU_COMBO`
  reruns (#402), not R4.
- **The plan cites both states.** `docs/live-trading/plan.md` cites R4,
  labelled as the 2026-10-04 data state, with the current-state figures
  beside it.
- **Risk planning** uses the current state's drawdowns: 46.05% (1999–2015)
  and 40.47% (2016 to 2026-06-30).
- **Studies built on the R4 curves are figures of the 2026-10-04 state**,
  and stay as recorded. They include the blends, the retirement bonds work
  and the golden cases (ADR 0026). Re-derive them on the current data only
  where a decision depends on the drawdown.
- **Pair and fingerprint every run.** QuantConnect's data can change
  without notice, so a figure is reproducible only together with its data
  state. Every comparison runs as a same-day pair, and each run records
  its `Fundamentals` counter (reads / hits) as a coarse fingerprint of the
  data it saw (a difference flags a change; a match does not prove none).
