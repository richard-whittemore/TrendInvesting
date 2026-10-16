# Golden cases for the target-weight modules (2026-10-16)

Research only; not investment advice.

ADR 0026 (point 7, item 1) requires golden cases transcribed from
QuantConnect before the Go momentum and Boost modules are accepted: a
risk-off month, a binding sector cap, a stock under 253 closes, a Boost
entry, exit and next-day re-entry, and Boost's first decision from cash.
This records the runs that log them. The logs themselves hold prices,
scores and volatilities, which are market data, so they are kept outside the
repository, in `data/golden/` beside it (one JSON list of log lines per run,
named `golden_<run>.json`). They are never committed.

## Runs

Logging templates: `research/edge-search/momentum/golden_log.py` (PR #310,
with `GOLDEN_FROM_RANK` from PR #314) and `research/edge-search/boost/golden_log.py`
(PR #311). Each adds lines only to the recorded strategy file, which a test
checks, so it trades exactly as the recorded runs do. Each run was made once,
on QuantConnect project 37096083. "Lines" is the number of GOLD lines in the
log, and it matches the run's own `Golden` statistic in every run, so no log
was cut short.

| Run | Backtest ID | Month(s) | Lines | Risk-off | Names under 253 closes |
| --- | --- | --- | --- | --- | --- |
| IS_GOLD_2000_03 | 79fbb7859826065cde3e4466f579a8ab | 2000-03, ranks 1–144 | 49 of 71 (cut, see below) | no | in part B |
| IS_GOLD_2000_03_B | 10c772fba2740e120e5b32c808a2477f | 2000-03, ranks 145–188, weights | 23 | no | 58 |
| IS_GOLD_2008_11 | 65c3df3b07a2b767aea1728227f955a9 | 2008-11 | 23 | yes | 4 |
| IS_GOLD_2009_06 | 2fb0d2e10d31b1e23d07d7575e22e873 | 2009-06 | 22 | no | 2 |
| IS_GOLD_2013_05 | e06aef39d46fbee897dec6bcc1f6b8a0 | 2013-05 | 20 | no | 10 |
| OOS_GOLD_2016_02 | b47f8d83062ae28f37afb7ba8ebd7df3 | 2016-02 | 20 | yes | 6 |
| OOS_GOLD_2020_04 | 42990a145d27507d1fc05aa56f3d713a | 2020-04 | 29 | yes | 13 |
| OOS_GOLD_2021_03 | a372202d0c2d363065f840286f4a43b0 | 2021-03, ranks 1–138 | 47 of 61 (cut, see below) | no | in part B |
| OOS_GOLD_2021_03_B | 31df8fbfea2f2f54a47628e0b5bd95d5 | 2021-03, ranks 139–159, weights | 15 | no | 26 |
| OOS_GOLD_2022_06 | 7c6b52dbb531c3690f10b2128820c554 | 2022-06 | 28 | yes | 5 |
| OOS_GOLD_2024_03 | f4a350cd99ad8d4623f52be5166d937a | 2024-03 | 37 | no | 3 |
| BX_GOLD | 7104a08efcb3154a8aa400f25f0d84e9 | first decision; 2017-08, 2019-05, 2024-04 | 68 | | |
| BX_GOLD_2 | 3b8d97ba0b2e22cf8ae68a5f628cd6bb | first decision; 2023-10, 2024-07 | 45 | | |

**Two months logged in two runs.** QuantConnect's Free plan keeps 10 KB of
log a run. In 2000-03 and 2021-03 the sector cap skipped so many names that
the ranked list ran past rank 137, and the cap cut the first run before the
weights (47 of 61 and 49 of 71 lines). Those runs are kept as recorded. Each
second run (`_B`) writes the ranked list from the next rank, with every
earlier name still marked as in the full run. Together the two parts are
contiguous (ranks 1–159 and 1–188), share the same header line, and hold all
25 picks.

## Cases required by ADR 0026

| Case | Where |
| --- | --- |
| Risk-off month | 2008-11, 2016-02, 2020-04, 2022-06 |
| Binding sector cap (a name skipped by the cap, marked C) | every momentum month; most in 2000-03 and 2021-03 |
| A name under 253 closes, left out | every momentum month (for 2000-03 and 2021-03, in part B); most in 2000-03, 58 names |
| Boost entry | e.g. 2017-08-10, 2019-05-09 |
| Boost exit | e.g. 2017-08-14, 2019-05-15 |
| Boost exit then re-entry the next session | 2024-07-23 exit, 2024-07-24 re-entry (BX_GOLD_2) |
| Boost's first decision, from cash (98%, no boost) | the first line of BX_GOLD and BX_GOLD_2 |

BX_GOLD's three months held no next-day re-entry. The months for BX_GOLD_2
came from the orders of the recorded run BX_SPY (b275c80d), which re-enters
the session after an exit on 2023-10-02/03 and 2024-07-23/24.

## Precision

The momentum lines write every number with `repr`, so they round-trip
exactly. Boost's lines write prices and indicators to six decimals. That
suffices to check Boost's decisions, but a value lying exactly on a threshold
could round the other way; a golden test that hits one should be checked
against the run rather than adjusted.

## Next

Committed fixtures stay synthetic, so real market data never enters the
repository. The Go modules' golden tests therefore read these files from
`data/golden/` and are skipped, saying so, where the files are absent (as in
CI). Phase 2 decides the loader.
