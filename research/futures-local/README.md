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
| `loader.py` | Reads one Pinnacle CLC text or CSV file. It detects the delimiter, header and date format, and rejects bad rows loudly. |
| `markets.py` | Faith's portfolio [T p.10-11], with each market's multiplier, tick size, correlation groups, roll months and the fields still TO-VERIFY. |
| `backtest.py` | The event loop, costs, metrics, reconcile check and command line. |
| `test_*.py` | Unit tests on synthetic fixtures. They run in `make research-test`. |

## How to run it

1. Put Pinnacle's back-adjusted files in `~/Desktop/Trend_Investing/data/pinnacle/`, one file per market.
2. Work through "Verify the real files first" below.
3. Run the in-sample default, 1980 to 2015:

   ```sh
   python3 research/futures-local/backtest.py
   ```

Useful flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--data-dir` | `~/Desktop/Trend_Investing/data/pinnacle` | Where the files are. |
| `--start`, `--end` | `1980-01-01`, `2015-12-31` | The span. Bars before `--start` warm up N, the channels and Strength, but trade nothing. |
| `--allow-holdout` | off | **Required** for any `--end` in 2016 or later. Without it the run refuses to start (exit code 2). |
| `--markets` | all in `markets.py` | A comma-separated list, e.g. `GC,SI,CL`. |
| `--file-template` | `{stem}` | The file name without extension. `{stem}` and `{symbol}` are substituted, e.g. `{stem}_B` for a suffix. |
| `--commission` | `2.50` | Dollars per contract per side. |
| `--slippage-n` | `0.05` | Slippage per fill, in N (ADR 0013). |
| `--unit-fraction` | `0.01` | Faith's 1%. Use `0.005` for the equity Baseline's figure. |
| `--equity` | `1000000` | Starting equity. |
| `--long-only` | off | Skip every short breakout. |
| `--no-roll-costs` | off | Don't charge rolls. |

A market with no file is skipped with a warning; the run continues. The report gives:

- CAGR, max drawdown, CAGR ÷ max drawdown, and CAGR per decade;
- the number of Campaigns, win rate, and average win and loss in R;
- P&L per market, net of costs, with open Campaigns marked to the last settle;
- total commission, and total roll cost with the number of rolls;
- decline counts;
- a `Reconcile` line.

The exit code is 1 if the reconcile check fails.

## Verify the real files first

The data hadn't arrived when this was written, so the file layout is unknown. Check each item on one or two real files before trusting a result.

1. **Column order.** `loader.HEADERLESS_COLUMNS` assumes Date, Open, High, Low, Settle, Volume, Open Interest, Pinnacle's historical CLC order. If the files have a header row, `HEADER_ALIASES` maps it instead. Check which of Pinnacle's fields (Open Interest, Total Volume, Total Open Interest) sits in which column. If the order differs, change that one line.
2. **Date format.** The loader accepts YYYYMMDD, YYYY-MM-DD, MM/DD/YYYY and MM/DD/YY. Two-digit years of 50 and above are 19xx, and below 50 are 20xx (`CENTURY_PIVOT`). It refuses to guess a six-digit YYMMDD or MMDDYY date; pass `date_format` if the files use one.
3. **File names.** Every `file_stem` in `markets.py` is a placeholder, the market's own short code. Pinnacle may add a suffix for the adjustment type; `--file-template` handles that without editing the table.
4. **Price units: the most important check.** For each market, compare one day's settle with the exchange's quote for the same contract. The multiplier assumes these units:
   - US and TY in decimal points, not 32nds;
   - JY in dollars per yen, not per 100 yen;
   - SI in dollars per ounce, not cents;
   - HG, HO and HU in dollars, not cents;
   - KC, SB and CT in cents per pound.

   A units mismatch scales every Unit size and every dollar of P&L by the same factor. `loader.classify_price_scale(symbol, price)` and `loader.check_series_scale(symbol, bars)` automate a first pass at this: they compare one real settle against the plausible 1980-2015 range for `markets.py`'s assumed unit and, where `markets.py`'s own comment names one, the alternate unit (`loader.PRICE_SCALE_HINTS`). `backtest.py`'s command line runs this automatically on each hinted market's last loaded bar and prints a warning -- never an error -- if it looks like the alternate unit or neither. It only flags a mismatch; it does not resolve one.
5. **Back-adjustment.** Use the *back-adjusted* (additive) series, not ratio-adjusted or unadjusted. Negative prices are expected in it and are accepted.
6. **Settle outside the day's range.** The loader rejects an open or settle outside high-low. If Pinnacle's files contain such rows, look at how many before deciding how to treat them; the loader never repairs data silently.
7. **Roll dates.** If Pinnacle documents its own roll schedule, replace the approximate `roll_months` and `roll_day` in `markets.py` with it.

## TO-VERIFY

Every market's `file_stem` and `roll_months` are unverified -- Pinnacle's own file names and roll dates need the real data, not exchange research. A pass against CME Group, ICE, CBOT and Federal Reserve sources settled most of the rest; `markets.py`'s per-market comments cite the source for each. What's left, and why:

| Market | Still unverified | Why |
| --- | --- | --- |
| US, TY | price units | Exchange convention (32nds) is confirmed and cited, but whether Pinnacle's file uses it or plain decimal is Pinnacle's own choice, not the exchange's -- needs a real bar. |
| TB | trade window | Confirmed to have traded well past Faith's era at very low volume, but no source gives the exact year it stopped, or whether Pinnacle carries it at all. |
| FR | multiplier, tick size, price units | Genuinely unconfirmed: no CME rulebook chapter or spec page for the historical French franc contract could be found (CME Group's site blocks automated fetches; third-party archives cover only currencies still traded today). |
| BP | multiplier | The GBP 25,000 early contract size, and when it changed to 62,500, could not be sourced. |
| JY, SI, HO, HU | price units | Same Pinnacle-scaling caveat as US/TY (per yen vs. per 100 yen; dollars vs. cents). |
| HG | price units, tick size | Same caveat; COMEX's own quote convention for copper is cents per lb (unlike gold and silver), so this one needs the real data more than most. |
| HU | trade window | Whether Pinnacle's file splices unleaded gas and its 2005-2006 RBOB successor into one series, or covers only one, is unknown. |

`loader.classify_price_scale` / `loader.check_series_scale` (above, "Price units") automate the price-unit checks once the files arrive; nothing here can be resolved further without them.

`python3 -c "import markets; print('\n'.join(markets.to_verify_report()))"`, run in this folder, prints the same list from the table itself.

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

The roll months approximate each contract's delivery cycle, not Pinnacle's actual roll dates (TO-VERIFY):
- quarterly for financials and currencies;
- six times a year for gold;
- five times for silver, copper, coffee and cocoa;
- four times for sugar and cotton;
- monthly for energy.

**P&L** is points × multiplier throughout. No percentage is ever taken from a back-adjusted price level, which can be zero or negative. A market's multiplier on the entry date is frozen for its Campaign, like N and the Unit size (ADR 0006). This matters only for the S&P 500's 1997 change.

**Reconcile.** The account side is daily variation margin, less commission and roll costs. The Campaign side is `rules.Campaign`'s own fill-to-exit price distance × Unit size × multiplier, less the same costs, plus open Campaigns marked to the last settle. The two are computed independently and must agree to within $0.01.

**Tradable window.** `markets.tradable` stops new Campaigns in the Deutschmark and French franc after 1998, and in the euro before 1999. Open Campaigns are never closed for leaving the window (CONTEXT.md, "Eligible").

**Ruin.** If equity reaches zero, the run stops and the report says so.

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

`test_loader.py` covers delimiters, headers, every date format and the century rule, and every rejection.

`test_markets.py` checks the table's integrity and its agreement with `main.py`'s correlation groups.

`test_backtest.py` covers:
- a hand-computed long Campaign: entry, Add, stop-out, P&L and R;
- gap-through-stop fills on both sides;
- the price cap;
- an Exit-Channel exit;
- the roll cost;
- no trading before the start date;
- the reconcile invariant on a three-market synthetic run;
- the holdout refusal, in both the API and the command line.
