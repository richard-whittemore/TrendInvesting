# Sector labels: Morningstar against GICS (2026-10-15)

Research only; not investment advice.

## Question

The combination caps the book at 3 stocks per sector, using Morningstar's
sectors (`docs/live-trading/plan.md`, section 2). No free source gives
Morningstar sectors live. iShares' daily holdings files give GICS sectors
for free. How often do the two disagree on the stocks the strategy picks
from, and does it change the picks?

## Part 1: how often the labels differ

- **Morningstar:** `research/edge-search/sectors/snapshot.py`, run once on
  QuantConnect (backtest 03930cbde28764747ebdac294a4a55f1). The backtest's
  own universe of 500, with each stock's Morningstar sector. Count 500, no
  stock without a sector. The run's own clock read 2026-07-18 when it
  selected, although its start was set to 2026-10-01; sector labels change
  rarely, so this does not affect the comparison, but the universe is as of
  that date.
- **GICS:** iShares IWV (Russell 3000) holdings as of 2026-10-14 (not
  committed: derived from market data).
- **Mapping:** GICS sectors onto Morningstar's eleven by name
  (`sectors/compare.py`), a judgment rather than an official concordance.

| | Stocks |
| --- | --- |
| Universe | 500 |
| Labelled by both | 475 |
| Same sector | 455 (95.8%) |
| Different sector | 20 (4.2%) |
| Not in IWV | 25 |

**Where they differ** (Morningstar / GICS):
- Financial Services / Technology, 6: the bitcoin miners and a stablecoin
  issuer (MARA, RIOT, CLSK, HUT, IREN, CRCL);
- Technology / Industrials, 4: payroll and services (ADP, PAYX, UBER, NXT);
- Technology / Financials, 3: payments (FISV, TOST, XYZ);
- Consumer Cyclical / Industrials, 2 (MOD, ROL); and one each of Real
  Estate / Financials (AGNC), Consumer Cyclical / Materials (AMCR),
  Technology / Communication (ASTS), Consumer Cyclical / Consumer Staples
  (CASY), Industrials / Communication (SPCX).

**Not in IWV**, so no free GICS label from it: AEM, APGE, ATAI, AZN, B,
CBRS, CCJ, CLS, CNQ, CRNX, EA, MELI, NBIS, NVMI, NXPI, RY, SDOT, SHOP, STX,
SU, TEL, TSEM, VG, WBD, WBS. Most are companies headquartered outside the
US, which Russell's indexes leave out (as in
`docs/research/universe-parity-2026-10.md`). A few are US companies whose
absence from IWV on that date this check did not investigate.

**Why the first group matters.** Under GICS the bitcoin miners count as
Technology, the sector the cap binds most often; under Morningstar they
count as Financial Services. Momentum picks them in strong crypto runs, so
the cap may admit or exclude different names in exactly those months.

## Part 2: does it change the picks?

Limits: it relabels only stocks in today's universe. Stocks that were in
the universe earlier and differ between the two schemes are not relabelled,
so it understates the effect in earlier years.

Run `OOS_P_GICS`: the backtest's own universe and rules, 2016 to September
2026, with the 20 stocks above relabelled their GICS way, compared month by
month with `OOS_P_COARSE` (same settings, Morningstar labels;
33a5889249957d233913da47d26ab5be). `OOS_P_GICS` is backtest
b0f7eb651ee24c5181d7321b74a97db1; all 20 tickers were relabelled
(`Sectors` statistic: 20 tickers, 828 monthly rows).

| 2016–Sep 2026 | Return per year | Worst drop, peak to trough, on daily equity |
| --- | --- | --- |
| SPY | 15.07% | 33.7% |
| Morningstar labels (`OOS_P_COARSE`) | 20.10% | 31.0% |
| GICS labels for the 20 (`OOS_P_GICS`) | 20.08% | 28.6% |

| Overlap of the monthly holdings (`parity_compare.py`) | |
| --- | --- |
| Months compared | 129 |
| Mean share of the stocks also held | 96.9% |
| Months with any difference | 47, by 1 to 3 stocks |
| Worst month | 2026-06, 80% |
| Months where holding IEF differed | 0 |

The differences cluster in 2021 and 2024 to early 2026, the crypto runs,
when the bitcoin miners moved between Financial Services and Technology
and the cap admitted different names.

## What it means

- **The label source changes about one name a month and, here, not the
  return.** Most months are identical; the differing months swap one to
  three of 25 names. Over this window the return is the same to 0.02 points
  a year.
- **It understates the effect before about 2024**, since only today's
  universe was relabelled (above). The years when today's names were not
  yet in the universe are relabelled less.
- **Free GICS labels are an acceptable stand-in for the 475 names iShares
  covers**, with the difference measured rather than assumed. The live
  system records each month's sector source (ADR 0026, point 1), and the
  monthly comparison with QuantConnect during dry-run and paper trading
  (`docs/live-trading/plan.md`, section 4) keeps measuring it.
- **The 25 names outside IWV still need a label.** Without one they would
  be uncapped, as names with no sector are in the backtest
  (`defensive_v2.py:69-73`), which is a different rule for them. Candidate:
  the SEC's industry code (SIC) for each, mapped to sectors, checked against
  Morningstar the same way before use.

Research only; this is not a recommendation to trade any security.

