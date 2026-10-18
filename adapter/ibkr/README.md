# IBKR adapter

The live broker adapter (ADR 0025): Python, over IBKR's TWS API, between IB
Gateway and the Go engine. It has no strategy logic.

**Built so far: the connection and account layer, the payloads of the
broker snapshot and the morning reconciliation, and paper sending against
the fake gateway.** It does not yet speak the envelope transport (ADR 0014),
so everything here produces plain records that the transport layer will map
to envelopes later. Only a paper configuration on a paper account (ID
beginning `DU`) can send, and the real `ibapi` client still refuses
`place_order` until the paper-account test enables it (AGENTS.md rule 5).

| Module | What it does |
| --- | --- |
| `tws.py` | `TWSApi`, the narrow interface over IBKR's client, and `Inbox`, the thread-safe queue every callback lands in |
| `real_client.py` | `TWSApi` implemented with IBKR's `ibapi`. The only module that imports it. Each connect is a new session (its own `EWrapper` and `EClient`, bound to that connect's `Inbox`); the old session's reader is joined first, and no new session starts while it is still running; the socket connect is bounded by `connect_timeout`. `can_place_orders()` is false and `place_order` raises `LiveSendingDisabled`; when the paper-account test enables it, `place_order` builds ibapi's `Contract` and `Order` from the adapter's request shape (`tws.py`) |
| `config.py` | host, port, per-engine client ID, account ID, mode, and the timing bounds, `commission_wait` (how long an execution may wait for its commission report) among them, and `con_ids`, the contract map from the engine's instrument ID to IBKR's contract ID (positive integers, one contract per instrument: two instruments sharing a contract ID are refused, naming both; kept read-only). Every value is required. Credential-like keys are refused |
| `connection.py` | connect, reconnect with capped exponential backoff, disconnect detection (socket close, IBKR codes 1100/1101/1102/2110/504/1300, an unanswered `reqCurrentTime` heartbeat), and `ConnectionStatus` records for ADR 0019's heartbeat. Each session records the accounts IBKR's `managedAccounts` names, and `mark_reconciled` refuses (`AccountNotManaged`) unless the configured account is among them. `maintain()`, called from the adapter's loop, is what brings a lost link back: at once after a drop, or after `restore_timeout` if Gateway has lost IBKR's servers (1100/2110) and not restored them |
| `account.py` | `read_snapshot`: positions, NetLiquidation, TotalCashValue and ExcessLiquidity for the configured account, as exact decimals with currency |
| `snapshot.py` | `read_account`: positions, the account summary and every open order in one pass, marking what could not be read. `broker_snapshot_record` and `reconciliation_record` build the `account.broker.snapshot` and `account.reconciliation` payloads from that one read, every field stated (`[]` for none, the `unreadable` marker for what failed), times in whole seconds. `tests/testdata/` holds the payloads the engine's own test decodes and validates |
| `orders.py` | `OrderTracker`: order status, open-order, execution and commission callbacks as records. `collect_broker_orders` and `match_on_reconnect`: matching by client order ID, under this engine's own client ID, on reconnect |
| `sender.py` | `DryRunSender` journals with `send=False` and holds no IBKR client; its `check` validates only. `PaperSender` sends a released order to IB Gateway: only on a connection whose configuration is `paper` on a paper account, only for that account and only if the session's login manages it, only through a client that can place orders (`can_place_orders()`), only while the link is up and reconciled, as a whole-share DAY limit order released in today's session (New York), under the next IBKR order ID (never reused); `check` runs every refusal, and the relay calls it itself before it marks the order about to be sent or an order ID is used, so a refused order leaves no mark. `release_to_request` maps a released order and the session it was released in (its input's event time) to a request, refusing one not marked `send`, released in another mode, or with no session. `make_sender` refuses `live` |
| `reports.py` | `OrderReportJournal`: IBKR's reports on this engine's orders (its own client ID, a client order ID) as the engine's inputs, `execution.order.lifecycle` version 2 when the order's status changes and `execution.fill` version 5 once an execution's commission arrives, describing the order as IBKR holds it so the engine judges it against the release. A report it cannot state exactly raises `UnreportableOrder`: a fill price that is not finite and above zero, a commission not finite, negative or not below the order's notional, an execution still without its commission `commission_wait` seconds on (`check_overdue`), a naive time, IBKR's correction of an execution (the same execId base, another last segment) unless the owner acknowledged it (then passed over, never a fill), or an order IBKR shows with another side, quantity or limit than already reported. A cancellation or refusal is held while an execution of that order awaits its commission, and journalled after the fills of every execution of that order seen before it is journalled; the hold is written durably to `held_path` (beside the journal) before it is accepted, rewritten before a further execution of the held order is accepted, and read strictly at start (its own account and client ID, a complete lifecycle payload, one entry per order), so a stop before the commission loses nothing: IBKR replays the executions and their commissions, and the held report follows the fills once. An execution or another status of an order whose terminal report is already journalled stops for a person, and so does any status other than a held report's own while it is held. IBKR replays the day's executions on every start, so an execution after a journalled cancellation or refusal stops every start until the owner acknowledges it (`late_fills`, from `relay.py acknowledge-late-fill`); it is then passed over and never a fill, since the engine's `OrderReports` refuses a fill on an ended order, and is left to reconciliation. The held file is read and written only while the adapter holds its exclusive lock (`owns_lock`, `Relay.holds`: construct the journal after the relay). A held report leaves the file only after its own journal write, so a failed write stops the run and is retried by the next record or a restart; one whose execution IBKR does not replay within `commission_wait` of the link being up and reconciled (`ready`; the wait restarts after a reconnect) stops for a person (`check_overdue`). A commission report is read before the overdue check. Seeded with the records already journalled, so IBKR's replay of statuses and commissions after a restart writes nothing twice. The loop that pumps the connection must call `check_overdue` after each pump; that loop is not built yet |
| `relay.py` | `Relay`: the adapter's side of re-delivery (ADR 0027 section 4). It keeps the input awaiting its reply on disk (`PendingInput`), asks for that input's committed reply again by sending it again under its ID after the engine or the adapter restarts, and hands each released order marked `send` to the sender once: it records "about to send" on disk before each hand-over and never sends a recorded order again, skips one IBKR shows open or executed, and stops (`HandoverUnconfirmed`) rather than resend one it was handing over when it stopped that IBKR shows neither open nor executed. A changed retry under the waiting input's ID is refused (`InputChanged`) after the original is settled. The "about to send" record is read strictly (`HandoverRecordInvalid` on any unknown mark), and a person settles a stopped hand-over with `python3 relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent \| --resend \| --never-sent --reason TEXT`, which writes only a known mark (`--never-sent` only for an order with no mark while an input awaits its reply: the sender refused it for good, and the relay then passes over it), settles an order marked about to be sent or holding a resend permission (`--sent` withdraws a permission; an unknown or already sent order is refused), allows a resend only for an order committed in today's session and only on that day (a later start stops with `ResendExpired`), refuses while the adapter holds the pending file (each relay holds its own lock on `PENDING_FILE.lock` from construction until close, and re-checks it before every mark and send; a second adapter, or a closed relay, stops with `PendingInUse`; `close` from another thread waits for the step in flight, at most `close_timeout` seconds, then raises `RelayCloseTimeout`), and keeps each decision, its reason, its UTC time and session date in the `.handover.decisions` file. The relay takes a sender with `check` and `send` and calls `check` before every mark, whatever `to_request` does. `--never-sent` is accepted only for the order the relay recorded it is refusing for the waiting input (`PENDING_FILE.refused`), and a never-sent order is passed over only while IBKR shows no open order or execution under its tag; otherwise the relay stops (`NeverSentAtBroker`, a `HandoverUnconfirmed`) and the owner marks it `--sent`. `relay.py acknowledge-correction PENDING_FILE --journal ENGINE_LOG EXEC_ID --corrects ORIGINAL --reason TEXT` reads the engine's durable log (its `-log` file, streamed as the engine runs; ADR 0027) strictly, counting only committed fill inputs and refusing a log it cannot read as the engine writes it (never read as holding no fills), and refuses a correction of an execution that log does not hold as a fill (the original was never recorded: reconcile by hand), and otherwise records the owner's acknowledgement of IBKR's correction of an execution in `PENDING_FILE.corrections`, read strictly at start and passed to `OrderReportJournal`. `relay.py acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason TEXT` reads the same log (`journalled_reports`: committed fills, and the orders a committed lifecycle input cancelled or refused), refuses a trade that log holds as a fill or one after an order it does not hold ended, and otherwise records the acknowledgement in `PENDING_FILE.late-fills`, read strictly at start and passed to `OrderReportJournal` as `late_fills`; the trade is never a fill and is settled by reconciliation |

## Rules this code keeps

- **One client ID per engine** (ADR 0026 point 8). A client-ID conflict
  (IBKR code 326) fails closed. It is not retried, and no other ID is tried.
  Client ID 0, IBKR's master binding for manual TWS orders, is refused.
- **Reconciliation before orders.** Every connect and every restoration sets
  `needs_reconciliation`. Only a caller that has matched IBKR's orders,
  executions and balances to the journal clears it (ADR 0025 point 5).
- **An unavailable balance is not zero** (ADR 0019). A missing tag or end
  marker, a drop mid-read, an IBKR error, or no server time raises
  `SnapshotIncomplete`, which names what is missing. A conflicting or
  unparseable value raises `SnapshotInvalid`.
- **IBKR's timestamps.** IBKR does not timestamp positions or account-summary
  values. A snapshot is bracketed by two IBKR server times
  (`reqCurrentTime`), one before and one after, plus the adapter's own receipt
  time. Executions keep IBKR's time string verbatim, and `occurred_at` is its
  UTC reading. A time with no zone is refused. Every time the engine records
  for a snapshot (`as_of`) or a fill (`filled_at`) is a whole second, as IBKR
  stamps them: the risk gate orders snapshots and fills by the second, and
  event validation refuses a time with a fraction of a second. IBKR answers `reqCurrentTime`
  in order, so each request's answer is found by counting. Within
  `request_timeout` of a restoration that found requests unanswered, a late
  old answer may still arrive, and a read fails closed rather than take it as
  fresh. If any request is still unanswered after that, a lost answer and a
  late one cannot be told apart, so the link is dropped and the adapter's loop
  reconnects; the new session counts from zero and no old answer reaches it.
- **Order IDs come only from IBKR.** Records carry IBKR's `clientId`,
  `orderId` and `permId` as reported. IBKR's `orderId` is unique only within
  one client ID, so an order is identified by (`clientId`, `orderId`), or by
  `permId` for a manual TWS order with no positive `orderId` (`order_key`). The decision's tag travels as `orderRef`
  (`client_order_id`). Reconnect matching uses the tag, and each tag belongs
  to one IBKR order: executions of that order are its fills, and a second
  order with the same tag, open or executed, is unknown. The result is
  evidence for reconciliation and never triggers a resend.
- **ADR 0022's closed status set.** `Submitted`/`PreSubmitted` are
  `submitted`, `PendingCancel` is `cancel-pending`, and
  `Cancelled`/`ApiCancelled` are `canceled`. A rejection (IBKR code 201) is
  `invalid`. `Inactive` counts as `invalid` only after that rejection,
  because IBKR also uses `Inactive` for held orders. Any other status raises
  `UnrecognisedOrderReport`. `PendingSubmit`/`ApiPending`/`Filled` are
  recorded with no lifecycle status: the order is not yet acknowledged, or it
  is finished, which executions report.
- **Seen only once recorded.** A status or execution counts as a duplicate
  only after the sink has accepted it, so IBKR's redelivery after a failed
  journal write is recorded, not dropped.
- **Other accounts are skipped.** One IBKR login may reach all three accounts.
  Rows, orders and executions for any other account are ignored, and so is a
  commission report whose execution was not this account's. An order status
  carries no account, so it is held until an open order (or IBKR's rejection)
  shows the order is this account's, and discarded if it is another's. A
  held status whose record failed stays ahead of every later status for that
  order, so history is never recorded out of order.
- **No credential.** The adapter never handles the IBKR password, which IBC
  holds. Logs name host, port and client ID only.

## ibapi

ADR 0025 point 4 (amended 2026-10-16 and 2026-10-18) pins IBKR's own
`ibapi` to one released build, the TWS API 10.51.01 Linux installer, by its
SHA-256 in `deploy/versions.env`. It is fetched and verified on the server
by `deploy/provision.sh twsapi` (its own step, not run by `all`), never committed, and not needed for the
tests. `real_client.py` imports it only if present. Installing it into the
adapter's environment comes with the paper-account test.

## Tests

`make adapter-test` runs `python3 -m unittest discover -s tests` here, against
`tests/fake_tws.py`, a scripted stand-in for IB Gateway with synthetic
accounts and prices. A paper-account test against IBKR's servers is run by
hand (ADR 0025 Consequences) and is not written yet. `tests/test_paper.py`
sends one released SPY order through the relay and the paper sender to the
fake and journals IBKR's answers; `tests/testdata/paper_*.json` are the
order and the records the engine's own test reads by its report rule.
