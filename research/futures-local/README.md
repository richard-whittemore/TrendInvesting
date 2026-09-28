# Faith's Turtle rules on Pinnacle CLC futures: a local backtester

## What this is

This folder is a small, local, standard-library Python backtester. It runs Faith's own, unadapted Turtle rules on back-adjusted continuous futures from Pinnacle Data's CLC database:

- System 2, 55/20, long and short;
- a 1% Unit, a 2N stop and ½N Adds;
- Faith's four Unit caps and the drawdown rule for the Notional Account.

It exists because the QuantConnect Cloud run in `research/qc-cloud-futures` was unusable. QuantConnect's free futures data covers only ES before 2007, and roll fills on sparse contracts distorted the P&L (that folder's README, "Cloud result").

- **The rule core is `research/qc-cloud-futures/rules.py`, imported, not copied.** `backtest.py` puts that folder on `sys.path`.
- **`research/qc-cloud-futures/main.py` is the spec** for everything around the rule core: the Session order, entries, Adds, exits, Unit caps, the Notional Account, Strength ranking and slippage. "Differences from main.py" below lists every place this file departs from it.
- **It is research only.** It is not the production Go engine, and makes no claim of parity with it.

**No market data is committed.** Pinnacle's files live outside the repository. The tests use only small synthetic series built inside the test files.

| File | What it does |
| --- | --- |
| `loader.py` | Reads one Pinnacle CLC text or CSV file, clipped at the run's end date. It detects the delimiter, header and date format, and rejects bad rows loudly. |
| `markets.py` | Faith's portfolio [T p.10-11]: each market's Pinnacle file stem, dollars per point in Pinnacle's units, tick size, correlation groups and roll months, each with its source. |
| `rates.py` | Reads a 3-month T-bill rate CSV and looks up the prevailing rate on any date, with carry-forward ("Interest on idle cash" below). |
| `backtest.py` | The event loop, costs, metrics, reconcile check and command line. |
| `test_*.py` | Unit tests on synthetic fixtures. They run in `make research-test`. |

## How to run it

The Pinnacle CLC files live in `~/Desktop/Trend_Investing/data/pinnacle/DATA/CLCDATA/`, outside the repository. Run the in-sample default, 1980 to 2015, with and without T-bill interest and with the S&P benchmark:

```sh
python3 research/futures-local/backtest.py \
  --interest-rates ~/Desktop/Trend_Investing/data/rates/TB3MS.csv --benchmark SP
```

Useful flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--data-dir` | `~/Desktop/Trend_Investing/data/pinnacle/DATA/CLCDATA` | Where the files are. |
| `--start`, `--end` | `1980-01-01`, `2015-12-31` | The span. Bars before `--start` warm up N, the channels and Strength, but trade nothing. Every file is read only up to `--end`. |
| `--allow-holdout` | off | **Required** for any `--end` in 2016 or later. Without it the run refuses to start (exit code 2). |
| `--markets` | all in `markets.py` | A comma-separated list of `markets.py` symbols, e.g. `GC,SI,CL`. Excluded markets are skipped with a note. |
| `--file-template` | `{stem}_REV` | The traded, back-adjusted file, without extension. `{stem}` and `{symbol}` are substituted. |
| `--scale-template` | `{stem}_NON` | The non-adjusted file, used by the price-scale check and `--benchmark`. |
| `--benchmark` | off | A `markets.py` symbol whose non-adjusted close is reported as price-only buy and hold, e.g. `SP`. **Dividends are excluded.** |
| `--commission` | `2.50` | Dollars per contract per side. |
| `--slippage-n` | `0.05` | Slippage per fill, in N (ADR 0013). |
| `--unit-fraction` | `0.01` | Faith's 1%. Use `0.005` for the equity Baseline's figure. |
| `--equity` | `1000000` | Starting equity. |
| `--long-only` | off | Skip every short breakout. |
| `--no-roll-costs` | off | Don't charge rolls. |
| `--interest-rates` | off (no interest) | Path to a 3-month T-bill rate CSV ("Interest on idle cash" below). With it, the report runs and prints the backtest twice, with and without interest. |
| `--interest-haircut` | `0.0` | Annual rate (fraction, e.g. `0.01` for 1%) subtracted from the loaded curve before crediting -- a broker's spread. Only meaningful with `--interest-rates`. |

A market with no file is skipped with a warning; the run continues. The report gives:

- CAGR, max drawdown, CAGR ÷ max drawdown, and CAGR per decade and per report period;
- the number of Campaigns, win rate, and average win and loss in R;
- P&L per market, net of costs, with open Campaigns marked to the last settle;
- total commission, and total roll cost with the number of rolls;
- decline counts;
- a `Reconcile` line.

The exit code is 1 if the reconcile check fails.

## The Pinnacle files (checked 2026-09-28)

- **Layout.** Headerless CSV, `MM/DD/YYYY,Open,High,Low,Close,Volume,OpenInterest`, one file per market and adjustment: `<SYM>_REV.CSV` back-adjusted, `<SYM>_NON.CSV` non-adjusted, `<SYM>_RAD.CSV` ratio-adjusted. The backtester trades `_REV` and checks price scale on `_NON`.
- **Units and dollars per point.** `markets.py` cites, per market, Pinnacle's manual (Appendix B-1) and a check against the files. The manual's "BigPoint Value" column is not always in the file's units (ZI, silver, says 5000, but the file quotes cents: the right figure is $50). So dollars per 1.00 of the file's price is derived as the manual's Min $ Move ÷ tick size, and checked by multiplying a real `_NON` settle by it to get the contract's notional value. Pinnacle quotes silver, copper, heating oil and gasoline in cents, and scales the currencies up (the yen × 10,000, the rest × 100).
- **Roll dates.** Pinnacle's back-adjustment steps only on a roll, so its roll dates are the days on which `_REV` minus `_NON` changes. `markets.py`'s `roll_months` and `roll_day` come from those days, 1990-2015, and agree with the manual's "RollOverDate" column.
- **Splices.** FN is the Deutschmark to March 1999, then the euro; ZB is NYMEX unleaded gas, then RBOB from 2006. Each splice shows up as one step in `_REV` minus `_NON`, like a roll, so `_REV` has no false jump. The Deutschmark and euro are therefore one market (`EC`), not two with trade windows.
- **Not available.** Pinnacle's CLC database has no 90-day T-bill or French franc contract. `TB` and `FR` stay in `markets.py` with `excluded` set.
- **Short histories.** Copper (ZK) starts in 1989, crude (ZU) in 1984, gasoline (ZB) in 1985, the 10-year note in 1983, the Eurodollar (Pinnacle's EC) in 1982 and the S&P 500 in April 1982. SP ends in 2021 and the Eurodollar in 2023.

## Data problems found

- **Corrupt rows, all after 2015.** ZI (silver, December 2025: a high of -942 against a low of 5,266), ZK (copper, from January 2026, and a close near 20 cents in September 2026) and SB (sugar, November 2025: the close above the high) have rows the loader rejects. All are in the held-out period, so `loader.load_series(end=...)` stops reading at the run's end date: those rows are never validated or read. A bad row on or before the end still stops the run.
- **In-sample, 1980-2015, all 19 traded files load cleanly.** A scan for daily moves above 6N found only real events, such as the October 1987 crash, the September 1985 Plaza Accord in the yen, the September 1999 Washington Agreement in gold and the Swiss franc's January 2015 de-pegging.

## TO-VERIFY

Only one item is left. The IMM British pound contract was reportedly GBP 25,000 before the mid-1980s, not today's 62,500, and no source dates the change. The error changes only how many whole contracts a Unit rounds to, not the dollars per point per pound.

`python3 -c "import markets; print('\n'.join(markets.to_verify_report()))"`, run in this folder, prints each market's stem and anything unverified or excluded.

## Assumptions and how the loop works

Each Session is one date in the union of every market's dates. A market with no bar that day does nothing: no fills, no decisions, and it keeps its last settle for the account's equity.

1. **Fills.** Orders resting from earlier Sessions fill against today's bar, per market, in this order:
   - **Exits.** Each Unit's own Exit Order rests at the more protective of its stop and, while one is proposed, the Exit Channel level (`rules.exit_order_level`). It fills when the bar touches it, at min(level, open) − slippage for a sell and max(level, open) + slippage for a buy. A bar that gaps through a stop fills at the open.
   - **Adds.** A stop-limit order at the rung, capped at 1N beyond it (`rules.stop_limit_fill_price`, ADR 0005 as amended). Any exit fill cancels a resting Add.
   - **Entries.** The same kind of stop-limit order, at the Entry Channel.
2. **Decisions at the close,** as in `main.py`'s `_session`:
   - expire the day's unfilled entry orders, and any Add that was not fill-chained today (ADR 0011, as amended);
   - propose Exit-Channel exits;
   - decide Adds in ascending symbol order;
   - decide entries ranked by Strength: every long first, strongest first, then every short, weakest first [T p.27-29].

   Unit caps are reserved with `rules.SessionCapLedger`.
3. **Mark to market.** Every open Unit settles to today's settle (daily variation margin).
4. **Roll costs** are charged (below).
5. **The Notional Account** observes today's equity [T p.17].

**Costs.**
- Commission is $2.50 per contract per side by default, charged on every entry, Add and exit.
- Slippage is 0.05N per fill, against the trader (ADR 0013). N is the Campaign's frozen N for Adds and exits, and the entry's N for entries.

**Roll costs.** The series is back-adjusted, so a roll moves no price. A real position still pays to roll. On each market's first Session on or after `roll_day` in each of its `roll_months`, every open position is charged one extra round trip:

```
contracts x 2 x (commission + 0.05N x multiplier)
```

The roll months and days are Pinnacle's own, read off the files ("The Pinnacle files" above):
- quarterly for financials and currencies;
- five times a year for gold (Pinnacle skips the October contract), silver, copper, coffee and cocoa;
- four times for sugar and cotton;
- monthly for energy.

**P&L** is points × multiplier throughout. No percentage is ever taken from a back-adjusted price level, which can be zero or negative. A market's multiplier on the entry date is frozen for its Campaign, like N and the Unit size (ADR 0006). This matters only for the S&P 500's 1997 change.

**Reconcile.** The account side is daily variation margin, less commission and roll costs. The Campaign side is `rules.Campaign`'s own fill-to-exit price distance × Unit size × multiplier, less the same costs, plus open Campaigns marked to the last settle. The two are computed independently and must agree to within $0.01. **Interest is excluded from both sides** ("Interest on idle cash" below): it is a separate account line, not part of Campaign P&L, so the invariant holds exactly whether or not interest is switched on.

**Tradable window.** `markets.tradable` stops new Campaigns outside a market's `trade_from`..`trade_until`. No traded market has one now: Pinnacle's FN splices the Deutschmark and the euro into one series. Open Campaigns are never closed for leaving a window (CONTEXT.md, "Eligible").

**Ruin.** If equity reaches zero, the run stops and the report says so.

## Interest on idle cash

Before 2009, a futures account earned Treasury-bill interest on its whole balance -- not just uninvested cash, but the margin backing open positions too -- and that interest was a large share of trend followers' returns. This is optional here, off by default, so results can be reported with and without it.

**Rate source.** This backtester never fetches market or economic data itself, and none is committed to the repository. Get the rate from FRED yourself:

1. Open <https://fred.stlouisfed.org/series/TB3MS> ("3-Month Treasury Bill Secondary Market Rate, Discount Basis", monthly) and download its CSV. (`DTB3`, the daily version at <https://fred.stlouisfed.org/series/DTB3>, works too -- `rates.load_rate_series` reads either shape.)
2. Save it to `~/Desktop/Trend_Investing/data/rates/TB3MS.csv`. FRED's own TB3MS download has the header `observation_date,TB3MS`, one row per month dated the first of the month (e.g. `1934-01-01,0.72`), the rate in annual percent, and `.` for a missing observation.
3. Pass `--interest-rates ~/Desktop/Trend_Investing/data/rates/TB3MS.csv` on the command line.

**Accrual.** `rates.RateCurve.rate_on(day)` looks up the latest rate dated on or before `day` -- a missing date (every day between TB3MS's monthly rows, or a FRED `.` placeholder) carries the last known rate forward, never zero and never interpolated. Before the series' first date there is no rate to carry forward, so the credit is zero and a one-time warning prints to stderr. `Backtester._credit_interest` credits the account's *whole* cash equity (`self.cash`), not a separate uninvested-cash sleeve, for every calendar day since the previous Session -- including a weekend or holiday gap, using each of those days' own rate (`rates.RateCurve.accrued_fraction`), because the cash balance itself does not change while no Session runs. The day-count convention is **actual/360**: FRED's `DTB3`/`TB3MS` both quote the T-bill's own bank-discount rate, which the Treasury and the money market quote on a 360-day year, so 360 matches the rate's own quoting convention (365 would understate the daily accrual a quoted annual rate implies). An optional `--interest-haircut` (default 0) subtracts a spread, in the same annual-rate units, from the loaded curve before crediting -- what a broker or futures commission merchant kept rather than passing through -- but never from the zero credited before the curve's first date.

Because the credited interest lands in `self.cash`, it compounds into the Notional Account and so into the next Unit's size, exactly as real interest income would have. This means a run with interest is not simply the no-interest run's cash plus interest bolted on afterward; it can trade slightly differently. The report therefore runs the whole backtest twice when `--interest-rates` is given -- once with the curve, once without -- and prints both, along with the total interest earned.

**Per-period CAGR.** Every report additionally breaks the CAGR down into five fixed windows, independent of whether interest is switched on: everything through 1989, the 1990s, 2000 through the 2008 crisis, the post-crisis 2009-2015 span, and 2003-2015 -- from Faith's own publication of these rules (*The Original Turtle Trading Rules*, Curtis Faith, OriginalTurtles.org, 2003; `docs/methodology/Methodology_Analysis.md`, source T) through the end of the in-sample span, i.e. only the years in which anyone outside the original Turtles could have traded the published rules.

## In-sample result (2026-09-28)

The command in "How to run it": 19 markets, 1980-01-02 to 2015-12-31, $1M, Faith's 1% Unit, default costs. Reconcile OK in both runs.

| | No interest | With T-bill interest | S&P price only (SP_NON, no dividends, from 1982-04-21) |
| --- | --- | --- | --- |
| CAGR | +5.83% | +10.11% | +8.83% |
| Max drawdown | 99.75% | 99.53% | 57.12% |
| CAGR ÷ MaxDD | 0.058 | 0.102 | 0.155 |
| start-1989 | +70.82% | +86.18% | +15.53% |
| 1990-1999 | +11.62% | +11.54% | +15.33% |
| 2000-2008 | -22.61% | -17.60% | -5.40% |
| 2009-2015 | -25.95% | -25.83% | +12.37% |
| 2003-2015 | -23.91% | -22.13% | +6.67% |

Without interest: 2,095 Campaigns, a 21.7% win rate, an average win of +7.88R and an average loss of -1.55R. Commission was $133.5M and roll cost $602.9M over 1,213 rolls.

- **The dollar figures are dominated by the 1990s.** Equity compounded to $3.1B by May 1997, so later Unit sizes are far beyond real liquidity: 36% of entries would put 4 Units above 10% of the contract's open interest. Percentages are scale-free; dollars by market are not.
- **Volatility is the story.** At 1% per Unit, with Faith's caps allowing up to 12 Units a side, the daily equity series has about 64% annualised volatility, and single days of -20% to -24% occur (1987-10-20, 2000-09-22, 2006-03-16, 2008-09-19). Diagnostics only, not a variant to adopt: with no costs at all the 1% run still returns -18.6% a year over 2003-2015, and a 0.25% Unit returns -3.5% a year over 2003-2015 with costs.

## Differences from main.py

- **One series per market.** Signals, fills and P&L all use the back-adjusted series. There are no dated contracts, no raw-price offsets and no roll orders, so `main.py`'s raw-stop-at-or-below-zero decline doesn't apply.
- **Exit stops trigger on a touch.** In LEAN, the native stop-market fill needed a strict cross. Here exits trigger on a touch, consistent with the entry and Add fill rule (ADR 0005).
- **Both entries filling on one bar.** If one bar fills both a long and a short entry order, the order whose level is nearer the open is taken as having triggered first, and the other is cancelled. `main.py` filled both and liquidated the second.
- **Commission** is a flat per-contract charge, where the cloud version used LEAN's Interactive Brokers model.
- **Unchanged from `main.py`:**
  - a newly filled Unit's own stop is first live on the market's next bar;
  - no Add is decided in a Session that proposes an exit;
  - an Add or entry that would breach a Unit cap is declined and counted.

## Tests

```sh
make research-test
```

`test_loader.py` covers delimiters, headers, every date format and the century rule, every rejection, clipping at the end date, and the price-scale hints in Pinnacle's units.

`test_markets.py` checks the table's integrity, its agreement with `main.py`'s correlation groups, and the Pinnacle mapping: each stem and dollars per point, the excluded markets, and the Deutschmark-euro splice.

`test_rates.py` covers the rate loader and lookup: FRED's own header and percent-to-fraction conversion, the `.` missing-observation placeholder, carry-forward, the zero-with-one-warning credit before the first available rate, the haircut, and the actual/360 accrued-fraction arithmetic (including across a rate change).

`test_backtest.py` covers:
- a hand-computed long Campaign: entry, Add, stop-out, P&L and R;
- gap-through-stop fills on both sides;
- the price cap;
- an Exit-Channel exit;
- the roll cost;
- no trading before the start date;
- the reconcile invariant on a three-market synthetic run;
- the holdout refusal, in both the API and the command line;
- interest: accrual arithmetic over a known period, carry-forward across a mid-run rate change, zero credit before the first available rate, the flag off leaving results unchanged, the reconcile invariant with interest and a haircut on, and the command line's side-by-side with/without report;
- the five per-period CAGR windows;
- the command line's end-date clipping, excluded-market note, scale check on `_NON`, and the price-only benchmark.

# Time-series momentum (TSMOM): a second, independent strategy

`tsmom.py` backtests the core strategy of Moskowitz, Ooi & Pedersen (2012), "Time Series Momentum", *Journal of Financial Economics* 104(2), 228-250 ("MOP"), on the same Pinnacle files. It shares only `loader.py`, `rates.py` and a few helpers from `backtest.py`; it has no Turtle rules in it.

| File | What it does |
| --- | --- |
| `tsmom_markets.py` | The universe: 41 USD-settled Pinnacle markets, each with dollars per point and tick, sourced; and `EXCLUDED`, every other Pinnacle symbol with the reason. |
| `tsmom.py` | Signal, EWMA volatility, sizing, the daily loop, costs, reconcile, metrics and command line. |
| `test_tsmom.py` | Synthetic-data tests, in `make research-test`. |

## The rules (MOP)

- **Signal.** At the last session of each month, the sign of the market's past 12-month return (MOP Section 4). The daily return is the `_REV` change divided by the previous day's `_NON` settle, and the 12-month return compounds those. **No return is ever taken relative to a `_REV` level**, so negative back-adjusted levels (US, ZH, ZB, the grains) are harmless. The `_NON` price is always positive in-sample; a non-positive one stops the run.
- **Volatility.** An exponentially weighted variance of daily returns, centre of mass 60 days, annualised by 261 (MOP eq. 1), with the weights normalised by their sum.
- **Sizing.** Each position's notional is sign × 40% / σ × equity / N, with N the markets that have a signal that month (MOP eq. 5). Whole contracts, rounded half away from zero, priced at the `_NON` settle × dollars per point.
- **Optional 10% portfolio target.** The whole book is also scaled by 10% / (the trailing one-year realised volatility of the current unscaled book's daily returns), using only sessions up to the decision day.
- **Execution.** Each market trades to its target at its own next session's `_REV` settle.
- **Costs.**
  - Commission: $2.50 per contract per side.
  - Slippage: one tick per contract per side.
  - Rolls: one round trip per open contract on every roll. A roll is any day on which `_REV` minus `_NON` steps, read straight off the files, the same way `markets.py` found its roll months.
- **A file that ends** before the run does (RL in 2008) is flattened on its last bar.
- **The SP contract change** of November 1997 closes and reopens the position at the new size.
- **Reconcile.**
  - The account side is daily variation margin, less costs, with interest as a separate line.
  - The position side is each market's cash-flow ledger of fills, plus the open position at its last settle, less that market's costs.
  - The two must agree to within $0.01.
- **Held-out period.** `--allow-holdout` is required for any `--end` in 2016 or later (ADR 0012), exactly as in `backtest.py`.

## The universe

41 markets, preferring Pinnacle's composite or electronic symbol (Appendix B-1):

- **Equity:** SP, EN, MD, YM, RL, NK.
- **Bonds:** US, TY, FB, TU.
- **Currencies:** AN, BN, CN, JN, SN, FN, MP.
- **Energy:** ZU, ZH, ZB, ZN, BC, BG.
- **Metals:** ZG, ZI, ZK, ZA, ZP.
- **Grains:** ZC, ZS, ZL, ZM, ZW, KW.
- **Livestock:** ZT, ZZ, ZF.
- **Softs:** SB, KC, CC, CT.

Dollars per point is Min $ Move ÷ tick. Two cases where the manual's own columns disagree with the files:
- ZI's BigPoint (5000) is per dollar, not per cent.
- YM's "$10" is CBOT's $5 mini Dow; the price level × $5 gives the real notional.

Excluded (full list and reasons in `tsmom_markets.EXCLUDED`):

- **Non-USD, not converted:** SPI 200, DAX, CAC 40, Euro STOXX 50, STOXX 50, FTSE 100, Hang Seng, Bund, Bobl, Schatz, Long Gilt, Short Sterling, Canadian 10-year.
- **Duplicates of an included market:** day-session and pit versions, ES/SC, ND, DJ/ZD.
- **Baskets:** DX, CRB, GSCI.
- **Thin:** Fed Funds, Minneapolis wheat, oats, rice, orange juice, lumber, milk.
- **ER:** a corrupt in-sample row.
- **The Eurodollar (EC).** MOP trade no short-rate futures. Near the zero bound its ex-ante volatility fell to 0.04% a year, so 40%/σ asked for about 25 times equity in notional (1,719 contracts at $1M). A $1M, 40%-per-market run with it included: CAGR +10.73%, Sharpe 0.89, MaxDD 22.42%.

## In-sample result (2026-09-28)

```sh
python3 research/futures-local/tsmom.py --interest-rates ~/Desktop/Trend_Investing/data/rates/TB3MS.csv
```

1985-01-02 to 2015-12-31, all costs, reconcile OK in all eight runs. The Sharpe ratio is the monthly return in excess of T-bills; without interest the return is already an excess return.

| Run | CAGR | Vol | Sharpe | MaxDD | CAGR/DD | 1985-89 | 1990-99 | 2000-08 | 2009-15 | 2003-15 | 2008 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| $1M, 40%/mkt, no interest | 10.33% | 12.4% | 0.86 | 24.5% | 0.42 | 13.96% | 12.91% | 10.08% | 4.62% | 5.57% | +21.9% |
| $1M, 40%/mkt, interest | 14.19% | 12.5% | 0.85 | 24.4% | 0.58 | 21.23% | 18.60% | 13.37% | 4.62% | 6.82% | +23.4% |
| $1M, 10% target, no interest | 10.98% | 11.3% | 0.98 | 21.5% | 0.51 | 12.89% | 14.08% | 10.26% | 6.28% | 6.57% | +20.4% |
| $1M, 10% target, interest | 15.03% | 11.3% | 0.99 | 16.5% | 0.91 | 20.77% | 20.07% | 13.54% | 6.26% | 7.83% | +21.8% |
| $10M, 40%/mkt, no interest | 10.32% | 12.4% | 0.86 | 24.4% | 0.42 | 13.89% | 12.97% | 10.06% | 4.58% | 5.51% | +21.8% |
| $10M, 40%/mkt, interest | 14.32% | 12.5% | 0.86 | 24.2% | 0.59 | 21.96% | 18.64% | 13.38% | 4.65% | 6.86% | +23.5% |
| $10M, 10% target, no interest | 10.99% | 11.2% | 0.99 | 21.6% | 0.51 | 12.99% | 14.15% | 10.20% | 6.22% | 6.48% | +20.3% |
| $10M, 10% target, interest | 15.03% | 11.3% | 0.99 | 16.5% | 0.91 | 21.04% | 19.90% | 13.55% | 6.30% | 7.85% | +21.9% |

**Against the S&P** (SP_NON, price only):

- The correlation of monthly returns is −0.03 to −0.04.
- In the S&P's 10 worst months, which averaged −12.2%, TSMOM averaged +3.3% to +3.7%. Examples:
  - October 1987: 0%, against −20.4%.
  - October 2008: +6.6%, against −17.1%.
  - August 1998: +8.0%, against −15.1%.

**Markets** ($1M, 40% per market, no interest):

- **Top five:** TU, JN, ZG, ZH, SP, each +$1.3M to +$1.7M.
- **Bottom five:** CC −$1.7M, KC −$0.9M, ZS, SN, ZC.
- **No market dominates.** The largest is 6-7% of all positive market P&L, and every sector is positive.

**Costs** ($1M, 40% per market, no interest): commission $0.13M, slippage $0.60M, and roll $1.99M over 6,051 rolls, against a net P&L of $20.1M. Costs cut the CAGR by about 1.4 points a year: with no costs at $10M it is 11.72%.

**Against MOP.** Gross of every cost, 1985-2009, $10M, 40% per market:

| | This run | MOP |
| --- | --- | --- |
| Sharpe | 1.09 | "1+" gross |
| Volatility | 11.7% | about 12% |
| Correlation with the S&P | −0.11 | low |
| 2008 | +21.9% net ($1M, 40%/mkt) | strongly positive |

Net of costs over the same span, the Sharpe is 0.97. The remaining gap has three sources:
- a smaller, US-only universe: 41 markets, not 58, with no non-USD equity indices or bonds;
- the costs;
- whole-contract rounding, which costs little: the $1M and $10M runs agree to within 0.15 point of CAGR.

**Read.** The edge is real in-sample but decays after 2008:
- 2009-2015 returns only 4.6-6.3% a year without interest.
- Losing years: 2005 (−5.1%), 2006 (−6.5%), 2009 (−7.3%), 2011 (−3.3%) and 2012 (−7.6%), at $1M, 40% per market, no interest. 2013-2015 recovered (+20%, +16%, +14%).

This is consistent with the published post-2009 weakness of trend following. The held-out run (2016 on) has not been done.
