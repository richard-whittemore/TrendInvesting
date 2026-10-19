# Boost: the joint account's rule, end to end

*For the owner. What the Boost rule decides, when it turns on and off, how
its one order is sized and priced, what happens after an order is refused,
what one Boost day looks like from the data to IBKR and back, and what can
go wrong.*

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

- An order IBKR received leaves at IBKR's first word on it: a fill, its
  expiry or cancellation at the end of its day, or a rejection. A DAY
  order always gets one, so in paper and live the list holds only orders
  still working that day, or whose word from IBKR is late.
- An order the adapter **never sent** gets no word from IBKR, so it stays
  on the list, in every decision, until you settle it. After `relay.py
  resolve --never-sent`, record `operator rebalance-cleared` naming the
  decision that released it and the order ([operator](operator.md), "A
  rebalance clearance"). The order then leaves the list, counted neither
  accepted nor rejected. That is all the clearance does for Boost: the
  order is still released in the program, so a never-sent sell still makes
  later exits smaller and a never-sent buy is not planned again. Reconcile
  and start a fresh run, as the table below says
  (`TestAClearedNeverSentOrderIsNoLongerUndecided`). A clearance that names
  anything else is refused.
- In **dry-run** no order reaches IBKR, so none is ever listed: each
  decision records `no_venue: "dry-run"` instead, and nothing is counted as
  accepted or rejected (`TestADryRunListsNoUndecidedOrder`). The page that tells you buys stopped names
both rejections, so you can find both in IBKR's order log. The fresh run
starts from none
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
stop new orders first (the kill switch where it exists, otherwise stop
the engine), cancel the working sale in IBKR's app, confirm that no
`working`, `pending` or `cancel-pending` sell remains, and only then sell.
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
([market data](market-data.md#the-one-time-warm-up)).

The shipped dry-run file says 2024-01-02, which is outside Massive's
two-year window. The data job therefore refuses to fetch a warm-up for that
file. A run fed by the data job needs a date inside the window; that is a
different configuration, so it starts as a new run. A paper run uses a date inside Massive's
two-year window (the first-paper-order runbook, step A5, recommends
2025-01-02).

## The hard limits for this account

From `deploy/etc/engines/boost.config`, `limits` and `execution`:

| Limit | Value | In plain words |
| --- | --- | --- |
| `max_exposure` | 1.5 | A buy is refused if it would leave SPY above 150% of net liquidation, valued at the previous close. A sell is never refused by it, and nothing is sold because the market moved the account above 150% |
| `max_orders_per_decision` | 1 | At most one order that is not risk-reducing per decision |
| `max_order_fraction` | 1 | No single order larger than 100% of net liquidation (Boost's step from 98% to 147% is about 49%) |
| `price_band` | 0.05 | The order's reference price (the raw previous close) must be within 5% of IBKR's latest recorded price for SPY (the **mark**); also sets the limit price above |
| `excess_liquidity_warning`, `_urgent`, `_stale_minutes` | 0.40, 0.20, 30 | The margin-cushion alarm ([states-and-kill-switch](states-and-kill-switch.md#the-margin-cushion-alarm)) |
| `execution.max_snapshot_age_minutes` | 30 | How old IBKR's latest snapshot may be when a sell is checked against it |

A **risk-reducing** sell (one that moves the account toward 98% or lower)
is exempt from the order count, the order size and the price band, so one
late mark cannot block Boost's exit (ADR 0026, points 3 and 4). The
[momentum chapter](momentum.md#the-hard-limits-in-plain-words) explains each
limit once for both strategies.

## One Boost day, from the data to IBKR and back

This is the path a normal paper day will take. **Stops here today** marks
the first step that is not on `main` yet: up to that point the pieces run
and are tested; from there on, they are built only in parts, or not at all.

| # | When (New York) | What happens | Where it is explained | Built? |
| --- | --- | --- | --- | --- |
| 1 | 16:00 | The market closes (13:00 on an early close). | [market data](market-data.md) | — |
| 2 | 17:05 (retries 18:05, 20:05) | `ti-data` fetches the day's whole-market bars and recent splits and dividends from Massive, records them, and writes the engine's input for the session: SPY's bar, then "the session closed", to `/var/lib/trend-investing/data/streams/boost/<TI_RUN>/boost-<date>.jsonl`. | [market data](market-data.md), "The daily data job" | Built |
| 3 | after 17:05 | **Stops here today.** Something must read that file, add IBKR's account snapshot (SPY held and net liquidation, as of the close) and IBKR's latest SPY price (the mark), number each input in the engine's one series, and send them over the engine's socket. That is the adapter's job, and the adapter does not yet talk to the engine's socket, read the data files, or send account snapshots and marks. | [IBKR adapter](ibkr-adapter.md) | **Not built** |
| 4 | at the input | The engine writes each input to its durable log, flushed to disk, then decides: updates RSI(2) and the averages, moves the latch, plans at most one order and checks it against the hard limits. A released order is written down before anything is told about it. | [engine](engine.md), this chapter, [states-and-kill-switch](states-and-kill-switch.md) | Built (tested against recorded inputs and `make demo-boost-dry-run`) |
| 5 | same evening | The adapter writes "about to send", hands the order to IB Gateway as a DAY limit order under its client order ID, and writes "sent". | [paper trading](paper-trading.md), [re-delivery](redelivery.md) | Built against a stand-in gateway only; the real IBKR client still refuses to place orders, and the adapter's main loop is not on `main` |
| 6 | next session | IBKR works the order. Each status change and each fill (with IBKR's commission) comes back; the adapter records them as the engine's inputs. | [paper trading](paper-trading.md), [order lifecycle](order-lifecycle.md) | Records built against the stand-in; not yet delivered to the engine |
| 7 | every 5 minutes | The reporter reads the durable log and sends any alert, and the daily summary. | [alerts and dashboard](alerts-and-dashboard.md) | Built and installed; its alerts need your ntfy and PagerDuty entries in `secrets.env` ([server](server.md)) |
| 8 | next morning, before the open | The adapter reads IBKR's positions and cash; the engine compares them with its book. | [reconciliation](reconciliation.md) | Engine side built; the adapter's record is not delivered |

In dry-run, step 5 never sends (every order says `send: false`); see the
[engine](engine.md), "What dry-run simulates", for what that leaves out.

## What you see in the record

Each decided or withheld session writes one `strategy.targets.decided`
record. For Boost it carries the close, RSI(2), both averages (and whether
each was ready), the latch before and after, the target in force,
whether a re-plan is pending and why (`replan`), the rejection that made
two in a row (`replan_withheld`), the rejections in a row
(`venue_rejections`) and the orders they name (`rejected_in_a_row`), the
orders IBKR has not yet ruled on (`undecided_orders`, and
`undecided_before_rejection`; in dry-run `no_venue: "dry-run"` instead),
any rejection after the stop
(`rejected_while_stopped`, counted in `rejections_while_stopped`), the
order being watched
(`watched`, with each of its fills, when it ended and IBKR's ID for it), and any sessions with no bar since the last close it read
(`gap_sessions`). An order adds `strategy.order.planned`, then
`risk.order.released` or `risk.order.refused`. `journal-query orders` and
`journal-query refusals` list them ([journal-query](journal-query.md)).

## What can go wrong, and what the program does then

| What happens | What the program does | What you do |
| --- | --- | --- |
| A session closes with no SPY bar | That session is withheld (`missing-bars`) and nothing trades; the session is skipped and the next session with a bar decides as normal, its readings over the closes received, naming the gap (`gap_sessions`). One page per gap. | Find why the bar was missing (`ti-data`'s log, [market data](market-data.md)). Nothing else: the run recovers by itself. |
| No account snapshot as of the close, or one taken after it | The session is withheld (`stale-account-snapshot`). The latch still advances, as the backtest's does; a change of target withheld one session is ordered at the next that decides. | Check the adapter and the Gateway (`docs/runbooks/gateway-login.md`). |
| No mark at or after the close, or the previous close is more than 5% from the mark | A buy is refused (`missing-mark` or `price-band`) and the account goes Degraded on SPY. A risk-reducing sell is not checked against the mark. | `docs/runbooks/halted-account.md`. |
| The kill switch is on | Every order is refused except the sale back to 98%; the account stays Normal; the order is re-planned at the first decided close after you release the switch. | Nothing more, once released. Check it with `journal-query orders`. |
| IBKR's latest snapshot is too old when a sell is checked | The sell is refused (`stale-broker-snapshot`), no state change, one urgent alert per outage, re-planned at the next decision. | Check the Gateway. |
| The exit is refused because the account is Halted, Degraded on SPY or under the kill switch | The one sale back to 98% goes instead ([states-and-kill-switch](states-and-kill-switch.md#the-one-sale-back-to-98)). | Follow the alert; do not sell by hand while that sale is working. |
| The DAY order expires, or IBKR cancels it, with part or all unfilled | The cancellation is recorded and ends the order. The next session that decides after the program has read it plans the rest of the difference to the target (`replan` cause `part-filled` or `unfilled`), unless a newer released order already replaced it. A session that decides without ordering does not cancel the plan. | Nothing, normally. Do not buy or sell the rest by hand as well: the program will. If you must, engage the kill switch first. |
| IBKR rejects the order (`invalid`) | The rejection is recorded. The next session that decides plans it once more (`replan` cause `rejected`). If IBKR rejects two orders in a row, that one or an order to a new target, the rejection that makes it two is recorded (`replan_withheld`), even an older order's rejected late, you get one urgent page, and the program sends no more buys for the account, a change of target included, until you start a fresh run. Sells still go: Boost's exit, tried again at each decided session while it is due (each rejection of it recorded, `rejected_while_stopped`, and paged as another rejection while buys are stopped), and the margin alarm's sale back to 98%. | Find the reason in IBKR's order log (`journal-query orders` lists the rejections) and fix it. Then start a fresh run you approve (`operator approve`, adding `-clear-incident` if the run also ended in an incident), whose first decision plans to the target from scratch. Before any sale by hand, stop new orders with the kill switch on (or stop the engine where there is none), cancel any working program sale in IBKR's app, confirm none remains, and only then sell ([states](states-and-kill-switch.md#sells-that-keep-going-while-degraded)). |
| An order is never sent (the adapter refused it for good and you settled it `--never-sent`) | The order stays released in the log. A never-sent **sell** keeps counting as being sold, so every later exit in the run is smaller, or nothing, as in dry-run. A never-sent **buy** is not re-planned: the engine never sees the order end (the target in force has not changed). It stays on `undecided_orders` in every decision until you record `operator rebalance-cleared` for it, which takes it off that list and does nothing else. | Record `operator rebalance-cleared` naming the decision and the order, then reconcile and start a fresh run you approve: its first decision plans to the target from scratch ([IBKR adapter](ibkr-adapter.md)). |
| The engine crashes while the boost is on | Resumed from its log, it carries on in the same latch, neither entering nor exiting by mistake (`TestARestartWhileBoostedNeitherEntersNorExits`). | Resume it (`docs/runbooks/durable-log.md`). |
| The engine restarts while an order is still working | It depends on how it starts. **Resume after a crash** replays the whole log: the order, every fill and report on it already recorded, are read again in order, so the engine watches it exactly as before; a fill IBKR sends again counts once, and a fill or cancel IBKR reports after the crash is read and any remainder planned as above. **A fresh run after your approval** does not watch the earlier run's order at all: you start it after the session's end, when a DAY order has expired, and after reconciling; IBKR's reports on that order are refused as reports on an order this run did not release, and the fresh run's first decided session plans to the target from the account as it stands. (Each decision also records the watched order, its fills and its end, `watched`, so a restart from that decision would count a fill sent again once and keep a working sell out of later exits: `TestARunRecoveredReadsAFillRecordedAfterItsDecisionOnce`, `TestARunRecoveredWatchingASellSizesLaterSellsNetOfIt`.) | After a crash: resume, nothing more. Before a fresh run: wait for the session's end and reconcile, which compares IBKR's open orders too (`docs/runbooks/durable-log.md`, "Start a fresh run after an earlier one"). |
| A fresh run after a stop | The approval states the latch in force; the engine refuses an approval whose latch is not the one the previous log ended with. | `operator approve` reads it for you ([operator](operator.md)). |

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
