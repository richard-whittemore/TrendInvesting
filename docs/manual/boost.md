# Boost: the joint account's rule, end to end

*For the owner. What the Boost rule decides, when it turns on and off, how
its one order is sized and priced, what happens after an order is refused,
what one Boost day looks like from the data to IBKR and back, and what can
go wrong.*

Words in **bold** on first use, and the terms net liquidation, mark, latch,
broker snapshot and kill switch, are explained in the [glossary](glossary.md).

## What it is and why

Boost runs the joint taxable margin account (plan section 1). It holds SPY
almost all the time, and borrows for a short extra step after a sharp
one- or two-day drop in an uptrend. It is the backtested rule in
`research/edge-search/boost/template.py` (BOOST_SIZE 1.5, RSI_ENTRY 10,
TREND_DAYS 200, EXIT_SMA 5), restated in plan section 2, "Boost", and in
ADR 0026, point 2, "Boost". The owner chose margin Boost, not a leveraged
fund, on 2026-10-14 (plan decision D3).

The rule lives in `internal/targetweight/boost` (the arithmetic) and
`internal/targetweight/handler.go` (the session-by-session run). It knows
nothing about IBKR: it reads recorded inputs and writes recorded decisions
(ADR 0026, point 1).

## The rule in plain words

Every session, after the close, the engine reads SPY's closing price
(split- and dividend-adjusted, so a dividend is not mistaken for a fall)
and updates three readings:

- **RSI(2)**: a 0 to 100 gauge of the last two days' moves, smoothed the
  way QuantConnect smooths it (Wilder's method). Low means SPY has just
  fallen hard.
- **The 200-day average**: the mean of the last 200 closes, today's
  included. A close above it means the long trend is up.
- **The 5-day average**: the mean of the last 5 closes, today's included.

"The last 200 closes" means the last 200 the engine received. A session
with no SPY bar is skipped, not filled in ([below](#a-session-with-no-spy-bar)).

Then it decides whether the boost is on. This on/off state is called the
**latch**.

| The latch is | It changes when | To |
| --- | --- | --- |
| off | RSI(2) is below 10 **and** the close is above the 200-day average (both strictly, both readings ready) | on |
| on | the close is above the 5-day average (strictly) | off |

Two things follow from the table. First, once on, the boost stays on even
if SPY then closes below its 200-day average: only the 5-day exit turns it
off (`TestLatchedBoostSurvivesACloseBelowTheTrendMean`). Second, the latch
is state, not something worked out afresh each day: a restart takes it
from the record, never from prices (plan section 2; ADR 0026, point 5).

The **target** follows the latch:

| Latch | SPY target, as a share of the account's net liquidation |
| --- | --- |
| off | 98% (`invested_fraction` 0.98) |
| on | 147% (0.98 x `boost_size` 1.5) |

147% is Boost's own target. **150% is a different number**: the hard
limit on exposure that any buy must not exceed ([below](#the-hard-limits-for-this-account)).

The figures are in the account's configuration,
`deploy/etc/engines/boost.config`, under `boost`. Changing any of them
changes the configuration hash, so it is a new run, never a silent edit
(ADR 0016).

## The one order

The engine decides after the close and the order is for the next session
(plan section 2: "decide after the close, trade at the next open"). The
order planner (`internal/orderplan`, `PlanBoost`) makes at most **one**
order a session:

- **When.** Only when the target changes (98% to 147%, or back), or on the
  run's first decision (there is no target in force yet, so the first
  decision buys to 98% from cash), or after a refusal that left the account
  Normal, or after the last order ended with shares unfilled (part-filled
  or unfilled), or once after IBKR rejected it (next section). On any other
  session no order is planned, and after IBKR has rejected two orders in a
  row no buy is sent until you start a fresh run, though a sell still is
  (next section).
- **How many shares.** target x net liquidation / SPY's raw (unadjusted)
  close of the session just decided, called the **previous close** because
  the order is for the session after it, minus the shares held, rounded **toward zero** to whole shares. Rounding
  toward zero never buys above the target and never sells below it. A sell
  is also reduced by the shares already being sold by an earlier sell that
  has not finished, so two sells never together go below the target. A
  difference that rounds to no share plans nothing.
- **What kind of order.** A whole-share **limit** order, good for the
  session only (a DAY order), at the edge of the price band: that previous
  close **plus 5%** for a buy (rounded down to the cent), **minus
  5%** for a sell (rounded up to the cent). It is marketable, but can never
  fill worse than the band (ADR 0026, point 2, "Order type and price";
  `price_band` 0.05).
- **Its name.** A client order ID made from the decision, the instrument and
  the side (`tw-` and 24 hex digits). IBKR shows it as the order's
  reference (`orderRef`). It is a label: what stops an order going twice is
  that it is written down before it is sent ([re-delivery](redelivery.md)).

Then the order goes through the hard limits (the risk gate) before
anything can be sent ([states-and-kill-switch](states-and-kill-switch.md)).

## A refused or unfilled order is planned again (re-plan)

Normally Boost orders only when its target changes. But if the gate refuses
the order **and the account stays Normal** (the kill switch was on, or
IBKR's latest snapshot was too old to check a sell against), the next
session that decides plans the difference between what the account holds
and the target in force, even though the target has not changed (owner's
decision of 2026-10-18; ADR 0026, amendment of that date; rules version
1.16.0). So a buy the kill switch refused goes at the first decided close
after you release the switch.

The same happens when the order **ends with shares unfilled**: the DAY
order expires, or IBKR cancels it, having filled only part (`part-filled`)
or nothing (`unfilled`). The next session that decides plans the rest of
the difference to the target in force, from the account as it then stands
(rules version 1.18.0; ADR 0026, amendment of 2026-10-18). The part-filled
case is the owner's decision of 2026-10-18; the nothing-filled case extends
it, confirmed by the owner on 2026-10-18. An order filled in full plans
nothing more, even if the rest of its fills arrive after the cancellation:
the program checks at each close until the session that plans the rest, so
a fill IBKR reports late still counts, and that decision records the shares
filled as they then stand.

The program keeps watching the last order a decided session released until
it has filled in full, or it has ended and a later session has decided. A
session that decides but orders nothing does not stop the watch, so an
expiry IBKR reports after the next close is still planned for. The one
exception: if a later decision has already **released** a new order
(because the target changed), an expiry reported after it plans nothing.
That new order was sized from the account as it stood, so planning again
would trade drift, or buy twice while the new buy is still working.

If IBKR **rejects** the order outright (`invalid`, it never rested), the
reason may last (a permission, a margin requirement), so the program plans
it again **once** (`replan` cause `rejected`). The kill switch or a stale
IBKR snapshot refusing that one retry does not earn it another: the order
sent once the refusal clears is still the one retry.

**Two rejections in a row stop Boost's buys.** If IBKR rejects two of the
account's orders in a row, whatever their targets (the retry, or an order
to a new target after the boost turned on or off), the program sends no
more buys for the account: that session's decision records the second
rejection (`replan_withheld`), you get **one** urgent page, and no session
buys again, not even at a change of target, until you start a fresh run
you approve (`operator approve`, adding `-clear-incident` if the run also
ended in an incident). The program counts each order when IBKR's first
word on it arrives, however late. A rejection is one more in a row. An
order IBKR accepts sets the count back to none: one with any fill, or any
report from IBKR but "submitted", even after a later order replaced it.
"Submitted" is not enough, because IBKR reports it before some
rejections. But only an order sent **after** the rejections being counted
can end them: if IBKR's word that an older order expired arrives late,
after a rejection, it does not reset the count, so the one retry of a
rejected order, rejected again, is always the second in a row and stops
buys (`TestALateAcceptanceOfAnOlderOrderDoesNotEndTheRunOfRejections`). An order IBKR has said nothing about yet does not count until
it does. So if IBKR rejects an older order late, after a newer one, that
late rejection can be the one that makes two, and that session's decision
records it (`replan_withheld`)
and pages you, whichever order it was and even if the order now watched
was accepted (`TestALateRejectionThatStopsBuysIsRecordedAndPaged`,
`TestALateRejectionAfterManyUndecidedOrdersStopsBuys`). An acceptance that
arrives after the second rejection does not lift the stop. The count stops
at two. Without this stop, a boost that turns on and off at each session
would send IBKR a rejected order every session. Each decision records
three things so a restart from it counts exactly as the run before it did:
the rejections in a row (`venue_rejections`), the orders they name
(`rejected_in_a_row`, with IBKR's ID for each when known; once buys are
stopped, the two rejections that stopped them, until the fresh run), and
every order IBKR has not yet ruled on (`undecided_orders`, with
`undecided_before_rejection`, how many of them were sent before the last
rejection counted). Nothing is dropped from those, so a late word from
IBKR on any order counts as it would have without a restart.

Which orders stay on `undecided_orders`, and when they leave:

- An order IBKR received leaves at its first word beyond "submitted": a
  fill, its expiry or cancellation at the end of its day, or a rejection.
  An order IBKR has only reported "submitted" stays listed. A DAY order
  always gets one such word, so in paper and live the list holds only
  orders still working that day, or whose word from IBKR is late.
- An order the adapter **retired** leaves the list when the engine reads
  the retirement (`execution.order.retired`; the owner's decision of
  2026-10-22): one whose day ended before it could be sent, which the
  adapter retires itself, or one IBKR can never accept, which you retire
  with `relay.py retire` ([operator](operator.md), "Retiring an order the
  adapter can never send"). It counts neither accepted nor rejected and ends
  with nothing filled: a retired sell no longer counts as shares being sold,
  and the next session that decides plans the difference to the target
  again (`replan` cause `unfilled`, naming the order), as for an order that
  expired unfilled (`TestARetiredOrderIsReplannedAtTheNextDecision`,
  `TestARetiredSellIsNoLongerCountedAsBeingSold`). A retirement of an order
  IBKR has named or listed is refused, and so is an IBKR report on a
  retired order afterwards, which stops the program. A dry-run refuses
  every retirement.
- An order the adapter **never sent** for any other reason gets no word
  from IBKR, so it stays on the list, in every decision, until you settle
  it. After `relay.py
  resolve --never-sent`, record `operator rebalance-cleared` naming the
  decision that released it and the order ([operator](operator.md), "A
  rebalance clearance"). The order then leaves the list, counted neither
  accepted nor rejected. That is all the clearance does for Boost: the
  order is still released in the program, so a never-sent sell still makes
  later exits smaller and a never-sent buy is not planned again. Reconcile
  and start a fresh run, as the table below says
  (`TestAClearedNeverSentOrderIsNoLongerUndecided`). A clearance that names
  anything else is refused, and so is a clearance of an order IBKR has
  already named (even as "submitted"), or that IBKR has listed among its
  open orders: that order was sent. Boost records each cleared order under
  `cleared_orders`, with its release, in every later decision, so a run
  restarted from one still counts a cleared sell as sold, and a later
  exit is sized net of it (`TestAClearedNeverSentSellIsRecoveredFromALaterDecision`,
  `TestAClearanceOfAnOrderIBKRListsIsRefused`). A report from IBKR on a
  cleared order after all (a fill, a cancellation, a rejection, even
  "submitted") shows the clearance was wrong: Boost refuses the report,
  which stops the program for you to reconcile with IBKR
  (`TestAVenueReportOnAClearedOrderIsRefused`).
- An order IBKR accepted but that has not ended stays in the record while
  a later order replaces it as the watched one: each decision lists it
  under `working_orders`, with its fills, so a run restarted from the
  decision still reads IBKR's later fills and cancellation of it
  (`TestARunRecoveredBehindAWorkingOrderTheVenueAcceptedCountsAsTheFullRun`).
  That holds too when the restart is from a decision taken while the order
  was still the watched one
  (`TestARunRecoveredWatchingAnOrderTheVenueAcceptedCountsAsTheFullRun`).
  The entry carries IBKR's own order ID once a status report or an
  open-order listing names it, even when IBKR's first word on the order
  was a fill, so after a restart a listing of a working sell that does not
  echo Boost's order ID is still counted once, not as a second sell
  (`TestARunRecoveredBehindAWorkingSellMatchesItsListingByVenueOrderID`).
  A record that lists an unnamed working order, or the same fill under two
  orders, is refused (`TestBoostWorkingOrdersValidate`); so is one whose
  working or watched order states an execution already read on an order this
  run released before the recovery
  (`TestARecoveredOrderStatingAnExecutionAlreadyReadIsRefused`).
- In **dry-run** no order reaches IBKR, so none is ever listed: each
  decision records `no_venue: "dry-run"` instead, and nothing is counted as
  accepted or rejected (`TestADryRunListsNoUndecidedOrder`). A dry-run
  decision that lists a cleared or working order is invalid, there being
  nothing sent to clear.

The page that tells you buys stopped names both rejections, so you can
find both in IBKR's order log. The fresh run starts from none
(`TestWhatTheRejectionsRecordStaysBoundedWhileBuysAreStopped`,
`TestTheStopNamesTheTwoRejectionsThatMadeIt`,
`TestARunWithManyUndecidedOrdersCountsTheRejectionAndRecoversAsTheFullRun`,
`TestTwoRejectionsInARowAwaitTheOwnerAcrossATargetChange`,
`TestAnAcceptedOrderReplacedBeforeItsEndStillEndsTheRunOfRejections`,
`TestARunRecoveredBehindAnUndecidedOrderCountsAsTheFullRun`).

The stop never holds back a sell, because a sell lowers your borrowing.
Boost's exit is still planned and sent while buys are stopped, and so is
the margin alarm's sale back to 98%. If IBKR rejects the exit too, that
rejection is recorded (`rejected_while_stopped`, with the count since the
stop in `rejections_while_stopped`) and paged once as another rejection
while buys are stopped, saying which one since the stop it is and naming
the two rejections that stopped buys, and the
exit is tried again at each session that decides while it is due
(`TestTwoRejectionsInARowStillSendBoostsExit`). A late rejection of an
order released before the stop is recorded and paged the same way
(`TestALateRejectionOfAnOrderReleasedBeforeTheStopIsRecordedWhileStopped`).
So while stopped, a
program sale may be working, or be sent again at the next close. Before
you sell anything by hand, follow the hand-sale steps in
[states](states-and-kill-switch.md#sells-that-keep-going-while-degraded):
stop the adapter, then the engine (the kill switch is not enough: it does not
stop the sale back to 98%), cancel the working sale in IBKR's app, confirm
that no `working`, `pending` or `cancel-pending` sell remains, and only then
sell. Once your sale has filled, start again as a fresh run whose approval
states the book as it is after your sale (`-positions ... -cash ...
-currency USD`, typed right after reading the book in IBKR), never a
resume: the program recorded no fill of your sale, so a fresh run approved
without the book halts on its first reading.
A sell IBKR accepts does not lift the stop; only a fresh run does. The
one retry and this stop were confirmed by the owner on 2026-10-18.

A refusal that moves the account to Degraded or Halted does **not** set up a
re-plan: those states follow their own rules, and the way out is a fresh
run you approve, whose first decision plans to the target from scratch.

Each decision records whether a re-plan is pending after it
(`replan_pending`), so a run restarted from that decision picks it up. A
decision that re-plans, or is withheld while one is pending, also records
why (`replan`): `refused`, or `part-filled`, `unfilled` or `rejected` with
the order's client order ID and its filled and released shares. Each
decision also records the order being watched (`watched`: its client order
ID, side, shares, limit, shares filled so far, once IBKR ended it how, and
IBKR's own ID for it, `venue_order_id`, once IBKR has reported on it), so
that a start which reads the decision could watch the order still. IBKR
does not always repeat the client order ID when it lists an open order, so
a sell being watched is matched to IBKR's listing by that ID too. Any
other listing with no client order ID is counted beside the sell until
IBKR shows they are one order (a listing with the client order ID, or a
report naming the listing's ID). Until then the program counts a working
sell twice, which can only make its next sale smaller, never larger (ADR
0026, amendment of 2026-10-18). Which
of the two starts does what today is in the table below ("The engine
restarts while an order is still working"): a resume after a crash rebuilds
the watch by replaying the log; a fresh run does not watch the earlier
run's order.

## A session with no SPY bar

If a session closes without SPY's bar (the data job failed, or the vendor
sent a bad bar that was dropped), that session is **withheld**
(`missing-bars`) and nothing is traded. The latch does not move. The
session is **skipped**: at the next session with a bar, RSI(2) and both
averages carry on over the closes the engine has, as if the missing session
had not been in the series, and that decision lists the missing sessions
(`gap_sessions`). From there the run decides as normal, so a gap never
blocks Boost's exit (ADR 0026, point 5, amendment of 2026-10-18; rules
version 1.18.0).

Who decided what: you accepted the recommendation that a missing bar must
not freeze the run (2026-10-18). Skipping the session, rather than filling
its close forward as the backtest's engine does, is the orchestrator's
reading, put to you and **awaiting your confirmation**.

This is **not** what the QuantConnect backtest would do. Its engine fills a
missing daily bar forward: it repeats the previous close, feeds that to the
readings and decides on it, as if SPY had not moved. That is a made-up
price, so live trading skips the session instead. The backtest's SPY data
has no missing sessions, so the two never differed in the recorded runs.

You get one urgent page for the gap, however many sessions in a row are
missing ([alerts](alerts-and-dashboard.md)). Find why the bar was missing;
the run carries on by itself.

## Before the first decision: the warm-up

The 200-day average needs 200 closes before it is ready. A run's
configuration names `warm_up_start`, the first session whose close feeds the
readings; sessions before the run's first decision session (`-as-of`) only
feed the readings and decide nothing, as QuantConnect's warm-up does. The
daily data job's one-time warm-up delivers that history
([market data](market-data.md), "The one-time warm-up").

The shipped dry-run file says 2025-01-02, inside the two years of history
Massive's plan reaches back, so the data job can fetch its warm-up. That date
stays inside the window until 2 January 2027; a later warm-up needs a later
date, which is a different configuration (a new hash) and so a new run. A paper
run uses a date inside the window
(`docs/runbooks/first-paper-order.md`, step A5, recommends 2025-01-02).

## The hard limits for this account

From `deploy/etc/engines/boost.config`, `limits` and `execution`:

| Limit | Value | In plain words |
| --- | --- | --- |
| `max_exposure` | 1.5 | A buy is refused if it would leave SPY above 150% of net liquidation, valued at the previous close (the account snapshot's net liquidation is valued at that close too: its cash plus SPY at the official close). A sell is never refused by it, and nothing is sold because the market moved the account above 150% |
| `max_orders_per_decision` | 1 | At most one order that is not risk-reducing per decision |
| `max_order_fraction` | 1 | No single order larger than 100% of net liquidation (Boost's step from 98% to 147% is about 49%) |
| `price_band` | 0.05 | The order's reference price (the raw previous close) must be within 5% of **IBKR's last price** for SPY, read after the close and recorded beside the mark (`broker_last`); when IBKR gives no price, it is compared with the mark itself (the delivered bar's official close), which is the reference price, so it always passes. The order's figures say which was used (`mark_source`: `broker-last` or `official-close`). Plainly: **the band has teeth only when IBKR provides a price**; without IBKR market data the limit price is the only bound on the fill. This is the orchestrator's reading of your decision, put to you (ADR 0026, point 3, amended 2026-10-18). Also sets the limit price above |
| `excess_liquidity_warning`, `_urgent`, `_stale_minutes` | 0.40, 0.20, 30 | The margin-cushion alarm ([states-and-kill-switch](states-and-kill-switch.md#the-margin-cushion-alarm)) |
| `execution.max_snapshot_age_minutes` | 30 | How old IBKR's latest snapshot may be when a sell is checked against it |

A **risk-reducing** sell (one that moves the account toward 98% or lower)
is exempt from the order count, the order size and the price band, so one
late mark cannot block Boost's exit (ADR 0026, points 3 and 4). The
[momentum chapter](momentum.md#the-hard-limits-in-plain-words) explains each
limit once for both strategies.

## One Boost day, from the data to IBKR and back

This is the path a normal paper day takes. Every step is built; the whole
day runs in `deploy/rehearse-paper.sh` with the real engine against a
stand-in for IB Gateway, and is still to be run against IBKR's paper
servers ([first paper order](../runbooks/first-paper-order.md)).

| # | When (New York) | What happens | Where it is explained | Built? |
| --- | --- | --- | --- | --- |
| 1 | 16:00 | The market closes (13:00 on an early close). | [market data](market-data.md) | — |
| 1a | next weekday, 05:30 (again 06:30, 07:30) | The Flex reader reads IBKR's Trade Confirmations and writes the executions proof: every execution from the session's close to midnight. | [reconciliation](reconciliation.md), "The executions proof" | Built |
| 2 | next weekday, 06:05 (retries 07:05, 08:05) | `ti-data` fetches the day's whole-market bars and recent splits and dividends from Massive, records them, and writes the engine's input for the session: SPY's bar, then "the session closed", to `/var/lib/trend-investing/data/streams/boost/<TI_RUN>/boost-<date>.jsonl`. | [market data](market-data.md), "The daily data job" | Built |
| 3 | after 06:05 | The adapter reads the file and the account at IBKR once. That reading is the account as of the close when it is read after the close on the close's own New York date, or (your decision of 2026-10-27) on a later date before the next open with the executions proof showing nothing executed from the close to midnight, IBKR's own list nothing since, no fill recorded after the close and IB Gateway on New York time (until 09:00 the adapter waits for the proof), with no open order for the account at IBKR from any client (working, pending or cancel-pending), IBKR's executions read in full and none at or since the close, every holding with an official close in the file, IBKR's own net liquidation above zero, the holdings those of the previous close adjusted by the fills recorded since, the cash the previous close's adjusted by those fills within 1% of the net liquidation at the close, and no snapshot the engine holds as of this close or later (your decision of 2026-10-18). The previous close is the latest of the last account snapshot of this run or of the runs its approvals chain back to, and the starting book any approval states (the account's first run's, or a fresh run's). It is then valued at the close (cash plus SPY at the official close); otherwise it is stamped at its read time and step 4 withholds the session, except a run's first reading that is not the book: no snapshot is sent, and the account halts as unverifiable (below, "What can go wrong"). It sends, numbered in the engine's one series: the reconciliation of that reading (against the engine's book, before it is replaced), the account snapshot (SPY held and net liquidation), the broker snapshot, SPY's bar, the mark (the bar's official close, IBKR's last price beside it, which the price band compares with when it is set), then "the session closed". | [IBKR adapter](ibkr-adapter.md), [paper trading](paper-trading.md#a-paper-day) | Built |
| 4 | at the input | The engine writes each input to its durable log, flushed to disk, then decides: updates RSI(2) and the averages, moves the latch, plans at most one order and checks it against the hard limits. A released order is written down before anything is told about it. | [engine](engine.md), this chapter, [states-and-kill-switch](states-and-kill-switch.md) | Built (tested against recorded inputs and `make demo-boost-dry-run`) |
| 5 | that morning | The adapter writes "about to send", hands the order to IB Gateway as a DAY limit order under its client order ID, and writes "sent". | [paper trading](paper-trading.md), [re-delivery](redelivery.md) | Built; the real IBKR client places it only in paper with sending switched on; not yet run against IBKR's paper servers |
| 6 | next session | IBKR works the order. Each status change and each fill (with IBKR's commission) comes back; the adapter records them as the engine's inputs. | [paper trading](paper-trading.md), [order lifecycle](order-lifecycle.md) | Built |
| 7 | every 5 minutes | The reporter reads the durable log and sends any alert, and the daily summary. | [alerts and dashboard](alerts-and-dashboard.md) | Built and installed; its alerts need your ntfy and Pushover entries in `secrets.env` ([server](server.md)) |
| 8 | at every start and reconnect of the adapter | The adapter reads IBKR's positions and cash; the engine compares them with its book. | [reconciliation](reconciliation.md) | Built |

In dry-run, step 5 never sends; see the [engine](engine.md), "What dry-run
simulates, and what it does not", for what that leaves out.

### In dry-run: how an exit is sized

In dry-run each order the engine releases is recorded with `send` false and
then, straight after, cancelled by the engine itself: a
`synthetic-lifecycle-canceled` record in `journal-query orders`, marked
synthetic, saying it was never sent. That cancel is the engine's own note,
not IBKR's, and nothing fills. It means the order is over: the engine no
longer counts a dry-run sell as shares still being sold.

So a dry-run exit (147% back to 98%) is sized exactly as the paper account
would size it for the same holding: the shares the latest account snapshot
shows, less the 98% target, with nothing netted for an earlier sell. The
sale back to 98% keeps its own rule: it is sized from the latest broker
snapshot (its holding, net liquidation, mark and open orders, keeping just
enough shares for 98%), as
[the one sale back to 98%](states-and-kill-switch.md#the-one-sale-back-to-98)
describes, and in dry-run it too nets no earlier unsent sell. The sale is
also cancelled by the engine straight after its release, and it still uses
up that alarm's one attempt, so it is not released again at the next broker
snapshot (`TestADryRunBoostSaleBackTo98IsCanceledAndSizedWithNothingNetted`).

A dry-run does not freeze the account. Its own orders never change the
holding, since none is sent, but each decision reads the latest account and
broker snapshots, so anything else that changes the account (a stock split,
a deposit) is still sized from. With no such change the holding stays what
it was at the start, and every exit is sized from it, the first and the
fifth alike, as in the simulated demo (`make demo-boost-dry-run`), whose
account never changes.

If IBKR, or anything else, ever reports a fill or an order update on a
dry-run order, the engine refuses it: no venue received the order, so the
report can only be a fault upstream. The refusal is recorded with the
report and names the order, and nothing in the run changes because of it
(`TestADryRunBoostRefusesAVenueReportOnAnOrderItCanceled`).

A dry-run order is never re-planned because of its
synthetic cancel, and a dry-run resumed in the middle of an exit decides
exactly as one that was never stopped
(`TestADryRunBoostExitIsSizedAsPaperWouldSizeIt`,
`TestADryRunBoostRecordsEachUnsentReleaseCanceled`,
`TestADryRunBoostRestartedMidExitDecidesAsTheFullRun`).

## What you see in the record

Each decided or withheld session writes one `strategy.targets.decided`
record. For Boost it carries the close, RSI(2), both averages (and whether
each was ready), the latch before and after, the target in force,
whether a re-plan is pending and why (`replan`), the rejection that made
two in a row (`replan_withheld`), the rejections in a row
(`venue_rejections`) and the orders they name (`rejected_in_a_row`), the
orders IBKR has not yet ruled on (`undecided_orders`, and
`undecided_before_rejection`; in dry-run `no_venue: "dry-run"` instead),
the orders you cleared as never sent (`cleared_orders`), the orders IBKR
accepted that are still working beside the watched one (`working_orders`),
any rejection after the stop
(`rejected_while_stopped`, counted in `rejections_while_stopped`), the
order being watched
(`watched`, with each of its fills, when it ended and IBKR's ID for it), and any sessions with no bar since the last close it read
(`gap_sessions`). An order adds `strategy.order.planned`, then
`risk.order.released` or `risk.order.refused`. `journal-query orders` and
`journal-query refusals` list them ([journal-query](journal-query.md)).

## What can go wrong, and what the program does then

Alerts come from the reporter ([alerts and dashboard](alerts-and-dashboard.md));
`<account>` is the account the configuration names: `joint` in the shipped
dry-run file, your paper account (DU…) in a paper run. An **urgent**
alert pages you; a **routine** one only informs. A withheld session sends
one alert when a run of withheld sessions with the same reason starts, not
one per session.

| What you see | What the program does | What you do |
| --- | --- | --- |
| `margin-exposure`. Alert: `<account>: order refused (margin-exposure)` (urgent), its text naming the exposure after the order and the 1.5 limit; then `<account>: engine degraded (risk-limit-breached)`, which repeats until you acknowledge it | The risk gate refused a buy that would have left SPY above 150% of net liquidation, valued at the previous close. Nothing was sent. The account is Degraded on SPY: no buy goes, and only the one sale back to 98% may still sell SPY. Because the account is no longer Normal, the buy is **not** planned again. Boost's own target, 147%, is below the limit, so this means the account's figures are not what the plan expected (for example net liquidation fell between the snapshot and the decision, or the account already held more SPY than the engine's book) | Compare SPY held and net liquidation at IBKR with the order's figures (`journal-query refusals`). Follow `docs/runbooks/halted-account.md`. The way out is a fresh run you approve ([restart](restart.md)); its first decision plans to the target from scratch |
| Alert `<account>: decision withheld (missing-bars)` (urgent): a session closed with no SPY bar. One page for the gap, however many sessions in a row are missing; a later gap pages again | That session is withheld and nothing trades. Boost skips it: the next session with a bar decides as normal, its readings computed over the closes it has (the missing close is not filled in), and the decision names the missing sessions (`gap_sessions`). Held positions and orders are unchanged. The run recovers by itself | Find why the bar was missing (`ti-data`'s log, [market data](market-data.md)). Nothing else: do not start a new run for a gap. The rule is explained once, in [engine](engine.md), "What can go wrong" |
| Alert `<account>: decision withheld (stale-account-snapshot)` (urgent): no account snapshot as of the close, or one taken after it. This happens when the adapter's reading after the close could not be stamped as of the close (step 3 above) | The session is withheld and no order is planned. The latch still advances, as the backtest's does; a change of target withheld one session is ordered at the next that decides. See [engine](engine.md), "What can go wrong" | Check why in the adapter's log (the broker snapshot's `not_as_of_close` names each condition of step 3 that failed), and the adapter and the Gateway (`docs/runbooks/gateway-login.md`). A session withheld for a working order or a late execution decides normally the next day. If instead the account halted on a run's first reading, see the next row |
| Alert `<account>: reconciliation unverifiable (<id>)` (urgent) on a run's first reading: the adapter's log says the holdings or cash are not the previous close's (or your starting book's) adjusted by the fills, for example after a mistyped `-positions` or `-cash`, a transfer or a sale by hand | The adapter sends that reading's reconciliation alone, no snapshot, so the engine, holding no book, finds it unverifiable and the account goes **Halted**: no order is released except the one sale back to 98%. Nothing of that reading is taken as the book | Read the holdings and `TotalCashValue` in IBKR and start a fresh run whose approval clears the incident and states that book, typed right after reading it (`operator approve -log <this run's log> -account joint -by owner -clear-incident <id> -positions SPY=<shares> -cash <total cash> -currency USD -out <file>`); never a resume ([reconciliation](reconciliation.md), "What can go wrong"). Approved without the book, the fresh run is compared with the same book and halts again |
| Alert `<account>: order refused (missing-mark)` with `<account>: no recorded mark for SPY`, or `<account>: order refused (price-band)` (urgent) | A buy is refused: no mark at or after the close, or the previous close more than 5% from the mark. The account goes Degraded on SPY. A risk-reducing sell is not checked against the mark. Explained in [states-and-kill-switch](states-and-kill-switch.md), "What can go wrong" | `docs/runbooks/halted-account.md` |
| `<account>: order refused (kill-switch)` (urgent), sent for the first refusal of each engagement only | Every order is refused except the sale back to 98%; the account stays Normal; the order is planned again at the first decided close after you release the switch | Nothing more, once released. Check it with `journal-query orders`. The kill switch itself: [states-and-kill-switch](states-and-kill-switch.md), [operator](operator.md) |
| `<account>: order refused (stale-broker-snapshot)` (urgent), once per outage; for the exit, `<account>: sale of SPY withheld (stale-broker-snapshot)` | IBKR's latest broker snapshot was older than 30 minutes (`execution.max_snapshot_age_minutes`) when a sell was checked. The sell is refused, the state does not change, and it is planned again at the next decision. Explained in [states-and-kill-switch](states-and-kill-switch.md), "What can go wrong" | Check the Gateway and the adapter (`journalctl -u ti-adapter-boost`). The program can still sell: the exit is planned again at the next decision, and a withheld sale back to 98% is decided again from the next good snapshot. So if the exit matters now and you sell by hand, first stop the adapter, then the engine, as [states-and-kill-switch](states-and-kill-switch.md), "Selling by hand safely", says |
| The exit is refused because the account is Halted, Degraded on SPY or under the kill switch | The one sale back to 98% goes instead ([states-and-kill-switch](states-and-kill-switch.md), "The one sale back to 98%"), with its own sent, filled and failed alerts | Follow the alert; do not sell by hand while that sale is working |
| `journal-query orders` shows the DAY order expired, or IBKR cancelled it, with part or all unfilled | The cancellation is recorded and ends the order. The next session that decides after the program has read it plans the rest of the difference to the target (`replan` cause `part-filled` or `unfilled`), unless a newer released order already replaced it. A session that decides without ordering does not cancel the plan. Each order that ends with shares unfilled is planned again at the next session that decides, for as long as it keeps ending unfilled; there is no limit on these re-plans. Only an IBKR rejection is re-planned once and then counts toward the stop (`replan_withheld`, the row below) | Nothing, normally: do not buy or sell the rest by hand as well, the program will. If you must trade by hand, first stop the adapter, then the engine, cancel anything the program has working in IBKR's app, and confirm in IBKR's app that no order is open on SPY ([states-and-kill-switch](states-and-kill-switch.md), "Selling by hand safely"). The kill switch is not enough: it cancels nothing already at IBKR, and it does not stop the sale back to 98%, which a Boost exit it refuses fires |
| `<account>: order <id> rejected, two orders in a row; Boost buys stopped` (urgent), once per rejection that makes two. IBKR rejected an order (`invalid`) | The rejection is recorded. The next session that decides plans it once more (`replan` cause `rejected`). When IBKR rejects two of the account's orders in a row, that one or an order to a new target, even an older order's rejected late, the rejection that makes it two is recorded (`replan_withheld`, with the orders in `rejected_in_a_row` and their count in `venue_rejections`), you get this one urgent page, and the program sends no more buys for the account, a change of target included, until you start a fresh run. Sells still go: Boost's exit (tried again at each decided session while it is due) and the margin alarm's sale back to 98%. A dry-run records `no_venue: "dry-run"` instead and its count never moves | Find the reason in IBKR's order log (`journal-query orders` lists the rejections) and fix it. Then start a fresh run you approve (`operator approve`, adding `-clear-incident` if the run also ended in an incident), whose first decision plans to the target from scratch. Before you trade by hand, stop the adapter, then the engine, cancel anything the program has working in IBKR's app, and confirm in IBKR's app that no order is open on SPY ([states-and-kill-switch](states-and-kill-switch.md), "Selling by hand safely"). The kill switch is not enough: it cancels nothing already at IBKR, and it does not stop the sale back to 98%, which a Boost exit it refuses fires |
| `<account>: order <id> rejected: another rejection while Boost's buys are stopped (<nth> since the stop)` (urgent), once per rejected order | Buys are already stopped (the row above). IBKR rejected one more order: a sell, such as Boost's exit planned again at the next decided session while it is due, or an order released before the stop. It is recorded (`rejected_while_stopped`, counted in `rejections_while_stopped`) and paged as the nth since the stop, naming the two rejections that stopped buys. No buy is sent until you start a fresh run | As the row above: find the reason in IBKR's order log, fix it, start a fresh run you approve. Do not sell by hand until you have stopped the adapter, then the engine, and confirmed nothing is working, as above, because the exit is planned again at each decided session while it is due |
| `journal-query` shows an `execution.order.retired` for an order, and the routine message `<account>: order <id> retired (...)` arrived | The order never left the adapter: its day ended first (`session over`), or you retired it because IBKR could never accept it (`owner retired`). The program reads it as an order that ended with nothing filled: it leaves `undecided_orders`, it is listed under `retired_orders` in every decision after, a retired sell stops counting as being sold, and the next session that decides plans the difference to the target again (`replan` cause `unfilled`). The sale back to 98% is read the same way, with nothing filled; its one attempt is not used up, since nothing was sent, so it is tried again by its own rule at the next broker reading while the exit is still refused or the margin alarm still urgent, under a client order ID of its own. An IBKR report on a retired order afterwards is refused and stops the program, before and after a restart alike | Nothing. If the program stops on an IBKR report for the retired order, the retirement was wrong: reconcile with IBKR and start a fresh run ([order lifecycle](order-lifecycle.md)) |
| The adapter refuses an order for good, for a reason other than its day being over or a shape IBKR can never accept, and you settled it with `relay.py resolve ... --never-sent` | The order stays released in the log. A never-sent **sell** keeps counting as being sold, so every later exit in the run is smaller, or nothing. (A dry-run is different: there the engine itself cancels every order it releases, so nothing is left counting; see "In dry-run: how an exit is sized" above.) A never-sent **buy** is not planned again (the engine never sees the order end, and the target in force has not changed). It stays listed as awaiting a verdict (`undecided_orders`) in every decision until you record `operator rebalance-cleared` for it, which lists it under `cleared_orders` and changes nothing else. Clearing an order IBKR has named or listed is refused, and so is an IBKR report on a cleared order afterwards, which stops the program | Record `operator rebalance-cleared` naming the decision and the order, then reconcile and start a fresh run you approve: its first decision plans to the target from scratch ([re-delivery](redelivery.md), [paper trading](paper-trading.md), [operator](operator.md)) |
| The engine crashed (`systemctl status ti-engine@boost` shows it failed) while the boost was on | Resumed from its log, it carries on in the same latch, neither entering nor exiting by mistake (`TestARestartWhileBoostedNeitherEntersNorExits`) | Resume it (`docs/runbooks/durable-log.md`, "Restarting an engine") |
| The engine restarts while an order is still working | It depends on how it starts. **Resume after a crash** replays the whole log: the order, every fill and report on it already recorded, are read again in order, so the engine watches it exactly as before; a fill IBKR sends again counts once, and a fill or cancel IBKR reports after the crash is read and any remainder planned as in the row above. **A fresh run after your approval** does not watch the earlier run's order at all: you start it after the session's end, when a DAY order has expired, and after reconciling; IBKR's reports on that order are refused as reports on an order this run did not release, and the fresh run's first decided session plans to the target from the account as it stands | After a crash: resume, nothing more. Before a fresh run: wait for the session's end and reconcile, which compares IBKR's open orders too (`docs/runbooks/durable-log.md`, "Start a fresh run after an earlier one") |
| A fresh run is refused, naming the previous log's latch | The engine refuses an approval whose latch is not the one the previous log ended with, so a forgotten latch never switches the boost off | Let `operator approve` write the approval from the previous log ([operator](operator.md), [restart](restart.md)) |

## What backs this

- Plan section 2, "Boost"; plan decision D3; ADR 0026, point 2, "Boost",
  "Order type and price" and "Order identity", point 3 (the limits), point 4
  (risk-reducing), point 5 (state from the record), and its amendment of
  2026-10-18 (re-plan after a refusal) and its amendment of 2026-10-18
  (a session with no SPY bar; an order that ends unfilled); ADR 0019 (the sale back to 98%).
- Parity: Go's RSI(2), both averages, the latch and the target matched
  QuantConnect on every one of 7,166 sessions, within the thresholds in
  [parity](parity.md) (`docs/research/parity-2026-10.md`).
- Tests of the rule, in `internal/targetweight/boost`:
  `TestEntryNeedsRSIStrictlyBelowTheThreshold`,
  `TestEntryIsBlockedAtOrUnderTheTrendMean`,
  `TestEntryNeedsReadyIndicators`,
  `TestExitNeedsACloseStrictlyAboveTheExitMean`,
  `TestLatchedBoostSurvivesACloseBelowTheTrendMean`, `TestTargets`,
  `TestFirstDecisionFromCashIsNinetyEightPercentWithNoBoost`,
  `TestRSIIsWilderSmoothed`, `TestRSIWithNoLossIsOneHundred`,
  `TestMeansIncludeTheCloseAndWaitForTheirLength`; the golden cases
  `TestGoldenBoost` and `TestGoldenBoostExitThenNextDayReentry` (they run
  only with the QuantConnect logs, kept outside the repository).
- Tests of the order, in `internal/orderplan`:
  `TestFirstDecisionBuysToNinetyEightPercentFromCash`,
  `TestBoostEntryRoundsTowardZero`, `TestBoostExitRoundsTowardZero`,
  `TestNoOrderWithoutAStateChange`,
  `TestARefusedOrderIsReplannedAtTheNextDecision`,
  `TestNoOrderWhenTheRoundedQuantityIsZero`,
  `TestLimitPricesStayInsideTheBandOnCents`,
  `TestClientOrderIDIsDeterministicPerTagInstrumentAndPhase`,
  `TestAnExitIsNetOfSellsAlreadyWorking`.
- Tests of the run, in `internal/targetweight`:
  `TestBoostRunDecidesEnterAndExit`,
  `TestAMissingBarIsSkippedAndBoostDecidesAcrossTheGap`,
  `TestAGapWhileBoostedNeverBlocksTheExit`,
  `TestAGapRunIsByteIdenticalReplaysAndRestarts`,
  `TestSyntheticGoldenBoostGap` (a hand-computed case, since QuantConnect's
  logs hold no missing session, built so that skipping the session and
  filling it forward give a different RSI(2) and different averages),
  `TestAPartFilledOrderIsReplannedAtTheNextDecision`,
  `TestAnExpiredUnfilledOrderIsReplannedInFull`,
  `TestAFullyFilledOrderIsNotReplanned`,
  `TestAWithheldSessionKeepsThePartFillReplan`,
  `TestALaterOrderSupersedesAPartFill`,
  `TestACancelRecordedAfterItsExpirySessionIsStillReplanned`,
  `TestAPartFillCompletedAfterAWithheldCloseIsNotReplanned`,
  `TestAPendingPartFillReplanReadsTheLatestFills`,
  `TestAVenueRejectionIsReplannedOnceThenWithheld`,
  `TestACancelAfterARejectionIsReplannedAsAnyOther`,
  `TestARunRecoveredWithAnOrderWorkingStillReplansItsRemainder`,
  `TestARunRecoveredMidPartFillCountsTheFillsBeforeIt`,
  `TestARunRecoveredFromAPendingPartFillReplansIt`,
  `TestAPartFillRunIsByteIdenticalAndReplays`,
  `TestAStaleSnapshotWithholdsButTheLatchAdvances`,
  `TestAMissingMarkRefusesAndDegrades`,
  `TestARestartWhileBoostedNeitherEntersNorExits`,
  `TestARecoveredLatchIsNeverRecomputed`,
  `TestARefusalThatLeavesTheEngineNormalIsReplannedAtTheNextDecision`,
  `TestAWithheldSessionKeepsTheReplanForTheNextDecidedOne`,
  `TestARunRecoveredFromARefusedDecisionReplansIt`,
  `TestJournalIsByteIdenticalOnRerunAndReplays`.
