"""IBKR's reports on this engine's orders as the engine's inputs: lifecycle reports and fills.

ADR 0022 and ADR 0026 point 2: a venue's report on a released limit order is
execution.order.lifecycle version 2 (LimitOrderLifecyclePayload) and an
execution of it is execution.fill version 5 (TargetWeightFillPayload). The
engine reads both by one rule (internal/event OrderReports): a report must
describe the order as it was released, a fill must not take it past its
quantity, and a fill ID names one execution. So each report describes the
order as IBKR holds it (its last openOrder), never as the adapter meant it,
and the engine, not the adapter, judges whether that is the order released;
only an order IBKR refused before showing it is described by the request
the adapter sent.

An order is this engine's when it was placed under this engine's own client
ID (ADR 0026 point 8) in this account and carries a client order ID; any
other order or execution is left to reconciliation (ADR 0019). IBKR's order
ID comes only from its reports, as "order:<clientId>:<orderId>", the form
the broker snapshot names it by (ADR 0026 point 2, "Order identity").
IBKR's orderStatus carries no time, so a lifecycle report's occurred_at is
the adapter's receipt time; a fill's is IBKR's execution time. Both are whole
seconds. A report that cannot be stated in the engine's terms (no
identity, fractional shares, a price that is not a finite positive number,
a commission in another currency, not finite, negative, or not below the
order's notional) raises UnreportableOrder: it is never journalled
approximately (fail closed).

IBKR corrects an execution by reporting it again under an execId with the
same base (every segment but the last) and a later last segment, without
withdrawing the original. The engine reads a new fill ID as a new execution,
so a correction is never journalled as a fill: it raises UnreportableOrder
and the run stops for a person to reconcile the order (ADR 0019). IBKR
replays the day's executions on every start, so the correction would stop
every restart too; the owner's acknowledgement of it (relay.py
acknowledge-correction) names the correction and the execution it corrects,
and a journal given that acknowledgement passes over the correction. It is
still never journalled: the engine keeps the original fill, and the
difference is the owner's to reconcile.

A report that would end the order (canceled, invalid) is held while an
execution of that order still waits for its commission, and journalled
straight after the fills of every execution of that order seen before it
is journalled: the engine reads no fill on an order already ended. The
hold is kept on disk (held_path, beside the journal) before the report is
accepted, and rewritten before a further execution of the held order is
accepted, since IBKR replays an execution and its commission on every
start but never the status of an order no longer open: a stop before the
commission would otherwise lose the cancellation and leave the order
working in the engine, or journal it before a fill IBKR had already
reported. An execution of an order whose terminal report is already
journalled, and any other status for an order after its terminal report,
raises UnreportableOrder: a fill or a change after a cancellation is a
contradiction for a person to reconcile (ADR 0019). The held reports file
is this adapter's alone: it is read and written only while the adapter
holds its exclusive lock (owns_lock; relay.Relay.holds), so a second
adapter started beside the first journals nothing. A restarted journal
reads the file strictly and journals each held report once the executions
it waits for are journalled (at start, if they already are); one whose
executions IBKR does not replay within commission_wait of the link being
up and reconciled (ready) raises UnreportableOrder from check_overdue. A
held report leaves the file only after its own journal write: a failed
write stops the run with its cause and keeps the hold, and a repeated
record or a restart writes it once. A correction of an execution never
journalled as a fill is never acknowledged (relay.py
acknowledge-correction): the engine has no original to keep, and the
shares are reconciled by hand. An order IBKR shows with another side,
quantity or limit than the one already reported raises UnreportableOrder:
IBKR changed a working order, and the engine judges each report against
the order it released.
"""
import json
import math
import os
import re
from datetime import datetime, timezone
from decimal import Decimal

from orders import CommissionReport, ExecutionReport, OpenOrderRecord, OrderStatusReport
from snapshot import Record

LIFECYCLE_TYPE = "execution.order.lifecycle"
LIFECYCLE_VERSION = 2
FILL_TYPE = "execution.fill"
FILL_VERSION = 5
# The engine's balances and fills are in US dollars.
BASE_CURRENCY = "USD"
SIDES = {"BUY": "buy", "SELL": "sell"}
EXECUTION_SIDES = {"BOT": "buy", "SLD": "sell"}
# ibapi marks an unset price with sys.float_info.max (UNSET_DOUBLE); any
# figure this large is that marker, never a price or a commission.
UNSET_FIGURE = Decimal("1.7E308")
# The ADR 0022 statuses that end an order.
TERMINAL = ("canceled", "invalid")
# The fields of one held report in the held reports file.
HELD_FIELDS = ("awaits", "client_id", "order_id", "record", "status")
RECORD_FIELDS = ("event_type", "payload", "schema_version")
# The fields of a lifecycle report's payload, as _on_status writes them.
LIFECYCLE_FIELDS = ("account_id", "client_order_id", "instrument_id", "limit_price", "message", "occurred_at",
                    "order_id", "quantity", "side", "status")
# A whole-second UTC time, as _stamp writes it.
STAMP = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z")


class UnreportableOrder(Exception):
    """IBKR reported on this engine's order in a form the engine cannot read; the run must stop."""


def _utc(at, what):
    """at in UTC, refusing a time with no zone: astimezone would read it in the server's own zone."""
    if not isinstance(at, datetime) or at.tzinfo is None or at.utcoffset() is None:
        raise UnreportableOrder("{} {!r} has no time zone; the engine reads UTC times only".format(what, at))
    return at.astimezone(timezone.utc)


def _stamp(at, what="time"):
    return _utc(at, what).strftime("%Y-%m-%dT%H:%M:%SZ")


def _exec_base(exec_id):
    """An execId's base, every segment but the last, which a correction of the execution keeps."""
    base, dot, last = str(exec_id).rpartition(".")
    if not dot or not base or not last:
        raise UnreportableOrder("execution {!r} has no base and last segment to tell a correction by".format(
            exec_id))
    return base


def check_acknowledged(acknowledged):
    """Refuse (ValueError) an acknowledgement that does not name a correction of the execution it corrects.

    acknowledged maps a correction's execId to the execId it corrects; the
    two share their base and differ in their last segment.
    """
    for corrected, original in dict(acknowledged).items():
        try:
            same = corrected != original and _exec_base(corrected) == _exec_base(original)
        except UnreportableOrder as err:
            raise ValueError(str(err)) from None
        if not same:
            raise ValueError("execution {!r} is not a correction of execution {!r}: a correction keeps the base "
                             "and changes the last segment".format(corrected, original))


def _order_key(order_id):
    """(client ID, IBKR order ID) from a lifecycle record's "order:<clientId>:<orderId>", or None."""
    parts = str(order_id).split(":")
    if len(parts) != 3 or parts[0] != "order" or not parts[1].isdigit() or not parts[2].isdigit():
        return None
    return int(parts[1]), int(parts[2])


def _whole(value, what):
    if value != value.to_integral_value():
        raise UnreportableOrder("{} {} is not whole shares".format(what, value))
    return int(value)


class OrderReportJournal:
    """Turns OrderTracker records on this engine's orders into lifecycle and fill inputs passed to sink.

    Subscribe on_record as the OrderTracker's sink, and pass every SentRecord
    the paper sender journals to sent before the connection is next pumped.
    A lifecycle report is journalled when an order's ADR 0022 status
    changes, not for every orderStatus IBKR repeats. A fill is journalled
    when its commission arrives, which IBKR sends after the execution; an
    execution still without one commission_wait seconds after it arrived
    raises UnreportableOrder from check_overdue, so a fill IBKR made is never
    silently left unrecorded. Invariant for the caller: the loop that pumps
    the connection must call check_overdue after each pump; until it does,
    the check runs only when the next record arrives (on_record runs it after
    a commission report and before any other record, so the report that
    completes a waiting execution is never refused). now returns the
    adapter's receipt time, with its zone.

    journalled holds the records already written for the engine (the
    durable journal, read back on a restart), so IBKR's replay of each
    working order's status and each execution with its commission, which it
    sends on every read after a reconnect, writes nothing twice.
    acknowledged maps each correction the owner acknowledged to the execId
    it corrects (check_acknowledged; ValueError otherwise). held_path is the
    file, beside the journal, that keeps each report held for a waiting
    execution across a stop. owns_lock returns whether this adapter holds
    its exclusive lock (relay.Relay.holds), so construct the journal after
    the relay: the held file is read and written only while it does
    (UnreportableOrder otherwise, before anything is read or journalled).
    The file is read strictly at construction (UnreportableOrder for
    anything but what this journal writes, under its own account and client
    ID), and a held report whose executions are already journalled is
    journalled then. ready returns whether the link is up and reconciled
    (connected, and Connection.mark_reconciled since the last connect): the
    wait for IBKR's replay of a held report's executions starts at the first
    check_overdue that finds it so, and again after every reconnect.
    """

    def __init__(self, account_id, client_id, sink, now, commission_wait, held_path, owns_lock, ready,
                 journalled=(), acknowledged=None):
        if (isinstance(commission_wait, bool) or not isinstance(commission_wait, (int, float))
                or not math.isfinite(commission_wait) or commission_wait <= 0):
            raise ValueError("commission_wait must be a positive number of seconds, not {!r}".format(
                commission_wait))
        if not callable(owns_lock) or not callable(ready):
            raise TypeError("owns_lock and ready must be callables returning whether the adapter holds its lock "
                            "and whether its link is up and reconciled")
        check_acknowledged(acknowledged or {})
        self._acknowledged = dict(acknowledged or {})
        self.account_id = account_id
        self.client_id = client_id
        self._sink = sink
        self._now = now
        self._commission_wait = commission_wait
        self._requests = {}
        self._orders = {}
        self._status = {}
        # Executions awaiting their commission: exec_id to (report, receipt time).
        self._executions = {}
        # Each execution seen, journalled or awaiting its commission, by its
        # execId base: the full execId it was first reported under.
        self._seen = {}
        # Each order as last reported to the engine: (client order ID,
        # instrument, side, quantity, limit).
        self._described = {}
        # A report ending an order, held until the order's waiting
        # executions are journalled: order key to (record, status, the
        # execIds it waits for), kept in held_path.
        self._held = {}
        # Every fill ID journalled.
        self._fills = set()
        self._held_path = held_path
        self._owns_lock = owns_lock
        self._ready = ready
        # When the link was last found up and reconciled with a report held:
        # a held report waits for IBKR's replay of its executions no longer
        # than commission_wait from then.
        self._started = None
        for record in journalled:
            self._seed(record)
        self._require_lock()
        self._load_held()
        self._release_held()

    def _require_lock(self):
        if not self._owns_lock():
            raise UnreportableOrder(
                "this adapter does not hold its exclusive lock (another adapter or relay.py resolve has it, or it "
                "was closed), so the held reports in {} are neither read nor written; stop and check which "
                "adapter is running (docs/manual/paper-trading.md)".format(self._held_path))

    def _seed(self, record):
        payload = record.payload
        if payload.get("account_id") != self.account_id:
            return
        if record.event_type == LIFECYCLE_TYPE:
            key = _order_key(payload.get("order_id"))
            if key is not None:
                self._status[key] = payload.get("status")
                self._described[key] = (payload.get("client_order_id"), payload.get("instrument_id"),
                                        payload.get("side"), payload.get("quantity"), payload.get("limit_price"))
        elif record.event_type == FILL_TYPE:
            self._seen[_exec_base(payload["fill_id"])] = payload["fill_id"]
            self._fills.add(payload["fill_id"])

    def _load_held(self):
        """Read the held reports file strictly; a report already journalled (its status seeded) is dropped."""
        try:
            with open(self._held_path, encoding="utf-8") as f:
                entries = json.load(f)
        except FileNotFoundError:
            return
        except ValueError as err:
            raise UnreportableOrder("{} is not JSON: {}; check it against IBKR before starting "
                                    "(docs/manual/paper-trading.md)".format(self._held_path, err)) from None
        if not isinstance(entries, list):
            raise UnreportableOrder("{} is not a list of held reports".format(self._held_path))
        keys = set()
        for n, entry in enumerate(entries, 1):
            key = self._held_entry(entry)
            if key is None or key in keys:
                raise UnreportableOrder("{} entry {} is not a held report as this journal writes one{}; check it "
                                        "against IBKR before starting (docs/manual/paper-trading.md)".format(
                                            self._held_path, n, "" if key is None else " (a second for its order)"))
            keys.add(key)
            record = Record(LIFECYCLE_TYPE, LIFECYCLE_VERSION, entry["record"]["payload"])
            if self._status.get(key) != entry["status"]:
                self._held[key] = (record, entry["status"], frozenset(entry["awaits"]))

    def _held_entry(self, entry):
        """The order key of one held report exactly as this journal writes one, under its account and client ID, else None."""
        if not isinstance(entry, dict) or tuple(sorted(entry)) != HELD_FIELDS:
            return None
        record, awaits = entry["record"], entry["awaits"]
        if (not _is_id(entry["client_id"]) or not _is_id(entry["order_id"]) or entry["status"] not in TERMINAL
                or not isinstance(awaits, list) or not awaits
                or not all(isinstance(a, str) and a for a in awaits)
                or not isinstance(record, dict) or tuple(sorted(record)) != RECORD_FIELDS
                or record["event_type"] != LIFECYCLE_TYPE or record["schema_version"] != LIFECYCLE_VERSION
                or not isinstance(record["payload"], dict)):
            return None
        key = (entry["client_id"], entry["order_id"])
        payload = record["payload"]
        if (entry["client_id"] != self.client_id or tuple(sorted(payload)) != LIFECYCLE_FIELDS
                or payload["account_id"] != self.account_id or payload["status"] != entry["status"]
                or _order_key(payload["order_id"]) != key or not _is_text(payload["client_order_id"])
                or not _is_text(payload["instrument_id"]) or payload["side"] not in SIDES.values()
                or not _is_id(payload["quantity"]) or payload["quantity"] == 0
                or not _is_price(payload["limit_price"]) or not _is_stamp(payload["occurred_at"])
                or not isinstance(payload["message"], str)):
            return None
        return key

    def _save_held(self, held):
        """Write held as the held reports durably (fsync, atomic replace) before the caller adopts it.

        Only while this adapter holds its lock (UnreportableOrder otherwise,
        nothing written).
        """
        self._require_lock()
        entries = [{"client_id": key[0], "order_id": key[1], "status": status, "awaits": sorted(awaits),
                    "record": {"event_type": record.event_type, "schema_version": record.schema_version,
                               "payload": record.payload}}
                   for key, (record, status, awaits) in sorted(held.items())]
        tmp = self._held_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(entries, f, sort_keys=True, separators=(",", ":"), allow_nan=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self._held_path)
        fd = os.open(os.path.dirname(os.path.abspath(self._held_path)), os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    def _release_held(self):
        """Journal each held report whose executions are all journalled, then drop it from the file.

        The journal write comes first: if it fails, the report stays held,
        on disk and here, and the failure stops the run.
        """
        for key in sorted(self._held):
            record, status, awaits = self._held[key]
            if self._waiting(key) or not awaits <= self._fills:
                continue
            self._require_lock()
            self._sink(record)
            self._status[key] = status
            del self._held[key]
            self._save_held(self._held)

    def check_overdue(self):
        """Raise UnreportableOrder for an execution whose commission is commission_wait seconds late or more.

        Also for a held report whose executions IBKR has not replayed with
        their commissions within commission_wait of the link being found up
        and reconciled (ready); the wait starts again after a reconnect,
        since IBKR replays its executions on every one.
        """
        if not self._executions and not self._held:
            self._started = None
            return
        now = _utc(self._now(), "receipt time")
        if not self._held or not self._ready():
            self._started = None
        elif self._started is None:
            self._started = now
        for exec_id, (_, arrived) in self._executions.items():
            if (now - arrived).total_seconds() >= self._commission_wait:
                raise UnreportableOrder(
                    "execution {} has waited {} s or more for IBKR's commission report, without which its fill "
                    "cannot be recorded; check the trade in IBKR (docs/manual/paper-trading.md)".format(
                        exec_id, self._commission_wait))
        if self._started is not None and (now - self._started).total_seconds() >= self._commission_wait:
            for key, (record, status, awaits) in sorted(self._held.items()):
                missing = sorted((awaits - self._fills) - set(self._executions))
                if missing:
                    raise UnreportableOrder(
                        "order {} was reported {} while execution {} waited for its commission, and IBKR has not "
                        "replayed that execution within {} s of the start: neither can be recorded; check the "
                        "order and its trades in IBKR (docs/manual/paper-trading.md)".format(
                            record.payload.get("client_order_id"), status, ", ".join(missing),
                            self._commission_wait))

    def sent(self, record):
        """Remember the request an order was sent as, for a refusal IBKR reports before any openOrder."""
        self._requests[(record.client_id, record.broker_order_id)] = record.request

    def on_record(self, record):
        if isinstance(record, CommissionReport):
            self._on_commission(record)
            self._release_held()
            self.check_overdue()
            return
        self._release_held()
        self.check_overdue()
        if isinstance(record, OpenOrderRecord):
            self._on_open_order(record)
        elif isinstance(record, OrderStatusReport):
            self._on_status(record)
        elif isinstance(record, ExecutionReport):
            self._on_execution(record)

    def _ours(self, client_id, client_order_id):
        return client_id == self.client_id and bool(client_order_id)

    def _on_open_order(self, record):
        if record.account_id == self.account_id and self._ours(record.client_id, record.client_order_id):
            key = (record.client_id, record.broker_order_id)
            described = self._described.get(key)
            if described is not None and described != _figures(record):
                raise UnreportableOrder(
                    "IBKR shows order {} as {}, not as already reported to the engine ({}): a changed order is "
                    "not reported; check the order in IBKR (docs/manual/paper-trading.md)".format(
                        record.client_order_id, _figures(record), described))
            self._orders[key] = record

    def _on_status(self, report):
        key = (report.client_id, report.broker_order_id)
        if report.client_id != self.client_id or report.lifecycle is None:
            return
        if self._status.get(key) == report.lifecycle:
            return
        if key in self._held and self._held[key][1] == report.lifecycle:
            return
        ended = self._held[key][1] if key in self._held else self._status.get(key)
        if ended in TERMINAL:
            raise UnreportableOrder(
                "IBKR reported order {} of client {} as {} after reporting it {}: an order that ended does not "
                "change; check the order in IBKR and reconcile it (docs/manual/paper-trading.md)".format(
                    report.broker_order_id, report.client_id, report.lifecycle, ended))
        held = self._orders.get(key)
        request = self._requests.get(key)
        if held is not None:
            client_order_id, symbol, action = held.client_order_id, held.symbol, held.action
            quantity, limit = held.quantity, held.limit_price
        elif request is not None:
            client_order_id, symbol, action = request.client_order_id, request.symbol, request.action
            quantity, limit = request.quantity, request.limit_price
        else:
            raise UnreportableOrder("IBKR reported order {} of client {} as {} before showing it, and this "
                                    "adapter did not send it".format(report.broker_order_id, report.client_id,
                                                                     report.lifecycle))
        if limit is None or limit <= 0 or action not in SIDES:
            raise UnreportableOrder("order {} is not a buy or sell limit order".format(client_order_id))
        record = Record(LIFECYCLE_TYPE, LIFECYCLE_VERSION, {
            "account_id": self.account_id,
            "client_order_id": client_order_id,
            "order_id": "order:{}:{}".format(report.client_id, report.broker_order_id),
            "instrument_id": symbol,
            "side": SIDES[action],
            "status": report.lifecycle,
            "quantity": _whole(quantity, "order quantity"),
            "limit_price": float(limit),
            "occurred_at": _stamp(self._now(), "receipt time"),
            "message": report.reason or "",
        })
        self._described[key] = (client_order_id, symbol, SIDES[action], _whole(quantity, "order quantity"),
                                float(limit))
        if report.lifecycle in TERMINAL and self._waiting(key):
            awaits = frozenset(r.exec_id for r, _ in self._executions.values()
                               if (r.client_id, r.broker_order_id) == key)
            held = dict(self._held)
            held[key] = (record, report.lifecycle, awaits)
            try:
                self._save_held(held)
            except OSError as err:
                raise UnreportableOrder(
                    "IBKR reported order {} {} while its execution waited for a commission, and the report could "
                    "not be kept in {} ({}): it would be lost on a stop; check the order in IBKR "
                    "(docs/manual/paper-trading.md)".format(client_order_id, report.lifecycle, self._held_path,
                                                            err)) from None
            self._held = held
            return
        self._sink(record)
        self._status[key] = report.lifecycle

    def _waiting(self, key):
        """Whether an execution of the order key names still waits for its commission."""
        return any((r.client_id, r.broker_order_id) == key for r, _ in self._executions.values())

    def _on_execution(self, report):
        if report.account_id != self.account_id or not self._ours(report.client_id, report.client_order_id):
            return
        if report.side not in EXECUTION_SIDES:
            raise UnreportableOrder("execution {} has side {!r}".format(report.exec_id, report.side))
        _whole(report.shares, "execution {}".format(report.exec_id))
        base = _exec_base(report.exec_id)
        first = self._seen.get(base)
        if first == report.exec_id:
            # IBKR's replay of an execution already seen.
            return
        acknowledged = self._acknowledged.get(report.exec_id)
        if acknowledged is not None:
            if first != acknowledged:
                raise UnreportableOrder(
                    "IBKR reported execution {}, acknowledged as a correction of execution {}, but {}: check the "
                    "order's trades in IBKR and reconcile them (docs/manual/paper-trading.md)".format(
                        report.exec_id, acknowledged,
                        "execution {} was never recorded as a fill".format(acknowledged) if first is None
                        else "the execution first reported under its base is {}".format(first)))
            # The owner acknowledged this correction; the engine keeps the
            # original fill.
            return
        if first is not None:
            raise UnreportableOrder(
                "IBKR reported execution {} as a correction of execution {}; it is not recorded as another fill: "
                "check the order's trades in IBKR and reconcile it (docs/manual/paper-trading.md)".format(
                    report.exec_id, first))
        price = report.price
        if not isinstance(price, Decimal) or not price.is_finite() or price <= 0 or price >= UNSET_FIGURE:
            raise UnreportableOrder("execution {} has price {}; the engine reads a finite price above "
                                    "zero".format(report.exec_id, price))
        key = (report.client_id, report.broker_order_id)
        if self._status.get(key) in TERMINAL:
            raise UnreportableOrder(
                "IBKR reported execution {} of order {} after the order was recorded as {}: the engine reads no "
                "fill on an order already ended; check the order's trades in IBKR and reconcile them "
                "(docs/manual/paper-trading.md)".format(report.exec_id, report.client_order_id,
                                                         self._status[key]))
        if key in self._held and report.exec_id not in self._held[key][2]:
            # The held report now waits for this execution too, on disk
            # before the execution is accepted.
            record, status, awaits = self._held[key]
            held = dict(self._held)
            held[key] = (record, status, awaits | {report.exec_id})
            try:
                self._save_held(held)
            except OSError as err:
                raise UnreportableOrder(
                    "IBKR reported execution {} of order {}, whose {} report is held for its executions, and the "
                    "hold could not be rewritten in {} ({}): the report could be journalled before this fill; "
                    "check the order in IBKR (docs/manual/paper-trading.md)".format(
                        report.exec_id, report.client_order_id, status, self._held_path, err)) from None
            self._held = held
        self._executions[report.exec_id] = (report, _utc(self._now(), "receipt time"))
        self._seen[base] = report.exec_id

    def _notional(self, report):
        """The order's notional (quantity at its limit) if this adapter knows the order, else the execution's."""
        key = (report.client_id, report.broker_order_id)
        known = self._orders.get(key) or self._requests.get(key)
        if known is not None and known.limit_price is not None and known.limit_price > 0:
            return known.quantity * known.limit_price
        return report.shares * report.price

    def _on_commission(self, commission):
        pending = self._executions.get(commission.exec_id)
        if pending is None:
            return
        report = pending[0]
        amount = commission.commission
        if (commission.currency != BASE_CURRENCY or not isinstance(amount, Decimal) or not amount.is_finite()
                or amount < 0 or amount >= self._notional(report)):
            raise UnreportableOrder("execution {} has commission {} {}; the engine reads a commission of at "
                                    "least zero and below the order's value, in {}".format(
                                        commission.exec_id, amount, commission.currency, BASE_CURRENCY))
        self._sink(Record(FILL_TYPE, FILL_VERSION, {
            "account_id": self.account_id,
            "client_order_id": report.client_order_id,
            "fill_id": report.exec_id,
            "instrument_id": report.symbol,
            "side": EXECUTION_SIDES[report.side],
            "quantity": _whole(report.shares, "execution {}".format(report.exec_id)),
            "price": float(report.price),
            "commission": float(commission.commission),
            "filled_at": _stamp(report.occurred_at, "execution time"),
        }))
        self._fills.add(report.exec_id)
        del self._executions[commission.exec_id]


def _is_id(value):
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


def _is_text(value):
    return isinstance(value, str) and bool(value)


def _is_price(value):
    return (isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)
            and value > 0)


def _is_stamp(value):
    """Whether value is a whole-second UTC time as _stamp writes one."""
    if not isinstance(value, str) or not STAMP.fullmatch(value):
        return False
    try:
        datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        return False
    return True


def _figures(record):
    """An OpenOrderRecord's (client order ID, instrument, side, quantity, limit), as a lifecycle report states them."""
    quantity, limit = record.quantity, record.limit_price
    whole = int(quantity) if quantity == quantity.to_integral_value() else quantity
    return (record.client_order_id, record.symbol, SIDES.get(record.action), whole,
            float(limit) if limit is not None else None)
