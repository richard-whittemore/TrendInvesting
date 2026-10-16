# Live universe from Massive against the backtest's (2026-10-16)

Research only; not investment advice.

## Question

Decision D8 (`docs/live-trading/plan.md`, section 4) builds the live universe
as the backtest does: every US-listed common stock, ranked by one session's
unadjusted dollar volume, price at least $5, the top 500. Phase 1 stays open
until that rule has been run on recent months from dated inputs and compared
with QuantConnect's own universe for the same months. Massive's free plan
(two years of history) is the data source.

## Method

- **QuantConnect side:** `research/edge-search/universe/qc_universe.py`, the
  Coarse filter of `momentum/defensive_v2.py` exactly, logging each month's
  500 names (variants `U_LOG_*`, PR #315), each run once:

  | Run | Backtest ID |
  | --- | --- |
  | U_LOG_2024_11 | bae7a02af46fd862fe174ea7fbd7f7ce |
  | U_LOG_2025_01 | da89f488d07f57f82c3e835419e99e67 |
  | U_LOG_2025_03 | c0ba7f93cf8cdfa77881b81ce59e1359 |
  | U_LOG_2025_05 | 2194ece6e9e84febbf31fc2f09770bbd |
  | U_LOG_2025_07 | 56b8fe26d51238f40067b9b58967887f |
  | U_LOG_2025_09 | ec328ecbf508a8551069d6dcc98f3c87 |
  | U_LOG_2025_11 | 2dae292af3a2ae85874dd453c45f6c2c |
  | U_LOG_2026_01 | 31c279b36bd26e22290e7dd20c71a140 |
  | U_LOG_2026_03 | 9972b614c0692bb626c0ad6043cf909d |
  | U_LOG_2026_05 | ce9dd19c54d2c53608d8ce19900e16b9 |
  | U_LOG_2026_07 | 78d5198189e573b052eafe31eb4cc5ea |
  | U_LOG_2026_09 | 415048879041d08251b9062034a0b8ca |

  Every logged month holds exactly its 500 names (checked against each run's
  count statistic). **QuantConnect's US equity data ends on 2026-07-18**:
  U_LOG_2026_07 stopped there and U_LOG_2026_09 logged nothing. So 21 months
  are compared, 2024-11 to 2026-07, not 24.
- **Massive side:** `research/edge-search/universe/massive.py`: the grouped
  daily bars (unadjusted) for one session, and the reference tickers dated
  that session, filtered by type. Responses are cached outside the
  repository (`data/massive/`); none is committed.
- **Comparison:** `compare.py`: the share of QuantConnect's 500 names that the
  Massive rule also picks. Columns A, B and C measure **agreement between
  selections**: each month's Massive list against the selection QuantConnect
  logged for that month. In the seven months of Finding 2 the backtest's
  rebalance used the previous month's selection instead; the universe it
  traded in those months is compared separately there.

## Results

Three versions of the Massive rule:
- **A, as D8 was written:** common stock and ADRs (types CS and ADRC), the
  last session before the month.
- **B:** common stock only, the last session before the month.
- **C:** common stock only, on the session QuantConnect actually used (see
  the timing finding below).

| Month | 1st falls on | Last session before | A | B | Session QuantConnect used | C |
| --- | --- | --- | --- | --- | --- | --- |
| 2024-11 | Fri | 2024-10-31 | 93.4% | 97.0% | 2024-10-31 | 97.0% |
| 2024-12 | Sun | 2024-11-29 | 85.6% | 87.8% | 2024-12-02 | 96.2% |
| 2025-01 | Wed | 2024-12-31 | 94.4% | 97.0% | 2024-12-31 | 97.0% |
| 2025-02 | Sat | 2025-01-31 | 93.6% | 96.4% | 2025-01-31 | 96.4% |
| 2025-03 | Sat | 2025-02-28 | 92.6% | 95.4% | 2025-02-28 | 95.4% |
| 2025-04 | Tue | 2025-03-31 | 93.2% | 97.6% | 2025-03-31 | 97.6% |
| 2025-05 | Thu | 2025-04-30 | 95.0% | 97.8% | 2025-04-30 | 97.8% |
| 2025-06 | Sun | 2025-05-30 | 81.8% | 83.0% | 2025-06-02 | 96.6% |
| 2025-07 | Tue | 2025-06-30 | 93.0% | 96.4% | 2025-06-30 | 96.4% |
| 2025-08 | Fri | 2025-07-31 | 92.6% | 96.8% | 2025-07-31 | 96.8% |
| 2025-09 | Mon (holiday) | 2025-08-29 | 83.6% | 85.4% | 2025-09-02 | 97.0% |
| 2025-10 | Wed | 2025-09-30 | 92.8% | 96.4% | 2025-09-30 | 96.4% |
| 2025-11 | Sat | 2025-10-31 | 93.6% | 96.2% | 2025-10-31 | 96.2% |
| 2025-12 | Mon | 2025-11-28 | 83.2% | 86.2% | 2025-12-01 | 96.0% |
| 2026-01 | Thu | 2025-12-31 | 93.6% | 96.0% | 2025-12-31 | 96.0% |
| 2026-02 | Sun | 2026-01-30 | 84.0% | 85.6% | 2026-02-02 | 97.2% |
| 2026-03 | Sun | 2026-02-27 | 82.0% | 83.2% | 2026-03-02 | 96.6% |
| 2026-04 | Wed | 2026-03-31 | 91.0% | 96.0% | 2026-03-31 | 96.0% |
| 2026-05 | Fri | 2026-04-30 | 94.2% | 96.6% | 2026-04-30 | 96.6% |
| 2026-06 | Mon | 2026-05-29 | 83.6% | 84.4% | 2026-06-01 | 96.0% |
| 2026-07 | Wed | 2026-06-30 | 91.0% | 93.4% | 2026-06-30 | 93.4% |
| **Mean** | | | **89.9%** | **92.6%** | | **96.4%** |

C ranges from 93.4% to 97.8%. For comparison, IWB's holdings matched 62%
(`docs/research/universe-parity-2026-10.md`).

### Finding 1: QuantConnect's universe leaves out ADRs

With ADRs, about 25 of each month's 500 names were ADRs that QuantConnect's
universe does not hold (ASML, TSM, BABA, NVO, SAP, SHEL and others). Each
pushed out a name near rank 500 that QuantConnect does hold. Foreign
companies listed directly, such as Shopify and Teck, are common stock (CS) in
Massive and stay in, as in the backtest. AstraZeneca (AZN) is the one ADR
QuantConnect holds every month. **Changed:** `massive.py` now keeps type CS
only.

### Finding 2: in some months the backtest ranks the previous month's universe

QuantConnect runs universe selection at midnight, on the data of the
session just ended, and only after a day with a session. So when the 1st of
the month is a Sunday or a Monday (or a Monday holiday), the month's first
selection runs the day *after* its first session. The log's own timestamps
show this: 2025-06-03, 2025-09-03, 2025-12-02, 2026-02-03, 2026-03-03,
2026-06-02 and 2024-12-03. The rebalance runs at 8:00 on the first session
(`defensive_v2.py`, `MonthStart` at 8:00) and ranks `self.members`, which
that month's selection has not yet replaced. So **in those months the
backtest rebalanced on the previous month's universe**, chosen about a month
earlier. No future data is used; the list is a month old. That was 7 of the
21 months here. Over a long run it is about 2 months in 7, the months whose
1st is a Sunday or a Monday.

The Massive rule as written ranks the last session before the rebalance, so
in those months it is fresher than the backtest. That is why B falls to
83–88% exactly in those months, and C, on the session QuantConnect used,
recovers to 96–97%.

**Against the universe the backtest traded** in those seven months (the
previous month's selection), the Massive rule as written (latest session,
common stock only) shares 80.1% of names on average (78.8% to 81.4%):

| Month | Shared with the universe traded |
| --- | --- |
| 2024-12 | 81.0% |
| 2025-06 | 81.4% |
| 2025-09 | 79.8% |
| 2025-12 | 79.6% |
| 2026-02 | 79.2% |
| 2026-03 | 81.0% |
| 2026-06 | 78.8% |

QuantConnect's own consecutive selections share about the same (77.8% to
83.4%), so this is the month-to-month change in the top 500, not a defect of
the Massive rule.

### Finding 3: what is left

At about 96%, the remaining differences are mostly names near rank 500, which
a small difference in volume reorders, and a few names that QuantConnect
leaves out every month although Massive types them as common stock (CHTR,
EQR, FERG, FLUT). These fit the known gap: QuantConnect's
`HasFundamentalData` screen, which has no free equivalent.

## What it means

- **The D8 rule reproduces QuantConnect's selection closely** once ADRs are
  left out: 96.4% agreement on average when ranked on the same session. In
  the other 5 months out of 7 that is also the universe the backtest traded.
- **Decided 2026-10-16: choice 1** (owner: "Choice A"), with the effect
  on returns to be measured (`momentum/fresh_universe.py`).
- **The timing question (Finding 2).** ADR 0026 (point 8)
  says no backtest behaviour is copied into live trading without a decision.
  The two choices:
  1. **Live ranks the latest session** (as D8 is written). This is the
     intended rule, but in about 2 months out of 7 about 20% of the live
     universe differs from the one the backtest traded (80.1% shared), and
     the backtested returns include the stale universe.
  2. **Live copies the backtest:** when the month's 1st is a Sunday or a
     Monday, rank using the universe chosen for the previous month. This
     reproduces what was tested, but deliberately uses older data.
  Either way, the Phase 2 parity tests run both versions and report the
  difference in picks. A QuantConnect run with the selection moved before
  the rebalance measures the effect on returns (variants `FU_COMBO`,
  `OOS_FU_COMBO`).
- **Phase 1:** the comparison the plan requires is recorded here, on dated
  inputs, and the timing is decided.

## Notes

- 2024-11 and 2025-02 show 499 names on the QuantConnect side because two
  of its securities carry the ticker DOC in those months (QuantConnect
  identifies securities by more than the ticker).
- The Massive reference list was taken as of each tested session. The
  Nasdaq Trader files named in D8 are current only and were not used.
- Raw inputs (QuantConnect logs, Massive responses) are kept outside the
  repository, under `data/`.
