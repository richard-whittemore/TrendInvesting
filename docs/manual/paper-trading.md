# Paper trading: the one mode that sends orders, and only to a paper account

*For the owner. What the program sends in paper trading, what it never
sends, what you will see, and what it does when something goes wrong.*

**Where this stands.** The sending path is built and tested against a
stand-in for IB Gateway (a "fake gateway" that answers like IBKR, with
invented accounts and prices). It has not yet sent an order to IBKR's real
paper servers: that test waits for your paper account. Until it is run and
reviewed, the program's real IBKR client still refuses to place any order.
The adapter's main loop (the part that keeps reading IB Gateway's messages
and passing them on) is not in this build either. Until it is, the stop
after `commission_wait` below happens only when the next message about one
of this engine's orders arrives, not on a timer.

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
   on a connection set up from that paper configuration, only an order for
   that same account, and only if the IBKR login the adapter is connected
   to says it manages that account (IBKR sends that list as each session
   starts). If the login does not manage it, the adapter also refuses to
   treat the link as matched to IBKR, so nothing is sent at all.

When those all agree, one order goes out like this:

1. The engine decides, checks the order against the hard limits, writes it to
   its durable log, and only then hands it to the adapter marked `send: true`
   (see [engine](engine.md) and [re-delivery](redelivery.md)).
2. The adapter checks the order is one it can send right now: the link to IB
   Gateway is up, IBKR's open orders, trades and balances have been matched
   since the link last came up, the IBKR client in use is allowed to place
   orders (the real one is not, until the paper-account test), and the order
   is a whole-share limit order for the session (a DAY order) on a stock it
   knows IBKR's contract number for, released in **today's** session (New
   York date). If any of that fails, it sends nothing and records nothing
   about the order, and no order number is used. Whether it can go out
   later depends on why it was refused: see "The link is down" and the rows
   after it below.
3. It writes "about to send" for that order on disk, sends it to IB Gateway
   under IBKR's next order number, then writes "sent". Each order number is
   used once, even if the send failed partway.
4. IBKR's answers come back and are recorded for the engine:
   - when IBKR accepts the order (it is **working**), is **cancelling** it,
     has **cancelled** it, or has **refused** it, with IBKR's own reason;
   - each **fill**: shares, price, IBKR's commission and IBKR's time of the
     trade. A partly filled order shows one fill per execution. A fill is
     recorded only once IBKR's commission report for it arrives, which IBKR
     sends just after the trade. If it has not arrived within the
     configured wait (`commission_wait`, in seconds, in the adapter's
     configuration), the adapter stops rather than leave a trade unrecorded.

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
| The adapter stops after IBKR took the order but before it wrote "sent", and the order is still working or has traded | On restart it finds the order in IBKR's open orders or trades and does not send it again. | Nothing. |
| The adapter stops after IBKR took the order but before it wrote "sent", and IBKR cancelled the order (or it expired) before the restart | A cancelled order is in neither IBKR's open orders nor its trades, so on restart the adapter cannot tell it from an order that never arrived. It stops with "was being handed to the broker when the adapter stopped" and sends nothing more. | Look in IBKR's order history (Client Portal or the mobile app, switched to the paper account: orders, including cancelled ones) for an order whose order reference is the client order ID named. If it is there, cancelled, it reached IBKR: settle it with `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "cancelled at IBKR"`, and the next rebalance places a fresh order if one is still wanted. Never use `--resend` for an order IBKR shows at all. The full procedure is in [re-delivery](redelivery.md), "What can go wrong". |
| The adapter stops mid-send and IBKR shows no trace of the order | It stops with "was being handed to the broker when the adapter stopped" and sends nothing more. | Follow [re-delivery](redelivery.md), "What can go wrong": look in IBKR first, then settle it with the resolve command. |
| The link is down, or not yet matched to IBKR, when an order is due | The order is refused before "about to send" is written, and nothing goes out. The refusal and its reason are kept in the file `PENDING_FILE.refused` until the input is settled. It waits: the adapter asks for it again on every exchange. | Nothing, if the link is back and matched the same day (New York date): it goes out then. Do not settle a link-down refusal with `--never-sent`: the order goes out once the link is back. |
| The link stayed down past the end of the order's day | The order was for the session it was released in; its limit and the engine's checks were made for that day. On any later day it is refused ("was released for the session of ... not today's") before "about to send" is written. It is never sent, and the adapter cannot get past it by itself: it stops on that order at every exchange. | Stop the adapter. Confirm in IBKR (orders, including cancelled ones) that no order carries that client order ID as its order reference: nothing was sent. Then settle it with `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --never-sent --reason "released for <day>, refused as stale"`. The command accepts only the order the adapter is refusing (it names it if you give another). This is kept as a refusal with your reason, never as a cancel at IBKR, and the adapter goes on to its next input when started again, after checking IBKR once more: if an order or trade carrying that client order ID shows up after all, it stops with "was settled as never sent, but IBKR shows"; then mark it `--sent` (see [re-delivery](redelivery.md)). The order stays **working** in the engine, because nothing at IBKR will ever report on it, so the engine withholds the next rebalance (`rebalance-in-progress`) until you clear it with `operator rebalance-cleared` naming that rebalance and the order ([operator](operator.md), "A rebalance clearance"). Clearing is safe here because IBKR never received the order. Then reconcile and start again. Boost reads no clearance: its never-sent order stays released in its log; reconcile and report it. |
| An order the adapter can never send as it stands: no IBKR contract number configured for the stock, or a request that is not a whole-share DAY limit order for the configured account | It is refused before "about to send" is written, every time it is asked for again, so the adapter makes no progress until a person acts. | For a missing contract number: add the stock to `con_ids` in the adapter's configuration (the engine's instrument to IBKR's contract number, looked up in IBKR's contract search, never guessed by name; a change of configuration is a new run) and start the adapter again the same day; on a later day the order is stale (the row above). Any other shape refusal is a fault in the engine or the adapter: stop and investigate. If the order is never to go out, settle it with `--never-sent` and clear it in the engine, as in the row above. |
| IBKR's login does not manage the configured account | The adapter refuses to treat the link as matched to IBKR ("is not among the accounts this IBKR login manages"), so nothing is sent. | Check the account number in the adapter's configuration against the paper account the Gateway logs in to. A wrong account is a new configuration and a new run. |
| IBKR refuses the order | It records the refusal with IBKR's reason, and the engine reads the order as ended. | Read IBKR's reason. A margin or permission refusal in paper usually means a paper account setting to change. |
| IBKR reports on the order in a form the engine cannot read exactly (part shares, a price that is zero or not a number, a commission in another currency, negative, not a number, or as large as the order itself, an order it never showed) | The adapter stops rather than record an approximation. | A fault to investigate before starting again; check the order in IBKR. |
| IBKR corrects a fill (it reports the same trade again under an execution ID that differs only in its last part) | The adapter stops rather than record the correction as a second fill, which would count the shares twice. IBKR repeats the day's trades every time the adapter starts, so it stops again at every start until you acknowledge the correction. | Check the order's trades in IBKR. Then, with the adapter stopped, acknowledge it: `python3 adapter/ibkr/relay.py acknowledge-correction PENDING_FILE --journal ENGINE_LOG CORRECTED_EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "what IBKR shows"` (both IDs are in the stop message). `ENGINE_LOG` is the engine's durable log, the file given to the engine as `-log`: the engine writes it as it runs, so the engine need not stop. The command reads only the fills the engine has committed to that log. It refuses a file it cannot read as the engine writes it ("cannot be read at line"), including the journal the engine writes when it stops (`-out`): a log it cannot read is never taken as holding no fills. It also refuses if the log does not hold the original as a fill ("was never recorded as a fill"): that happens when IBKR's correction arrived before the original's commission. Then no fill records those shares, so acknowledging cannot help: reconcile the order by hand ([reconciliation](../runbooks/reconciliation.md)) before starting again. The adapter then passes over that correction. It is never recorded as a fill: the engine keeps the original fill's price and shares, so reconcile the account ([reconciliation](../runbooks/reconciliation.md)) before starting again. Any further correction stops it again. |
| IBKR cancels or refuses an order while one of its trades still waits for its commission, and the adapter stops before the commission arrives | The cancellation is held until every trade of that order reported before it is recorded, and the hold is written to the file ending `.held` beside the journal before it is accepted; a further trade IBKR reports on that order while the cancellation is held is added to the file before it is accepted. On restart IBKR repeats the trades and their commissions (not the cancelled order's status); the adapter records the fills, then the cancellation, once each. If writing the cancellation fails, the adapter stops with the cause and keeps the hold for the next start. Only the adapter holding the lock on `PENDING_FILE.lock` reads or writes the `.held` file: a second adapter started beside it is refused before it reads it. | Nothing, unless it stops with "has not replayed that execution" (IBKR did not repeat the trade within `commission_wait` seconds of the link being up and matched): then check the order and its trades in IBKR and reconcile before starting again. Leave the `.held` file alone: it is read strictly, and a hand edit stops the adapter. |
| IBKR reports a trade, or another status, on an order after the adapter recorded it cancelled or refused | The adapter stops ("after the order was recorded as", "after reporting it"): the engine reads nothing on an order that has ended, so a fill after a cancellation is a contradiction it does not record. | Check the order and its trades in IBKR, then reconcile the account ([reconciliation](../runbooks/reconciliation.md)) before starting again. |
| A trade's commission report never arrives | After `commission_wait` seconds the adapter stops ("has waited ... for IBKR's commission report"): the trade happened at IBKR but cannot be recorded without its commission. | Check the trade in IBKR, then reconcile the account before starting again. |
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
  without the link, or for another account, a market order, part shares or
  any time in force but DAY; an order released for another session, an
  account the login does not manage and a client that cannot place orders
  are all refused before any order number is used; an order number is never
  reused after a failed send.
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
  - `test_a_reconnect_never_resends_an_order_already_sent`;
  - `test_a_restart_after_reports_and_fills_were_journalled_writes_none_of_them_again`:
    IBKR replays each working order's status and each trade's commission
    after a restart, and nothing is recorded twice;
  - `test_a_corrected_execution_stops_for_a_person_and_is_never_a_second_fill`;
  - `test_an_execution_whose_commission_never_arrives_stops_for_a_person`;
  - `test_a_release_left_waiting_past_its_session_is_never_sent` and
    `test_a_stale_release_settled_as_never_sent_lets_the_adapter_go_on_and_reports_nothing`;
  - `test_the_relay_checks_with_the_sender_before_the_mark_whatever_the_mapper`;
  - `test_an_acknowledged_correction_lets_the_adapter_start_again_and_is_never_a_fill`
    and `test_a_correction_of_an_execution_never_recorded_as_a_fill_is_not_acknowledged`;
  - `test_a_cancellation_held_for_a_fill_survives_a_stop_before_the_fills_commission`:
    the adapter stops while a cancellation waits for a fill's commission;
    after the restart the fill, then the cancellation, are recorded once;
    with `test_a_failed_write_of_a_held_cancellation_stops_and_keeps_it_for_the_restart`
    and its neighbours on the `.held` file;
  - `test_an_execution_reported_after_a_held_cancellation_is_recorded_before_it_across_a_restart`:
    a second trade reported after the cancellation was held; after a stop
    and a restart, both fills, then the cancellation, once each;
  - `test_a_second_adapter_journals_nothing_held_while_the_first_holds_the_lock`;
  - `test_a_client_that_cannot_place_orders_leaves_no_mark` and
    `test_a_login_that_does_not_manage_the_account_is_never_reconciled_or_sent_on`.
- `internal/event`,
  `TestTheIBKRAdaptersPaperReportsAreReadByOrderReports`: the engine reads
  every record the adapter's paper test wrote, by the same rule it reads
  every venue report, to the order's end; and
  `TestAPaperOrderTheAdapterNeverSentIsWorkingUntilTheOwnersClearance`:
  that release, with no report at all, stays working until the owner's
  clearance names it.

Not yet tested: sending to IBKR's real paper servers. That is the second
half of this work and is run by hand once your paper account is open.
