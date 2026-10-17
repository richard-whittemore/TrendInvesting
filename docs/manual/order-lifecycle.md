# An order's lifecycle: committed, released, acknowledged

*For the owner. How to tell from the log where an order stands, and when the
reporter pages you about one.*

## What it does

Every order the risk gate releases passes through three states, and each one
is read from the durable log alone:

| State | What the log shows | What it means |
| --- | --- | --- |
| **committed** | The order's `risk.order.released` record is on disk. | The engine decided it and forced it to disk. It may or may not have reached the adapter yet. |
| **released** | A later input is on disk after it. | The adapter has it. The adapter sends its next input only after it holds the reply to its last one, so a later input proves the reply arrived. |
| **acknowledged** | IBKR's report about it is on disk and the engine read it: an `execution.order.lifecycle` (version 2) naming its client order ID that matches the order as released, or, for a momentum run, an `execution.fill` (version 5) of it. | IBKR has seen the order: accepted it, refused it, cancelled it, or filled it. |

The log keeps every input, including one the engine refused, and does not
mark which were refused. So the reporter reads IBKR's report the way the
engine does, and only a report the engine would accept counts:

- a lifecycle report must name the order's account, instrument, side and
  quantity, and its limit price to within half a cent (the limit is rounded
  to a cent, and IBKR's copy of it need not match to the last digit; prices
  are never compared for exact equality);
- a fill counts only in a momentum run, since Boost does not read fills yet,
  and only on the order's account, instrument and side, when all its fills
  together come to no more than the order's quantity, and under a fill id
  no other execution has used (the same execution sent twice counts once);
- in a momentum run, once an order is filled in full or IBKR reports it
  cancelled or invalid, nothing more about it counts.

The engine and the reporter share this rule in one place, so the reporter
cannot count a report the engine refused.

A Boost engine now records IBKR's report about an order it released and
decides nothing from it (ADR 0022). A report about an order the run did not
release is refused, because only reconciliation can explain it.

The reporter reads these states. For every order marked to be sent that IBKR
has not acknowledged within the allowed time of its commit, it sends one
urgent alert, "order … not acknowledged". The allowed time is the reporter's
`-ack-within` setting (default 10 minutes). It is one alert per order however
often the reporter runs. Once the acknowledgement is in the log, the alert is
no longer raised, and if you were already paged, a routine message says
"order … acknowledged".

A report from IBKR about an order the run released that the engine refused
acknowledges nothing. That covers a report at a version the run does not
read, one that does not validate, and one the rule above does not count. It
gets its own urgent alert, "venue report … not read", once. Two kinds of
report raise no alert: one about an order the run never released, which is
reconciliation's to explain, and a fill in a Boost run, which does not read
fills yet. The reporter's
other alerts keep coming: one bad report in the log does not stop the
reporter reading the rest of it.

Dry-run orders are recorded with `send` false and are never sent, so they are
never acknowledged and never alert.

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

## The tests

- `internal/report`, `TestAReleasedOrderIsCommittedThenReleasedThenAcknowledged`.
- `internal/report`, `TestAFillAcknowledgesAnOrder`.
- `internal/report`, `TestOnlyAReportTheEngineReadsAcknowledgesAnOrder`: a
  report with other figures, a fill in a Boost run, or a fill larger than the
  order acknowledges nothing; a limit differing by a rounding error does.
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
- `cmd/reporter`, `TestAnUnreadVenueReportDoesNotSilenceTheOtherAlerts`: an
  unreadable report raises its own alert once and the unacknowledged-order
  alert still reaches you.
- `cmd/engine`, `TestABoostEngineRecordsTheAcknowledgementOfAnOrderItReleased`
  and `TestTheLogRecordsAnOrderCommittedThenReleasedThenAcknowledged`: a real
  paper engine, fed over its socket, records the order and its states.

## What can go wrong

| What you see | What it means | What to do |
| --- | --- | --- |
| "order … not acknowledged", and the alert says no input followed it | The adapter may never have received the order (a crash right after the engine wrote it). | Make sure the adapter is running: on reconnect it asks for that reply again and sends the order once ([re-delivery](redelivery.md)). |
| "order … not acknowledged", and the adapter has it | The order was sent and lost, or is still waiting at the adapter. | Look in IBKR for an order whose `orderRef` is the client order ID **before** placing anything by hand. A second order with the same tag is one reconciliation cannot explain. |
| The engine refuses a lifecycle report naming an order "which this run did not release" | IBKR reported an order this run never released, for example one left working by the previous run. | The run stops on it. This is a reconciliation question: follow `docs/runbooks/reconciliation.md`. |
| "venue report … not read" | The log holds a report from IBKR that the engine refused: it is at a version the run does not read, does not validate, does not describe the order it names as it was released, takes it past its quantity, reuses a fill id, or arrives after the order finished. It acknowledged nothing. | Look in IBKR at the order the alert names. If its "not acknowledged" alert is also open, treat the order as unconfirmed and follow that alert first. |
| No alert ever fires in dry-run | Expected: nothing is sent, so nothing is acknowledged. | Nothing. |

What this does **not** do yet: a fresh run after an approval does not know the
previous run's orders that are still working, so IBKR's report about one of
them stops the run. Boost does not yet read fills. Both are open issues
linked from the slice's ticket.
