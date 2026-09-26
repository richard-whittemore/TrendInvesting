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

**No market data is committed to this repository.** This folder is text
files only; QuantConnect supplies the price history when you run a backtest
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
- `build_upload.py` -- writes stripped copies of `main.py`/`rules.py` (every
  module/class/function docstring and comment removed, via `ast`) to
  `dist/`, the two files that actually fit QuantConnect's Free-plan
  32,000-character-per-file limit. See "How to run it", below. Standard
  library only; run with `python3 research/qc-cloud/build_upload.py`.
- `dist/` -- `build_upload.py`'s output. Git-ignored: a build artifact,
  regenerated on demand, never the source of truth for comments or tests.
- `test_build_upload.py` -- checks that both stripped files stay under the
  32,000-character limit, that both compile, and that `test_rules.py`'s own
  full suite still passes against the stripped `rules.py`. Also run by
  `make research-test`.
- `fidelity/` -- the local check that this algorithm reproduces the Go
  engine's trades (see "Checking fidelity against the Go engine", below):
  `lean_main.py`, a LEAN entry point that runs `main.py` unchanged on one
  instrument; `run_local.sh`, which runs it on local LEAN; and
  `compare_fills.py`, which diffs the run's fills against a Go engine
  journal. Standard library only; not part of the upload.
- `test_compare_fills.py` -- unit tests for `compare_fills.py`'s matching,
  on synthetic fills. Also run by `make research-test`.
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
5. **Build the two files you will actually paste.** QuantConnect's Free
   plan caps each file at 32,000 characters; `main.py` and `rules.py`, as
   committed, are both over that once every comment and docstring is
   counted. From the repository root, run:

   ```
   python3 research/qc-cloud/build_upload.py
   ```

   This writes stripped copies -- comments and docstrings removed, the
   rules and behaviour identical -- to `research/qc-cloud/dist/main.py` and
   `research/qc-cloud/dist/rules.py`, and fails with a clear error if either
   one is still at or over the limit. **Paste the two files from `dist/`,
   never the originals** -- QuantConnect will reject (or silently truncate)
   a file at or over 32,000 characters.
6. **Replace `main.py`:**
   - Select all the text already in `main.py` (click inside the editor,
     then Ctrl+A / Cmd+A) and delete it.
   - Open this repository's `research/qc-cloud/dist/main.py` in any text
     editor, select all its text, copy it, and paste it into QuantConnect's
     `main.py` editor.
7. **Add `rules.py` as a second file:**
   - In QuantConnect's file panel (usually on the left of the code editor,
     above or beside `main.py`), find the button to add a new file --
     usually a small **+** icon or a right-click menu offering **New
     File**.
   - Name the new file exactly `rules.py`.
   - Open this repository's `research/qc-cloud/dist/rules.py`, select all,
     copy, and paste its contents into the new file.
   - You do **not** need to add `test_rules.py`, `build_upload.py`, or
     `README.md` to the QuantConnect project -- they exist for building the
     upload copies and for testing and reading on your own computer, not
     for the backtest itself.
8. Save the project (usually Ctrl+S / Cmd+S, or an explicit **Save**
   button).
9. Click **Backtest** (or **Build** then **Backtest** -- QuantConnect's
   button layout changes from time to time; look for a button that starts a
   backtest, sometimes shown as a play-button icon).
10. QuantConnect compiles the project and runs it. This scans roughly 200
    stocks a day over nearly three decades, so it can take several minutes;
    let it run.
11. If QuantConnect reports a compile error, it is almost certainly a
    paste problem (a missing line, or the two files' contents merged into
    one) -- re-run `build_upload.py` and re-copy the two files from `dist/`
    exactly as it produced them, with nothing added or removed, and try
    again. If an error persists, save the exact error text and bring it
    back to this project's tracker; do not try to "fix" the strategy code
    yourself, since the error is more likely a QuantConnect API detail than
    a rule mistake.

## How to find the results

**Read the results from the backtest's own statistics panel, not the
Logs tab.** QuantConnect's Free plan caps a backtest's log output at 10 KB
(and 10 KB a day, total) -- easily exhausted well before a nearly-thirty-year
run finishes (see "Deviations" below for how this script keeps the log
itself from doing that on its own). Every closing figure this script
reports is therefore published twice, by `_publish` (`main.py`): once as a
**runtime statistic** (`SetRuntimeStatistic`), which the backtest result
carries whatever the log budget, and is what you should actually read. A
statistic's value is truncated if long (about 200 characters survived in
practice), so every value is kept short and a tally is split into one key
per item. Each figure is also written as an ordinary **log line**, which may be truncated or missing if the log
budget ran out first.

**In the statistics panel**, once the backtest finishes, look for these
keys (QuantConnect's own CAGR/Sharpe/etc. sit alongside them; these are the
ones this script itself adds):

- `OVERALL` -- CAGR, max drawdown, and CAGR ÷ max drawdown (ADR 0012's
  primary metric) over the whole run.
- `REGIME ...`, one per named Regime Window (ADR 0012): `REGIME 1998-2000
  late bull`, `REGIME 2000-02 bear`, `REGIME 2003-07 bull`, `REGIME 2008-09
  crash`, `REGIME 2009-19 bull`, `REGIME 2020 COVID`, `REGIME 2022
  correction`. A window with fewer than two equity marks (for example, if
  the backtest failed before reaching it) reads `no-data` instead of
  numbers, rather than a misleading zero.
- `Campaigns` -- how many Campaigns closed, the fraction that were
  winners, and the average win and average loss, each expressed in "R" --
  multiples of the Campaign's own initial 1-Unit risk (Stop Multiple × N).
  R is the standard way to compare trades of different sizes; it is this
  script's own reporting convention, not itself a quantity any ADR names.
- `Commission` -- total commission paid over the run, in dollars, as
  charged by LEAN's own `InteractiveBrokersFeeModel`.
- `Declines` -- the total number of proposals this script declined, and
  one `Decline <reason>` key per reason with its count (an ineligible entry, an exhausted Unit cap, insufficient cash,
  and so on), counted rather than logged one line per decline, which is
  exactly what would exhaust the log budget by early in the run (see
  "Deviations").
- `SPY BUY-AND-HOLD` -- CAGR/max drawdown, in the same format as
  `OVERALL`, so the Baseline's own numbers can be read next to simply
  buying and holding the S&P 500 ETF over the identical span.

**In the Logs tab**, the same figures appear as `research: <key> = <value>`
lines, for a run short enough that the 10 KB budget never binds. On a full
run they are a convenience, not the record of truth -- a line missing or
cut off there does not mean the figure is missing from the statistics
panel, only that the log ran out first.

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
2. **Proposal expiry (ADR 0011), as the Go engine's LEAN runs have it.**
   An entry or ordinary Add proposal gets one Session to fill. When a Unit
   fills, the next rung is measured from that fill and decided against the
   last completed bar, the one that proposed the Unit just filled; if its
   high already reached the rung, the Add is proposed at once and is not
   expired by that Session's own close, so it has the Session after it to
   fill (`_chain_add`; internal/strategy's `evaluateAdd` chain from
   `openCampaign` and `applyAddFill`, and ADR 0021 section 7). What remains
   of #1 is that neither this script nor a LEAN run of the Go engine can
   fill the chained Add inside the bar that proposed it, as cmd/backtest
   does.
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
6. **Cash holds and Unit-cap reservations each have a per-order
   lifetime (ADR 0020).** `rules.SessionLedger` is rebuilt every trading
   day from the previous-close cash basis #10 describes, which already
   subtracts the hold of every entry or Add order still working (a
   fill-chained Add, #2, can outlive the Session that proposed it); each
   hold is released when its order fills, is cancelled or is refused. Its
   *Unit-cap* side is tracked explicitly too:
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
   the life of a run; that was fixed before this research check was ever
   run for real.)
7. **Approximate commission for the affordability check, LEAN's schedule
   on raw shares for the trade.** `rules.commission_estimate` (ADR 0013)
   estimates commission for the pre-trade cash check using the IBKR Pro
   Fixed schedule `cmd/backtest/testdata/configuration.json` states
   (per-share rate, $1 minimum, 1% cap), on split-adjusted shares, exactly
   as the Go engine's hold does. The commission actually *charged* is
   `main.py`'s `RawShareFeeModel`: the schedule QuantConnect's own
   `InteractiveBrokersFeeModel` was observed to charge ($0.005 a share, a
   $1.00 minimum that wins over a 0.5%-of-value cap;
   `adapter/lean/orders.py`'s `fill_model_report`, "commission"), applied to
   the RAW shares an order is (`rules.lean_ib_commission`). LEAN's own model
   charges per share of the order as stated, and this script's orders are
   stated in split-adjusted shares -- 56 times the raw count for AAPL in
   2003 -- so using it directly overcharged by up to that factor (an
   earlier version did: $46,877 of commission on the AAPL check below,
   where the Go engine's LEAN run paid $3,683). The `total_commission` the
   closing summary reports is the real, charged figure, never the
   estimate.
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
   loss, with the exact scenario transcribed as
   `test_r_multiple_aggregates_every_unit_not_only_the_last_exit` in
   `test_rules.py`. This aggregate-R convention is a standard,
   widely-used way to compare trades of different sizes, chosen for this
   script's reporting only.
9. **Corporate actions: splits by the split-adjusted subscription,
   dividends corrected, no cash in lieu.** ADR 0023 (a split's cash in
   lieu) and ADR 0024 (symbol changes, dividends as cash events) exist in
   the Go engine as explicit, journalled corporate-action facts, over a
   raw LEAN subscription. This script instead subscribes `SplitAdjusted`,
   so splits are already neutral in the prices and share counts it trades
   (#18). Two consequences are handled explicitly:
   - **Dividends.** LEAN credits a dividend's raw per-share distribution on
     the split-adjusted share count held -- 28 times too much for AAPL in
     2012. `OnData` takes the excess back, paying the distribution on the
     raw shares held (`rules.dividend_cash`), which is what the Go engine
     books. An earlier version did not, and on the AAPL check below ended
     about $1.1M richer than the Go engine from 2012's dividends on.
   - **Cash in lieu is not modelled.** A raw holding the split ratio does
     not divide into whole new shares loses the fraction to cash in lieu;
     here the holding simply continues. On AAPL's 7-for-1 of 2014-06-09 the
     Go engine's run lost one raw share (4 split-adjusted shares) to $91.21
     of cash in lieu, the one fill difference the AAPL check below
     reports.
10. **"Previous close" cash, read from QuantConnect's account (ADR 0010,
    ADR 0020).** The Go engine funds an entry or Add from the cash its last
    `account.snapshot` stated, less every buy fill's cost since and every
    working order's hold; a sale's proceeds count only from the next
    snapshot. This script reads `Portfolio.Cash` at the start of each
    Session's `OnData`, before that Session moves anything, and uses it as
    the basis from the NEXT Session on, less buy fill debits since the
    reading and the holds #6 describes (`_spendable_cash`) -- the same
    figures the Go engine's LEAN runs are given, read from QuantConnect's
    account rather than from a journal. An earlier version funded a
    Session from its own current cash, including that Session's sale
    proceeds.
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
    never as a traded or signalled instrument: `OnSecuritiesChanged` gives
    it no symbol state and none of the strategy's models. (An earlier
    version did, and traded it as a strategy instrument in its dividend-
    adjusted view: the fills at about $60 in the first cloud run of the
    AAPL check.) So ADR 0004's
    split-adjusted-signals rule does not apply to it. It is subscribed on
    QuantConnect's `DataNormalizationMode.Adjusted` (split AND dividend
    adjusted), and its curve is recorded only from the same Session
    `equity_curve` itself starts recording from (once warm-up ends) --
    both covering exactly the same span, so the two CAGR/max-drawdown
    lines are a fair like-for-like comparison. An earlier version of this
    script priced SPY split-adjusted only (omitting dividends, which
    understates a multi-decade buy-and-hold return) and began its curve
    before warm-up ended (a longer span than the strategy's own); both are
    now fixed.
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
      event it does not expect to see. This can only follow from a cause
      outside a backtest this script's
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
      cap for the rest of the run.

    Two defects in an earlier, more elaborate version of this handling
    (which accumulated every partial fill's quantity and price itself)
    are also fixed by this simplification: a guard meant to catch "nothing
    filled" treated a Unit's own Exit Order -- always a negative fill
    quantity, since it is a sell -- as no fill at all, so an Exit Order's
    `Filled` event never actually closed its Unit; and neither settlement
    path compared the filled quantity against the quantity actually
    requested, so a partially-filled-then-cancelled entry or Add could
    still be recorded as a whole Unit at the wrong size.
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
    to `rules.Campaign.add_unit`, which raises in exactly this situation.
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
    instrument.
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
    long it then remained selected. This assumes QuantConnect delivers
    `OnSecuritiesChanged`
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
    floor before 250 distinct bars had actually been observed. A
    `FIXED_SYMBOLS` symbol (#21) is not backfilled: it is subscribed from
    the first warm-up bar, so `SetWarmUp` alone gives it `WARMUP_BARS`.
18. **Price views (ADR 0004, as amended).** Signals, levels and a
    Campaign's money are split-adjusted, and this script's subscription
    trades in that view, so quantity x price is the same money as the raw
    trade. What is not invariant is read in the raw view, through each
    symbol's split ratio (split-adjusted shares per raw share, read from
    History when the symbol is added and moved by each split LEAN reports;
    a symbol with no completed bar to read it from yet is counted as
    `Decline ratio: unreadable when added`, read again at each later
    breakout, and its entry declined as `Decline entry: split ratio
    unreadable` only while it stays unreadable):
    a Unit is rounded down to whole raw shares (`rules.whole_raw_shares`); a
    stop is placed at the nearest raw cent and a stop-limit's limit at the
    raw cent at or below its cap (`rules.raw_tick_round`,
    `rules.raw_tick_floor`), declining a proposal whose limit falls below
    its stop; commission is charged on raw shares, its 0.5% cap valued at
    the fill's own price, which the fill model records since LEAN's
    fee-model parameters carry none (#7); and ADR 0009's $5
    floor reads the raw price. This is exactly how the Go engine's adapter
    places the engine's orders over its raw subscription. An earlier
    version rounded every order price down to the cent in the
    split-adjusted view -- AAPL's 2003 Protective Stops, at $0.3132 in the
    Go engine, rested at $0.30, more than 1N lower -- and read the $5 floor from split-adjusted prices, which excluded any
    stock that later split often (AAPL traded at $0.31 split-adjusted, $17
    raw) from the early years of a run.
19. **Sale proceeds settle immediately.** Every traded security gets
    QuantConnect's `ImmediateSettlementModel`. LEAN settles a cash
    account's sale proceeds only while the security stays subscribed, so
    proceeds from a stock that then left the universe never became
    spendable, and over a multi-decade run starved the account. A US cash
    account may buy with unsettled proceeds; free-riding rules (selling a
    stock bought with unsettled funds before they settle) are not
    modelled. The Go engine does not model settlement either.
20. **An Exit-Channel exit moves the Exit Orders only when the channel is
    breached (ADR 0002; ADR 0005's amendment).** Each Unit's Exit Order
    rests at its own Protective Stop. When a Session's low is strictly
    below the Exit Channel as it stood before that Session
    (`rules.exit_channel_breach`), every Exit Order is raised to the
    channel in that Session's `OnData`; LEAN evaluates an amended order
    against the bar it was amended after, so the exit fills in that same
    bar, at the channel or the bar's open if lower, less slippage. The Go
    engine's adapter relies on the same LEAN behaviour. No Add is decided
    in a Session that proposes an exit (ADR 0010). An earlier version
    rested every Exit Order at the channel as it stood one Session
    earlier. An Exit Order is amended only when its level moves, and LEAN's
    order events are queued and handled after the loop that caused them:
    an amendment can fill while it is being made, and handling that fill
    inside the loop once corrupted a Campaign's Units (the cloud run's
    sells of shares it no longer held, a negative account and the
    "equity must be finite and positive" stop).
21. **A fixed universe for local checks.** `FIXED_SYMBOLS` (default
    `None`) replaces the monthly ADR 0009 universe with a fixed list of
    tickers subscribed by `AddEquity` and skips ADR 0009's eligibility
    test, as a single-instrument Go engine run has none; `START_DATE`,
    `END_DATE` and `WARMUP_BARS` set the run's span and warm-up. The cloud
    research check uses none of them. Subscriptions are daily bars with
    fill-forward off in both modes: a fill-forward bar is not a completed
    bar (CONTEXT.md), and the Go engine's adapter subscribes the same way.

## Checking fidelity against the Go engine

This script's rules are only worth trusting if it trades as the production
Go engine does. The check runs it on local LEAN, on one instrument, and
compares its fills with the journal of the Go engine's own LEAN run over
the same span. It is free: it uses the pinned LEAN image and the market
data already on the machine, and never pulls either.

**Running it.** From a LEAN workspace whose `data/` holds US equity daily
data (the Go engine's acceptance runs use one), with a Go engine journal
of the same instrument and span:

```
research/qc-cloud/fidelity/run_local.sh <lean-workspace> <journal.jsonl>
```

`run_local.sh` copies `fidelity/lean_main.py` into the workspace's
`research-fidelity` project as `main.py`, beside `main.py` (as
`research_main.py`) and `rules.py`; runs `lean backtest` on the pinned
image with `--no-update`; and then runs `compare_fills.py` on the
backtest it wrote. `lean_main.py` subclasses this script's algorithm and
sets only `FIXED_SYMBOLS = ("AAPL",)`, the 2003-01-01 to 2014-12-31 span
and `WARMUP_BARS = 60` (the Go run's `warmup_bars`). The comparison alone:

```
python3 research/qc-cloud/fidelity/compare_fills.py \
    <lean-workspace>/research-fidelity/backtests/<run> <journal.jsonl> [--show 10]
```

It reads LEAN's orders and order events and the journal's
`execution.fill` envelopes (split-adjusted, like this script's orders),
groups each side's fills by session date and kind (entry, add, stop,
exit), and reports each group as `match`, `mismatch:quantity|price`,
`only-research` or `only-go`, then net profit, CAGR and max drawdown for
both, measured the same way from each side's daily equity. Its exit status
is 0 only when every group matches. It is not part of `make check`, since
it needs LEAN; its matching logic is unit-tested (`test_compare_fills.py`).

**Result: AAPL, 2003-01-01 to 2014-12-31, $1,000,000.** Against the Go
engine's run with the Baseline configuration
(`turtle-baseline/1.18.0+dev`, 60 warm-up bars):

| | Research | Go engine |
| --- | --- | --- |
| Fills | 192 (35 entries, 61 Adds, 37 stops, 59 per-Unit exits) | 153 (35 entries, 61 Adds, 37 stops, 20 exits) |
| Fill groups matching | 140 of 141 | |
| Net profit | 195.351% | 195.351% |
| CAGR (LEAN's statistic) | 9.439% | 9.439% |
| Max drawdown | 33.704% | 33.704% |
| End equity | $2,953,514.42 | $2,953,509.22 |
| Commission | $3,683.26 | $3,683.26 |
| Declines for insufficient cash | 258 | 258 |

Every entry, Add, stop and exit falls on the same date, in the same
quantity, and at a price within 2.4e-6 of the Go engine's. The first ten
groups:

| Date | Kind | Research | Go engine |
| --- | --- | --- | --- |
| 2003-05-07 | entry | 505,960 @ 0.30995764 | 505,960 @ 0.30995838 |
| 2003-05-08 | add | 505,960 @ 0.31710048 | 505,960 @ 0.31710124 |
| 2003-05-09 | add | 505,960 @ 0.32763617 | 505,960 @ 0.32763695 |
| 2003-05-12 | add | 505,960 @ 0.33299410 | 505,960 @ 0.33299410 |
| 2003-05-30 | stop | 505,960 @ 0.31272019 | 505,960 @ 0.31272019 |
| 2003-06-02 | stop | 505,960 @ 0.31236305 | 505,960 @ 0.31236305 |
| 2003-06-03 | stop | 2 x 505,960 @ 0.30557733 | 2 x 505,960 @ 0.30557733 |
| 2003-06-19 | entry | 423,472 @ 0.34630381 | 423,472 @ 0.34630464 |
| 2003-07-08 | add | 423,472 @ 0.35273321 | 423,472 @ 0.35273321 |
| 2003-07-09 | add | 423,472 @ 0.36148235 | 423,472 @ 0.36148321 |

**The differences that remain, each explained:**

- **2014-10-15's exit: 110,040 shares against 110,036.** The Go engine's
  run holds raw shares, and at AAPL's 7-for-1 of 2014-06-09 lost one raw
  share (4 split-adjusted shares) of that Campaign to $91.21 of cash in
  lieu (ADR 0023), which this script does not model (Deviations, #9).
  With `--quantity-tolerance 0.0001` every group matches. The two splits'
  cash in lieu ($2.73 in 2005, $91.21 in 2014) and the price rounding
  below make up the $5.20 end-equity difference.
- **Prices within 2.4e-6.** A fill at the open is priced from LEAN's
  split-adjusted bar, which LEAN scales by the factor file's rounded
  factor (0.0178571 for 1/56); the Go engine's is LEAN's raw price divided
  by the whole ratio. Fills at a level agree exactly.
- **Exits are counted per Unit.** The Go engine reports an Exit-Channel
  exit as one fill for all its Units; this script rests one Exit Order
  per Unit (Deviations, #20), so its 59 exit fills are the Go engine's 20.
  `compare_fills.py` compares them as groups.
- **Warm-up.** The Go run warmed up on 60 bars, so the check does too.
  The cloud research check warms up on 250 (`WARMUP_BARS`: ADR 0009's
  250-bar history floor, which a fixed universe does not apply), which
  seeds N from an earlier start; N converges within a few dozen bars, but
  on another span or instrument a different warm-up can move a Unit by a
  share count or two.
- **Fill model.** Both runs fill through LEAN with the same ADR 0005
  stop-limit fill model, slippage and fee schedule, so neither shares
  cmd/backtest's same-bar fills (Deviations, #1).

**What the check found.** The first cloud run of this script on AAPL
alone lost 107% and stopped on 2007-10-23 with "equity must be finite and
positive". Its causes, each fixed and each named in Deviations: SPY, the
benchmark, traded as a strategy instrument (#13); order prices rounded to
the cent in the split-adjusted view (#18); commission charged on
split-adjusted shares (#7); a fill during an Exit Order amendment handled
inside the loop making it (#20); dividends credited on split-adjusted
shares (#9); and the Exit Channel, Add chaining and cash basis departing
from the Go engine (#20, #2, #10).

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
  reported by the run (the `Commission` runtime statistic, "How to find the
  results", above) ÷ starting equity, then annualised by the number of
  years the backtest covers if you want a rate rather than a lifetime
  total. QuantConnect's own free 1998-to-date backtest already prices this
  in: `Commission` is a real cost the equity curve has already paid, not
  something to add on top of the reported CAGR -- but it is worth looking
  at on its own, because a strategy that trades often on a small account
  can lose a meaningful share of its return to commission alone, even when
  the equity curve still ends up net positive.
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
