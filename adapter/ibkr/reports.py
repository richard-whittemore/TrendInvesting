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
and the run stops for a person to reconcile the order (ADR 0019).
"""
import math
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
    raises UnreportableOrder, from check_overdue (which the adapter's loop
    calls after each pump) or the next record, so a fill IBKR made is never
    silently left unrecorded. now returns the adapter's receipt time, with
    its zone.

    journalled holds the records already written for the engine (the
    durable journal, read back on a restart), so IBKR's replay of each
    working order's status and each execution with its commission, which it
    sends on every read after a reconnect, writes nothing twice.
    """

    def __init__(self, account_id, client_id, sink, now, commission_wait, journalled=()):
        if (isinstance(commission_wait, bool) or not isinstance(commission_wait, (int, float))
                or not math.isfinite(commission_wait) or commission_wait <= 0):
            raise ValueError("commission_wait must be a positive number of seconds, not {!r}".format(
                commission_wait))
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
        for record in journalled:
            self._seed(record)

    def _seed(self, record):
        payload = record.payload
        if payload.get("account_id") != self.account_id:
            return
        if record.event_type == LIFECYCLE_TYPE:
            key = _order_key(payload.get("order_id"))
            if key is not None:
                self._status[key] = payload.get("status")
        elif record.event_type == FILL_TYPE:
            self._seen[_exec_base(payload["fill_id"])] = payload["fill_id"]

    def check_overdue(self):
        """Raise UnreportableOrder for an execution whose commission is commission_wait seconds late or more."""
        if not self._executions:
            return
        now = _utc(self._now(), "receipt time")
        for exec_id, (_, arrived) in self._executions.items():
            if (now - arrived).total_seconds() >= self._commission_wait:
                raise UnreportableOrder(
                    "execution {} has waited {} s or more for IBKR's commission report, without which its fill "
                    "cannot be recorded; check the trade in IBKR (docs/manual/paper-trading.md)".format(
                        exec_id, self._commission_wait))

    def sent(self, record):
        """Remember the request an order was sent as, for a refusal IBKR reports before any openOrder."""
        self._requests[(record.client_id, record.broker_order_id)] = record.request

    def on_record(self, record):
        self.check_overdue()
        if isinstance(record, OpenOrderRecord):
            self._on_open_order(record)
        elif isinstance(record, OrderStatusReport):
            self._on_status(record)
        elif isinstance(record, ExecutionReport):
            self._on_execution(record)
        elif isinstance(record, CommissionReport):
            self._on_commission(record)

    def _ours(self, client_id, client_order_id):
        return client_id == self.client_id and bool(client_order_id)

    def _on_open_order(self, record):
        if record.account_id == self.account_id and self._ours(record.client_id, record.client_order_id):
            self._orders[(record.client_id, record.broker_order_id)] = record

    def _on_status(self, report):
        key = (report.client_id, report.broker_order_id)
        if report.client_id != self.client_id or report.lifecycle is None:
            return
        if self._status.get(key) == report.lifecycle:
            return
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
        self._sink(Record(LIFECYCLE_TYPE, LIFECYCLE_VERSION, {
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
        }))
        self._status[key] = report.lifecycle

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
        if first is not None:
            raise UnreportableOrder(
                "IBKR reported execution {} as a correction of execution {}; it is not recorded as another fill: "
                "check the order's trades in IBKR and reconcile it (docs/manual/paper-trading.md)".format(
                    report.exec_id, first))
        price = report.price
        if not isinstance(price, Decimal) or not price.is_finite() or price <= 0 or price >= UNSET_FIGURE:
            raise UnreportableOrder("execution {} has price {}; the engine reads a finite price above "
                                    "zero".format(report.exec_id, price))
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
        del self._executions[commission.exec_id]
