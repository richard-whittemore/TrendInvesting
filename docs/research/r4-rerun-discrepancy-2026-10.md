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
| `F_COMBO_R4` (recorded) | 24bdb0a9… | 1999–2015 | 11.25% | 34.0% | n/r | n/r |
| `F_COMBO` rerun | eee58c08c960a253047aab3b075c75f0 | 1999–2015 | 13.09% | 46.05% | 5,187 | $161,655 |
| `OOS_F_COMBO_R4` (recorded) | dd45f8ec… | 2016 to 2026-06-30 | 20.85% | 31.0% | n/r | n/r |
| `OOS_F_COMBO` rerun | a752a0ae4155142464abb218b826abc9 | 2016 to 2026-06-30 | 20.06% | 40.47% | 3,589 | $34,225 |

n/r: not recorded in the repository.

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

**Most likely cause: something changed on QuantConnect's side, in its data
or its engine, between the morning and the afternoon of 2026-10-16.** Of
the four candidates:
- (a) the template did not change;
- (b) the drawdown is measured the same way;
- (c) the script is not a different one.

Each is ruled out by byte-level or arithmetic evidence below. A data change
is directly visible in the runs' own fundamental-data counters. The engine
version, which would separate a data change from an engine change, has
not yet been read (see "What would settle it").

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
| R4 script, 2026-10-03 (`tax_r2.json`, chart) | 1999–2002 | 4.94% / 33.68% |
| **`F_COMBO` rerun eee58c08 (10-16 afternoon)** | 1999–2002 | **11.74% / 46.05%** |
| `IS_GOLD_2013_05` (10-16 morning) | 2009 to 2013-05 | 15.99% / 25.48% |
| R4 script, 2026-10-03 (`tax_r2.json`) | 2009–2015 | 15.99% / 23.63% |
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
| `OOS_P_GICS` (10-15) | 2015-01 to 2026-07 (139) | 69,500 | 128,758 |
| `G_OOS_F00` (10-15) | 2015-01 to 2026-07 (139) | 69,500 | 128,758 |
| `OOS_F_COMBO` rerun (10-16 afternoon) | 2015-01 to 2026-06 (138) | 69,000 | 126,718 |
| `OOS_FU_COMBO` rerun (10-16 afternoon) | 2015-01 to 2026-06 (138) | 69,000 | 126,722 |

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

### Not yet settled: data or engine

Both remain possible:
- a reprocessing or revision of QuantConnect's US equity
  universe/fundamental data, published during 2026-10-16;
- a LEAN engine update (for example, in how Python reads fundamental
  attributes, which this template's `_get` probes by name).

The morning golden runs log "LEAN Engine v2.5.0.0.18184". The engine
version of the afternoon reruns has not been read.

## What would settle it (fields to fetch, read-only, project 37096083)

1. **Engine version.** From the backtest log of eee58c08 and a752a0ae,
   the first line, "Launching analysis for … with LEAN Engine v…".
   Compare with 18184 (golden runs, 10-16 morning). Also read each
   backtest's `created` timestamp.
2. **R4's own counters.** From the runtime statistics of `F_COMBO_R4`
   24bdb0a9… and `OOS_F_COMBO_R4` dd45f8ec… (full IDs from the project's
   backtest list), read `Fundamentals`, `Mode`, `Borrow`, `OVERALL`,
   `1999-02`, `2003-07`, `2008-09`, `2009-15` and `2016-`. The same
   window and the same bytes as the reruns allow a direct test:
   - hits 194,862 (in-sample) and 126,718 (held out) mean the data read
     was the same;
   - different counts confirm a data change.
3. **Where the paths part.** The order list of eee58c08 (and a752a0ae),
   compared month by month with the 2026-10-03 R4-script fills already
   held locally (`data/qc_exports/tax_r2.json`). Use
   `momentum/parity_compare.py`'s holdings rebuild. The first month with
   different names, and whether those names differ in universe membership,
   sector code or price, says what changed.

## What the plan should cite

- **Keep citing the recorded R4 figures** (1999–2015 11.25% / 34.0%;
  2016 to mid-2026 20.85% / 31.0%) as the recorded result. They are
  consistent with at least six later same-logic runs up to the morning of
  2026-10-16. They are also what the golden cases (ADR 0026) and the
  retirement/blend studies were built on.
- **Add the 2026-10-16 reruns as an unexplained second data state.** Until
  item 1–3 above or the rerun below settles it, quote the worse drawdowns
  (46.05% and 40.47%) as the risk planning figure. This issue carries the
  capital-safety label, and a later data state that deepens the drawdown
  may be the one live trading resembles.
- **PR #402's note is wrong as written.** "Differ a little … because
  QuantConnect's data is revised over time" understates a 9–12 point
  drawdown change. The data-change explanation is now supported by the
  fundamentals counter, but not yet proven.
- **The D8 comparison itself is not invalidated.** Its four runs ran
  together on the same data state, so FU against F is paired. The absolute
  levels in that table belong to the new data state, not to R4.

## Reruns needed

- **Once each, `F_COMBO` and `OOS_F_COMBO`, the exact bytes above (md5
  `5777e081…` and `9c3c7b9d…`), now.** Record the engine version and the
  `Fundamentals` counter. Free research runs.
  - If they give 46% / 40% again, the new state persists. Then #398
    (parity) and #399 ($30k commission) must compare against a baseline
    run on that same state, not against R4. The plan should record both
    states.
  - If they give 34% / 31%, the afternoon of 2026-10-16 was a transient
    state. In that case the four D8 runs should be repeated once, as a
    pair, before #402's figures are used.
- No other backtest needs rerunning for this question.
