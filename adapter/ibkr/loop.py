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
their reply seed the report journal, oldest run first, so IBKR's replay
after a restart records nothing twice and each order keeps its latest
status.

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
from orders import BrokerOrdersIncomplete, OrderTracker, collect_broker_orders, match_on_reconnect
from relay import (OUTBOX_SUFFIX, EarlierRunInput, Outbox, OutboxInvalid, PendingInput, Relay, earlier_run,
                   read_engine_log, released_client_order_ids, run_identity, run_logs)
from reports import FILL_TYPE, LIFECYCLE_TYPE, OrderReportJournal
from sender import NotReconciled, make_sender, release_to_request
from snapshot import Record, read_account

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


class ReconcileIncomplete(Exception):
    """IBKR's account could not be read in full, so the link is not reconciled and nothing is sent."""


class ReconcileMismatch(Exception):
    """IBKR shows an open order or execution the journal does not explain; the link is not reconciled."""


def _utc_now():
    return datetime.now(timezone.utc)


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
    poll how long one step waits for a callback (seconds).
    """

    def __init__(self, config, api, engine_connect, pending_path, engine_log_path, now=_utc_now, today=None,
                 sleep=time.sleep, monotonic=time.monotonic, poll=1.0, new_id=_new_id):
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

    def start(self):
        """Take the pending file's lock, connect, reconcile, and settle any input left awaiting its reply."""
        cfg = self.config
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
        engine_logs = [read_engine_log(path) for path in logs]
        self.reports = OrderReportJournal(
            cfg.account_id, cfg.client_id, self._record, now=self._now, commission_wait=cfg.commission_wait,
            held_path=self._pending_path + HELD_SUFFIX, owns_lock=self.relay.holds, ready=self.ready,
            journalled=self._journalled(engine_logs), acknowledged=self.pending.acknowledged_corrections(),
            late_fills=self.pending.acknowledged_late_fills())
        self.tracker = OrderTracker(cfg.account_id, cfg.client_id, self.reports.on_record)
        self.conn.subscribe(self.tracker.on_event)
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
    def _journalled(self, engine_logs):
        """The report records this run's or an earlier run's engine holds, the outbox holds, or that await a reply: never recorded again.

        engine_logs is newest first (relay.run_logs); the journal keeps each
        order's last status seeded, so the logs are read oldest first.
        """
        records = [Record(i.event_type, i.schema_version, i.payload) for log_ in reversed(engine_logs)
                   for i in log_.inputs]
        records += [Record(e["event_type"], e["schema_version"], e["payload"]) for e in self.outbox.load()]
        settled = self.pending.run_ended()
        records += [Record(e["input"].get("type"), e["input"].get("schema_version"), e["input"].get("payload") or {})
                    for e in settled]
        records += [Record(o["event_type"], o["schema_version"], o["payload"]) for e in settled for o in e["outbox"]]
        waiting = self.pending.load()
        if waiting is not None:
            records.append(Record(waiting.get("type"), waiting.get("schema_version"), waiting.get("payload") or {}))
        return [r for r in records if r.event_type in EVENT_TIME_FIELD]

    def _record(self, record):
        """The report journal's sink: record IBKR's report as the engine's next input."""
        self.submit(record.event_type, record.schema_version, record.payload,
                    record.payload[EVENT_TIME_FIELD[record.event_type]])

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
            while self.conn.pump(0) is not None:
                pass
            self.reports.check_overdue()
            if not alive:
                return
            worker.join(ENGINE_WAIT_POLL)

    def _envelope(self, entry, engine_log):
        """The input envelope for an outbox entry: the run's identity and next sequence from the engine's log."""
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
                entry = entries[0]
                settled = self.pending.run_ended()
                if entry["id"] in {e["input"]["id"] for e in settled} | {o["id"] for e in settled
                                                                        for o in e["outbox"]}:
                    # Settled as left by an earlier run, or recorded behind
                    # an input so settled: never sent.
                    self.outbox.strike(entry["id"])
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

    def _status(self, status):
        log.info("ibkr link %s %s", status.state, status.reason)
