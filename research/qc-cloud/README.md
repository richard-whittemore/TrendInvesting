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
3. **Classification uses QuantConnect's free Morningstar codes, mapped
   onto ADR 0008's two levels by hierarchy depth, not by an exact label
   match.** The Go engine has no classification input even in the
   production system yet (`internal/strategy/unit_caps.go`'s
   `classificationOf` always reports Unclassified there, per ADR 0008's own
   2026-09-24 implementation note), so there is no production behaviour
   this script could match exactly. `main.py`'s `_classification_groups`
   instead reads QuantConnect's own free fine/fundamental universe data:
   Morningstar's Sector code for ADR 0008's "sector" (its loosely
   correlated, 10-Unit level -- the broadest Morningstar level available),
   and Morningstar's Industry Group code (not the finer Industry code) for
   ADR 0008's "industry" (its closely-correlated, 6-Unit level), reasoned
   through in that function's own comment. This is a considered mapping,
   not a verified one: this repository cannot run a QuantConnect backtest
   itself to confirm Morningstar's own Industry Group boundaries actually
   read as "closely correlated" in Faith's sense for every sector. An
   instrument Morningstar has no classification for at all -- code 0, or
   no `AssetClassification` -- falls into `rules.py`'s single, shared
   Unclassified Group, exactly the case ADR 0008 itself describes and
   blesses, capped once at the sector (10-Unit) level and never also
   checked against a separate, tighter industry cap of its own (see
   `rules.UnitCaps`'s own doc comment).
4. **Security-type filtering is best-effort, and unverified until the
   first real run.** `main.py`'s `FineSelectionFunction` filters to
   QuantConnect's own "common stock" Morningstar code
   (`SecurityReference.SecurityType == "ST00000001"`) to approximate ADR
   0009's "common stock on a US primary exchange (no ETFs, ADRs, or
   SPACs)" rule. This repository cannot run a QuantConnect backtest itself
   to verify that filter's exact coverage. **To confirm it on your own
   first run**, check the Logs tab for a line like:

   ```
   research: universe common-stock filter (SecurityType=='ST00000001') kept 187 dropped 340 of 527 fine candidates this month
   ```

   printed once every month the universe refreshes. If "kept" is
   implausibly low (near zero) or implausibly high (equal to the whole
   candidate count, meaning nothing was filtered), or if the run's universe
   otherwise looks wrong (an ETF or ADR clearly present), that is the
   filter to suspect first, and is expected to need a follow-up ticket, not
   a silent assumption that it is already correct.
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
6. **Cash resets every Session; Unit-cap reservations have their own
   per-order lifetime (ADR 0020).** `rules.SessionLedger`'s *cash* side is
   rebuilt fresh every trading day from that day's actual `Portfolio.Cash`
   -- it never carries a stale cash hold forward, and it never needs a
   multi-day cash lifetime of its own, since deviation #2 means no
   proposal here ever survives past its own next Session. Its *Unit-cap*
   side is different, and tracked explicitly:
   `TurtleBaselineResearch.reservations_by_order_id` records exactly which
   order reserved which instrument/industry/sector/total-long headroom at
   placement, and releases it the moment that order resolves with nothing
   filled under it -- cancelled, expired, or refused outright by LEAN
   (`OrderStatus.Invalid`) -- never earlier, and never left standing
   forever. (The one exception, itself narrow and explicit: if something
   HAS already filled under an order LEAN is cancelling -- the anomaly
   deviation #14 describes -- the reservation is deliberately left
   standing for that settlement to decide, rather than released at
   cancellation regardless.) A filled order's reservation converts into a
   real, committed Unit instead, released only when that Unit itself
   later closes (`_handle_unit_exit`). This is not merely "session-scoped":
   it is the
   same per-reservation lifetime discipline ADR 0020 describes for the Go
   engine's own holds, implemented against `rules.UnitCaps` directly
   rather than against a journalled event stream. (An earlier version of
   this script placed an order's reservation and never released it on
   cancellation, so unfilled proposals silently exhausted every cap over
   the life of a run -- caught in PR #253 review, Greptile rules.py:841
   and CodeRabbit main.py:411, both Critical, and fixed before this
   research check was ever run for real.)
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
   ADR defines. `rules.Campaign` accumulates each closed Unit's own
   `(exit price - its own fill price)` into `realized_price_pnl` as every
   Unit closes (`close_units`), and reports the whole Campaign's R
   multiple, once every Unit has closed, as that running total divided by
   the Campaign's 1-Unit initial risk (`r_multiple`:
   `realized_price_pnl / (Stop Multiple x campaign N)`). This is every Unit
   the Campaign ever held, not only the last one to close: comparing only
   the final exit against the Campaign's own entry price -- an earlier
   version of this script did exactly that -- can misreport a Campaign
   that lost money overall as a win, whenever its last Unit happens to
   exit above the original entry while earlier Units were stopped out at a
   loss (caught in PR #253 review, Greptile main.py:649 and CodeRabbit
   main.py:649, with the exact scenario transcribed as
   `test_r_multiple_aggregates_every_unit_not_only_the_last_exit` in
   `test_rules.py`). This aggregate-R convention is a standard,
   widely-used way to compare trades of different sizes, chosen for this
   script's reporting only.
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
    not this script's. It does, narrowly, react to LEAN refusing an entry
    or Add outright (`OrderStatus.Invalid`): that order's Unit-cap
    reservation is released (deviation #6) rather than left standing on a
    trade that never happened. That is bookkeeping hygiene, not
    reconciliation -- it does not compare this script's own state against
    the broker's at any point.
13. **The SPY comparison uses a dividend-adjusted, total-return basis,
    deliberately unlike the strategy's own SplitAdjusted instruments (ADR
    0004).** SPY exists only for the closing summary's buy-and-hold line,
    never as a traded or signalled instrument, so ADR 0004's
    split-adjusted-signals rule does not apply to it. It is subscribed on
    QuantConnect's `DataNormalizationMode.Adjusted` (split AND dividend
    adjusted), and its curve is recorded only from the same Session
    `equity_curve` itself starts recording from (once warm-up ends) --
    both covering exactly the same span, so the two CAGR/max-drawdown
    lines are a fair like-for-like comparison. An earlier version of this
    script priced SPY split-adjusted only (omitting dividends, which
    understates a multi-decade buy-and-hold return) and began its curve
    before warm-up ended (a longer span than the strategy's own) -- both
    caught in PR #253 review (Greptile and CodeRabbit, main.py:384).
14. **A `PartiallyFilled` order is treated as an anomaly, not a routine
    case, and is handled by one small, conservative path -- not by
    machinery that tracks a partial fill's progress.** A Unit is
    indivisible (CONTEXT.md "Unit"), and on daily equity data this is not
    expected to matter in practice: this script's own ADR 0005 buy fill
    model fills an order's whole quantity or none
    (`_stop_limit_buy_fill_price` returns a price or `None`, never a
    partial one), and every Exit Order is a plain stop-market sell, filled
    by LEAN's own native equity fill model, which does the same. So
    `OnOrderEvent` logs a `PartiallyFilled` event once, in full, and takes
    no action on it at all -- the order simply keeps resting. Action is
    taken only once LEAN reports the order fully resolved, `Filled` or
    `Canceled`, using LEAN's own order-ticket totals for the WHOLE order's
    life (`Ticket.QuantityFilled`, `Ticket.AverageFillPrice`) rather than
    this script accumulating anything itself. Three things are then kept
    consistent, deliberately kept small enough to reason about:
    - **An entry or Add that ends up short of the exact quantity it
      requested** (a `Canceled` order that partially filled first) is
      never opened as, or added to, a smaller Unit: it is declined --
      Faith's "no partial Units" (ADR 0010) is treated as the general rule
      here too -- by selling back whatever quantity did trade, releasing
      its Unit-cap reservation, and logging it as an anomaly.
    - **A Unit's own Exit Order settling at anything other than that
      Unit's own full recorded quantity** (or an order this script cannot
      match back to a specific Unit at all) fails closed: `campaign.units`,
      `unit_tickets`, and the Unit caps are left exactly as they were, and
      the anomaly is logged loudly rather than guessed at or silently
      absorbed -- this script has no representation for a Unit smaller
      than a whole one, and it does not invent one under pressure from an
      event it does not expect to see (PR #253 review round 2, CodeRabbit
      main.py:952: "Do not close a Unit on a partial canceled exit").
      This can only follow from a cause outside a backtest this script's
      own code fully controls, since this script never itself cancels a
      Unit's own Exit Order.
    - **A Unit-cap reservation is released exactly once, by exactly one
      code path.** `_cancel_ticket` releases it immediately only when
      NOTHING has filled under the order yet; if something has (the
      anomaly above), the reservation is left standing, and
      `OnOrderEvent`'s own settlement -- not the cancellation -- decides
      whether to commit or release it. Releasing it at cancellation
      regardless of what had already filled was an earlier version's own
      defect: `_commit_reservation` would then have nothing left to
      commit for shares LEAN really had bought, understating every Unit
      cap for the rest of the run (PR #253 review round 2, Greptile
      main.py:726: "Partial fills lose cap reservations").

    Two defects in an earlier, more elaborate version of this handling
    (which accumulated every partial fill's quantity and price itself)
    are also fixed by this simplification: a guard meant to catch "nothing
    filled" treated a Unit's own Exit Order -- always a negative fill
    quantity, since it is a sell -- as no fill at all, so an Exit Order's
    `Filled` event never actually closed its Unit (Greptile main.py:834:
    "Exit fills are discarded"); and neither settlement path compared the
    filled quantity against the quantity actually requested, so a
    partially-filled-then-cancelled entry or Add could still be recorded
    as a whole Unit at the wrong size (Greptile main.py:841 and
    CodeRabbit main.py:875: "partial buys become Units").
15. **An Add fill that arrives with no Campaign left able to take it is
    liquidated immediately, not silently dropped or left to raise.** A
    resting Add order and a Unit's own Exit Order can both be triggered by
    the same wide bar (a range spanning more than the ½N to the Add rung
    plus the distance to a stop). This script cancels any resting Add the
    moment ANY of its Campaign's Units closes, partial or full (not only
    the Campaign's last), but a fill that was already in flight can still
    arrive afterward. If it does, and the Campaign it targeted has since
    fully closed, been Loaded, or been partially stopped (ADR 0012: no
    further Add once partially stopped), the filled shares are sold back
    out with a market order and logged, rather than either being dropped
    (leaving a real LEAN position this script no longer tracks) or passed
    to `rules.Campaign.add_unit`, which raises in exactly this situation
    (caught in PR #253 review, CodeRabbit main.py:612, Critical).
16. **An entry whose own fill would leave a non-positive initial
    Protective Stop is declined, not allowed to abort the backtest.** The
    Turtle Rules p.22 (via `rules.protective_stop_level`) requires
    `entry price - Stop Multiple x N` to be positive; an unusually high N
    relative to a low-priced, highly volatile stock can violate that.
    `_decide_entry` declines such an entry BEFORE placing its order
    (checking the proposed level against 2N), and `OnOrderEvent`'s own
    settlement repeats the same check against the ACTUAL fill price as a
    second line of defence, selling the shares back out immediately if it
    still fails, rather than letting `rules.Campaign`'s own `ValueError`
    propagate out of a fill handler and stop the whole run over one
    instrument (caught in PR #253 review, Greptile main.py:603).
17. **A stock the universe selects only after the algorithm's own start is
    backfilled with QuantConnect's own History, not left permanently
    unable to qualify.** `OnSecuritiesChanged` calls QuantConnect's
    `History` API for `WARMUP_BARS` trailing daily bars (the same figure
    `SetWarmUp` itself uses) the moment a genuinely new symbol is added,
    feeding each historical bar through the identical evaluate-then-advance
    path (`_advance`) a live bar would use, in the same SplitAdjusted view
    (ADR 0004). Before this, a stock the monthly universe refresh only
    began selecting years into a multi-decade run had zero bars of its own
    history and could never clear ADR 0009's 250-bar floor no matter how
    long it then remained selected (caught in PR #253 review, Greptile
    main.py:360). This assumes QuantConnect delivers `OnSecuritiesChanged`
    for a newly added symbol before that same day's own `OnData` bar for
    it -- the ordinary universe-selection ordering, but one this
    repository cannot independently confirm without a live QuantConnect
    run. Every bar this script ever feeds into a symbol's N/channels
    (`_advance`) is stamped with its own date and refuses one dated on or
    before the last it already advanced through, so a History row that
    happens to cover a date the algorithm's own warm-up or live bar
    delivery also covers is skipped rather than counted twice -- which an
    earlier version of this backfill did not guard against, silently
    distorting N and the channels and potentially satisfying the 250-bar
    floor before 250 distinct bars had actually been observed (caught in
    PR #253 review round 2, Greptile main.py:413: "Warm-up bars counted
    twice").

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
  `(2,736 + 1,440 x 3) / 50,000 = 7,056 / 50,000 = 14.1%` of the account --
  a bar the strategy's own after-cost return has to clear before the data
  purchase even breaks even, before any trading cost at all. On a $500,000
  account the identical arithmetic gives `7,056 / 500,000 = 1.41%`, a far
  easier bar. The same formula, run with the account size you actually
  have, is the number to compare against a backtest's own CAGR.
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
