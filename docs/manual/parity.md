# Parity: does the Go code decide what the backtest decided?

## What it does and why it exists

The strategies were tested on QuantConnect, in Python. The money will be
traded by the Go engine. Before any dry run, the Go code has to be shown to
make the same decisions as the backtests did, from the same data. That is
the parity gate (ADR 0026, point 7, item 2; `docs/live-trading/plan.md`,
section 6, item 2). This chapter is about the tool that checks it,
`cmd/parity`.

It answers, month by month for momentum and session by session for Boost:

- **momentum**: given QuantConnect's own scores, volatilities and sectors,
  does Go rank the stocks in the same order, mark the same ones as picked,
  skipped by the sector cap (at most 3 per sector) or left out, and give
  the same weights? Given QuantConnect's own prices, does Go work out the
  same 12-month scores and volatilities, and the same risk-off reading from
  SPY's 200-day average?
- **Boost**: given QuantConnect's own SPY prices from the first one, does
  Go work out the same RSI(2), 200-day and 5-day averages, and the same
  boosted-or-not state and SPY target, every session?

The answer is a pass or a fail per check. A fail is recorded as it is and
never rewritten (AGENTS.md rule 6).

## The thresholds

They were fixed before any comparison was run:

| Check | Must hold |
| --- | --- |
| ranked order, cap marks | exactly the same |
| weights | within 0.000000000001 (1e-12) of QuantConnect's |
| scores, volatility, the SPY average | within one part in a billion (1e-9) |
| Boost RSI, both averages, state and target | exactly the same, every session |
| any other difference | fails, unless ADR 0026 point 8 lists it |

ADR 0026 point 8 lists the backtest behaviours the live code deliberately
does not copy: keeping last month's book when nothing can be scored, treating
SPY as risk-on with fewer than 200 prices, a price window that slides over a
missing day, equal scores ranked in whatever order the data arrived, a stock
whose price never moved weighted as if its volatility were 1, and RSI's
dependence on where its history starts. When one of these shows up the
report lists it with its size, and it does not fail the check. Anything else
fails, including a difference the code makes on purpose that is not on that
list (for example a stock with no sector label: the backtest left it
uncapped, live leaves it out).

Such a difference excuses only what can follow from it, and only when
QuantConnect's own data shows it (a SPY price count under 200, a missing
price in the window, a volatility of exactly zero, no sector label). A
stock left out changes the marks of the stocks ranked after it, but only if
QuantConnect's book held it (picked or capped); and it changes the weights
only in a month where the two sides picked different stocks. Every other
mark and weight is still compared, and a mismatch there fails.

A check that had nothing to compare fails too: it proves nothing. The
items put down to a difference are counted apart (the `Attributed` column)
and do not count as compared.

## Where the inputs come from

QuantConnect's free plan keeps only 10 KB of log per backtest and does not
let a backtest write files, so the backtests carry their data out in a
different way: each order has a free-text tag, and the logging versions of
the templates (`research/edge-search/momentum/parity_log.py` and
`research/edge-search/boost/parity_log.py`) write the month's data into the
tag of an order the strategy places anyway. Changing a tag changes no fill.
If there is no open order to tag, the template places a one-share limit
order at 1% of SPY's price and cancels it at once, which never fills; the
run's `Parity` statistic counts how often that happened.

The logging versions are the recorded strategy files with lines added and
none removed (a test checks this). They are meant to trade as the recorded
runs did, but the test does not prove that an added line changes nothing,
and the 2026-10 momentum runs were not checked against a same-day re-run of
the recorded strategy, so that identity is not verified
(`docs/research/parity-2026-10.md`, "Runs").
The months to log were chosen before any run (`PARITY_MONTHS_IS` and
`PARITY_MONTHS_OOS` in `research/edge-search/build_variants.py`): at least 24
in each window plus every month the golden-case runs logged.

The exports contain real prices, so they are kept outside the repository,
in `~/Desktop/Trend_Investing/data/parity/`, one `<run>_orders.json` (every
order of the run, tags included) and one `<run>_backtest.json` (its
statistics) per run.

## How to run it

```sh
go run ./cmd/parity \
  -momentum FU_PARITY=$HOME/Desktop/Trend_Investing/data/parity/FU_PARITY_orders.json \
  -months FU_PARITY=1999-03,1999-11,... \
  -boost BX_PARITY=$HOME/Desktop/Trend_Investing/data/parity/BX_PARITY_orders.json
```

Each `-momentum` run needs its `-months` list: the months chosen in advance.
The exact command used for the recorded result, with every month, is in
`docs/research/parity-2026-10.md`.

## What it prints

A line per input saying how many months or sessions it found, then a table
with one row per check and run: what was compared, the threshold, how many
comparisons, how many items were put down to a named difference instead of
compared (`Attributed`), how many comparisons failed, the largest difference
seen and PASS or FAIL.
Then the overall result, the first failure of each failing check, and a
table of the differences, each marked as on ADR 0026 point 8's list or not.

It exits with status 0 when everything passes, 1 when any check fails, and 2
when an input cannot be read at all (a missing file, a tag that is not
valid, a run with no planned months).

## When something goes wrong

- **A planned month is missing** (`months` fails): the backtest did not
  rebalance that month, or the tag was lost. Check the run's `Parity`
  statistic, which counts the months it exported.
- **`export` fails**: the export is malformed (a price column of the wrong
  length, dates out of order or repeated, a picked stock with no weight, a
  stock ranked twice, a member count that is not the ranked plus the too
  short); nothing in that month is compared further. It also fails when
  Go gave a stock a status the tool has no backtest mark for. A required field that is
  missing or null, or an empty ticker, stops the tool before any
  comparison (exit status 2).
- **A check fails**: the Go code and the backtest disagree. The first failure
  line says where. This is a result, not a tool fault: record it, open an
  issue for it, and do not change a threshold after seeing it.
