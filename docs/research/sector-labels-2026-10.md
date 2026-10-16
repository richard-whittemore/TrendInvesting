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

Run `OOS_P_GICS`: the backtest's own universe and rules, 2016 to September
2026, with the 20 stocks above relabelled their GICS way, compared month by
month with `OOS_P_COARSE` (same settings, Morningstar labels;
33a5889249957d233913da47d26ab5be). Results to follow in this document.

Limits of part 2: it relabels only stocks in today's universe. Stocks that
were in the universe earlier and differ between the two schemes are not
relabelled, so it understates the effect in earlier years.
