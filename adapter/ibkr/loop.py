"""The IBKR adapter's main loop: one engine's link to IB Gateway, kept up, reconciled, and drained into the engine.

ADR 0025 points 2 and 5, ADR 0027 section 4. At start the loop connects
with the engine's own client ID and, before any order can be sent, reads
the account (snapshot.read_account: positions, balances and open orders)
and IBKR's open orders and the session's executions
(orders.collect_broker_orders), refusing to go on if any of it could not be
read (ReconcileIncomplete; an unavailable balance is not zero, ADR 0019),
and only then marks the link reconciled (Connection.mark_reconciled, which
refuses an account the login does not manage). It does the same after
every reconnect or restoration. It then settles the input left awaiting
its reply by a stop, if any, before anything new.

Each step keeps the link up (Connection.maintain), takes one callback from
IB Gateway (Connection.pump), checks for executions whose commission report
is overdue (OrderReportJournal.check_overdue: the invariant reports.py
states, that the check runs after each pump, so a missing commission stops
the adapter on time and not only when IBKR next writes), and, while the
link is up and reconciled, sends the engine every input recorded since,
oldest first, through the relay, which hands each released order marked
send to the sender once (relay.Relay).

Inputs are recorded in the outbox (OUTBOX_SUFFIX beside the pending file)
before they are sent, written durably, so a report IBKR will not repeat (a
cancellation of an order no longer open) survives a stop. An outbox entry
carries its input ID from the moment it is recorded; its sequence and the
run's identity are taken from the engine's durable log when it is sent
(relay.read_engine_log), so a stop between the engine's reply and the entry
being struck off sends nothing twice: an entry the log already holds as a
committed input is struck off unsent. The records already committed, in the
outbox, or awaiting their reply seed the report journal, so IBKR's replay
after a restart records nothing twice.

The engine must be running, with its durable log begun, before the adapter
starts: the log is the run's identity. Nothing here sends when the
configuration is not paper (sender.make_sender), and the real client sends
only with paper_sending_enabled on (real_client.IbapiClient.can_place_orders).
"""
import hashlib
import json
import logging
import os
import time
import uuid
from datetime import datetime, timezone

from connection import Connection, NotConnected
from orders import OrderTracker, collect_broker_orders
from relay import PendingInput, Relay, read_engine_log
from reports import FILL_TYPE, LIFECYCLE_TYPE, OrderReportJournal
from sender import NotReconciled, make_sender, release_to_request
from snapshot import Record, read_account

log = logging.getLogger("ibkr")

# The adapter's name in every input it sends (event.AdapterIBKR; ADR 0025
# point 3).
SOURCE = "ibkr"
ENVELOPE_VERSION = 1
# The adapter's files beside its pending file (relay.PendingInput's own
# suffixes are .handover, .refused, .corrections, .late-fills and .lock).
OUTBOX_SUFFIX = ".outbox"
HELD_SUFFIX = ".held"
OUTBOX_FIELDS = ("event_time", "event_type", "id", "payload", "schema_version")
# The input types whose event time is in their payload.
EVENT_TIME_FIELD = {LIFECYCLE_TYPE: "occurred_at", FILL_TYPE: "filled_at"}


class ReconcileIncomplete(Exception):
    """IBKR's account could not be read in full, so the link is not reconciled and nothing is sent."""


class OutboxInvalid(ValueError):
    """The outbox file is not as the adapter writes it; the adapter stops before sending anything."""


def _utc_now():
    return datetime.now(timezone.utc)


def _stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _new_id(event_type):
    return "{}:{}:{}".format(SOURCE, event_type, uuid.uuid4().hex)


class Outbox:
    """The inputs recorded for the engine and not yet struck off, oldest first, in a JSON file rewritten atomically."""

    def __init__(self, path):
        self.path = path

    def load(self):
        try:
            with open(self.path, encoding="utf-8") as f:
                entries = json.load(f)
        except FileNotFoundError:
            return []
        except ValueError as err:
            raise OutboxInvalid("{} is not JSON: {}".format(self.path, err)) from None
        if not isinstance(entries, list) or not all(
                isinstance(e, dict) and tuple(sorted(e)) == OUTBOX_FIELDS and isinstance(e["id"], str) and e["id"]
                and isinstance(e["payload"], dict) for e in entries):
            raise OutboxInvalid("{} is not a list of inputs as the adapter records them".format(self.path))
        if len({e["id"] for e in entries}) != len(entries):
            raise OutboxInvalid("{} names one input twice".format(self.path))
        return entries

    def append(self, entry):
        self._write(self.load() + [entry])

    def strike(self, input_id):
        """Remove the entry for input_id; one not there is already struck off."""
        entries = self.load()
        kept = [e for e in entries if e["id"] != input_id]
        if len(kept) != len(entries):
            self._write(kept)

    def _write(self, entries):
        tmp = self.path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(entries, f, separators=(",", ":"), allow_nan=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)
        fd = os.open(os.path.dirname(os.path.abspath(self.path)), os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


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
        self.relay = Relay(self._engine_connect, sender, self.pending, lambda: collect_broker_orders(self.conn),
                           lambda order, session: release_to_request(order, cfg.mode, cfg.con_ids, session),
                           today=self._today)
        engine_log = read_engine_log(self._engine_log_path)
        self.reports = OrderReportJournal(
            cfg.account_id, cfg.client_id, self._record, now=self._now, commission_wait=cfg.commission_wait,
            held_path=self._pending_path + HELD_SUFFIX, owns_lock=self.relay.holds, ready=self.ready,
            journalled=self._journalled(engine_log), acknowledged=self.pending.acknowledged_corrections(),
            late_fills=self.pending.acknowledged_late_fills())
        self.conn.subscribe(OrderTracker(cfg.account_id, cfg.client_id, self.reports.on_record).on_event)
        self.conn.connect_with_backoff()
        self.reconcile()
        self._settle_waiting()

    def ready(self):
        """Whether the link is up and reconciled since it last connected or was restored."""
        return self.conn is not None and self.conn.connected and not self.conn.needs_reconciliation

    def reconcile(self):
        """Read IBKR's account, open orders and executions in full, then mark the link reconciled."""
        reading = read_account(self.conn)
        if reading.unread:
            raise ReconcileIncomplete("IBKR's account could not be read in full ({} unread); nothing is sent until a "
                                      "read is complete (ADR 0019: an unavailable balance is not zero)".format(
                                          ", ".join(sorted(reading.unread))))
        broker = collect_broker_orders(self.conn)
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
    def _journalled(self, engine_log):
        """The report records the engine holds, has in the outbox, or awaits a reply to: never recorded again."""
        records = [Record(i.event_type, i.schema_version, i.payload) for i in engine_log.inputs]
        records += [Record(e["event_type"], e["schema_version"], e["payload"]) for e in self.outbox.load()]
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
        """Ask again, byte for byte, for the reply to the input a stop left awaiting one; strike it off the outbox."""
        waiting = self.pending.load()
        if waiting is None:
            return
        self._exchange(waiting)

    def _exchange(self, envelope):
        reply = self.relay.exchange(envelope)
        self.outbox.strike(envelope["id"])
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
                engine_log = read_engine_log(self._engine_log_path)
                if entry["id"] in {i.id for i in engine_log.inputs}:
                    self.outbox.strike(entry["id"])
                    continue
                self._exchange(self._envelope(entry, engine_log))
        except (NotReconciled, NotConnected) as err:
            # The link went while an order was due: the input stays on disk
            # and is asked for again once the link is back and reconciled.
            log.info("ibkr input held until the link is reconciled: %s", err)

    def _status(self, status):
        log.info("ibkr link %s %s", status.state, status.reason)
