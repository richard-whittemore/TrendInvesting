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
| `real_client.py` | `TWSApi` implemented with IBKR's `ibapi`. The only module that imports it. Each connect is a new session (its own `EWrapper` and `EClient`, bound to that connect's `Inbox`); the old session's reader is joined first, and no new session starts while it is still running; the socket connect is bounded by `connect_timeout`. `place_order` raises `LiveSendingDisabled` |
| `config.py` | host, port, per-engine client ID, account ID, mode, and the timing bounds. Every value is required. Credential-like keys are refused |
| `connection.py` | connect, reconnect with capped exponential backoff, disconnect detection (socket close, IBKR codes 1100/1101/1102/2110/504/1300, an unanswered `reqCurrentTime` heartbeat), and `ConnectionStatus` records for ADR 0019's heartbeat. `maintain()`, called from the adapter's loop, is what brings a lost link back: at once after a drop, or after `restore_timeout` if Gateway has lost IBKR's servers (1100/2110) and not restored them |
| `account.py` | `read_snapshot`: positions, NetLiquidation, TotalCashValue and ExcessLiquidity for the configured account, as exact decimals with currency |
| `snapshot.py` | `read_account`: positions, the account summary and every open order in one pass, marking what could not be read. `broker_snapshot_record` and `reconciliation_record` build the `account.broker.snapshot` and `account.reconciliation` payloads from that one read, every field stated (`[]` for none, the `unreadable` marker for what failed), times in whole seconds. `tests/testdata/` holds the payloads the engine's own test decodes and validates |
| `orders.py` | `OrderTracker`: order status, open-order, execution and commission callbacks as records. `collect_broker_orders` and `match_on_reconnect`: matching by client order ID, under this engine's own client ID, on reconnect |
| `sender.py` | `DryRunSender` journals with `send=False` and holds no IBKR client. `PaperSender` sends a released order to IB Gateway: only on a connection whose configuration is `paper` on a paper account, only for that account, only while the link is up and reconciled, as a whole-share DAY limit order under the next IBKR order ID (never reused); `check` runs every refusal before the relay marks the order about to be sent, so a refused order leaves no mark. `release_to_request` maps a released order to a request, refusing one not marked `send` or released in another mode. `make_sender` refuses `live` |
| `reports.py` | `OrderReportJournal`: IBKR's reports on this engine's orders (its own client ID, a client order ID) as the engine's inputs, `execution.order.lifecycle` version 2 when the order's status changes and `execution.fill` version 5 once an execution's commission arrives, describing the order as IBKR holds it so the engine judges it against the release. A report it cannot state exactly raises `UnreportableOrder` |
| `relay.py` | `Relay`: the adapter's side of re-delivery (ADR 0027 section 4). It keeps the input awaiting its reply on disk (`PendingInput`), asks for that input's committed reply again by sending it again under its ID after the engine or the adapter restarts, and hands each released order marked `send` to the sender once: it records "about to send" on disk before each hand-over and never sends a recorded order again, skips one IBKR shows open or executed, and stops (`HandoverUnconfirmed`) rather than resend one it was handing over when it stopped that IBKR shows neither open nor executed. A changed retry under the waiting input's ID is refused (`InputChanged`) after the original is settled. The "about to send" record is read strictly (`HandoverRecordInvalid` on any unknown mark), and a person settles a stopped hand-over with `python3 relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent \| --resend --reason TEXT`, which writes only a known mark, settles an order marked about to be sent or holding a resend permission (`--sent` withdraws a permission; an unknown or already sent order is refused), allows a resend only for an order committed in today's session and only on that day (a later start stops with `ResendExpired`), refuses while the adapter holds the pending file (each relay holds its own lock on `PENDING_FILE.lock` from construction until close, and re-checks it before every mark and send; a second adapter, or a closed relay, stops with `PendingInUse`; `close` from another thread waits for the step in flight, at most `close_timeout` seconds, then raises `RelayCloseTimeout`), and keeps each decision, its reason, its UTC time and session date in the `.handover.decisions` file |

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
by `deploy/provision.sh gateway`, never committed, and not needed for the
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
