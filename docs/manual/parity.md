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

They were fixed before any comparison was run, and the owner amended one of
them on 2026-10-18 (ADR 0026, amendment of that date):

| Check | Must hold |
| --- | --- |
| ranked order, cap marks | exactly the same |
| weights | within 0.000000000001 (1e-12) of QuantConnect's |
| scores, volatility, the SPY average | within one part in a billion (1e-9) |
| Boost RSI, both averages | within one part in a billion (1e-9), every session; ready on exactly the same sessions |
| Boost state and target | exactly the same, every session |
| a live departure from the backtest | fails, unless ADR 0026 point 8 lists its cause |

Boost's RSI and averages were first required to match bit for bit. They
cannot: QuantConnect works them out in C#'s `decimal` numbers and Go in
ordinary 64-bit floating point, so the last digits differ (the largest
difference found was about six parts in ten trillion). None of those
differences changed a decision: the boosted-or-not state and the SPY target
matched on every one of 7,166 sessions. Matching QuantConnect to the last
bit would mean rewriting its arithmetic in Go operation for operation, so
the owner accepted the same one-in-a-billion tolerance the scores already
had. The state and target must still match exactly.

### The one rule: no mismatch is ever excused

The tool never puts a mismatch down to a cause. It does two separate
things.

**First, it checks Go against QuantConnect exactly.** Go recomputes each
month the way the backtest did, including the cases where live trading
deliberately does something else: equal scores stay in QuantConnect's
order; a stock whose price never moved is picked and weighted as if its
volatility were 1; a price history with a missing day is scored over the
prices it has, and one with fewer than 253 prices is left out; SPY's
average is taken over the prices it has, and with fewer than 200 it counts
as risk-on. That
recomputation is compared with QuantConnect's export: the ranked order,
every cap mark, the picked stocks and every weight, every month, with no
allowance beyond the thresholds above. A single mismatch fails the check.

One thing the export cannot show: the order the backtest's universe
arrived in, which is how it broke ties. Go keeps tied stocks in
QuantConnect's own order, so the ranked-order check proves that every
other pair of stocks is in the right order, but not the order within a
tie.

The same applies to gaps in a stock's prices. The export carries the
prices of the stocks picked and of at most five more stocks with a gap in
each month, so the gap rule is applied to those stocks only. A month with
more gapped stocks than that has the rest unchecked, and the tool does not
list their live difference.

**Second, it lists every live departure.** Go then runs the live rule on
the same inputs and lists every way the live result differs from that
backtest-compatible one, each with its cause. It works this out by
applying the live rules one at a time: equal scores by ticker, then
leaving out a stock with a gap in its prices, then one whose price never
moved. Every stock whose mark changes at a
step, and the month's weights if they change, is listed under that
step's cause, and the live result must then match the last step exactly.
Anything left over is listed as `unexplained`.

ADR 0026 point 8 lists the backtest behaviours live trading deliberately
does not copy: keeping last month's book when nothing can be scored,
treating SPY as risk-on with fewer than 200 prices, a price window that
slides over a missing day, equal scores ranked in whatever order the data
arrived, a stock whose price never moved weighted as if its volatility
were 1, and RSI's dependence on where its history starts. The
`differences` check passes only if every listed departure has one of those
causes. `unexplained` is not on the list, so it fails it.

A stock with no sector label is not a departure at all. Live trading used
to leave such a stock out, which the backtest did not, and the 2026-10-18
comparison failed on it. The owner decided on 2026-10-18 that live trading
should do what the backtest did: such a stock is ranked and can be picked,
and it counts toward the 25 picks but not toward any sector's limit of 3
(target-weight rules version 1.13.0). So the live result must equal the
backtest's for it, and a live result that left one out would be listed as
`unexplained`.

A check that had nothing to compare fails too: it proves nothing.

## The glide path, the minimum lot and the IRA orders

The first comparisons covered the momentum book and Boost. Three more
parts of the IRAs' rules are compared the same way
(`docs/research/parity-2026-10.md`, "Remaining scope, plan, 2026-10-19"):

- **the glide path** (`-glide`): each month, does Go work out the owner's
  age, the bond share it gives (0% at 38 rising to 70% at 72), and the
  combined targets, the momentum stocks scaled down and the bond sleeve
  added, half IEF and half SHY, with IEF held once when both the momentum
  book and the bond sleeve hold it?
- **the minimum lot, D7-L** (`-lot`): given the backtest's ranked stocks,
  their raw prices, the account's value and what it holds, does Go skip
  the same stocks whose one share costs more than their target, keep the
  same held ones, pick the same replacements, and give the same weights?
  A month in which every stock was skipped must be withheld by both.
- **the IRA orders** (`-ira`): given the backtest's targets, holdings,
  prices and account value, does Go plan the same sells and buys (shares
  and limit prices) and skip the same trades; scale the buys to the cash on
  hand the same way; did the backtest place exactly those orders; and were
  the buys placed only after every sell had filled?

The minimum lot and the IRA planner were not in the original backtests,
which sized every order with QuantConnect's `SetHoldings`, and the glide
path had no export. Each has a backtest template that adds
the Go rule, ported line for line (`momentum/glide_parity_log.py`,
`momentum/minimum_lot_parity_log.py`, `momentum/ira_parity_log.py`, with
the shared code in `momentum/parity_export.py`). So for the minimum lot and
the IRA orders, parity shows that the backtest that measured the rule
decided what the Go code decides, given the backtest's own data; it is not
a check against an older backtest.

The thresholds:

| Check | Must hold |
| --- | --- |
| `glide-age`, `glide-share` | within 1e-12 |
| `glide-weights`, `glide-sleeves`, `d7l-weights` | the same stocks; each weight within 1e-12 |
| `d7l-picks`, `d7l-skipped`, `d7l-kept` | exactly the same, a withheld month included |
| `ira-plan` | exactly the same sells and buys (shares and limit prices) and the same skips (side, reason, shares) |
| `ira-scale` | the same shares; the scale within 1e-12 |
| `ira-orders` | the orders the backtest placed, by stock and phase, exactly the shares Go's sells and scaled buys call for; each one filled, in its phase's direction (a buy positive, a sell negative) and in whole shares. Only quantities: the backtest sends market orders, so no limit price is checked here. An order the backtest could not place because the stock was delisted or acquired at the rebalance passes when the export records how that was detected (below); it is listed as a `delisted` departure. Every refusal and every unplaced buy is either so listed or fails; an unplaced buy is listed only after five sessions waited for a price, counted on the calendar. Every other order of the run is accounted for too: an export carrier is skipped only when it was cancelled or refused and its events fill nothing (a carrier partly filled, still working, or of an unknown status fails), or when it is the run's very last order, left cancel-pending because the run ended before the cancel was confirmed, and filled nothing: it is the template's own record-keeping order, not a trade; a sell set aside (below) that never filled and is exactly Go's planned sell in shares is reported as `set-aside`, not failed, and so is an unpriced sale of a holding with no reference price that was not a pending target (below); any other set-aside sell fails; LEAN's own sale at a delisting is a listed difference, and so is the buy outside the planner of a stock that was a target with no reference price (`pending`), matched by its security ID; any other order fails, the buy of an unpriced holding included. The sale of a holding with no reference price must be of such a stock that was not a target |
| `ira-sequence` | every sell filled before the first buy, and no sell of any month still holding the buys when one was placed; a sell still working at 8:00 on the fifth session it worked through (its own day the first when placed before 8:00) had no price at any of those opens and cannot complete, so it is set aside and holds no buy from then on (the owner's decision of 2026-10-18, ADR 0026); for every month, one that placed no order included, the rebalance and the buys each on a trading session; the sessions from the rebalance to the buys, counted on a trading calendar, are the delay (one session, none when no sell was placed or working), then each session at whose 8:00 the orders listing shows a sell still holding them, then any waited for a stock's first price |
| `differences` (IRA) | none off ADR 0026 point 8's list. Since the owner's decision of 2026-10-18 the list includes three IRA causes, each only within its bound: a wait for a newly added stock's first price (`price-wait`) of at most five sessions, counted on the calendar; a stock with no reference price, and its buy outside the planner (`unpriced`), only in a month before the first month any export records a price for it (an instrument not yet listed, such as IEF before July 2002): every export the tool read for the run counts, each month's prices of its targets and holdings (a pending target has none), and a stock that no export prices at all fails as `unexplained`; the exports cannot tell a stock not yet listed from one first seen during a gap in its data and priced later; and LEAN's own sale at a delisting (`liquidated`) of a stock Go held. A stock set aside (a sell that cannot complete) is reported as `set-aside`. Anything outside those bounds is reported `unexplained` and fails |

These thresholds are settled: the owner confirmed them as the rule on
2026-10-18 (ADR 0026, amendment of that date, "the remaining parity
scope"). They were first proposed in the plan, before any run. The
`ira-sequence` rule was amended twice after
that: after the smoke run SMOKE_IRA it allowed the recorded waits for a
working sell or a first price, and on review (2026-10-18) the sessions are
counted on a calendar rather than taken from the export, every working
sell of any month holds the buys, and a price wait became a listed
difference. The `ira-orders` rule gained the status, direction and
whole-share conditions then (`docs/research/parity-2026-10.md`, "Review
fixes and re-runs, 2026-10-18"). On the second review the same day the
rebalance and the buys must each fall on a session, every order of the
run is accounted for, and a refusal or an unplaced buy is listed only on
the detection described below ("Second review fixes, 2026-10-18").

The buys wait for every sell of the run still working, of any month or
tag, not only the rebalance's own; the delay applies when a sell is placed
or still working at the rebalance. That is how the tool and the template
apply ADR 0026 point 2 (ADR 0026, amendment of 2026-10-18). What should
happen when a sell can never complete, as in the held-out IRA run, is an
owner decision still open.

**Fractional shares (D7-F) are out of the parity scope**: they are not
built, the owner chose whole shares for the IRAs on 2026-10-16, and on
2026-10-18 dropped D7-F from what parity must show (ADR 0026, amendment
of 2026-10-18).

**A stock delisted or acquired at the rebalance.** The backtest's
universe still ranks such a stock from the session before, so the IRA
planner plans an order QuantConnect cannot fill. Since 2026-10-18 this is
one of ADR 0026 point 8's listed causes. The tool lists each such order
under `delisted` with the detection the export records: QuantConnect
refused it and gave the reason "non-tradable", or the stock had no price
within five sessions (`unplaced`, listed only when the export records all
five sessions waited for a price, which `ira-sequence` checks against the
calendar). Either must name an order Go planned that the backtest did not
fill. Every other refusal fails `ira-orders`: one for any other reason, one
with no reason recorded, one of an order that filled after all or that Go
never planned. A placeholder order ID -10, all a version 1 export records
of a refusal, says only that QuantConnect refused, not why, so a version 1
refusal fails. The cause itself is never inferred by the tool.

In the IRA backtest a stock that joins the universe at the rebalance has
no price until its first trading day, so the backtest cannot order it at
once. The buys then wait a session (at most five) and the wait is
recorded. A buy still without a price after five sessions is recorded as
unplaced; it passes `ira-orders` and is listed as a `delisted` departure
(above), while the sessions waited are listed as a `price-wait`
difference, which fails `differences`. A buy recorded as unplaced without
the five sessions fails `ira-orders`. Live trading has the previous close,
so it does not wait.

The IRA sequence counts sessions on a calendar, not by the backtest's
word: `internal/nysecalendar` from 2022, and before that a list given with
`-ira-calendar FILE`, a JSON list of session dates. The recorded list is
the sessions QuantConnect's own history returned in the kept momentum
exports (`~/Desktop/Trend_Investing/data/parity/sessions_qc_history.json`,
1998-02-17 to 2026-05-29); the tool refuses a list that disagrees with
`internal/nysecalendar` on any day both cover. A rebalance on the list's
first date can be checked. A month whose span no calendar covers fails
`ira-sequence`. An orders listing whose filled, cancelled or invalid event
has no usable time is refused, since it would end a sell before every buy.

The IRA template records an order QuantConnect refused apart from the
placed ones, with the reason it gave (`refused`), and a month whose buys
were never placed because a sell was still working at the next rebalance
as `unfinished`. A refusal given as non-tradable, of a planned order the
backtest did not fill, is listed as `delisted` and passes `ira-orders`;
every other refusal fails it. An unfinished month fails `ira-orders` on
every planned buy, and fails `ira-sequence`. A sell that cannot complete
no longer causes one: as the owner decided on 2026-10-18, a sell refused
as non-tradable, or one still working on the fifth session it worked
through (no price at any of those opens), is set aside and the month's
buys go ahead, scaled to the cash actually on hand, so its proceeds are
never counted. The holding it was meant to sell stays held and is
reported: a refused sell as `delisted`, a sell still working on the fifth
session as `set-aside` (live trading pages the owner). The fifth session
is counted as the template counts it: each session at whose 8:00 the sell
was still working, so the sell's own day is the first when it was placed
before 8:00 that day (LEAN's own sale at a delisting) and not when the
template placed it at 8:00. A set-aside sell excuses Go's planned sell
only when it is that sell: the same stock, a sell in whole shares, of
exactly Go's shares (two set aside are summed), with none of them filled;
a set-aside sell of a stock Go planned no sell for, or of another size,
fails `ira-orders`. A sale of a holding with no reference price that is
set aside is reported as `set-aside` too, never dropped. Exports made before
2026-10-18 (version 1) have neither field; the tool reads their refusals
from the placeholder order ID -10, and each fails, since no reason was
recorded. Version 3 also records which stocks with no reference price
were targets (`pending`), which the template keeps waiting and never
sells, so the sale of a holding with no price can be told from the sale of
a target; an unpriced sale in an earlier export fails `ira-orders`.
Version 4 also records each pending stock's security ID (`pending_ids`),
because the orders listing names a stock by its latest ticker: the buy
outside the planner is matched by it. In an earlier export such a buy is
matched only when the name recorded is the security ID itself; one matched
by ticker alone fails `ira-orders`. When a sell still working holds some
of a holding with no reference price, the template sells the rest at once
and the shares that sell held at the first buy phase after it ended
unfilled, so it never sells them twice and never leaves them unsold. A
record whose carrier QuantConnect refuses is kept and sent again on a
later carrier; the run's `IraParity` statistic counts as `lost` any that
never went.

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
  -boost BX_PARITY=$HOME/Desktop/Trend_Investing/data/parity/BX_PARITY_orders.json \
  -sessions BX_PARITY=research/edge-search/boost/parity_plan.json
```

and, for the remaining checks,

```sh
go run ./cmd/parity \
  -glide G_IS_P38_PARITY=$D/G_IS_P38_PARITY_orders.json -glide-months G_IS_P38_PARITY=2002-08..2015-12 \
  -lot D7L_PARITY=$D/D7L_PARITY_orders.json -lot-months D7L_PARITY=1999-03,1999-11,... \
  -ira IRA_PARITY=$D/IRA_PARITY_orders.json -ira-months IRA_PARITY=1999-03,1999-11,... \
  -ira-calendar $D/sessions_qc_history.json
```

A months list can give a range, `2002-08..2015-12`, every month from the
first to the last. A glide, lot or IRA run may export more months than were
planned (every month, or every month the rule acted); those are compared
too, and the input line says how many were planned.

Each `-momentum` run needs its `-months` list: the months chosen in advance.
Each `-boost` run needs its `-sessions` plan file, a JSON file of the first
session, last session and session count, committed before the run (the
recorded one is `research/edge-search/boost/parity_plan.json`). The tool
refuses a plan whose first session is not before its last, that has no
sessions, or that holds more sessions than there are weekdays between them;
an export that stops short of it, for example one missing its final chunk,
fails the `export` check. The
momentum report also lists, for each month, how many comparisons each check
made, so a month that compared less cannot hide behind the others.
The exact command used for the recorded result, with every month, is in
`docs/research/parity-2026-10.md`.

## What it prints

A line per input saying how many months or sessions it found (for a
momentum run, "N logged months from M tagged orders of K"), then a table with
one row per check and run: the check, its threshold, how many comparisons,
how many failed, the largest difference seen and PASS or FAIL. Then the
overall result (`**Overall: PASS**` or `**Overall: FAIL**`); for
`d7l-skipped` and `d7l-kept`, which compare every month though in most of
them neither side names a stock, the number of months in which the rule
fired on either side; the first failure of each failing check (or "nothing
was compared"); a table of how many comparisons each month made; and, last,
a table of the live departures, each with its cause and whether ADR 0026
point 8 lists it, or "No differences."

It exits with status 0 when everything passes, 1 when any check fails, and 2
when an input cannot be read at all (a missing file, a tag that is not
valid, a run with no planned months, `-months` or `-sessions` with no
matching `-momentum` or `-boost`, or nothing named to compare). It is run by
hand: nothing on the server runs it, and the engine never calls it.

## The result so far

Recorded in `docs/research/parity-2026-10.md`. Two runs of momentum (30 months
of 1999 to 2015 and 31 months of 2016 to 2026) and one of Boost (every one of
7,166 sessions from 1998-01-02 to 2026-06-30) were compared on 2026-10-17
and 2026-10-18.

- **First comparison: FAIL**, on three checks of thirteen. The momentum
  `differences` check failed because live trading then left out stocks with
  no sector label and the backtest did not (1,634 and 1,214 differences). The
  Boost RSI and moving-average checks failed because they required bit-exact
  numbers and the two languages' arithmetic differs in the last digits
  (largest difference about 4e-12). Every ranking, cap mark, weight not put
  down to those stocks, score, volatility, risk-off flag and every Boost
  state and target matched. The FAIL is kept as recorded.
- **After the owner's decisions of 2026-10-18: PASS**, every check of all
  three runs. He chose that live trading copies the backtest for stocks with
  no sector label (rules version 1.13.0), and that Boost's indicators match
  within one part in a billion (ADR 0026, amendment of that date). The
  comparison was re-run on the kept QuantConnect exports, with no new
  backtest. The largest Boost indicator difference is 6.1e-13 relative; the
  momentum numbers did not change. A later rule change, a held stock scored
  across a gap (rules version 1.15.0), was re-run the same way and left the
  reports byte for byte the same.

What the PASS does not show, as the research note says: that the logging
versions of the strategies trade exactly as the recorded ones (no same-day
control pair was run), the order within a tie, the gap rule for gapped
stocks the export does not carry, and the held-stock path (covered by tests
instead). The glide-path runs, a repeat with each D7 option, the IRA buy delay
and the new buy scaling were not run through it. Live labels come from IWV
and Nasdaq, not Morningstar, so which stocks lack a label differs live;
dry-run and paper trading measure that.

## What can go wrong, and what the program does then

Parity is a check you run, not a program that runs by itself. Nothing is
sent as an alert; you see a failure in the command's output and its exit
status.

| What you see | What the program does | What you do |
| --- | --- | --- |
| `months` FAIL (momentum and Boost): a planned month is missing, or an unplanned one is present | The check fails and that month is not compared further. | The backtest did not rebalance that month, or the tag was lost. Check the run's `Parity` statistic, which counts the months it exported. |
| `glide-months`, `d7l-months` or `ira-months` FAIL: a planned month was not exported exactly once, or any other month was exported more than once | The check fails. A month exported once that was not planned is not a failure: it is compared like the planned ones. | The backtest did not export that month, or exported it twice, or the tag was lost. Re-export from QuantConnect and check the run's `Parity` statistic. |
| `export`, `glide-export`, `d7l-export` or `ira-export` FAIL: the export is malformed (a price column of the wrong length, dates out of order or repeated, a picked stock with no weight, a stock ranked twice, a member count that is not the ranked plus the too short), Go gave a stock a status the tool has no backtest mark for, or, for Boost, a close that is not positive or an RSI outside 0 to 100 | The check fails and nothing in that month is compared further. An RSI just over 100 is refused so it cannot pass the 1e-9 tolerance against a Go RSI of 100. A Boost export that stops short of its plan, for example one missing its final chunk, fails here too. | Re-export from QuantConnect. If Go gave a status the tool does not know, that is a tool or rule defect: open an issue. |
| Exit status 2 and a line starting `parity:`: a required field missing or null (a weight or a close count included), an empty ticker, a tag that is not valid, a file that cannot be read, a plan file whose first session is not before its last, that has no sessions or more sessions than weekdays between them, or a run with no planned months | The tool stops before any comparison and prints no table. | Fix the input named in the message. |
| `ira-orders` FAIL on a buy the backtest never placed | The check fails for that month. A refusal given as non-tradable, or an `unplaced` stock waited for five sessions, would have been listed as `delisted` and passed, so this is a refusal of another reason or an `unfinished` month (a sell still working at the next rebalance). Since the owner's decision of 2026-10-18 a sell that cannot complete is set aside after five sessions and the buys go ahead; it is reported as `set-aside` (a sell refused as non-tradable as `delisted`) | Look at the export's `refused` list and whether the month is `unfinished` before suspecting the planner. In the held-out 2026-10 runs one sell never filled (CBAY, 2024-04), and with that template every later month is `unfinished` |
| `differences` FAIL, with an `unexplained` row in the departures table | The live rule departs from the backtest for a reason ADR 0026 point 8 does not list. | Treat it as a Go defect until shown otherwise: open an issue, do not edit the threshold or the list to make it pass. |
| Any other check FAIL (`ranked-order`, `cap-marks`, `weights`, `scores`, `volatility`, `risk-off`, `history`, `boost-rsi`, `boost-sma`, `boost-state`, and the other `glide-`, `d7l-` and `ira-` checks). The first failure line says where | The Go code and the backtest disagree. This is a result, not a tool fault. | Record it, open an issue for it, and do not change a threshold after seeing it. |
| A check that compared nothing shows FAIL with "nothing was compared" | It proves nothing, so it fails. | Check the plan and the export actually cover what you meant to compare. |
| A later re-run fails after the PASS | The tool prints FAIL and exits 1. Nothing else happens: no alert, no stop to trading, no automatic run. | Nothing in the plan (`docs/live-trading/plan.md`, section 6), ADR 0026 or the deploy files says who re-runs the comparison after a rule change, or when. They name it as a gate before dry-run, and the research note re-ran it by hand after each rule change so far. Whether to re-run it after every change to the rules, and who must, is your decision. If it fails, treat it as the row above says and do not start a dry run or paper run on code that fails it. |

## Tests that back it

The comparison itself is tested on synthetic exports, so the tool is known to
pass what matches and fail each kind of alteration.

- `internal/parity/momentum_test.go`: `TestMatchingExportPassesEveryCheck`,
  `TestAMissingPlannedMonthFails`, `TestAnUnplannedMonthFails`,
  `TestTwoRankedNamesSwappedFailRankedOrder`, `TestAChangedCapMarkFails`,
  `TestAWeightOffByMoreThanTheToleranceFails`,
  `TestAWeightWithinTheToleranceIsMeasuredNotFailed`,
  `TestAScoreOffByMoreThanTheRelativeToleranceFails`,
  `TestAFlippedRiskOffFlagFails`, `TestAMalformedExportFailsIntegrity`,
  `TestANameWithNoSectorIsNoDifference`,
  `TestAWindowShiftedAcrossAGapIsAListedDifference`,
  `TestTiedScoresRankedInAnotherOrderAreAListedDifference` and
  `TestAZeroVolatilityPickIsAListedDifference`.
- `internal/parity/boost_test.go`: `TestMatchingBoostExportPassesEveryCheck`,
  `TestAnRSIOneBitOffPasses`, `TestAnRSIOffByMoreThanTheToleranceFails`,
  `TestBoostIndicatorThresholdsAreRelative`, `TestAFlippedStateFails`,
  `TestAnRSIOutsideItsRangeFailsTheExport` and
  `TestABoostExportThatStartsLateOrCountsOtherwiseFails`.
- `internal/parity/report_test.go`: `TestACheckThatComparedNothingFails`,
  `TestThresholdsAreTheSpecifications` and
  `TestTheReportListsEveryCheckWithItsThresholdAndResult`.
- `internal/parity/momentum_internal_test.go`:
  `TestALiveBookTheListedRulesExplainIsNoDeparture`,
  `TestALiveRankingStatusOrWeightNoListedRuleProducesIsUnexplained` and
  `TestAnUnlabelledZeroVolatilityNameIsLeftOutByTheZeroVolatilityRule`.
- `cmd/parity/main_test.go`: `TestTheCommandRefusesWhatItCannotCompare`,
  `TestAMomentumExportIsComparedAgainstItsPlannedMonths` and
  `TestABoostExportMissingItsFinalChunkFailsAgainstThePlan`.

## What backs it

ADR 0026 point 7 (item 2) and point 8, and its amendment of 2026-10-18;
`docs/live-trading/plan.md` section 6; the recorded results in
`docs/research/parity-2026-10.md`; the tool in `cmd/parity` and
`internal/parity`.
