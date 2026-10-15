# Universe parity: IWB's holdings against the backtest's universe (2026-10-15)

Research only; not investment advice.

## Question

Live trading cannot use QuantConnect's universe (the 500 largest US-listed
common stocks by dollar volume with fundamental data). The plan proposed
taking the holdings of iShares IWB (Russell 1000), ranking them by dollar
volume and keeping 500 (`docs/live-trading/plan.md`, section 4). Does that
pick the same stocks as the backtest, month by month?

## Method

- Template `research/edge-search/momentum/universe_parity.py` and
  comparison `research/edge-search/momentum/parity_compare.py` (PR #299).
- Three runs on the combination's held-out settings (`OOS_F_COMBO`), 2015
  warm-up, measured from January 2016 to September 2026, each run once:
  - `OOS_P_COARSE`: the backtest's own universe;
  - `OOS_P_IWB7`: IWB's holdings at least 7 days old (QuantConnect's US ETF
    Constituents data may lag the sponsor by up to a week);
  - `OOS_P_IWB1`: the same at 1 day old, the sensitivity check.
- The comparison rebuilds each run's holdings after each month's fills from
  its full order list (not committed: derived from market data) and counts
  the backtest's stocks also held by the IWB run.

| Run | Backtest ID | Orders |
| --- | --- | --- |
| OOS_P_COARSE | 33a5889249957d233913da47d26ab5be | 3,617 |
| OOS_P_IWB7 | b897d7e8b111bf4bd56aa96bc4cf36b1 | 3,272 |
| OOS_P_IWB1 | 03bb8089fe01a8ad89c6b0b303684d64 | 3,277 |

## Results

| 2016–Sep 2026 | Return per year | Worst drop (daily) |
| --- | --- | --- |
| SPY | 15.07% | 33.7% |
| Backtest's universe | 20.10% | 31.0% |
| IWB, 7-day lag | 18.74% | 26.6% |
| IWB, 1-day lag | 19.49% | 26.5% |

| Overlap with the backtest's stocks | IWB 7-day | IWB 1-day |
| --- | --- | --- |
| Months compared | 129 | 129 |
| Mean share of the backtest's stocks also held | 62.3% | 62.8% |
| Worst month | 2021-03, 20% | 2021-03, 20% |
| Months where risk-off differed | 0 | 0 |

By year, 7-day lag: 2016 75%, 2017 64%, 2018 72%, 2019 68%, 2020 59%,
2021 39%, 2022 77%, 2023 76%, 2024 61%, 2025 49%, 2026 37%.

**The lag barely matters** (62.3% against 62.8%). **The fund does.** The
stocks the backtest held most often that the IWB run could not, by months
held, include CBAY 32, RIOT 22, MARA 20, B (Barrick) 19, VRT 18, SHOP 17,
KGC 16, SMCI 16, OKLO 16, HIMS 14, PLUG 13, CCJ 12, NBIS 12, MELI 10. They
fall into two groups:
- **Companies domiciled outside the US** with US listings (Shopify,
  MercadoLibre, Barrick, Kinross, Cameco, Agnico Eagle, Teck, Nebius, CRH).
  Russell's US indexes leave them out; QuantConnect's universe does not.
- **Small companies whose trading volume ranks them in the top 500** but
  which sit in the Russell 2000, or joined the Russell 1000 only at a later
  reconstitution: bitcoin miners (RIOT, MARA, CLSK, IREN, HUT), OKLO, HIMS,
  PLUG, and meme stocks in 2021 (AMC, BBBY).
Momentum picks exactly these in strong trends, so the overlap is lowest in
2021, 2025 and 2026.

## What it means

- **IWB is not good enough as a stand-in.** The plan's Phase 1 gate is
  "overlap good enough, or a data source chosen". At 62%, the live
  strategy would be a different strategy from the one tested, with its own
  return (here 1.4 points a year lower at a 7-day lag, with a shallower
  drop) and no tuning-year evidence: QuantConnect's IWB data starts in 2009,
  monthly, so 1999–2015 cannot be rerun on it.
- **Returns are not the test.** These runs are on the held-out years, so
  choosing a universe because its return is higher would be tuning on them.
  The test is fidelity: how closely the live universe reproduces the
  backtest's.
- **A fund with more small companies would not fix the first group.**
  Russell 3000 (IWV) would add the Russell 2000 names but still leave out
  companies domiciled outside the US.

## Options for the live universe (for the owner)

1. **Rank every US-listed common stock by dollar volume**, as the backtest
   does, from a market-wide daily data source: one request a day returns
   every listed stock's volume and close (for example a "grouped daily bars"
   endpoint from a market-data vendor; some have free tiers, which need an
   account the owner opens). The listing filters (common stock, price at
   least $5, fundamental data) are applied from free exchange listing files.
   This is the closest to the backtest. A parity check would measure it
   against `OOS_P_COARSE` before use.
2. **Accept IWB and treat the result as a new strategy**, with only the
   2016–2026 runs above as evidence. Not recommended.
3. **IWV (Russell 3000) holdings.** Still free and daily, closer on small
   companies, still missing the non-US group. Would need its own parity run.

Nothing here commits to any of them; the choice, and any account a data
source needs, are the owner's.

## Not covered

- The GICS-against-Morningstar sector check (plan section 4), still to do.
- An in-sample check: none was possible with IWB data (from 2009 only).
