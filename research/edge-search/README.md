# Edge-search research scripts

These are the QuantConnect Cloud scripts behind the "prove an edge"
investigation: the report is `docs/research/edge-search-2026-09.md`, this
folder is the code that produced it, preserved so the numbers can be
reproduced or re-run.

## What this is, and what it is not

**This is not the production Go engine** (`internal/strategy`,
`internal/sizing`, `internal/indicator`, `internal/fills`) and it is not the
Turtle Baseline / Sublime research checks in `research/qc-cloud`,
`research/qc-cloud-sublime`, and `research/qc-cloud-futures` (those have
their own TDD suites, ADR citations, and a Go-engine fidelity check; see
their own READMEs). This folder is a wider, faster, rougher search across
published strategies — ETF trend rules, stock factors, timing effects, risk
parity, and combinations — asking one question before any more money or
engine work is spent: **does anything here beat buy-and-hold, out of
sample?** (`docs/research/edge-search-2026-09.md`, "Purpose".)

Each script was first copied here **unchanged apart from a header comment**
noting its purpose and how it fits the report. Later changes, none to a
trading rule: every script's header gained a provenance tag per rule
(published, adapted, or the project's own; references in the report);
`timing/main.py`'s docstring now describes every mode and its `days_in`
statistic counts signal days in the "Core" and "Boost" modes; and
`timing/build_realistic_boost.py` and `timing/test_timing.py` were added.
If any rule here becomes a candidate for the Go engine, its rules go into
`docs/methodology/Methodology_Analysis.md` with page-level citations first,
as AGENTS.md requires of every declared strategy. These are research
scripts, not production code: no ADR citations and no fidelity check
against the Go engine. They were written before their tests, so they did
not follow TDD; the one strategy that beat buy-and-hold out of sample
(Boost, in `timing/`) now has behavioural tests after the fact (see
"Testing", below), and the others have only a syntax check.

**The QuantConnect scripts cannot be run outside QuantConnect.** Every
algorithm script and template (each `main.py`, `boost/*.py`, `shorting/*.py`)
starts `from AlgorithmImports import *`, a module only QuantConnect's own
cloud environment supplies; it does not exist on this machine or in CI.
Locally each is syntax-checked, and the timing script and the follow-up
templates run against a small stand-in for that module (see "Testing",
below). The builders (`timing/build_realistic_boost.py`,
`build_variants.py`) and the futures studies (`futures/*.py`) are ordinary
local Python.

**No market data is committed to this repository.** These are text files
only; QuantConnect supplies the price and fundamentals history when a
backtest runs on their servers.

## Folders

- `etf-trend/` — `main.py`, `main_oos.py`. Faber's 10-month-SMA asset-class
  rotation and Antonacci's GEM (Global Equities Momentum), on SPY / EFA /
  IEF / VNQ / DBC / AGG / SHY. `main.py` is the in-sample run
  (1998–2015); `main_oos.py` is the identical algorithm with its dates
  moved to the single 2016+ held-out run (`START_DATE`/`MEASURE_FROM`/
  `END_DATE` at the top of the class).
- `factors/` — `main.py`. US stock factor strategies on QuantConnect's
  point-in-time coarse/fine universe: 12-1 momentum, momentum with a
  200-day SPY trend filter, low volatility, quality (gross profit /
  assets), value (earnings yield), momentum+quality, and an equal-weight
  control.
- `timing/` — `main.py`. SPY timing effects: RSI-2 (Connors & Alvarez),
  turn-of-the-month, overnight, the Halloween effect, a levered Faber
  rotation, and two combinations layered on RSI-2 — "Core" (80% SPY / 20%
  RSI-2 sleeve, else SHY) and "Boost" (100% SPY, 150% while RSI-2 signals,
  flat financing charge).
- `riskparity/` — `main.py`. Naive inverse-volatility risk parity across
  SPY / TLT / IEF / GLD / DBC, unlevered or scaled to a volatility target.
- `bh-rsp/` — `main.py`. Buy-and-hold RSP (equal-weight S&P 500), the
  control that isolates the equal-weight effect from the factor tilts in
  `factors/main.py`.

**Follow-up (`docs/research/boost-followup-2026-09.md`):**

- `boost/` — `template.py` (Boost, a constant-leverage control and
  buy-and-hold on one ETF) and `mix_template.py` (fixed-weight blends of
  SPY with managed-futures ETFs, optionally with Boost on the SPY part).
- `shorting/` — `trend_template.py` (a 200-day trend rule going to cash or
  short) and `factors_ls.py` (the factor script with long/short and 130/30
  momentum modes and a borrow fee).
- `build_variants.py` — writes all 49 follow-up variants exactly as run,
  from these templates and a local TB3MS CSV.
- `futures/` — `carry.py` and `blend.py`, local studies on the
  `research/futures-local` engine and the Pinnacle files, which are not in
  the repository.

## How each script is run on QuantConnect Cloud

Each script is a single self-contained `QCAlgorithm`. There is no
`build_upload.py` step for this folder (unlike `research/qc-cloud`): every
file here is well under QuantConnect's Free-plan 32,000-character-per-file
limit, so it is pasted as-is.

1. Sign in at <https://www.quantconnect.com> (Free/Researcher plan, no
   card required).
2. **Algorithm Lab → Create New Algorithm**, Python, any project name.
3. Select all the text in the project's default `main.py` and replace it
   with the contents of the script you want to run (e.g.
   `research/edge-search/timing/main.py`).
4. Set the class constants at the top of the file for the run you want
   (below), save, and click **Backtest**.
5. Read the results from the backtest's own **statistics panel**
   (`SetRuntimeStatistic` keys: `OVERALL`, one key per named period, and
   the matching `SPY ...` buy-and-hold line), not only the Logs tab, which
   QuantConnect's Free plan caps at 10 KB and can truncate on a long run.

### The switches, per folder

- **`etf-trend/main.py` / `main_oos.py`** — `MODE`: `"faber"` or `"gem"`.
  `main.py` runs 1998-01-01 to 2015-12-31 (in-sample); `main_oos.py` runs
  2014-11-01 to 2026-06-26 with `MEASURE_FROM = 2016-01-01` (warm-up months
  before the held-out period, so the first 2016 decision has full 10/12-
  month lookbacks; statistics are measured only from `MEASURE_FROM`).
- **`factors/main.py`** — `MODE`: `"mom"`, `"mom_trend"`, `"lowvol"`,
  `"quality"`, `"value"`, `"mom_qual"`, or `"ew"` (the equal-weight
  control). `UNIVERSE_SIZE` (default 500) and `TOP_N` (default 50) set the
  universe and how many names are held. `START_DATE`/`END_DATE` set the
  span; the committed file runs 1998–2015 in-sample, measured from 1999 (a
  year of warm-up for the 12-month lookback).
- **`timing/main.py`** — `MODE`: `"rsi2"`, `"tom"` (turn of the month),
  `"overnight"`, `"halloween"`, `"faber_lev"` (levered Faber rotation),
  `"core_rsi2"` (the "Core" combination), or `"boost"` (the "Boost"
  combination). `RSI_ENTRY`, `RSI_EXIT`, `RSI_TREND_FILTER`, `RSI_SIZE`
  tune the RSI-2 rule for the robustness checks (entry threshold, exit
  rule, trend filter on/off, leverage). `LEVERAGE` and `FINANCING_RATE` set
  the levered Faber and "Boost" financing cost — `FINANCING_RATE` here is
  the committed script's flat annual rate (3%). The "Boost re-test with
  realistic margin rates" in `docs/research/edge-search-2026-09.md`
  replaced that flat rate with the monthly FRED TB3MS (3-month T-bill)
  rate plus 1.5 points, charged daily; that variant (with the rate table
  embedded) is **not** committed here, since it would mean committing the
  FRED rate data alongside it — only this flat-rate `boost` mode is
  preserved. `timing/build_realistic_boost.py` regenerates it from a local
  TB3MS CSV. By default it charges each month that month's own average
  T-bill rate from the month's first day (a small look-ahead, and what the
  reported runs used); `--prior-month` charges the previous month's
  average instead, which uses only data available at the time. `START_DATE`/`END_DATE`/`MEASURE_FROM` set the
  span; the committed file runs 1998–2015 in-sample, measured from 1999.
- **`riskparity/main.py`** — `TARGET_VOL`: `None` for the unlevered run, or
  a fraction (e.g. `0.10`) to scale the book to that annualised volatility,
  capped at `MAX_LEVERAGE`. `VOL_DAYS` sets the trailing window.
  `START_DATE`/`END_DATE`/`MEASURE_FROM` set the span; the committed file
  runs 2005–2015 in-sample, measured from 2006.
- **`bh-rsp/main.py`** — no switches; a fixed 2003-05-01 to 2015-12-31 RSP
  vs. SPY buy-and-hold comparison.

### Protocol

The full protocol — why the study is split into in-sample, robustness, and
a single holdout run, and what it means for a holdout to be "spent" — is in
`docs/research/edge-search-2026-09.md` ("Protocol"). In short:

1. **In-sample (1998/1999–2015).** Every `MODE` and every robustness
   variant (different `RSI_ENTRY`, `TARGET_VOL`, universe size, and so on)
   is tried and compared freely against this span. This is where a
   strategy is allowed to be tuned.
2. **Robustness.** A result is trusted more if nearby settings (a
   slightly different entry threshold, a different lookback) give similar
   answers, and trusted less if it only works at one exact setting.
3. **A single 2016+ holdout run, with fixed settings.** Once a
   configuration is chosen from step 1–2, it is run once, unchanged, on
   the 2016-to-present span it was never tuned against
   (`etf-trend/main_oos.py` is the pattern; the other folders' 2016+ runs
   moved `START_DATE`/`MEASURE_FROM`/`END_DATE` the same way for their own
   held-out runs). Going back and re-tuning against the holdout after
   seeing its result "spends" it — the whole point of holding it out is
   that it stops being an honest out-of-sample test the moment it
   influences a choice, which is why the report calls out where the
   holdout has already been looked at more than once (the "Boost" result).

## Testing

`make research-test` covers this folder in two ways:

- **Every script is syntax-checked** (`python3 -m py_compile`). None can be
  run locally, since each imports `AlgorithmImports`, which only
  QuantConnect's cloud supplies. (`research/qc-cloud` and its siblings
  avoid this by keeping their rules in a standard-library-only `rules.py`;
  these one-off searches do not.)
- **`timing/test_timing.py`** covers the Boost strategy and the
  realistic-margin builder. It installs a minimal stand-in for
  `AlgorithmImports` (only the names `main.py` uses, and an algorithm that
  records orders rather than trading), then drives `OnData` with synthetic
  values. It tests the Boost entry, hold and exit, the 200-day filter, the
  daily financing charge, RSI-2 entry and exit with SHY parking, the CAGR
  and drawdown statistics, and the builder's rate table, its
  `--prior-month` shift, the T-bill-plus-spread charge, and the refusal to
  build a `--prior-month` table without December 1997.

- **`test_followup.py`** covers the follow-up code: `build_variants.py`
  (the rate table, the settings, every variant building to valid Python),
  the templates' daily rules against the same kind of stand-in (Boost
  on/off, monthly constant leverage, the margin charge, blends, going short
  with a borrow fee), and the carry measure in `futures/carry.py`.

QuantConnect Cloud's own compile step, on the first paste of each script,
is the real test of its QuantConnect API calls.

Results and caveats: `docs/research/edge-search-2026-09.md` and
`docs/research/boost-followup-2026-09.md`.
