"""The IBKR adapter's main loop: one engine's link to IB Gateway, kept up, reconciled, and drained into the engine.

ADR 0025 points 2 and 5, ADR 0027 section 4. At start the loop connects
with the engine's own client ID and, before any order can be sent, reads
the account (snapshot.read_account: positions, balances and open orders)
and IBKR's open orders and the session's executions
(orders.collect_broker_orders), refusing to go on if any of it could not be
read (ReconcileIncomplete; an unavailable balance is not zero, ADR 0019).
It then matches IBKR's open orders and executions against the orders the
engine's durable log released to be sent, and the logs of the earlier runs
its Run approvals chain back to (relay.run_logs), since IBKR's day lists
still show an earlier run's orders after a fresh run starts
(orders.match_on_reconnect, by client order ID under this engine's client
ID): an open order or execution in the account that none of them explains
(an order placed by hand or by another client, or one no run of this
engine released) stops the adapter for a person
(ReconcileMismatch) before anything is sent. A released order IBKR no
longer shows is not a refusal here: a DAY order ended or filled on an
earlier day leaves nothing in today's lists, and the relay checks IBKR for
the one order it is about to send. Positions and balances are read in full
but not matched here: that is the engine's reconciliation (ADR 0019).
Only then is the link marked reconciled (Connection.mark_reconciled, which
refuses an account the login does not manage). It does the same after
every reconnect or restoration. It then settles the input left awaiting
its reply by a stop, if any, before anything new: only an input of the
run now running is asked for again, and one an earlier run left stops the
adapter for a person (relay.EarlierRunInput) before anything is sent.

Each step keeps the link up (Connection.maintain), takes one callback from
IB Gateway (Connection.pump), checks for executions whose commission report
is overdue (OrderReportJournal.check_overdue: the invariant reports.py
states, that the check runs after each pump, so a missing commission stops
the adapter on time and not only when IBKR next writes), and, while the
link is up and reconciled, sends the engine every input recorded since,
oldest first, through the relay, which hands each released order marked
send to the sender once (relay.Relay). While the engine decides, the
exchange runs on a thread of its own and this thread keeps reading IB
Gateway's callbacks and checking for overdue commissions, so a slow reply
neither leaves the heartbeat's answer unread (which would drop a healthy
link) nor lets an arrived commission count as missing.

Inputs are recorded in the outbox (OUTBOX_SUFFIX beside the pending file)
before they are sent, written durably, so a report IBKR will not repeat (a
cancellation of an order no longer open) survives a stop. An outbox entry
carries its input ID from the moment it is recorded; its sequence and the
run's identity are taken from the engine's durable log when it is sent
(relay.read_engine_log). An entry is struck off once its reply has been
acted on and before the input awaiting its reply is cleared (Relay's
settled), so a stop between the two leaves that input awaiting its reply,
which only its own run is asked for again: no entry an earlier run
answered is ever left for a fresh run to take. A stop before the strike
sends nothing twice: an entry the log already holds as a committed input,
with the same type, schema version and payload, is struck off unsent, and
one whose ID the log holds with any other content stops the adapter
(OutboxInvalid). The records already committed, in the outbox, or awaiting
their reply seed the report journal in the order they were made (each
run of the chain oldest first with the settlements of the runs it ended,
then the input awaiting its reply, then the outbox), never by their
times, so IBKR's replay after a restart records nothing twice and each
order keeps its latest status; a report whose event time cannot be read
stops the start (ReportRecordInvalid).

With a streams folder (delivery.py), each step while the link is up and
reconciled also delivers the run's market-data stream: the files of
<streams>/<run> it has not delivered, oldest session first, behind one
broker snapshot (the account read with IBKR's marks for its holdings), all
recorded in the outbox in one write, so the snapshot is journalled before
the bars and each session's bars before its close (ADR 0019's snapshot
rule; ADR 0021). A stream entry is sent as the data job wrote it, with the
engine's next sequence (the file's own is a file-local ordinal) and its
send time as its recorded time, as every input the adapter sends, so the
snapshot is never recorded after the input it serves (ADR 0019's snapshot
rule measures its age at the trigger's recorded time, while the adapter's
clock keeps time with IBKR's: one behind it gives a negative age, which the
engine finds stale and fails closed on). An earlier run's batch left in
the outbox, its snapshot and its stream entries, is marked dropped, so that
run gets its files again if it is resumed (delivery.py), and then struck
off unsent in one outbox write, since a run's own stream carries its whole
history from its warm-up: a stop leaves the batch whole or wholly gone,
never its stream entries without their snapshot.

The broker snapshot is one consistent reading (ADR 0019: its components
share one cutoff): every callback already received is applied first, so a
report IBKR sent before the read is recorded ahead of it; it is not taken
while an execution awaits its commission (its fill is not yet recorded);
the account is read, then the marks, then the account again, and the
snapshot is the second read. It is read again, up to SNAPSHOT_ATTEMPTS
times, if an order report is recorded or an execution arrives (of any
client's order in the account, not only this adapter's) between the start
of the first read and the end of the second, if the two reads' holdings,
open orders or cash differ (a trade IBKR reports to another client only,
such as one placed in its own app), or if any component could not be read.
A reading still incomplete then stops the adapter for a person
(delivery.SnapshotRefused); one still overtaken waits for the next step.
IBKR's positions and open orders carry no request ID, so a read cut off
before either ended (it timed out, or Gateway lost IBKR's servers, which it
restores on the same socket) is never followed by another on that link,
where the late end of the abandoned list would complete the new one: the
link is dropped (Connection.drop; after Gateway's restoration or
restore_timeout if it had lost IBKR's servers), and the snapshot waits
until it is back and reconciled. The order due is held the same way when
the link went part-way through the relay's open-order read; a read that
times out with the link up stops the adapter (BrokerOrdersIncomplete).
Each snapshot's cutoff is logged: the last report recorded before it.

The outbox goes oldest first, but a broker report recorded behind a stream
entry (a fill or a cancellation IBKR reported while the engine decided a
bar) is sent before that stream entry, so no decision runs on a state
behind a report already received. While an execution awaits its
commission, no stream entry is sent at all: its fill is not recorded yet,
and goes ahead of the stream once its commission arrives; one that never
arrives stops the adapter for a person after commission_wait
(OrderReportJournal.check_overdue).

The engine must be running, with its durable log begun, before the adapter
starts: the log is the run's identity. The loop connects to the engine's
socket before it reads that log or IBKR (EngineUnavailable otherwise): the
engine opens its socket only after it has recorded its start, a fresh run's
Run approval included, so the chain of runs is never read without it. The
chain stops at the first earlier run whose inputs all predate yesterday's
session and whose released orders have all ended, at IBKR or in the
adapter's own records (relay.run_logs, PendingInput.settled_orders), so an
older run's log is never needed. Nothing here sends when the
configuration is not paper (sender.make_sender), and the real client sends
only with paper_sending_enabled on (real_client.IbapiClient.can_place_orders).
"""
import hashlib
import json
import logging
import os
import threading
import time
import uuid
from datetime import datetime, timezone

from connection import Connection, NotConnected
from delivery import DELIVERED_SUFFIX, Deliverer, SnapshotRefused, market_open, run_name
from orders import BrokerOrdersIncomplete, OrderTracker, collect_broker_orders, match_on_reconnect
from relay import (OUTBOX_SUFFIX, EarlierRunInput, Outbox, OutboxInvalid, PendingInput, Relay, earlier_run,
                   read_engine_log, released_client_order_ids, run_identity, run_logs)
from reports import FILL_TYPE, LIFECYCLE_TYPE, OrderReportJournal
from sender import NotReconciled, make_sender, release_to_request
from account import TOTAL_CASH
from snapshot import Record, broker_snapshot_record, read_account, read_marks

log = logging.getLogger("ibkr")

# The adapter's name in every input it sends (event.AdapterIBKR; ADR 0025
# point 3).
SOURCE = "ibkr"
ENVELOPE_VERSION = 1
# The adapter's files beside its pending file (relay.PendingInput's own
# suffixes are .handover, .refused, .not-handed-over, .run, .run-ended,
# .corrections, .late-fills and .lock, and relay.OUTBOX_SUFFIX the outbox's).
HELD_SUFFIX = ".held"
# The input types whose event time is in their payload.
EVENT_TIME_FIELD = {LIFECYCLE_TYPE: "occurred_at", FILL_TYPE: "filled_at"}
# How long, in seconds, the loop waits on the engine's thread between reads
# of IB Gateway's callbacks while the engine decides.
ENGINE_WAIT_POLL = 0.05
# How many times one delivery reads the account and the marks for a broker
# snapshot that is complete and not overtaken by an order report.
SNAPSHOT_ATTEMPTS = 3


class ReconcileIncomplete(Exception):
    """IBKR's account could not be read in full, so the link is not reconciled and nothing is sent."""


class ReconcileMismatch(Exception):
    """IBKR shows an open order or execution the journal does not explain; the link is not reconciled."""


class ReportRecordInvalid(ValueError):
    """A report the adapter keeps (in an engine's log, the outbox, a settlement or awaiting its reply) is not as it writes one."""


def _utc_now():
    return datetime.now(timezone.utc)


def _batch_run(entry):
    """The run a stream batch's outbox entry was recorded for (its envelope's, or its snapshot's), or None for any other."""
    if "envelope" in entry:
        return entry["envelope"].get("correlation_id")
    return entry.get("run")


def _holdings(reading):
    """What a trade or an order changes in an account read: its positions, open orders and total cash."""
    return reading.positions, reading.open_orders, reading.balances.get(TOTAL_CASH)


def _next_entry(entries):
    """The outbox entry to send next: the oldest, except that a broker report behind a stream entry goes before it."""
    if "envelope" in entries[0]:
        for entry in entries[1:]:
            if _batch_run(entry) is None:
                return entry
    return entries[0]


def _event_time_readable(payload, field):
    """Whether payload holds field as a time with its zone, as the adapter stamps every report it records."""
    text = payload.get(field) if isinstance(payload, dict) else None
    try:
        at = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except (AttributeError, ValueError):
        return False
    return at.tzinfo is not None


def _stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _new_id(event_type):
    return "{}:{}:{}".format(SOURCE, event_type, uuid.uuid4().hex)


class _AskedOnItsOwnThread:
    """An engine client whose decide runs on a thread of its own while await_engine(worker) keeps this one busy."""

    def __init__(self, engine, await_engine):
        self._engine = engine
        self._await = await_engine

    def decide(self, envelope):
        outcome = {}

        def ask():
            try:
                outcome["reply"] = self._engine.decide(envelope)
            except BaseException as err:  # handed back to the caller below
                outcome["error"] = err
        worker = threading.Thread(target=ask, name="engine-exchange", daemon=True)
        worker.start()
        self._await(worker)
        if "error" in outcome:
            raise outcome["error"]
        return outcome["reply"]


class AdapterLoop:
    """One engine's adapter: config's account on IB Gateway through api, and the engine through engine_connect.

    api is a tws.TWSApi (real_client.IbapiClient on the server, the fake in
    tests). engine_connect is the relay's connect (engine_client.connector).
    pending_path is the relay's pending-input file; every other file of the
    adapter's sits beside it. engine_log_path is the engine's durable log
    (its -log file). now returns the adapter's receipt time with its zone;
    today the session's New York date; new_id an input ID for an event type;
    poll how long one step waits for a callback (seconds). streams, when
    given, is the folder holding each run's stream folder (delivery.py);
    without it no stream is delivered.
    """

    def __init__(self, config, api, engine_connect, pending_path, engine_log_path, now=_utc_now, today=None,
                 sleep=time.sleep, monotonic=time.monotonic, poll=1.0, new_id=_new_id, streams=None):
        self.config = config
        self._api = api
        self._engine_connect = engine_connect
        self._pending_path = pending_path
        self._engine_log_path = engine_log_path
        self._now = now
        self._today = today
        self._sleep = sleep
        self._monotonic = monotonic
        self._poll = poll
        self._new_id = new_id
        self.conn = None
        self.relay = None
        self.reports = None
        self.tracker = None
        self.outbox = Outbox(pending_path + OUTBOX_SUFFIX)
        self.pending = PendingInput(pending_path)
        # The order reports recorded by this adapter: how many, and the last
        # one's input ID (a broker snapshot's cutoff).
        self._reports_recorded = 0
        self._last_report = None
        # Every execution IBKR reports in the account, whichever client or
        # app placed its order (a broker snapshot's cutoff watermark too).
        self._account_executions = 0
        self.delivery = None if streams is None else Deliverer(
            streams, pending_path + DELIVERED_SUFFIX, self.outbox, engine_log_path, self._broker_snapshot, new_id)

    def start(self):
        """Take the pending file's lock, connect, reconcile, and settle any input left awaiting its reply."""
        cfg = self.config
        if self.delivery is not None:
            self.delivery.check()
        self.conn = Connection(self._api, cfg, self._status, sleep=self._sleep, monotonic=self._monotonic,
                               wall_clock=self._now)
        sender = make_sender(cfg, self._sent, conn=self.conn, today=self._today)
        # The relay takes the adapter's exclusive lock first; the report
        # journal reads its held file only while that lock is held.
        self.relay = Relay(lambda: _AskedOnItsOwnThread(self._engine_connect(), self._await_engine), sender,
                           self.pending, lambda: collect_broker_orders(self.conn),
                           lambda order, session: release_to_request(order, cfg.mode, cfg.con_ids, session),
                           today=self._today, settled=lambda envelope: self.outbox.strike(envelope["id"]))
        # The engine opens its socket only once its start is recorded, a
        # fresh run's Run approval included (cmd/engine), so nothing of the
        # log is read, and nothing of IBKR's journalled, before it listens:
        # a log read earlier could lack the approval naming the run before.
        self.relay.connect_engine()
        # The earlier runs' reports too: IBKR replays the day's executions
        # and the session's orders after a fresh run starts, and a report an
        # earlier run's engine recorded is never recorded again.
        logs = run_logs(self._engine_log_path, self._session(), settled=self.pending.settled_orders())
        chain = [(run_identity(path), read_engine_log(path)) for path in logs]
        self.reports = OrderReportJournal(
            cfg.account_id, cfg.client_id, self._record, now=self._now, commission_wait=cfg.commission_wait,
            held_path=self._pending_path + HELD_SUFFIX, owns_lock=self.relay.holds, ready=self.ready,
            journalled=self._journalled(chain), acknowledged=self.pending.acknowledged_corrections(),
            late_fills=self.pending.acknowledged_late_fills())
        self.tracker = OrderTracker(cfg.account_id, cfg.client_id, self.reports.on_record)
        self.conn.subscribe(self.tracker.on_event)
        self.conn.subscribe(self._count_execution)
        self.conn.connect_with_backoff()
        self.reconcile()
        self._settle_waiting()

    def _session(self):
        """Today's session date (New York) as the loop was given it, or None for the relay's own default."""
        return self._today() if self._today is not None else None

    def ready(self):
        """Whether the link is up and reconciled since it last connected or was restored."""
        return self.conn is not None and self.conn.connected and not self.conn.needs_reconciliation

    def reconcile(self):
        """Read IBKR's account in full, match its open orders and executions to the journal, then mark the link reconciled."""
        reading = read_account(self.conn)
        if reading.unread:
            raise ReconcileIncomplete("IBKR's account could not be read in full ({} unread); nothing is sent until a "
                                      "read is complete (ADR 0019: an unavailable balance is not zero)".format(
                                          ", ".join(sorted(reading.unread))))
        broker = collect_broker_orders(self.conn)
        # An order an earlier run of this engine released (the runs its Run
        # approvals chain back to) is this engine's too: IBKR's day lists
        # still show it after a fresh run starts.
        expected = set(self.pending.handovers())
        for path in run_logs(self._engine_log_path, self._session(), settled=self.pending.settled_orders()):
            expected |= released_client_order_ids(path)
        match = match_on_reconnect(expected, broker, self.config.client_id)
        if match.unknown_at_broker or match.unknown_executions:
            unexplained = ["open order orderRef={} client_id={} order_id={} perm_id={}".format(
                r.client_order_id or "-", r.client_id, r.broker_order_id, r.perm_id) for r in match.unknown_at_broker]
            unexplained += ["execution {} orderRef={} client_id={} order_id={}".format(
                r.exec_id, r.client_order_id or "-", r.client_id, r.broker_order_id) for r in match.unknown_executions]
            raise ReconcileMismatch(
                "IBKR shows {} unexplained open order(s) or execution(s) in the account, which no order the engine "
                "released explains: {}. Nothing is sent: check them in IBKR (an order placed by hand or by another "
                "client, or one no run of this engine released) and reconcile the account before starting again "
                "(docs/manual/paper-trading.md)".format(len(unexplained), "; ".join(unexplained)))
        self.conn.mark_reconciled()
        log.info("ibkr reconciled client_id=%d positions=%d open_orders=%d executions=%d", self.config.client_id,
                 len(reading.positions), len(broker.open_orders), len(broker.executions))

    def submit(self, event_type, schema_version, payload, event_time, input_id=None):
        """Record an input for the engine in the outbox; it is sent, in order, while the link is ready."""
        self.outbox.append({"id": input_id or self._new_id(event_type), "event_type": event_type,
                            "schema_version": schema_version, "event_time": event_time, "payload": payload})

    def step(self):
        """One turn of the loop: keep the link up, reconcile it if needed, take one callback, check, and drain."""
        self.conn.maintain()
        if self.conn.connected and self.conn.needs_reconciliation:
            self.reconcile()
        self.conn.pump(self._poll)
        self.reports.check_overdue()
        if self.ready():
            if self.delivery is not None:
                self.delivery.deliver()
            self._drain()

    def run(self, should_stop):
        """start, then step until should_stop() is true; the adapter's files are released however it ends."""
        try:
            self.start()
            while not should_stop():
                self.step()
        finally:
            self.close()

    def close(self):
        if self.relay is not None and self.relay.holds():
            self.relay.close()
        if self.conn is not None:
            try:
                self._api.disconnect()
            except Exception as err:  # the adapter is stopping either way
                log.info("ibkr disconnect at stop failed: %s", type(err).__name__)

    # -- the engine side ---------------------------------------------------
    def _journalled(self, chain):
        """The report records this run's or an earlier run's engine holds, the outbox holds, or that await a reply: never recorded again.

        chain is each run's (identity, EngineLog), newest first
        (relay.run_logs, relay.run_identity). The journal keeps the last
        status seeded for each order, so the records are seeded in the
        order they were made, never by their times: a lifecycle report's
        time is the adapter's clock at receipt (IBKR's orderStatus carries
        none), whole seconds only, and a clock set back would reorder them.
        That order is total: each run of the chain oldest first, its
        committed inputs in log order, then the run-ended settlements of
        that run in the order they were recorded (each input, then the
        outbox entries struck off with it, which the outbox held behind the
        run's committed inputs); then the input awaiting its reply, which is
        the outbox's head while it is exchanged; then the outbox's entries
        in order. A settlement whose run is no run of the chain was made
        before every run it reaches; one with no recorded run (its .run
        record was missing or named another input) has no place that can
        be known, so run_ended refuses it and the start stops for a person.
        The outbox's own copy of a
        record placed earlier is not seeded again. A record whose event
        time cannot be read stops the start (ReportRecordInvalid; fail
        closed, AGENTS.md), as every other record the adapter keeps is read
        strictly.
        """
        oldest = list(reversed(chain))
        position = {run: n for n, (run, _) in enumerate(oldest)}
        settled = self.pending.run_ended()
        placed = [None] * len(settled)
        bound = len(oldest) - 2
        for k in reversed(range(len(settled))):
            placed[k] = bound = min(position.get(settled[k]["run"], -1), bound)
        sources = []

        def add_settled(at):
            for k, entry in enumerate(settled):
                if placed[k] != at:
                    continue
                sources.append((self.pending.run_ended_path, entry["input"].get("id"), entry["input"].get("type"),
                                entry["input"].get("schema_version"), entry["input"].get("payload") or {}))
                sources.extend((self.pending.run_ended_path, o["id"], o["event_type"], o["schema_version"],
                                o["payload"]) for o in entry["outbox"])
        add_settled(-1)
        for n, (run, log_) in enumerate(oldest):
            sources.extend(("the durable log of run {}".format(run), i.id, i.event_type, i.schema_version, i.payload)
                           for i in log_.inputs)
            add_settled(n)
        # The outbox's copies of what is placed earlier (the input awaiting
        # its reply, an entry a settlement names, an input this run
        # committed before its strike) are never sent, so never seeded again.
        placed_ids = {i.id for i in oldest[-1][1].inputs} if oldest else set()
        placed_ids |= {e["input"].get("id") for e in settled} | {o["id"] for e in settled for o in e["outbox"]}
        waiting = self.pending.load()
        if waiting is not None:
            sources.append((self.pending.path, waiting.get("id"), waiting.get("type"), waiting.get("schema_version"),
                            waiting.get("payload") or {}))
            placed_ids.add(waiting.get("id"))
        sources.extend((self.outbox.path, e["id"], e["event_type"], e["schema_version"], e["payload"])
                       for e in self.outbox.load() if e["id"] not in placed_ids)
        records = []
        for where, input_id, event_type, schema_version, payload in sources:
            if event_type not in EVENT_TIME_FIELD:
                continue
            field = EVENT_TIME_FIELD[event_type]
            if not _event_time_readable(payload, field):
                raise ReportRecordInvalid(
                    "the report {} in {} has no {} that can be read (a time with its zone): it is not as the "
                    "adapter records one, so it is never seeded as an order's status; check it against IBKR "
                    "before starting (docs/manual/redelivery.md)".format(input_id, where, field))
            records.append(Record(event_type, schema_version, payload))
        return records

    def _record(self, record):
        """The report journal's sink: record IBKR's report as the engine's next input."""
        input_id = self._new_id(record.event_type)
        self.submit(record.event_type, record.schema_version, record.payload,
                    record.payload[EVENT_TIME_FIELD[record.event_type]], input_id=input_id)
        self._reports_recorded += 1
        self._last_report = input_id

    def _sent(self, record):
        log.info("ibkr sent client_order_id=%s client_id=%d order_id=%d", record.request.client_order_id,
                 record.client_id, record.broker_order_id)
        self.reports.sent(record)
        self.tracker.placed(record.broker_order_id)

    def _await_engine(self, worker):
        """While worker asks the engine, read every callback IB Gateway has sent and check for overdue commissions."""
        while True:
            # Read after the worker is seen finished too, so whatever arrived
            # during the exchange is applied before anything else.
            alive = worker.is_alive()
            self._pump_all()
            if not alive:
                return
            worker.join(ENGINE_WAIT_POLL)

    def _pump_all(self):
        """Apply every callback IB Gateway has already sent, then check for overdue commissions."""
        while self.conn.pump(0) is not None:
            pass
        self.reports.check_overdue()

    def _count_execution(self, event):
        """Count an execution IBKR reports in the account, of any client's order or none (see _activity)."""
        if event.name == "exec_details" and len(event.args) == 3:
            if getattr(event.args[2], "acctNumber", None) == self.config.account_id:
                self._account_executions += 1

    def _activity(self):
        """A broker snapshot's cutoff watermark: the order reports recorded, and the executions seen.

        The executions are this adapter's (IBKR's replays not counted again)
        and every one IBKR reports in the account from any client: an order
        placed in IBKR's own app or by another client changes the holdings
        and cash as much as this adapter's do.
        """
        return self._reports_recorded, self.reports.executions_seen(), self._account_executions

    def _broker_snapshot(self):
        """The account.broker.snapshot of one consistent account read, with IBKR's marks (see the module).

        None when it cannot be taken now (the link went, an execution awaits
        its commission, a read was cut off before answers that carry no
        request ID ended, or the account changed on every attempt); SnapshotRefused when
        the account could not be read in full within SNAPSHOT_ATTEMPTS.
        Marks are asked for each holding the configuration has IBKR's
        contract number for; any other holding is stated with no mark.
        market_open is the NYSE session's state at the read's time.
        """
        unread = None
        for attempt in range(1, SNAPSHOT_ATTEMPTS + 1):
            self._pump_all()
            if not self.ready():
                log.info("ibkr link went during the broker snapshot; the stream waits until it is reconciled")
                return None
            if self.reports.awaiting_commission():
                log.info("ibkr broker snapshot waits: an execution awaits its commission, so its fill is not "
                         "recorded yet")
                return None
            before = self._activity()
            reading = read_account(self.conn)
            if self._unended(reading):
                return None
            con_ids = {s: self.config.con_ids[s] for s in reading.positions if s in self.config.con_ids}
            marks = read_marks(self.conn, con_ids)
            # Read again after the marks: a trade IBKR reports to another
            # client only (an order placed in its own app) still changes
            # the holdings, open orders and cash, and only a second read
            # shows it.
            confirm = read_account(self.conn) if not reading.unread else None
            if confirm is not None and self._unended(confirm):
                return None
            if not self.ready():
                log.info("ibkr link went during the broker snapshot; the stream waits until it is reconciled")
                return None
            if self._activity() != before:
                log.info("ibkr broker snapshot read again (attempt %d of %d): an order report or execution "
                         "arrived between its reads", attempt, SNAPSHOT_ATTEMPTS)
                unread = None
                continue
            incomplete = reading.unread | (confirm.unread if confirm is not None else frozenset())
            if incomplete:
                unread = sorted(incomplete)
                log.info("ibkr broker snapshot read again (attempt %d of %d): %s could not be read", attempt,
                         SNAPSHOT_ATTEMPTS, ",".join(unread))
                continue
            if _holdings(reading) != _holdings(confirm):
                log.info("ibkr broker snapshot read again (attempt %d of %d): the account's holdings, open orders "
                         "or cash changed between its reads", attempt, SNAPSHOT_ATTEMPTS)
                unread = None
                continue
            log.info("ibkr broker snapshot as_of=%s cutoff: no order report, execution or account change between "
                     "its reads; the last report recorded before it %s", _stamp(confirm.as_of),
                     self._last_report or "none")
            # The second read's as_of is IBKR's time after every component
            # and the marks were read.
            return broker_snapshot_record(confirm, market_open(confirm.as_of), marks)
        if unread is not None:
            raise SnapshotRefused(
                "the account could not be read in full for the broker snapshot in {} attempts ({} could not be "
                "read), so the day's stream is not delivered: a snapshot with values IBKR did not give would "
                "withhold the sale back to 98% for nothing. Check IB Gateway's link and the account's currency "
                "and positions, then start the adapter again (docs/manual/market-data.md, \"What happens to the "
                "file\")".format(SNAPSHOT_ATTEMPTS, ", ".join(unread)))
        return None

    def _unended(self, reading):
        """Whether reading was cut off before answers that carry no request ID ended; if so the link is dropped (see the module)."""
        if not reading.unended:
            return False
        log.info("ibkr broker snapshot: IBKR had not ended its %s when the read gave up, and a late end would "
                 "complete a later read on this link, so the link is made again and reconciled first",
                 ",".join(sorted(reading.unended)))
        self.conn.drop("untagged-read-interrupted")
        return True

    def _envelope(self, entry, engine_log):
        """The input envelope for an outbox entry: the run's identity and next sequence from the engine's log.

        A stream entry is its envelope as the data job wrote it, with the
        engine's next sequence in place of the file's own and its send time
        as its recorded time, as every input's (see the module).
        """
        if "envelope" in entry:
            envelope = dict(entry["envelope"])
            envelope["sequence"] = engine_log.next_sequence
            envelope["recorded_at"] = _stamp(self._now())
            return envelope
        encoded = json.dumps(entry["payload"], separators=(",", ":"), allow_nan=False).encode("utf-8")
        return {"id": entry["id"], "type": entry["event_type"], "envelope_version": ENVELOPE_VERSION,
                "schema_version": entry["schema_version"], "event_time": entry["event_time"],
                "recorded_at": _stamp(self._now()), "sequence": engine_log.next_sequence, "source": SOURCE,
                "strategy_version": engine_log.strategy_version,
                "configuration_hash": engine_log.configuration_hash,
                "payload_hash": hashlib.sha256(encoded).hexdigest(), "payload": entry["payload"]}

    def _settle_waiting(self):
        """Ask again, byte for byte, for the reply to the input a stop left awaiting one; strike it off the outbox.

        Only this run's input is asked for again (ADR 0027, the restart
        model: a crash resumes the same run; any other restart is a fresh
        run): one an earlier run left stops the adapter for a person
        (EarlierRunInput), and nothing of it is sent.
        """
        waiting = self.pending.load()
        if waiting is None:
            return
        ended = earlier_run(self.pending, self._engine_log_path)
        if ended is not None:
            raise EarlierRunInput(ended, self._pending_path, self._engine_log_path)
        self._exchange(waiting)

    def _exchange(self, envelope):
        # The relay strikes the entry off before it clears the input
        # awaiting its reply (its settled).
        reply = self.relay.exchange(envelope)
        kinds = [d.get("type") for d in reply["payload"]["decisions"]]
        log.info("ibkr input=%s type=%s answered with %d decisions %s", envelope["id"], envelope["type"],
                 len(kinds), ",".join(kinds))

    def _drain(self):
        try:
            self._settle_waiting()
            while True:
                entries = self.outbox.load()
                if not entries:
                    return
                entry = _next_entry(entries)
                settled = self.pending.run_ended()
                if entry["id"] in {e["input"]["id"] for e in settled} | {o["id"] for e in settled
                                                                        for o in e["outbox"]}:
                    # Settled as left by an earlier run, or recorded behind
                    # an input so settled: never sent.
                    self.outbox.strike(entry["id"])
                    continue
                batch_run = _batch_run(entry)
                # The run's stream folder name: a crash resume keeps the
                # first run's (delivery.stream_run), so its batch goes on.
                running = self.delivery.run if self.delivery is not None else run_name(self._engine_log_path)
                if batch_run is not None and batch_run != running:
                    # An earlier run's stream batch: the run now running is a
                    # fresh run with its own, and the earlier run gets it
                    # again if resumed.
                    # Marked dropped first, then struck off whole in one
                    # write, so a stop leaves the batch whole in the outbox
                    # (sent whole to that run if it is resumed) or wholly
                    # gone, never its stream without its snapshot.
                    batch = [e["id"] for e in entries if _batch_run(e) == batch_run]
                    log.info("ibkr stream batch of run %s (%d inputs) struck off unsent: the engine now runs %s",
                             batch_run, len(batch), running)
                    if self.delivery is not None:
                        self.delivery.dropped(batch_run)
                    self.outbox.strike_all(batch)
                    continue
                engine_log = read_engine_log(self._engine_log_path)
                committed = {i.id: i for i in engine_log.inputs}.get(entry["id"])
                if committed is not None:
                    if (committed.event_type, committed.schema_version, committed.payload) != (
                            entry["event_type"], entry["schema_version"], entry["payload"]):
                        raise OutboxInvalid(
                            "the outbox entry {} carries other content than the input the engine committed under "
                            "that ID; it is neither sent nor struck off: a person decides which is right "
                            "(docs/manual/paper-trading.md)".format(entry["id"]))
                    self.outbox.strike(entry["id"])
                    continue
                if batch_run is not None and self.reports.awaiting_commission():
                    # An execution IBKR reported awaits its commission, so its
                    # fill is not recorded yet: no stream input goes ahead of
                    # it (ADR 0019: no decision on a book behind a report
                    # received). The next steps keep reading IB Gateway, and
                    # check_overdue stops the adapter for a person once the
                    # commission is commission_wait late.
                    log.info("ibkr stream input %s waits: an execution awaits its commission, so its fill is "
                             "not recorded yet", entry["id"])
                    return
                # Recorded before it is first sent, so a restart tells this
                # run's input from an earlier run's.
                self.pending.record_run(entry["id"], run_identity(self._engine_log_path),
                                        os.path.basename(self._engine_log_path))
                self._exchange(self._envelope(entry, engine_log))
        except (NotReconciled, NotConnected) as err:
            # The link went while an order was due: the input stays on disk
            # and is asked for again once the link is back and reconciled.
            log.info("ibkr input held until the link is reconciled: %s", err)
        except BrokerOrdersIncomplete as err:
            if "connection" not in err.missing:
                raise
            # The link went while IBKR's orders were read for an order due:
            # nothing was marked or sent, so the input is held as above.
            log.info("ibkr input held until the link is reconciled: %s", err)
            if "open-orders" in err.missing:
                # IBKR's open-order list carries no request ID, and Gateway
                # restores a link it lost IBKR's servers on as the same
                # socket, where the abandoned list's late end would complete
                # the next read: the link is made again (see the module).
                self.conn.drop("untagged-read-interrupted")

    def _status(self, status):
        log.info("ibkr link %s %s", status.state, status.reason)
