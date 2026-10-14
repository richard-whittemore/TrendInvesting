# Momentum plus bonds in a retirement account (2026-10)

Issue #289. The IRAs are to run momentum (the combination: half filter,
sector cap 3, inverse volatility), which is mostly stocks: while SPY closes
below its 200-day average its half filter already moves half the book into
IEF (`momentum/momentum.py`, `mom_trend` with `RISK_OFF_FRACTION` 0.5). A
retirement account usually holds more bonds as its owner ages. This study
measures what a separate bond sleeve in the same account costs in return
and saves in drawdown, at the mixes a glide path passes through.

The mix percentages are the sleeve's share, not the account's total bond
share. In risk-off months a 70% combination / 30% IEF mix holds about 65%
bonds (30% + half of 70%), before cash; in risk-on months it holds 30%. A
glide path set on these mixes holds more bonds than its label in
downtrends, and SPY plus bonds, which has no filter, always holds exactly
its label.

## Set-up (declared in #289 before any run)

- Bond fund: **IEF** (7–10 year Treasuries), chosen in advance. TLT (20+
  years) and SHY (1–3 years) at 70/30 only, as a check on that choice.
- Bond curves: `boost/multi.py` in `bh` mode with one sleeve (98% held,
  reset monthly). Each first holds its fund on 2002-07-29.
- Momentum curves are the existing runs, unchanged: F_COMBO_R4 24bdb0a9 and
  OOS_F_COMBO_R4 dd45f8ec; half filter F_HALF_R4 4270416e and OOS_F_HALF_R4
  7b48232b; SPY held MB_SPY_BH_R1 08e1ecfa. Bonds: MB_IEF_BH 9dfd703e,
  MB_TLT_BH 82361132, MB_SHY_BH 6b4d77d3 (QuantConnect project 37096083).
- Mixes are one account reset to its weights at each month-end
  (`portfolio/blend.py`, books marked `"monthly"`). The resets are free in
  this calculation, so the mixes are a slight upper bound.
- Periods as in `strategies_together.json`: tuning 2002-08 to 2015-12, held
  out 2015-12 to 2026-06. Nothing was chosen on either period: the mixes
  and the bond fund were fixed beforehand.
- Drawdowns are measured on month-end values, so they are smaller than daily
  ones.

## Results

| Book | Tuning CAGR | Tuning worst DD | Held-out CAGR | Held-out worst DD |
| --- | --- | --- | --- | --- |
| SPY held | 8.20% | 48.9% | 14.47% | 22.8% |
| Momentum, combination | 13.13% | 28.0% | 20.24% | 24.0% |
| IEF held | 4.95% | 7.1% | 1.18% | 22.5% |
| TLT held | 6.62% | 21.6% | −0.41% | 46.6% |
| SHY held | 2.12% | 1.3% | 1.64% | 5.2% |
| 90% combination + 10% IEF | 12.45% | 24.3% | 18.47% | 23.3% |
| 80% combination + 20% IEF | 11.74% | 20.6% | 16.67% | 22.6% |
| 70% combination + 30% IEF | 11.00% | 16.8% | 14.83% | 21.9% |
| 60% combination + 40% IEF | 10.22% | 12.8% | 12.96% | 21.2% |
| 50% combination + 50% IEF | 9.42% | 10.0% | 11.06% | 20.6% |
| 70% combination + 30% TLT | 11.71% | 17.2% | 14.36% | 28.6% |
| 70% combination + 30% SHY | 10.02% | 18.3% | 14.97% | 17.6% |
| 70% half filter + 30% IEF | 11.36% | 21.7% | 17.11% | 23.6% |
| 90% SPY + 10% IEF | 8.02% | 44.3% | 13.20% | 22.0% |
| 70% SPY + 30% IEF | 7.56% | 33.9% | 10.62% | 20.5% |
| 50% SPY + 50% IEF | 6.97% | 22.0% | 7.98% | 18.9% |

Monthly-return correlation with the combination: IEF −0.19 in the tuning
years, +0.10 held out (SPY with IEF: −0.26, then +0.07).

## Findings

1. **In the tuning years bonds did what they are meant to.** Each 10 points
   moved from momentum to IEF cost about 0.7–0.8 points of return a year and
   cut the worst drop by about 3–4 points; 50/50 fell 10% at worst against
   28% for momentum alone.
2. **In the held-out years they barely helped.** Stocks and bonds fell
   together in 2022, and IEF itself fell 22.5% from its peak. 50/50 still
   fell 20.6% against 24.0%, while giving up about 9 points of return a
   year. A bond share protects against a 2008-style crash, not against
   rising interest rates.
3. **Short-term bonds were the steadier choice held out.** 70/30 with SHY
   had the smallest held-out drop (17.6%) and slightly more return than IEF;
   in the tuning years it returned about 1 point less. Long bonds (TLT) made
   the held-out drop worse than momentum alone. This is a check on the
   predeclared choice, not a reason to swap it on held-out evidence: a mix
   of short and intermediate bonds is the obvious thing to test next.
4. **Momentum plus bonds beat SPY plus bonds at every mix,** in both
   periods, with much smaller drops in the tuning years and drops 1–2 points
   larger held out. 70/30 momentum/IEF returned at least as much as SPY
   alone in both periods (11.00% against 8.20%, 14.83% against 14.47%),
   with about a third of SPY's tuning-years drop.
5. **The trend filter is not a substitute for a bond sleeve.** Momentum
   alone fell 24–28%, even though the filter moves half the book into IEF
   once SPY is below its 200-day average: it acts only after a fall begins.
   A sleeve held all the time lowers the drop that comes before it reacts.
   Because the filter also uses IEF, in 2022 it moved into a fund that was
   itself falling.

## Reproduction

From the repository root:

1. `python3 research/edge-search/build_variants.py TB3MS.csv OUT_DIR`
   writes every variant as `OUT_DIR/<name>/main.py`, from a local FRED
   TB3MS file. The bond holds are `MB_IEF_BH`, `MB_TLT_BH` and `MB_SHY_BH`;
   the momentum and SPY runs are `F_COMBO_R4`, `OOS_F_COMBO_R4`, `F_HALF_R4`,
   `OOS_F_HALF_R4` and `MB_SPY_BH`.
2. Paste each `main.py` into a QuantConnect project and run a backtest.
3. Export each backtest's "Strategy Equity" chart, Equity series, as a JSON
   list of `[unix seconds, equity]` (for example from QuantConnect's
   `backtests/chart/read` API with `name: "Strategy Equity"`, keeping each
   point's time and close), into one EXPORT_DIR. Name each file as
   `bonds.json`'s `curves` names it: `eq_F_COMBO_R4.json`,
   `eq_OOS_F_COMBO_R4.json`, `eq_F_HALF_R4.json`, `eq_OOS_F_HALF_R4.json`,
   `eq_MB_SPY_BH_R1.json`, `eq_MB_IEF_BH.json`, `eq_MB_TLT_BH.json`,
   `eq_MB_SHY_BH.json`. The exports are not committed.
4. `python3 research/edge-search/portfolio/blend.py
   research/edge-search/portfolio/bonds.json EXPORT_DIR` prints both
   periods' tables.
