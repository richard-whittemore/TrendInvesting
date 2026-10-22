# The IBKR adapter: between the engine and IBKR

*For the owner. What the adapter is, what it does between an engine and
IBKR, what of it runs today and what does not, every stop it can make and
the command that settles each, and what can go wrong. The sending rules in
detail are in [paper trading](paper-trading.md) and
[re-delivery](redelivery.md); this chapter is the map.*

## What it is and why

The engines never talk to IBKR. Each engine has one **adapter**, a Python
program in `adapter/ibkr/` that sits between IB Gateway (IBKR's own
login-and-connection program, kept logged in on the server by IBC) and the
engine's private socket (ADR 0025; ADR 0014 for the socket). It has no
strategy logic. It turns IBKR's account figures and reports into the
engine's inputs, and the engine's released orders into IBKR orders.

It is separate from the engine because IBKR's own client library is Python
(`ibapi`), and because a fault on the IBKR side should stop one account's
link, not the program that holds the record (ADR 0025, plan section 3).

## What it does, piece by piece

| Piece (file) | What it does | Built? |
| --- | --- | --- |
| Configuration (`config.py`) | Host, port (4002 is IB Gateway's paper port), this engine's client ID, the IBKR account, the mode, the timing bounds (`commission_wait` among them) and `con_ids`, IBKR's contract number for each instrument. Every value is required but one: `paper_sending_enabled` (off when left out). That includes `mark_timeout`, how long the adapter waits for IBKR's last prices (the example sets 15 seconds: IBKR sends the last price at once but only finishes the request at the end of an 11-second window, and the adapter takes a price only from a finished request, so a wait of 11 seconds or less can miss a price IBKR did send). A key that looks like a credential is refused: the adapter never holds the IBKR login, IBC does. A paper configuration must name a paper account (beginning `DU`), and `live` is refused | Built |
| Connection (`connection.py`) | Connects under the engine's own **client ID** (Boost 11, Traditional IRA 12, Roth IRA 13; 0 is refused). Set IB Gateway's **Master API client ID** to it (Configure > Settings > API > Settings): IBKR shows an API client other clients' and TWS's executions only then, and the adapter asks for every client's (`docs/runbooks/first-paper-order.md`, A4); its check of the holdings and cash since the previous close needs no client ID ([paper trading](paper-trading.md), "A paper day"). It notices a lost link (a closed socket, IBKR's codes 1100, 2110, 504, 1300, or no answer to a heartbeat) and a restored one (1101, 1102), after which it matches IBKR's orders and balances again before sending, and reconnects with growing pauses. When Gateway says it has lost IBKR's servers (1100 or 2110) it waits for Gateway to restore them (1101, 1102) for at most `restore_timeout`, and only while Gateway's connection stays open: if the connection closes during that wait, it reports the link lost at once and connects again straight away. Each new session records which accounts the IBKR login manages | Built |
| Account read (`account.py`, `snapshot.py`) | Reads positions, net liquidation, total cash, Excess Liquidity and every open order in one pass, bracketed by IBKR's clock, and builds the records the engine reads from one reading: the engine's **account snapshot** (positions and net liquidation, which orders are valued against), the **broker snapshot** the risk gate sizes sells from and the margin alarm watches, both stamped as of the session's close when the reading stands for it (your decision of 2026-10-18) or at the read time with the reason, the **mark** (the delivered bar's official close, IBKR's last price beside it, which the price band compares with when it is set), and the **reconciliation** the engine compares against its book. Anything IBKR did not deliver, or delivered in a form the engine cannot hold exactly, is marked **unreadable**, never sent as zero ([reconciliation](reconciliation.md), "What the adapter sends") | Built |
| Relay (`relay.py`) | Keeps the input waiting for the engine's reply in a small file on disk (the **pending file**), asks the engine for that reply again after either side restarts, and hands each released order marked `send` to the sender once, writing "about to send" before and "sent" after (the **hand-over** marks) ([re-delivery](redelivery.md)) | Built |
| Sender (`sender.py`) | Sends a released order to IB Gateway only in paper, on a paper account the login manages, while the link is up and matched, as a whole-share DAY limit order released in today's New York session, under IBKR's next order number (never reused). Dry-run never sends. Live is refused | Built, against a stand-in for IB Gateway only |
| Reports (`orders.py`, `reports.py`) | Turns IBKR's answers on this engine's orders into the engine's inputs: a status change (working, cancelling, cancelled, refused) and each fill with IBKR's commission and time. A fill is recorded only once its commission arrives. Every read of IBKR's open orders, here and in the account read, takes IBKR's whole list: IBKR marks the end of the list without saying which request it answers, so the adapter counts only the end of the list it asked for. An end-of-list mark that IB Gateway sends without being asked (it does when a session starts) or that is left over from an earlier list never cuts the list short, so an order working at IBKR is never reported missing because the list was read only in part | Built |
| Real IBKR client (`real_client.py`) | The only part that imports `ibapi` | Built; it places an order only in paper, on a paper account the login manages, with `paper_sending_enabled` switched on, and the paper-account test against IBKR is still to be run by hand |
| Main loop (`loop.py`) | At start: takes the pending file's lock, connects to the engine's socket, reads the engine's durable log, connects to IB Gateway, matches IBKR's orders against the orders the engine released, records IBKR's positions and cash for the engine to reconcile once the engine holds a book (read only once every replayed fill is recorded, sent before any market data, and tried at most three times in a row if IBKR does not give the positions or cash, then it stops for you), then asks again for any input a stop left waiting. It does the same after every reconnect. Each step reads IB Gateway's callbacks and, while the link is up and matched, sends the engine its waiting inputs in order ([re-delivery](redelivery.md)). It checks whether a reading after the close can stand for the close: read on the close's own date, after it; no open order at IBKR, from any client; no execution at or since the close; every holding with an official close in the file and IBKR's own net liquidation positive; and the holdings and cash those of the previous close adjusted by the fills since (cash within 1% of net liquidation). The previous close is the last account snapshot of this run or of the earlier runs its approvals name, or the starting book you stated when approving the account's first run, so a fresh run's first reading is compared too; one that differs is never taken as the book ([paper trading](paper-trading.md), "A paper day"). When it stops it closes its connection to the engine, which takes one at a time | Built, against the stand-in gateway, a stand-in engine and, in the rehearsal, the real engine |
| Previous close's book (`book_basis.py`) | Finds the book a reading after the close is compared with, back through the chain of runs: the last account snapshot of this run or of the earlier runs its Run approvals name, or the starting book an approval states, whichever is later, with every fill since. It also keeps the figure behind `cash_drift` (the day's cash difference) and `unexplained_cash` (that difference added up since the cash was last explained; for Boost, since your starting book, as its book states no cash). The earlier runs' books never change, so they are kept once per run in a file beside the pending file (`.basis`); a kept file that does not read is only a warning, and is walked again and replaced. A log that cannot be read, or whose record hash chain is broken, stops it, naming the log. A starting book in another currency than the account's is never compared | Built, against a stand-in for IB Gateway and the engine |
| Market-data delivery (`delivery.py`) | Picks up each file the data job writes for the run (`--streams`), oldest first, and hands it to the engine through the outbox behind one fresh account reading: the account snapshot, the broker snapshot, a run's first reconciliation, then the file's lines with SPY's mark (the bar's official close) just before the last session's close; each input once, with the engine's own input numbers; refuses a file not as the data job writes it, not this run's, or older than one delivered ([market data](market-data.md), "What happens to the file"). **No decision needs IBKR market-data permissions**: nothing is sized from IBKR's last price; the price band compares with it when it is set. Every file carries a reconciliation of the same reading, ahead of its snapshots once the engine holds a book; in a run's first file it goes behind them when the reading matched the previous close or your starting book; when it did not it goes alone, the snapshots withheld, so the engine, holding no book, halts rather than take the difference as the book | Built, against a stand-in for IB Gateway and the engine, and rehearsed with the real engine |
| Engine link (`engine_client.py`) | The adapter's side of the engine's socket: one input at a time, and a reply is used only if it is the answer to that input from the run now running | Built, against a stand-in engine |
| `ibapi` pin (`ibapi_pin.py`) | Refuses to start unless the `ibapi` installed (`provision.sh twsapi`) is the version pinned with the adapter's code | Built |
| Start command (`cli.py`) | `python3 -m adapter.ibkr run ...`, what `ti-adapter-boost.service` runs; reads the account number from the adapter's own account file and never logs it ([paper trading](paper-trading.md), "Starting the adapter") | Built; not yet run on the server |

### "Reconciled": what the adapter means by it

On every connect and reconnect the adapter must match IBKR's open orders,
trades and balances with its own records before it sends anything (ADR
0025, point 5). The code and the runbooks call this "reconciled". It is
**not** the engine's reconciliation, which compares IBKR's positions and
cash with the engine's book and can put the account in Degraded or Halted
([reconciliation](reconciliation.md)); the adapter sends the record for
that one from the same read, at a run's first delivery and at every later
start and reconnect. The
[glossary](glossary.md) lists all three meanings.

## What runs today, and what does not

What of the adapter is built, and what is not, is in the one table in the
[README](README.md#built-today-and-not-built-yet). The first-paper-order
runbook marks the steps that wait on unbuilt pieces.

## Every stop, and the command that settles it

The adapter is built to stop, and send nothing more, whenever it cannot be
sure what happened. A stop in a paper account costs nothing. Four rules
hold for every row below: do not place the order by hand at IBKR; stop the
adapter first (the commands refuse while it runs); never edit a log, a
`.handover` file or any number; if unsure, stop, look at IBKR and ask.

In the commands, `PENDING_FILE` is the adapter's pending file (named in the
stop message), `ENGINE_LOG` is the engine's durable log (its `-log` file)
and `CLIENT_ORDER_ID` is the order's tag, which IBKR shows as the order
reference. The commands are run from the repository: `python3
adapter/ibkr/relay.py ...`.

| The adapter stops with | What it means | What settles it |
| --- | --- | --- |
| "was being handed to the broker when the adapter stopped" | It stopped mid-send, and IBKR shows the order neither open nor executed. It may have arrived and been cancelled, or never arrived | Look at IBKR's order history for the reference. Seen at all: `resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "..."`. No trace, and released today: `--resend` instead, then start the adapter the same day |
| "was allowed to be sent again on" | A `--resend` permission from an earlier day: it expires with its day | Check IBKR's history for every day since, then `resolve ... --sent` |
| "was released for the session of ... not today's" | The order was due on a day the link was down; its limit was for that day, so it is never sent | Confirm IBKR has no order with that reference, then `resolve ... --never-sent --reason "..."`. Then the engine side (below) |
| A refusal for a missing contract number, or an order shape it cannot send | It can never send this order as it stands; it asks again at every exchange | A missing contract: add it to `con_ids` (a new configuration) and start again the same day. Anything else is a fault: investigate, and if the order is never to go, `--never-sent` and the engine side |
| "was settled as never sent, but IBKR shows" | An order you settled `--never-sent` is at IBKR after all | `resolve ... --sent` (the only decision accepted), then reconcile |
| "is not among the accounts this IBKR login manages" | The Gateway is logged in to another login than the configured account's; nothing is sent | Compare the configured account with the paper account the Gateway logged in to. A corrected account is a new configuration and a new run |
| "which is none of", "not an object of marks" or "is not JSON", naming the `.handover` file | The hand-over record holds something the adapter did not write | Restore it to the adapter's own marks, then settle each "handing-over" order with `resolve` ([re-delivery](redelivery.md)) |
| "is held by another adapter or by relay.py resolve", or "stop it first" | Another process has the pending file | Stop that process. Never delete the `.lock` file |
| "retried with other content" or "committed with different content" | The adapter sent a changed input under a used ID | A fault in the adapter: find why before starting again |
| A stop for IBKR correcting a trade (two execution IDs differing only at the end) | Recording the correction would count the shares twice; it stops at every start until you acknowledge it | `acknowledge-correction PENDING_FILE --journal ENGINE_LOG CORRECTED_EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "..."`, then reconcile. If it says the original "was never recorded as a fill", reconcile by hand instead |
| "after the order was recorded as" or "after reporting it" (a trade on an order already ended) | IBKR reports a trade the engine cannot record, since the order ended | `acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason "..."`. The trade is never a fill, so the account then differs from the engine's book by its shares: follow `docs/runbooks/reconciliation.md` |
| "has waited ... for IBKR's commission report" | A trade happened but cannot be recorded without its commission | Check the trade at IBKR, then reconcile before starting again |
| "has not replayed that execution" | A cancellation held for a trade's commission, and IBKR did not repeat that trade after a restart | Check the order and its trades at IBKR, then reconcile. Leave the `.held` file alone |
| A report it cannot state exactly (part shares, a zero or odd price, a commission in another currency) | It will not record an approximation | A fault: investigate before starting again |
| "the stream file ... is refused" | A market-data file is not as the data job writes it, is another run's, or is older than one already delivered; nothing from it was sent | With the adapter stopped, move the file aside (never edit it), `sudo systemctl start ti-data`, then start the adapter ([market data](market-data.md), "What happens to the file") |
| "the run's stream folder ... does not exist" | The data job has not written this run's warm-up (after a crash resume: the first run's log is missing) | Run the warm-up, then start the adapter. If the log already holds committed delivered inputs, the adapter says so instead ("the log <path> already holds N committed delivered inputs beyond its configuration"): it is a crash resume, so restore the first run's log and do not run a warm-up ([market data](market-data.md), "A crash resume") |
| "already holds N committed delivered inputs beyond its configuration" | The log is a crash resume's (it holds copied history a fresh run's cannot; N counts delivered inputs only, market data and the adapter's broker snapshots, never operator, approval or configuration inputs), and the first run's log is not in the engine's folder, so the run's stream folder cannot be found. A stream folder with no log beside it, named in the message, is likely the first run's | Restore the first run's log, unchanged, from the backup and start again. Do not run a warm-up under the new name ([market data](market-data.md), "A crash resume") |
| "has more than one stream folder" | Two stream folders belong to the run, the one named after the running `TI_RUN` included (a warm-up run for a resumed `TI_RUN` while the first run's log was missing) | Move the extra folder out of `streams/boost/`, keep it, start the adapter |
| "nothing shows it is that run's crash resume" | The run shares the identity of the run whose folder it found but is not shown to be its resume. One of three cases: a dry-run first run started again without an approval (a paper engine refuses such a start itself); a resume of a first run that crashed having committed only its configuration; or a log that is not a copy of that run's records, record hash for record hash (one repeating its input IDs with other content, say) | For the first two, start a fresh run with its own warm-up (a dry run needs no approval; a paper run needs its approval); for the third, resume the crashed run's own log with `engine -resume` ([market data](market-data.md), "A crash resume") |
| "names this run in its first records but cannot be read" | The log beside a stream folder starts as this run's but cannot be read further (damaged) | Restore that log from the backup as the engine wrote it; never edit it ([market data](market-data.md), "A crash resume") |
| "does not follow its record hash chain" | A log compared to find the run's stream folder states record hashes its records do not give: it was changed after the engine wrote it | Restore that log from the backup as the engine wrote it; never edit it ([market data](market-data.md), "A crash resume") |
| "cannot be matched to any run" (the adapter stops) | A log beside a stream folder cannot be read from its first records (whether or not a folder of the run's own name exists): it may be this resumed run's own first log, damaged | After a crash resume, restore the first run's log from the backup; never run a warm-up for the new name. If the log is an old run's, once its run has ended move the folder out of `streams/boost/` and its log out of the engine's log folder (beside the running log), keeping both as evidence in a folder outside both ([market data](market-data.md), "A crash resume") |
| A warning: "stream folder ... skipped: its log's first records cannot be read, so it cannot be matched to any run" | The warning that comes with the stop in the row above | As that row |
| "the delivery record ... cannot be read" | The `.delivered` file beside the pending file holds something the adapter did not write | Restore it from the backup as it was; never edit it |
| IBKR error 326 | Another program holds this engine's client ID; the adapter does not retry or pick another | Stop the other program |
| Started in live mode | Refused until the limited-live gate | Use paper |
| "no order the engine released explains" or "IBKR shows N unexplained open order(s) or execution(s)" | At start or after a reconnect IBKR shows an open order or a trade of today that no run of this engine released: one placed by hand or by another program, or by an earlier run no longer read. The link is not treated as matched; nothing is sent | Look at each in IBKR (the message lists them by order reference, client ID and order number). Cancel each unexplained open order that should not be there; reconcile each unexplained execution, which cannot be cancelled ([reconciliation](../runbooks/reconciliation.md), "Find the cause"). Reconcile before starting again ([paper trading](paper-trading.md), "What can go wrong") |
| "could not be read in full" | IBKR did not answer every part of the account read at start or after a reconnect | Check IB Gateway is logged in, then start the adapter again |
| "the reconciliation owed after the start or reconnect could not be read in 3 attempts", or "the account could not be read in full for the broker snapshot in 3 attempts" | IBKR gave neither the positions and cash for the reconciliation nor the whole account for the day's snapshot three times running; no stream input is sent | Check IB Gateway's link and that the account reports in US dollars, then start the adapter again; the file is delivered then ([market data](market-data.md), "What happens to the file") |
| "awaits its reply from an earlier engine run" | The adapter stopped while it waited for the engine's answer to an input, and the engine now running is a fresh run, not that run resumed | Resume the crashed run, or cancel at IBKR what the message names and any order the earlier run sent that is still open, confirm each cancelled, then with the adapter stopped settle with `resolve --run-ended ... --journal ENGINE_LOG --reason "..."` ([paper trading](paper-trading.md), "What can go wrong") |
| "carries other content than the input the engine committed", or "has no ... that can be read" naming a report | An input or report in the adapter's files is not as the adapter records it; it is neither sent nor dropped | Never edit the files; investigate against IBKR before starting again |
| "gave up after N attempts", "no handshake within connect_timeout", "Gateway closed the connection during the handshake" | The adapter could not connect to IB Gateway after its growing pauses (`max_connect_attempts`) | `docs/runbooks/gateway-login.md`, then start the adapter |
| "cannot connect to the engine's socket", "durable log ... cannot be read", "EngineUnavailable" or "the engine refused the input" | The engine is not running, has not finished starting (a fresh run records your Run approval before it listens), has stopped, or refused an input. Nothing is sent and the input stays where it is | Start or resume the engine ([the durable log runbook](../runbooks/durable-log.md), "Restarting an engine"), then start the adapter. A refusal is a fault: investigate |
| "refused: ..." at start | The configuration (unknown or missing keys, a value of the wrong kind, `paper_sending_enabled` outside paper, a key that looks like a credential), the account (named in both places or neither, or written with quotes or spaces), or the installed `ibapi` ("install the pinned build") | Correct what it names; for `ibapi`, `sudo deploy/provision.sh twsapi` and `adapter` ([paper trading](paper-trading.md), "Installing IBKR's `ibapi` on the server") |

### The settling commands at a glance

| Command | What it settles | Never use it |
| --- | --- | --- |
| `resolve ... --sent` | An order IBKR shows in any form (working, filled or cancelled): it is never sent again | — (it is always the safe answer once IBKR shows the order) |
| `resolve ... --resend` | An order committed **today** that IBKR has no trace of today: sent once more at its original limit, the same day only | For an order IBKR shows at all, or one from an earlier day (refused) |
| `resolve ... --never-sent` | The one order the adapter is refusing before it wrote "about to send", and always will: kept as a refusal with your reason | For a link that is merely down today: that order goes out once the link is back |
| `acknowledge-correction` | IBKR's correction of a trade already recorded: passed over, never a second fill | When the original was never recorded as a fill (refused) |
| `acknowledge-late-fill` | A trade on an order already recorded as ended: passed over, left to reconciliation | For a trade the log holds as a fill (refused) |

Each command keeps your decision and reason in a file beside the pending
file: `.handover.decisions`, `.corrections` or `.late-fills`. The adapter's
other files there are `.lock` (never delete it), `.handover` (the
hand-over marks) and `.refused` (what the sender refused and why). The
held-reports file (`.held`), the delivery record (`.delivered`) and the kept
previous-close books (`.basis`) sit beside the pending file too.

### The engine side after `--never-sent`

The adapter passing over an order does not tell the engine. In the engine
the order stays released:

- **An IRA** withholds the next rebalance (`rebalance-in-progress`) until you
  clear it: `operator rebalance-cleared` naming the rebalance and the order
  ([operator](operator.md), "A rebalance clearance"). Clearing is safe here
  because IBKR never received the order.
- **Boost** lists the order as one IBKR has not ruled on
  (`undecided_orders`) in every decision until you clear it the same way:
  `operator rebalance-cleared` naming the decision that released it and
  the order. The clearance only takes it off that list, and Boost refuses it for an
  order IBKR has already named (that order was sent). A never-sent sell
  keeps counting as being sold, so every later exit in the run is smaller
  (as in dry-run: [engine](engine.md), "What dry-run simulates"), and a
  never-sent buy is not planned again, because the target in force has not
  changed. So clear it, reconcile, then start a fresh run you approve
  (`docs/runbooks/durable-log.md`, "Restarting an engine"): its first
  decision plans to the target from scratch.

## What can go wrong, and what the adapter does then

| What you see | What the program does | What you do |
| --- | --- | --- |
| The link to IB Gateway drops | Reconnects under the same client ID; reads IBKR's whole open-order list and matches IBKR's orders, trades and balances before anything is sent; never resends an order IBKR already has, and an order that is still working is matched, not reported missing | Nothing, unless it gives up: `docs/runbooks/gateway-login.md` |
| IB Gateway loses IBKR's servers (IBKR's message 1100), then its connection closes | The adapter's link status says `connectivity-lost`, then `disconnected` the moment the connection closes; it connects again at once, with growing pauses, and matches everything again before sending. It never waits on a closed connection: the longest it waits for Gateway to restore IBKR's servers is `restore_timeout`, and only while the connection is still open. After `max_connect_attempts` failed tries it stops (`gave up after N attempts`) | Nothing, unless it gives up: `docs/runbooks/gateway-login.md` |
| The Gateway's nightly restart (11:45 PM New York) or Sunday re-login (09:00) | The link drops and comes back as above. An order IBKR accepted stays at IBKR, and is found again on reconnect | Approve the Sunday phone prompt. Not yet rehearsed against a real Gateway |
| Gateway is down when an order is due | The order is refused before "about to send" and waits; it goes out once the link is back the same New York day | Bring the Gateway back. Past the day, it is stale (above) |
| The adapter stops mid-send | Stops with "was being handed to the broker" | The first row of the stops table |
| The engine restarts after a crash | On reconnect the adapter asks for its waiting input's reply again and receives the order decided before the crash, once | Nothing ([re-delivery](redelivery.md)) |
| IBKR refuses the order | Records the refusal with IBKR's reason; the engine reads the order as ended | Read the reason (`journal-query orders`). In paper it is usually an account setting |
| A sell order the program did not send (a sale you placed by hand) is working | The broker snapshot lists it among the open orders, and the reading after the close cannot stand for the close while it works (the session is withheld); at the adapter's next start or reconnect it stops on it ("no order the engine released explains"). The engine counts it as being sold, and the joint account's sale back to 98% is withheld (`unknown-working-sell`, CONTAINMENT REQUIRED) | Do not trade by hand while a program order is working ([states-and-kill-switch](states-and-kill-switch.md)) |

## What backs this

- ADR 0025 (the adapter, its points 3 to 6 and its amendments of 2026-10-16
  and 2026-10-18 on `ibapi`), ADR 0027 (journal before send, re-delivery on
  request), ADR 0022 (what IBKR's reports become), ADR 0026, points 2, 3
  and 8 (the order, the limits, one client ID per engine), ADR 0019
  (unreadable is never zero), and ADR 0019, ADR 0025 point 5 and ADR 0026
  point 1 as amended 2026-10-18 (the reading after the close stands for the
  close; the mark is the official close).
- `adapter/ibkr/README.md` lists every module's rules.
- Tests in `adapter/ibkr/tests/`: `test_config.py`, `test_connection.py`,
  `test_account.py`, `test_snapshot.py`, `test_book_basis.py`, `test_post_close.py`,
  `test_delivery.py`, `test_orders.py`,
  `test_sender.py`, `test_relay.py`, `test_paper.py`,
  `test_real_client.py`, `test_loop.py`, `test_engine_client.py`,
  `test_engine_log.py`, `test_ibapi_pin.py`, `test_cli.py` and
  `test_rehearsal.py`, all against the stand-in gateway (and, for the
  engine's side, a stand-in engine; the rehearsal runs the real one). The tests named
  row by row are in [paper trading](paper-trading.md), "The tests", and
  [re-delivery](redelivery.md), "The tests".
- The whole open-order list, against the stand-in gateway sending the list
  in parts behind an end-of-list mark nobody asked for, or behind the late
  end of an earlier list: `test_orders.py` (`WholeOpenOrderListTests`),
  `test_snapshot.py`
  (`test_the_end_gateway_volunteers_on_a_reconnect_does_not_cut_the_order_list_short`)
  and `test_loop.py`
  (`test_a_reconnect_with_an_order_working_reads_ibkrs_whole_list_and_stays_reconciled`).
  A lost connection to IBKR's servers followed by a closed connection:
  `test_connection.py`
  (`test_connectivity_lost_then_a_socket_close_is_reported_and_reconnected_within_restore_timeout`
  and its neighbours).
- Not yet tested: anything against IBKR's real paper servers, including
  whether IB Gateway sends its unasked end-of-list mark before or after
  the adapter's first request.
