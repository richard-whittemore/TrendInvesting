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
identity, fractional shares, a commission in another currency) raises
UnreportableOrder: it is never journalled approximately (fail closed).
"""
from datetime import timezone

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


class UnreportableOrder(Exception):
    """IBKR reported on this engine's order in a form the engine cannot read; the run must stop."""


def _stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


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
    when its commission arrives, which IBKR sends after the execution.
    now returns the adapter's UTC receipt time.
    """

    def __init__(self, account_id, client_id, sink, now):
        self.account_id = account_id
        self.client_id = client_id
        self._sink = sink
        self._now = now
        self._requests = {}
        self._orders = {}
        self._status = {}
        self._executions = {}

    def sent(self, record):
        """Remember the request an order was sent as, for a refusal IBKR reports before any openOrder."""
        self._requests[(record.client_id, record.broker_order_id)] = record.request

    def on_record(self, record):
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
            "occurred_at": _stamp(self._now()),
            "message": report.reason or "",
        }))
        self._status[key] = report.lifecycle

    def _on_execution(self, report):
        if report.account_id != self.account_id or not self._ours(report.client_id, report.client_order_id):
            return
        if report.side not in EXECUTION_SIDES:
            raise UnreportableOrder("execution {} has side {!r}".format(report.exec_id, report.side))
        _whole(report.shares, "execution {}".format(report.exec_id))
        self._executions[report.exec_id] = report

    def _on_commission(self, commission):
        report = self._executions.get(commission.exec_id)
        if report is None:
            return
        if commission.currency != BASE_CURRENCY or commission.commission < 0:
            raise UnreportableOrder("execution {} has commission {} {}; the engine reads a commission of at "
                                    "least zero in {}".format(commission.exec_id, commission.commission,
                                                              commission.currency, BASE_CURRENCY))
        self._sink(Record(FILL_TYPE, FILL_VERSION, {
            "account_id": self.account_id,
            "client_order_id": report.client_order_id,
            "fill_id": report.exec_id,
            "instrument_id": report.symbol,
            "side": EXECUTION_SIDES[report.side],
            "quantity": _whole(report.shares, "execution {}".format(report.exec_id)),
            "price": float(report.price),
            "commission": float(commission.commission),
            "filled_at": _stamp(report.occurred_at),
        }))
        del self._executions[commission.exec_id]
