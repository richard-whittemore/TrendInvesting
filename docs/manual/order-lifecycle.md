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
| **acknowledged** | IBKR's report about it is on disk: an `execution.order.lifecycle` (version 2) or an `execution.fill` (version 5) naming its client order ID. | IBKR has seen the order: accepted it, refused it, cancelled it, or filled it. |

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
- `internal/report`,
  `TestAnOrderNotAcknowledgedInTimeRaisesOneAlertThatItsAcknowledgementClears`.
- `cmd/reporter`,
  `TestTheReporterAlertsOnceOnAnUnacknowledgedOrderUntilItIsAcknowledged` and
  `TestTheAcknowledgementBoundMustBePositive`.
- `cmd/engine`, `TestABoostEngineRecordsTheAcknowledgementOfAnOrderItReleased`
  and `TestTheLogRecordsAnOrderCommittedThenReleasedThenAcknowledged`: a real
  paper engine, fed over its socket, records the order and its states.

## What can go wrong

| What you see | What it means | What to do |
| --- | --- | --- |
| "order … not acknowledged", and the alert says no input followed it | The adapter may never have received the order (a crash right after the engine wrote it). | Make sure the adapter is running: on reconnect it asks for that reply again and sends the order once ([re-delivery](redelivery.md)). |
| "order … not acknowledged", and the adapter has it | The order was sent and lost, or is still waiting at the adapter. | Look in IBKR for an order whose `orderRef` is the client order ID **before** placing anything by hand. A second order with the same tag is one reconciliation cannot explain. |
| The engine refuses a lifecycle report naming an order "which this run did not release" | IBKR reported an order this run never released, for example one left working by the previous run. | The run stops on it. This is a reconciliation question: follow `docs/runbooks/reconciliation.md`. |
| No alert ever fires in dry-run | Expected: nothing is sent, so nothing is acknowledged. | Nothing. |

What this does **not** do yet: a fresh run after an approval does not know the
previous run's orders that are still working, so IBKR's report about one of
them stops the run. Boost does not yet read fills. Both are open issues
linked from the slice's ticket.
