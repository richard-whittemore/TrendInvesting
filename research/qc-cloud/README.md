# Baseline Turtle research check, on QuantConnect Cloud (Free plan)

## What this is, and what it is not

This folder is a **free, research-only** check of one question: do the
Baseline Turtle rules make money on US stocks, after realistic costs, over
QuantConnect's free 1998-to-date US equity history? It answers that question
**before** spending anything on the market data this repository's production
Go engine would otherwise need to backtest locally (see issue #41: about
$2,736 once, plus $1,440 a year).

This is **not** the production Go engine (`internal/strategy`,
`internal/sizing`, `internal/indicator`, `internal/fills`). It is a separate,
much simpler Python re-implementation of the same rules, built so it can run
entirely inside QuantConnect's browser-based IDE, on the Free plan, with no
locally-installed tools and no API key. It makes no claim of bit-for-bit
parity with the Go engine. Every place the two disagree is listed under
"Deviations" below, and none of them is hidden.

**No market data is committed to this repository.** This folder is four
text files; QuantConnect supplies the price history when you run a backtest
on their servers.

## Files

- `rules.py` -- the rule core. Pure Python, standard library only, **no
  QuantConnect imports**. Every function or class cites the ADR (in
  `docs/adr/`) or the `docs/methodology/Methodology_Analysis.md` section it
  transcribes.
- `test_rules.py` -- unit tests for `rules.py`, on synthetic numbers and a
  handful of worked examples transcribed directly from
  `Methodology_Analysis.md` (which itself cites *The Original Turtle Trading
  Rules*, Curtis Faith, 2003, by page number). Run with
  `python3 -m unittest discover -s research/qc-cloud -p "test_*.py"`, or via
  `make research-test` from the repository root.
- `main.py` -- the thin QuantConnect algorithm (`QCAlgorithm` subclass) that
  drives `rules.py` over a broad, point-in-time US equity universe.
- `README.md` -- this file.

## How to run it on QuantConnect (step by step, for a non-programmer)

1. Go to <https://www.quantconnect.com> and sign in (or create a free
   account -- no card required for the Free/Researcher plan).
2. In the left sidebar, open **Algorithm Lab**, then click **Create New
   Algorithm** (sometimes labelled **+ New Project**).
3. Choose **Python** as the language, and give the project a name, e.g.
   `turtle-baseline-research`.
4. QuantConnect creates the project with one file, usually called
   `main.py`, already open, containing a default example strategy. You are
   going to replace its contents and add a second file.
5. **Replace `main.py`:**
   - Select all the text already in `main.py` (click inside the editor,
     then Ctrl+A / Cmd+A) and delete it.
   - Open this repository's `research/qc-cloud/main.py` in any text editor,
     select all its text, copy it, and paste it into QuantConnect's
     `main.py` editor.
6. **Add `rules.py` as a second file:**
   - In QuantConnect's file panel (usually on the left of the code editor,
     above or beside `main.py`), find the button to add a new file --
     usually a small **+** icon or a right-click menu offering **New
     File**.
   - Name the new file exactly `rules.py`.
   - Open this repository's `research/qc-cloud/rules.py`, select all,
     copy, and paste its contents into the new file.
   - You do **not** need to add `test_rules.py` or `README.md` to the
     QuantConnect project -- they exist for testing and reading on your own
     computer, not for the backtest itself.
7. Save the project (usually Ctrl+S / Cmd+S, or an explicit **Save**
   button).
8. Click **Backtest** (or **Build** then **Backtest** -- QuantConnect's
   button layout changes from time to time; look for a button that starts a
   backtest, sometimes shown as a play-button icon).
9. QuantConnect compiles the project and runs it. This scans roughly 200
   stocks a day over nearly three decades, so it can take several minutes;
   let it run.
10. If QuantConnect reports a compile error, it is almost certainly a
    paste problem (a missing line, or the two files' contents merged into
    one) -- re-copy the two files exactly as they are in this repository,
    with nothing added or removed, and try again. If an error persists,
    save the exact error text and bring it back to this project's tracker;
    do not try to "fix" the strategy code yourself, since the error is more
    likely a QuantConnect API detail than a rule mistake.

## How to find the results

QuantConnect shows an equity curve and standard statistics automatically.
The numbers this issue asked for are printed as **log lines**, in the
**Logs** tab (sometimes called **Log** or shown as a console icon) once the
backtest finishes. Look for lines starting `research:` -- for example:

```
research: OVERALL: start=1998-01-02 end=2026-09-01 CAGR=0.0842 max_drawdown=0.31 CAGR/MaxDD=0.27
research: REGIME 2008-09 crash: start=2008-01-02 end=2009-12-31 CAGR=-0.05 max_drawdown=0.22 CAGR/MaxDD=-0.23
research: campaigns=1842 win_rate=0.38 avg_win_R=1.9 avg_loss_R=-0.6
research: total_commission=48213.10
research: SPY BUY-AND-HOLD: start=1998-01-02 end=2026-09-01 CAGR=0.081 max_drawdown=0.55 CAGR/MaxDD=0.147
```

(The numbers above are illustrative formatting only -- they are not a
prediction of what your own run will show.)

You should see, in order:

1. One `OVERALL` line: CAGR, max drawdown, and CAGR ÷ max drawdown over the
   whole run (ADR 0012's primary metric).
2. One `REGIME ...` line per named Regime Window (ADR 0012): 1998-2000 late
   bull, 2000-02 bear, 2003-07 bull, 2008-09 crash, 2009-19 bull, 2020
   COVID, 2022 correction. A window with fewer than two equity marks (for
   example, if the backtest failed before reaching it) prints `no-data`
   instead of numbers, rather than a misleading zero.
3. One `campaigns=...` line: how many Campaigns closed, the fraction that
   were winners, and the average win and average loss, each expressed in
   "R" -- multiples of the Campaign's own initial 1-Unit risk (Stop
   Multiple × N). R is the standard way to compare trades of different
   sizes; it is this script's own reporting convention, not itself a
   quantity any ADR names.
4. One `total_commission=...` line: total commission paid over the run,
   in dollars, as charged by LEAN's own `InteractiveBrokersFeeModel`.
5. One `SPY BUY-AND-HOLD` line, in the same format as `OVERALL`, so the
   Baseline's own CAGR/drawdown can be read next to simply buying and
   holding the S&P 500 ETF over the identical span.

## Universe size

`TurtleBaselineResearch.UNIVERSE_SIZE` (near the top of `main.py`) is **200**:
each month, the algorithm keeps the top 200 Baseline-eligible stocks by
20-day dollar volume (ADR 0009's eligibility test still applies within that
200; the 200 is a pre-filter on top of it, not a replacement for it).

**Why 200, and what it costs.** QuantConnect's Free plan enforces backtest
node limits (CPU time and memory) shared across every free user; a universe
that is too large, run over almost thirty years of daily history, is liable
to be slow or to be cut off before it finishes. 200 is a deliberately
conservative starting point that comfortably fits within the Free plan's
node limits in early testing, while still being large enough that the
Baseline's own Unit caps (4 per instrument, 6 per industry, 10 per sector,
12 total long -- ADR 0008) are the thing that actually limits how many
Campaigns can be open at once, not the universe size itself. A materially
smaller universe (say, 50) would understate how the Baseline performs on a
genuinely broad market, since it would mostly hold only the very largest,
most liquid names; a materially larger one (1,000+) risks the backtest
timing out or being throttled on the Free plan before it reaches the
present day. **If your own backtest times out or is throttled, lower
`UNIVERSE_SIZE`** (100 is a reasonable next step) and re-run; if it finishes
comfortably with time to spare, raising it (to 300 or 500) gives a broader,
more representative universe at the cost of a longer run.

## Deviations from the Go Baseline

Every one of these is a place the free cloud environment, or this script's
own deliberate simplicity, differs from `internal/strategy`'s reducer. None
is hidden; each names the ADR it touches.

1. **Entry timing, one bar later (ADR 0005).** The Go engine can propose
   an entry or Add and see it fill within the *same* bar that raised the
   proposal (The Turtle Rules p.19-20: "all four could be added in one
   day"). This script decides at the close of one Session and can only
   place the resulting order for QuantConnect to evaluate against the
   *next* Session's bar, because QuantConnect delivers one full day's data
   per `OnData` call. This is the identical, already-documented deviation
   `adapter/lean/orders.py`'s own `fill_model_report` records for the
   production LEAN adapter ("entry timing: the engine proposes an entry or
   Add at a Session's close, and LEAN can fill it only in the next
   session"). It very occasionally delays or entirely skips an
   entry/Add that a same-bar chain would have taken.
2. **Proposal expiry is one bar, uniformly (ADR 0011).** The Go engine
   gives a fill-chained Add two Sessions to fill rather than one, because a
   live adapter must relay fills asynchronously. This script never
   distinguishes a fill-chained Add from an ordinary one (it never attempts
   the same-bar chaining #1 describes in the first place), so every entry
   and Add proposal gets exactly one Session to fill before it expires --
   the ordinary-case rule, applied uniformly.
3. **No shared, provider-backed corporate-actions or classification
   feed.** The Go engine reads point-in-time industry/sector labels from a
   dedicated classification input (ADR 0008's "classification seam",
   `internal/strategy/unit_caps.go`), which does not exist yet even in the
   production system (`classificationOf` always reports Unclassified there
   too, per ADR 0008's own 2026-09-24 implementation note). This script
   makes the identical, ADR-0008-endorsed choice deliberately and by
   design: it never attempts to read QuantConnect Fundamental data's own
   sector/industry classification, so **every instrument is Unclassified
   throughout**, and the industry (6) and sector (10) caps collapse onto
   the single shared Unclassified Group's own 10-Unit ceiling (ADR 0008:
   "unclassified names are treated as correlated ... capped at the
   loosely-correlated level"). This is the same fail-safe state the
   production engine is in today, not a shortcut invented for this script.
4. **Security-type filtering is best-effort.** `main.py`'s
   `FineSelectionFunction` filters to QuantConnect's own "common stock"
   Morningstar code (`SecurityReference.SecurityType == "ST00000001"`) to
   approximate ADR 0009's "common stock on a US primary exchange (no ETFs,
   ADRs, or SPACs)" rule. This repository cannot run a QuantConnect backtest
   itself to verify that filter's exact coverage; if a run's universe looks
   wrong (an ETF or ADR clearly present), that is the first thing to
   check, and is expected to need a follow-up ticket, not a silent
   assumption that it is already correct.
5. **No 20-day-median-dollar-volume universe eligibility inside the coarse
   filter.** ADR 0009's $5M 20-day median dollar volume test needs 20
   Sessions of an instrument's own history, which `main.py` only has once
   the instrument has traded inside the algorithm for 20 Sessions. The
   monthly coarse/fine universe selection (`CoarseSelectionFunction`)
   instead pre-filters on QuantConnect's own coarse `DollarVolume` (a
   single-day figure, not a 20-day median); the *entry* decision itself
   (`_decide_entry`) then re-applies the exact ADR 0009 test, including the
   proper 20-day median, using this script's own rolling window, and
   declines an entry that does not clear it. So the coarse universe filter
   is a size/liquidity pre-filter only; the eligibility rule that actually
   gates a new Campaign is ADR 0009's own, computed exactly as `rules.py`
   states it.
6. **Cash and Unit-cap reservation resets every Session, not continuously
   (ADR 0020).** The Go engine's holds are released individually, exactly
   when their own proposal fills, expires, or is cancelled, so a hold from
   three days ago can still be standing today. This script's
   `rules.SessionLedger` is rebuilt fresh every trading day from that day's
   actual `Portfolio.Cash` and the Unit caps' own running totals (which
   persist correctly across days), so it never carries a *stale* hold
   forward, but it also never models a hold with a multi-day lifetime of
   its own -- which cannot arise here anyway, since deviation #2 means no
   proposal here ever survives past its own next Session.
7. **Approximate commission for the affordability check, real commission
   for the trade.** `rules.commission_estimate` (ADR 0013) estimates
   commission for the pre-trade cash check using the IBKR Pro Fixed
   schedule `cmd/backtest/testdata/configuration.json` states (per-share
   rate, $1 minimum, 1% cap). The commission actually *charged*, on every
   real fill, is QuantConnect's own `InteractiveBrokersFeeModel` --
   observed by this repository's own LEAN adapter to differ from that
   schedule (a $1.00 minimum per order versus the same $1.00 minimum but a
   0.5%-of-trade-value cap rather than 1%; see
   `adapter/lean/orders.py`'s own `fill_model_report`, "commission"). The
   `total_commission` the closing summary reports is the real, charged
   figure, never the estimate.
8. **R-multiple accounting is this script's own convention.** "Campaign
   count, win rate, average win and loss in R" is not itself a quantity any
   ADR defines; `main.py` computes each closed Unit's R multiple as
   `(exit price - Campaign entry price) / (Stop Multiple x campaign N)` and
   reports it per Campaign (a Campaign closes, for this purpose, the moment
   its last Unit is sold). This is a standard, widely-used convention for
   comparing trades of different sizes, chosen for this script's reporting
   only.
9. **No corporate-action handling beyond what QuantConnect's own
   split-adjusted data already neutralises.** ADR 0023 (a split's cash in
   lieu) and ADR 0024 (symbol changes, dividends as cash events) exist in
   the Go engine as explicit, journalled corporate-action facts. This
   script relies entirely on QuantConnect's own `SplitAdjusted` data
   normalisation to neutralise splits in the price series it reads (exactly
   as `adapter/lean/algorithm.py`'s own subscription does, for the same
   reason -- ADR 0004), and on QuantConnect crediting real dividend cash
   into the account automatically for a long equity holding. Cash in lieu
   of a fractional fill and any need to resize a resting Exit Order at the
   moment of a split are not separately modelled; they are expected to be
   small and infrequent enough not to change the overall research
   conclusion, but they are not proven to be.
10. **A daily, single-institution research view of "previous close"
    cash.** ADR 0020's own cash ledger tracks fill debits and standing
    holds explicitly, event by event, and is provably deterministic under
    replay. This script instead reads `self.Portfolio.Cash` directly from
    QuantConnect at the moment `OnData` is called for a Session -- which,
    per QuantConnect's own documented ordering (and this repository's own
    LEAN adapter's observation of it: "LEAN reports a session's fills
    before it delivers that session's bar"), already reflects that
    Session's own fills but none of this Session's about-to-be-decided
    orders. This is the same basis ADR 0010/0020 describe, read from
    QuantConnect's own account state rather than rebuilt from first
    principles.
11. **No Watchlist, no Tier B (ADR 0011).** The Go engine tracks every
    Eligible instrument's Tier (B: approaching entry; A: entry condition
    met) as a first-class, always-on observable. This script only ever
    evaluates whether today's bar itself is a Tier-A breakout; it keeps no
    separate Watchlist and reports no Tier B state, since neither changes
    which Campaigns are opened.
12. **Order-lifecycle and reconciliation halting (ADR 0019, ADR 0022) are
    not implemented.** The Go engine treats an unexplained difference
    between its own state and the broker's as a halting condition. This
    script trusts QuantConnect's own backtest simulator throughout a run
    and does not attempt to detect or halt on a state mismatch; a
    backtest's own internal consistency is QuantConnect's responsibility,
    not this script's.

## Reading the results against costs, in plain words

**This is not personal financial advice, and nothing below is a
recommendation to trade, or not to trade, any strategy.** It is only a way
to compare a backtest's own numbers with the two costs that matter before
spending any money: the cost of the market data (if you outgrow
QuantConnect's free plan) and the cost of trading it.

**The arithmetic, in general form.** Whatever a backtest reports, compare it
against costs as a *percentage of the account size you would actually
trade with*, not as a raw dollar figure:

- **Data cost as a percentage of account size** = (one-time data cost +
  annual data cost × number of years you expect to use it) ÷ account size.
  Issue #41 gives this repository's own reference figures for buying full
  US equity history locally: about $2,736 once, plus $1,440 a year. On a
  $50,000 account, three years of use, that is
  `(2,736 + 1,440 x 3) / 50,000 = 14.4%` of the account -- a bar the
  strategy's own after-cost return has to clear before the data purchase
  even breaks even, before any trading cost at all. On a $500,000 account
  the identical arithmetic gives `1.44%`, a far easier bar. The same
  formula, run with the account size you actually have, is the number to
  compare against a backtest's own CAGR.
- **Trading cost as a percentage of account size** = total commission
  reported by the run (`total_commission`, this script's own log line) ÷
  starting equity, then annualised by the number of years the backtest
  covers if you want a rate rather than a lifetime total. QuantConnect's
  own free 1998-to-date backtest already prices this in: `total_commission`
  is a real cost the equity curve has already paid, not something to add on
  top of the reported CAGR -- but it is worth looking at on its own,
  because a strategy that trades often on a small account can lose a
  meaningful share of its return to commission alone, even when the equity
  curve still ends up net positive.
- **Putting the two together.** A strategy's own reported CAGR, after both
  of the above have already been paid out of it (trading cost is already
  inside the backtest's CAGR; data cost is not, since QuantConnect's own
  free data was used here), is what is actually available to compare
  against any other use of the same money -- and the honest comparison
  point is what the SPY buy-and-hold line in the same summary reports over
  the identical span, since that is the cost- and effort-free alternative
  the strategy would have to beat to be worth the trouble at all.

**What "no Variant wins" (ADR 0012) would mean here.** This script does
not tune anything -- there is exactly one configuration, the Baseline, and
this research check either shows it clearing its own costs on this
particular universe and span or it does not. Either answer is useful,
cheaply learned, and is exactly why this check exists before any market
data is purchased.
