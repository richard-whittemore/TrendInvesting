# Paper trading: the one mode that sends orders, and only to a paper account

*For the owner. What the program sends in paper trading, what it never
sends, what you will see, and what it does when something goes wrong.*

**Where this stands.** The sending path is built and tested against a
stand-in for IB Gateway (a "fake gateway" that answers like IBKR, with
invented accounts and prices). It has not yet sent an order to IBKR's real
paper servers: that test waits for your paper account. Until it is run and
reviewed, the program's real IBKR client still refuses to place any order.

## What sends

Only an engine and an adapter both started in **paper** mode, on a **paper
account**, send anything. IBKR's paper account numbers begin `DU`; live
account numbers never do. The check is made in three places, and any one of
them stops the order:

1. **The engine** refuses to start if the mode you start it with is not the
   mode in its configuration, or if a paper configuration names an account
   that does not begin `DU`.
2. **The adapter's configuration** is refused the same way: paper must name a
   `DU` account.
3. **The adapter's sender** checks again before every order: it sends only
   on a connection set up from that paper configuration, and only an order
   for that same account.

When those all agree, one order goes out like this:

1. The engine decides, checks the order against the hard limits, writes it to
   its durable log, and only then hands it to the adapter marked `send: true`
   (see [engine](engine.md) and [re-delivery](redelivery.md)).
2. The adapter checks the order is one it can send right now: the link to IB
   Gateway is up, IBKR's open orders, trades and balances have been matched
   since the link last came up, and the order is a whole-share limit order
   for the session (a DAY order) on a stock it knows IBKR's contract number
   for. If any of that fails, it sends nothing and records nothing about the
   order, so it can be sent later without a manual check.
3. It writes "about to send" for that order on disk, sends it to IB Gateway
   under IBKR's next order number, then writes "sent". Each order number is
   used once, even if the send failed partway.
4. IBKR's answers come back and are recorded for the engine:
   - when IBKR accepts the order (it is **working**), is **cancelling** it,
     has **cancelled** it, or has **refused** it, with IBKR's own reason;
   - each **fill**: shares, price, IBKR's commission and IBKR's time of the
     trade. A partly filled order shows one fill per execution.

   Each report describes the order as IBKR holds it, not as the adapter
   meant it, so the engine can see if the two differ and refuse the report.

## What never sends

- **Dry-run.** Every order is marked "do not send" and the adapter never
  sends it.
- **Live.** The engine refuses to start in live mode, and the adapter refuses
  to build a live sender, until the limited-live gate (plan section 9,
  Phase 5). That is a separate change, reviewed on its own.
- **A paper configuration pointed at a live account**, or the reverse: both
  are refused before anything starts.
- **An order sent before.** The adapter never sends the same order (its
  client order ID, which IBKR shows as `orderRef`) twice, after a reconnect
  or a restart; see below.
- **An order released in another mode** than the adapter's, or one not
  marked to be sent.

## What you will see

- In the engine's log: each order released with `mode: paper` and
  `send: true`.
- In the adapter's records: the order handed over, then IBKR's reports on it
  (working, cancelling, cancelled or refused) and each fill with its
  commission. The engine reads these the same way it will read live ones.
- In IBKR's paper account (Client Portal or the mobile app, switched to the
  paper account): the order, with your client order ID in its order
  reference.

## What can go wrong, and what the program does

| What happens | What the program does | What to do |
| --- | --- | --- |
| The link to IB Gateway drops after an order was sent | It reconnects with the same client ID, then matches IBKR's open orders, trades and balances before anything else is sent. The order already sent is not sent again. | Nothing, unless the reconnect gives up; then see `docs/runbooks/lightsail-deployment.md`. |
| The adapter stops after IBKR took the order but before it wrote "sent" | On restart it finds the order in IBKR's open orders or trades and does not send it again. | Nothing. |
| The adapter stops mid-send and IBKR shows no trace of the order | It stops with "was being handed to the broker when the adapter stopped" and sends nothing more. | Follow [re-delivery](redelivery.md), "What can go wrong": look in IBKR first, then settle it with the resolve command. |
| The link is down, or not yet matched to IBKR, when an order is due | The order is refused before "about to send" is written, so nothing is recorded and nothing goes out. | Nothing; it goes out once the link is up and matched. |
| IBKR refuses the order | It records the refusal with IBKR's reason, and the engine reads the order as ended. | Read IBKR's reason. A margin or permission refusal in paper usually means a paper account setting to change. |
| IBKR reports on the order in a form the engine cannot read exactly (part shares, a commission in another currency, an order it never showed) | The adapter stops rather than record an approximation. | A fault to investigate before starting again; check the order in IBKR. |
| You start the engine or adapter in live mode | It refuses to start and says the limited-live gate has not been passed. | Use paper. |
| A paper configuration names an account not beginning `DU` | It refuses to start. | Correct the account number in the configuration (a new configuration and a new run). |

## The rules and their sources

- AGENTS.md rule 5: no live-order behaviour before the paper-trading and
  limited-live gates.
- ADR 0025 point 3 and the engine's configuration rule: a paper
  configuration names a paper account, and a live one does not.
- ADR 0025 point 5: on every start and reconnect, IBKR's orders, trades and
  balances are matched before any order is accepted.
- ADR 0027 and [re-delivery](redelivery.md): journal before send, and each
  order sent once.
- ADR 0022 and ADR 0026 point 2: what IBKR's reports become, and the order
  as a whole-share DAY limit order.
- ADR 0025 point 4, amended 2026-10-18: IBKR's own TWS API client (`ibapi`
  10.51.01) is pinned by its checksum and fetched and verified on the
  server, never stored in the repository.

## The tests

- `cmd/engine`, `TestSendingIsEnabledOnlyInPaperUntilTheLimitedLiveGate`: a
  live engine on a live account refuses to start and leaves no log.
- `cmd/engine`, `TestTheAccountMustMatchTheMode`: a paper configuration on a
  live account, or a live one on a paper account, is refused; paper on a
  paper account starts.
- `adapter/ibkr/tests/test_config.py`,
  `test_the_account_is_bound_to_the_mode`: the adapter's configuration
  refuses the same mismatches.
- `adapter/ibkr/tests/test_sender.py`: paper sends one order to the gateway
  and records it; live is refused; paper is refused on any connection that is
  not the paper configuration's; nothing is sent before reconciliation or
  without the link, or for another account, a market order or part shares;
  an order number is never reused after a failed send.
- `adapter/ibkr/tests/test_paper.py`, against the fake gateway:
  - `test_one_spy_order_is_sent_and_its_status_and_fills_are_journalled_as_the_engine_reads_them`:
    one SPY order, accepted, filled in two parts; the records are
    `tests/testdata/paper_reports.json`;
  - `test_a_rejection_is_journalled_as_invalid_with_ibkrs_reason` and
    `test_a_cancellation_is_journalled_as_cancel_pending_then_canceled`;
  - `test_a_crash_between_the_send_and_its_mark_never_resends`: IBKR took
    the order and the adapter stopped before writing "sent"; after a restart
    it is not sent again;
  - `test_a_crash_before_ibkr_shows_the_order_stops_rather_than_resend`;
  - `test_an_order_the_link_cannot_take_yet_leaves_no_mark_and_goes_out_once_reconciled`;
  - `test_a_reconnect_never_resends_an_order_already_sent`.
- `internal/event`,
  `TestTheIBKRAdaptersPaperReportsAreReadByOrderReports`: the engine reads
  every record the adapter's paper test wrote, by the same rule it reads
  every venue report, to the order's end.

Not yet tested: sending to IBKR's real paper servers. That is the second
half of this work and is run by hand once your paper account is open.
