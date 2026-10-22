# An order's lifecycle: committed, released, acknowledged, retired

*For the owner. How to tell from the log where an order stands, and when the
reporter pages you about one.*

## What it does

Every order the risk gate releases passes through three states, and each one
is read from the durable log alone:

| State | What the log shows | What it means |
| --- | --- | --- |
| **committed** | The order's `risk.order.released` record is on disk. | The engine decided it and forced it to disk. It may or may not have reached the adapter yet. |
| **released** | A later input is on disk after it. | The adapter has it. The adapter sends its next input only after it holds the reply to its last one, so a later input proves the reply arrived. |
| **acknowledged** | IBKR's report about it is on disk and the engine read it: an `execution.order.lifecycle` (version 2) naming its client order ID that matches the order as released, or an `execution.fill` (version 5) of it that the run's rule accepts (the shared rule in a momentum run, the risk gate's rule in a Boost run, below). | IBKR has seen the order: accepted it, refused it, cancelled it, or filled it. |
| **retired** | An `execution.order.retired` (version 1) naming it is on disk and the engine read it, and IBKR had said nothing of the order before. | The order never left the adapter, and never will. See "An order IBKR can never accept", below. |

The log keeps every input, including one the engine refused, and does not
mark which were refused. So the reporter reads IBKR's report the way the
engine does, and only a report the engine would accept counts:

- a lifecycle report must name the order's account, instrument, side and
  quantity, and its limit price to within half a cent (the limit is rounded
  to a cent, and IBKR's copy of it need not match to the last digit; prices
  are never compared for exact equality). The engine's risk gate reads it
  by the same match from rules version 1.8.0; 1.6.0 refused a limit that was
  not identical, so a journal holding such a report does not replay across
  the two;
- in a momentum run, a fill counts only on the order's account, instrument and side, when all its fills
  together come to no more than the order's quantity, and under a fill id
  no other execution has used (the same execution sent twice counts once);
  a Boost run judges its fills by the risk gate's rule instead (below);
- in a momentum run, once an order is filled in full or IBKR reports it
  cancelled or invalid, nothing more about it counts.

The engine and the reporter share this rule in one place, so the reporter
cannot count a report the engine refused.

A dry-run engine, momentum or Boost, records its own **synthetic** report on
each order it releases, since it never sends one (`execution.order.lifecycle`
version 3, status canceled, see [the engine](engine.md)). It is the engine's
decision, not IBKR's report, so it acknowledges nothing and counts no fill.
It does end the order, as it did in the engine, so a report from IBKR on
that order afterwards is one the engine refused, and it is "not read".

A **cancelled unsent release** means exactly this: the order was written
down with `send` false, never left the engine, and is over. It is not an
order IBKR cancelled, and nothing about it can still fill. For Boost it also
means the order no longer counts as shares being sold, so the next exit is
sized from the holding alone, as paper would size it ([boost](boost.md),
"In dry-run: how an exit is sized").

A Boost engine now records IBKR's report about an order it released and
decides nothing from it (ADR 0022), beyond what the risk gate's pending-sell
count takes from it: a cancelled or invalid sell's remainder is no longer
being sold (see
[the one sale back to 98%](states-and-kill-switch.md#the-one-sale-back-to-98)).
A report about an order the run did not release is refused, because only
reconciliation can explain it.

A Boost engine also reads IBKR's fills, for the same pending-sell count, by
the risk gate's own check (`risklimit.CheckFill`) rather than the shared rule
above (it does not, for example, refuse a fill on an order already reported
cancelled). The reporter reads a Boost run's fills by that same check, so a
fill it accepts acknowledges the order, even after a cancellation, and a fill
it refuses acknowledges nothing.

IBKR's report on an order carries one of four statuses: submitted (IBKR
reports it working, which is not yet a promise: IBKR's PreSubmitted maps to it and
can still be followed by a rejection, so Boost's count does not take it as the
venue accepting the order), cancel-pending (a cancel was asked for and
the order may still fill), canceled (it no longer works) or invalid (IBKR
refused it). A fill is a separate report. The word "reported" below means
one of these, or a fill.

The reporter (see [alerts and dashboard](alerts-and-dashboard.md)) reads
these states each time it runs, every 5 minutes. For every order marked to
be sent that IBKR has not acknowledged within the allowed time of its
commit, it sends one **urgent** alert, "order … not acknowledged". The
allowed time is the reporter's `-ack-within` setting (default 10 minutes,
which the shipped reporter unit does not change), so the alert can arrive up
to 5 minutes after the bound. It is one alert per order however often the
reporter runs, and it does not repeat. Once the acknowledgement is in the
log, the alert is no longer raised, and if you were already paged, one
**routine** message says "order … acknowledged".

A report from IBKR that the engine refused acknowledges nothing, and gets
its own **urgent** alert, "venue report … not read", once per report (it
does not repeat). That covers any report
the run would read (an order status report or a fill, in either run) that
cannot be read at all, at a version the run does not read
or one that does not validate, whatever order it names: its client order ID
in the alert may be empty, because the report was refused before the order it
names was known, so check IBKR for every order around the time it was
recorded. It also covers a report about an order the run released that the
rule above does not count. A report that can be read about an order the run
never released raises no alert: it is reconciliation's to explain. A fill in
a Boost run that can be read is judged by the risk gate's own rule: one the
rule refuses raises this alert only when its order is already acknowledged,
since otherwise the order's "not acknowledged" alert covers it. The reporter's
other alerts keep coming: one bad report in the log does not stop the
reporter reading the rest of it.

Dry-run orders are recorded with `send` false and are never sent, so they are
never acknowledged and never alert.

## An order IBKR can never accept

Sometimes the adapter refuses an order before it writes "about to send", so
the order never leaves the adapter. Since the owner's decision of
2026-10-22 such an order no longer wedges the run. There are two cases.

**Its session is over.** An order is a limit order for the day it was
released in. If the adapter could not send it that day (most often because
the link to IBKR was down until the next day), it is never sent: its limit
and the engine's checks were for a day that has gone. The adapter now
**retires it by itself**. It checks IBKR first, as for every order: if IBKR
shows anything under the order's client order ID, the order was sent and is
not retired. Otherwise the adapter records an `execution.order.retired`
input for the engine, reason `session over`, retired by `adapter`, with the
adapter's own words for why. That is a refusal of an order that never left
the adapter, never a cancel of an order at IBKR. Then the adapter goes on to
its next order and its next input. You get one **routine** message, "order …
retired (session over)". You are not paged for it.

**IBKR can never accept it as it stands.** The order is not a limit order,
is for a fraction of a share, or the stock has no IBKR contract number in
the adapter's configuration. That will not cure itself, so the adapter
**stops at that order**, every time it is started, and sends nothing more.
Because the order never reaches IBKR, the reporter's **urgent** "order … not
acknowledged" page fires for it within the usual 10 minutes, and the
adapter's stop names the order and the command to run. Find the cause, then
retire the order yourself with `relay.py retire` (see
[operator](operator.md), "Retiring an order the adapter can never send").
That records the same `execution.order.retired` input, reason `owner
retired`, with your name and your reason, and when you start the adapter
again it goes on. You then get the same routine "order … retired (owner
retired)" message.

Either way, in every strategy, the engine reads the retirement as an order
that ended with nothing filled. Boost no longer waits for IBKR's word on it,
no longer counts a retired sell as shares being sold, and plans the
difference to its target again at its next decision (recorded as a re-plan
for an `unfilled` order, [boost](boost.md)). The sale back to 98% is read the
same way: nothing was sent, so its one attempt is not used up, and it is
tried again by its own rule at the next chance (the next broker reading
while what fired it still holds), as a new order with a client order ID of
its own. The reporter counts it as the engine does: a retired order no
longer awaits IBKR's word in Boost's count of rejections in a row, so the
state-at-start message never names it as a rejection that stops buys. In a
momentum rebalance a retired sell ends the rebalance's buying: its buys,
which would have been bought from that decision's prices and cash, are never
planned, and once its orders are all finished the next decision decides
afresh. A report from IBKR on a
retired order afterwards proves the retirement wrong: the engine refuses it
and the reporter raises the "venue report … not read" alert for it. A
dry-run never sends an order, so the engine refuses any retirement there.
Engines before target-weight rules version 1.23.0 did not read a
retirement at all, so a journal holding one does not replay under them.

## Why

Journal-before-send (ADR 0027) means an order on disk is only *possibly*
sent. Without a check, an order that was lost between the engine and IBKR
would sit unnoticed until the next reconciliation. ADR 0019 asks that every
incident reach a person; an order IBKR never acknowledged is one.

## The rules and their sources

- ADR 0027 §4: durable means possibly sent.
- ADR 0022: an order lifecycle report is recorded evidence, not a decision.
- ADR 0026, Consequences: a target-weight order is a limit order, reported at
  lifecycle version 2 and fill version 5.
- ADR 0019, "Alerting": every incident reaches the owner, once.
- The owner's decision of 2026-10-22 (ADR 0026, point 2, "Boost", amended
  2026-10-22): an order whose session is over is retired by the adapter and
  reported as a routine notice; one IBKR can never accept stops the adapter
  and is retired by the owner. Every retirement ends the order with nothing
  filled, the sale back to 98% included, which spends no attempt (ADR 0019,
  amended 2026-10-17, reading 4, as refined 2026-10-22).

## The tests

- `internal/report`, `TestAReleasedOrderIsCommittedThenReleasedThenAcknowledged`.
- `internal/report`, `TestAFillAcknowledgesAnOrder`.
- `internal/report`, `TestOnlyAReportTheEngineReadsAcknowledgesAnOrder`: a
  report with other figures, or a fill larger than the order (in a Boost run
  the gate refuses it), acknowledges nothing; a limit differing by a rounding
  error does.
- `internal/report`, `TestEveryReportTheEngineRefusesIsUnread`: a lifecycle
  report at another version, fills past the order's quantity in total, a
  reused fill id, and a report on a finished order each raise the "not read"
  alert.
- `internal/event`, `TestOrderReportsReadsAReportAsTheMomentumRunDoes`: the
  shared rule itself.
- `internal/report`, `TestAnUnreadVenueReportRaisesItsOwnAlertOnce` and
  `TestOrderLifecyclesReadsOnlyWhatItCanRead`.
- `internal/targetweight`, `TestBoostRecordsAReportOnlyForAnOrderItReleased`
  and `TestMomentumVenueReportRules`: the engine accepts a limit within half a
  cent and refuses one a cent away.
- `internal/report`,
  `TestAnOrderNotAcknowledgedInTimeRaisesOneAlertThatItsAcknowledgementClears`.
- `cmd/reporter`,
  `TestTheReporterAlertsOnceOnAnUnacknowledgedOrderUntilItIsAcknowledged` and
  `TestTheAcknowledgementBoundMustBePositive`.
- `internal/report`, `TestABoostFillAcknowledgesAnOrderByTheGatesRule` and
  `TestAnUnreadableBoostFillPagesByItsInputID`: a Boost fill that cannot be
  read pages as not read, keyed by its input ID, even on an acknowledged
  order.
- `cmd/reporter`, `TestAnUnreadVenueReportDoesNotSilenceTheOtherAlerts`: an
  unreadable report raises its own alert once and the unacknowledged-order
  alert still reaches you.
- `internal/report`, `TestARetiredOrderIsARoutineNoticeNeverAPage`: a
  retired order gets one routine notice and is never paged as
  unacknowledged; a retirement the engine refuses retires nothing; a report
  on a retired order afterwards is "not read".
- `internal/report`, `TestARetiredOrderIsNoRejectionInTheStanding`: a
  report on a retired order is no rejection on the state-at-start message.
- `internal/targetweight`, `TestBoostVerdictsRetireAnOrderNeverSent`: Boost's
  count, shared by the engine and the reporter, takes a retired order off
  the orders awaiting IBKR's word and refuses one IBKR has named.
- `internal/event`, `TestOrderReportsRetiresOnlyAnOrderNoVenueReceived` and
  `TestTheIBKRAdaptersRetirementIsReadByOrderReports`: the shared rule, and
  the adapter's own retirement read by it.
- `internal/targetweight`, `TestARetiredOrderIsReplannedAtTheNextDecision`,
  `TestARetiredSellIsNoLongerCountedAsBeingSold`,
  `TestARetirementOfAnOrderAVenueMayHaveIsRefused`,
  `TestAVenueReportOnARetiredOrderIsRefused`,
  `TestARunWithARetirementIsByteIdenticalAndReplays`,
  `TestARetiredOrderIsRecoveredAsTheFullRun`,
  `TestARetiredOrderSurvivesARecoveryFromAWithheldDecision`,
  `TestRetiredOrdersEndARebalanceInProgress` and
  `TestRetiringARebalancesSellsNeverStartsItsBuys`.
- `internal/targetweight`, `TestARetiredSaleBackTo98IsReattempted`,
  `TestARetiredUrgentSaleReleasedOutsideADecisionIsReattempted`,
  `TestAVenueReportOnARetiredSaleIsRefused`,
  `TestARetirementOfASaleAVenueReportedOnIsRefused`,
  `TestADecisionRecordsTheSaleItsOwnSessionAttempts` and
  `TestARunsOwnSaleRetiredAfterARestartIsAttemptedAgain`;
  `internal/risklimit`, `TestARetiredSaleSpendsNoAttempt`,
  `TestARetiredUrgentSaleIsAttemptedAgainWhileTheAlarmIsUrgent` and
  `TestARunsOwnLiveSaleIsHeldAsItsOwnAfterARestart`;
  `internal/restart`, `TestReadEndStateFindsWhatAFreshRunMustBeToldOf`: a
  retired sale back to 98% spent no attempt and is tried again as a new
  order.
- `internal/targetweight`, `TestARecoveredSaleAVenueNamedWorksOnAsInTheFullRun`
  and `TestARecoveryReadsADecisionRecordedAtVersion7`; `internal/risklimit`,
  `TestARunsOwnLiveSaleAVenueNamedIsRestoredWithItsExecutions`: a sale back
  to 98% IBKR acknowledged or part filled is kept, filled as far as it was
  and named as it was, by a run restarted from a decision, so IBKR's later
  reports on it are read and it is not tried twice
  ([states and the kill switch](states-and-kill-switch.md)).
- `internal/targetweight`, `TestARecoveredPartFilledSaleCountsEachExecutionOnce`:
  an execution of that sale IBKR sends again after the restart counts once,
  its commission with it.
  `TestAVenueReportGivingAnotherOrderTheRecoveredSalesVenueIDIsRefused`: a
  report giving another order the sale's IBKR order ID is refused when it
  arrives. `TestARefusedRecoveryRestoresNothing`, and `internal/risklimit`,
  `TestARecoveryIsRestoredWholeOrNotAtAll`: a decision refused for any part
  of what it restores restores none of it.
- `internal/report`,
  `TestABoostRetirementOfAnOrderTheCountDoesNotHoldIsReadByItsOwnRule` and
  `TestARetirementOfAnOrderTheVenueNamedLeavesItsRejectionCounted`.
- `adapter/ibkr`, `RetireTests`:
  `test_an_owners_retirement_a_stop_left_unfinished_is_finished_and_never_sent`
  and `test_never_sent_is_refused_for_an_order_ibkr_can_never_accept`.
- `cmd/engine`, `TestABoostEngineRecordsTheAcknowledgementOfAnOrderItReleased`
  and `TestTheLogRecordsAnOrderCommittedThenReleasedThenAcknowledged`: a real
  paper engine, fed over its socket, records the order and its states.

## What can go wrong, and what the program does then

Alerts come from the reporter, whose urgent alerts do not repeat; see
[alerts and dashboard](alerts-and-dashboard.md) for how they reach you.
`<account>` is the account name in the configuration, such as `joint`
([glossary](glossary.md), "Account names").

| What you see | What the program does | What you do |
| --- | --- | --- |
| Urgent alert `<account>: order <client order ID> not acknowledged`; its text says either that no input followed the order or that the adapter has it | The reporter alerts once, when the order has gone unacknowledged for 10 minutes (the default) from its commit. No input followed: the order is committed and no later input is on disk, so the adapter may never have received it (a crash right after the engine wrote it). The adapter has it: the order is released, so it was sent and lost, or is still waiting at the adapter. | In both cases, look in IBKR for an order whose `orderRef` is the client order ID **before** placing anything by hand: a second order with the same tag is one reconciliation cannot explain. No input followed: make sure the adapter is running; on reconnect it asks the engine for that reply again and sends the order once ([re-delivery](redelivery.md)). The adapter has it: if IBKR shows nothing and the adapter is stopped on the order, follow the adapter's stop in [re-delivery](redelivery.md), "What can go wrong". |
| Routine message `<account>: order <client order ID> acknowledged` | IBKR's report on the order is now in the log and was read by the engine. The earlier "not acknowledged" alert is cleared. It is sent once, and only if you were paged. | Nothing. |
| Routine message `<account>: order <client order ID> retired (<reason>)`, where the reason is `session over` or `owner retired` | The order never left the adapter and was retired: by the adapter, because its day ended before it could be sent, or by you, with `relay.py retire`. The text names the order, who retired it, when, and why. The engine has read the retirement, so it re-plans at its next decision. The message is sent once, whether or not the order was paged as "not acknowledged" first, and it answers that page. | Nothing for `session over`, beyond finding out why the order could not go out on its day (usually the link to IBKR was down; see [paper trading](paper-trading.md)). For `owner retired`, nothing more. If IBKR ever shows an order under that client order ID, the retirement was wrong: the adapter stops ("was settled as never sent, but IBKR shows"), so follow [re-delivery](redelivery.md) and reconcile. |
| Urgent alert `<account>: venue report <input> not read` | The log holds a report from IBKR that the engine refused: it is at a version the run does not read, does not validate, does not describe the order it names as it was released, takes it past its quantity, reuses a fill id, or arrives after the order finished. It acknowledges nothing. The reporter alerts once per report and goes on with its other alerts. The client order ID in the text may be empty. | Look in IBKR at the order the alert names, or at every order around the time it was recorded when the ID is empty. If that order's "not acknowledged" alert is also open, treat the order as unconfirmed and follow that alert first. |
| Urgent alert `<account>: operator clearance <input> not read` | On a Boost account, the log holds a clearance you journalled that the engine did not apply because the record is unreadable: it is at a version the run does not read, does not decode, or does not validate (for example, it names an order twice). It clears nothing, so each order it names still counts as awaiting the venue's word. The text names the clearance's orders when they can be read. One alert per clearance on every channel. | Check which of those orders IBKR never received, then journal a corrected clearance. |
| Urgent alert `<account>: operator clearance <input> refused` | On a Boost account, the log holds a clearance you journalled that the engine's rule refuses as a whole: it names an order this run never released (or one the named decision did not release), an order the venue has already named, or it is for another account. It clears nothing, so each order it names still counts as awaiting the venue's word. The text names the clearance's orders when they can be read. One alert per clearance on every channel. | Check which of those orders IBKR never received, then journal a corrected clearance. |
| The engine refuses a lifecycle report naming an order "which this run did not release" | IBKR reported an order this run never released, for example one left working by the previous run. The engine applies nothing from it, and the reporter raises no alert for it. It is reconciliation's to explain. | Follow [reconciliation](reconciliation.md) and `docs/runbooks/reconciliation.md`. |
| No alert ever fires in dry-run | Expected: dry-run orders are recorded with `send` false and never sent, so none is ever waiting for IBKR, and the engine's own synthetic report acknowledges nothing. | Nothing. |

Two things the reporter does not do. A fresh run after an approval does not
know the previous run's orders that are still working, so IBKR's report
about one of them is refused by the engine and raises no "not read" alert.
And a Boost run's fills are read by the risk gate's rule, which the reporter
shares: an accepted fill acknowledges its order, and a refused one raises the
"not read" alert only when its order is already acknowledged.
