"""IBKR order status, open-order and execution callbacks as plain records.

ADR 0022: an order lifecycle report is recorded evidence with a closed status
set (submitted, updated, cancel-pending, canceled, invalid), and a status
outside it fails closed rather than being mapped onto the nearest one.
plan.md section 5 ("No order sent twice") and ADR 0025 point 5: IBKR's own
order ID comes only from IBKR's reports, the decision's tag (IBKR's orderRef,
the client order ID here) is kept separate from it, and on reconnect IBKR's
open orders and executions are matched to the journal by that tag. Matching
never resends.

IBKR's orderId is unique only within one API client ID, and reqAllOpenOrders
reports other clients' and manual orders too, so an order is identified by
(clientId, orderId), or by permId when it has no positive orderId (a manual
TWS order); see order_key.

The `updated` status is not produced here: an amendment's acknowledgement
arrives as a changed openOrder, which needs the order this adapter sent to
compare against, and there is no sending path yet.
"""
from collections import deque, namedtuple
from datetime import datetime, timezone

from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from values import to_decimal

# IBKR's order statuses ("Order Status" in IBKR's TWS API documentation) and
# the ADR 0022 status each one is. None means IBKR has not yet acknowledged
# the order (PendingSubmit, ApiPending) or that it is finished by execution
# (Filled), which execution.fill reports, not a lifecycle record. Inactive is
# absent on purpose: IBKR uses it both for a rejection and for an order it
# holds back, so it is `invalid` only when IBKR's own rejection message for
# that order has arrived.
STATUS_LIFECYCLE = {
    "PendingSubmit": None,
    "ApiPending": None,
    "PreSubmitted": "submitted",
    "Submitted": "submitted",
    "PendingCancel": "cancel-pending",
    "Cancelled": "canceled",
    "ApiCancelled": "canceled",
    "Filled": None,
}
INACTIVE = "Inactive"
# IBKR message code 201: "Order rejected - reason: ...".
REJECTION_CODES = frozenset({201})
# IBKR message codes that refuse a newly placed order before it works, but
# that IBKR also uses for other requests, whose request IDs may equal an
# order ID ("Message Codes" in IBKR's TWS API documentation): 103 duplicate
# order ID, 110 price not a multiple of the minimum tick, 200 no security
# definition, 203 security not allowed for this account, 321 error
# validating the request (the Read-Only API refuses every order this way).
# Each is a rejection only for an order this client placed in this session
# (OrderTracker.placed) that IBKR has not yet shown working.
PLACEMENT_REJECTION_CODES = frozenset({103, 110, 200, 203, 321})
# ibapi marks an unset price with sys.float_info.max (UNSET_DOUBLE).
_UNSET_PRICE = 1.7e308


class UnrecognisedOrderReport(Exception):
    """IBKR reported an order state this adapter cannot classify; the run must stop (ADR 0022)."""


class BrokerOrdersIncomplete(Exception):
    """IBKR's open orders or executions did not arrive in full; missing names which."""

    def __init__(self, missing):
        super().__init__("broker orders incomplete: missing {}".format(", ".join(missing)))
        self.missing = tuple(missing)


OpenOrderRecord = namedtuple("OpenOrderRecord", (
    "account_id client_id broker_order_id perm_id client_order_id con_id symbol action quantity "
    "order_type limit_price aux_price tif ibkr_status"))


class OrderStatusReport(namedtuple("OrderStatusReport", (
        "client_id broker_order_id perm_id client_order_id ibkr_status lifecycle filled remaining "
        "avg_fill_price last_fill_price ibkr_code reason"))):
    """One order status change; lifecycle is an ADR 0022 status or None (see STATUS_LIFECYCLE)."""

    __slots__ = ()

    @property
    def partially_filled(self):
        return (self.filled is not None and self.remaining is not None
                and self.filled > 0 and self.remaining > 0)


ExecutionReport = namedtuple("ExecutionReport", (
    "account_id exec_id client_id broker_order_id perm_id client_order_id con_id symbol side "
    "shares price "
    "cum_qty avg_price time occurred_at"))
"""One execution; time is IBKR's string verbatim and occurred_at its UTC reading."""

CommissionReport = namedtuple("CommissionReport", "exec_id commission currency")

BrokerOrders = namedtuple("BrokerOrders", "open_orders executions")

ReconnectMatch = namedtuple("ReconnectMatch", (
    "matched executed missing_at_broker unknown_at_broker unknown_executions clean"))
"""IBKR's view of the orders the journal expects, matched by client order ID and client ID.

matched: tag to the OpenOrderRecord still open at IBKR, which carries the
order's full identity (client ID, IBKR order ID, permId). executed: tag
to its executions this session. missing_at_broker: tags neither open nor
executed. unknown_at_broker / unknown_executions: IBKR's orders and
executions with no expected tag (a manual order, or a crash after IBKR
accepted an order the journal never recorded), including a second order,
open or executed, carrying an already-matched tag. Several executions of one
order (partial fills) all go to executed. clean is true only when the last three are
empty; anything else is for reconciliation to classify (ADR 0019), never a
reason to resend.
"""


def order_key(client_id, order_id, perm_id):
    """One order's identity across every client of the login.

    IBKR assigns orderId per client ID, so (client_id, order_id) is unique for
    an API order; a manual TWS order has no positive orderId, and its permId
    (account-wide) identifies it. An order with neither cannot be told apart
    from another and fails closed.
    """
    if order_id is not None and order_id > 0:
        return ("order", client_id, order_id)
    if perm_id:
        return ("perm", 0, perm_id)
    raise UnrecognisedOrderReport(
        "order with client_id {} has neither an orderId nor a permId".format(client_id))


def parse_execution_time(text):
    """Read IBKR's execution time ("YYYYMMDD HH:MM:SS Zone") as a UTC datetime.

    A time without a zone is refused: it is in whatever zone the Gateway's
    login is set to, which the record alone cannot say.
    """
    parts = text.split()
    if len(parts) != 3:
        raise ValueError("execution time {!r} is not 'YYYYMMDD HH:MM:SS Zone'".format(text))
    try:
        zone = ZoneInfo(parts[2])
    except (ZoneInfoNotFoundError, ValueError):
        raise ValueError("execution time {!r} names an unknown zone".format(text)) from None
    local = datetime.strptime(parts[0] + " " + parts[1], "%Y%m%d %H:%M:%S")
    return local.replace(tzinfo=zone).astimezone(timezone.utc)


def _price(value, what):
    if value is None or (isinstance(value, float) and value >= _UNSET_PRICE):
        return None
    return to_decimal(value, what)


def open_order_record(order_id, contract, order, order_state):
    return OpenOrderRecord(
        account_id=order.account, client_id=order.clientId, broker_order_id=order_id,
        perm_id=order.permId,
        client_order_id=order.orderRef or None, con_id=contract.conId, symbol=contract.symbol,
        action=order.action, quantity=to_decimal(order.totalQuantity, "totalQuantity"),
        order_type=order.orderType, limit_price=_price(order.lmtPrice, "lmtPrice"),
        aux_price=_price(order.auxPrice, "auxPrice"), tif=order.tif,
        ibkr_status=getattr(order_state, "status", None))


def execution_report(contract, execution):
    return ExecutionReport(
        account_id=execution.acctNumber, exec_id=execution.execId, client_id=execution.clientId,
        broker_order_id=execution.orderId, perm_id=execution.permId,
        client_order_id=execution.orderRef or None, con_id=contract.conId,
        symbol=contract.symbol, side=execution.side,
        shares=to_decimal(execution.shares, "shares"), price=to_decimal(execution.price, "price"),
        cum_qty=to_decimal(execution.cumQty, "cumQty"),
        avg_price=to_decimal(execution.avgPrice, "avgPrice"),
        time=execution.time, occurred_at=parse_execution_time(execution.time))


class OrderTracker:
    """Turns one account's order callbacks into records passed to sink, in arrival order.

    Subscribe on_event to a Connection. Orders and executions of other
    accounts under the same login are skipped. orderStatus carries no
    account, so a status is held, unclassified, until the order's owner is
    known: an openOrder in this account (or IBKR's rejection, which reaches
    only the client that placed the order) releases it, an openOrder in
    another account discards it. An identical repeat of the
    last status IBKR sent for an order is collapsed, since IBKR repeats them.
    Per-order state is keyed by order_key; client_id is this engine's own,
    which an order rejection (error with reqId = orderId) implicitly carries.
    A code in PLACEMENT_REJECTION_CODES is that order's rejection only once
    placed(order_id) has named the order and before IBKR shows it working
    (an openOrder or a status).

    Invariant: a report counts as seen only once sink has accepted it, so a
    sink that raises (a failed journal write) leaves IBKR's redelivery of the
    same report free to be recorded.
    """

    def __init__(self, account_id, client_id, sink):
        self.account_id = account_id
        self.client_id = client_id
        self._sink = sink
        self._client_ids = {}
        self._foreign = set()
        self._held = {}
        self._last = {}
        self._rejections = {}
        self._executions = set()
        self._commissions = set()
        self._placed = set()

    def placed(self, order_id):
        """Record that this client placed order_id in this session, so IBKR's refusal of it is read as one."""
        self._placed.add(order_key(self.client_id, order_id, None))

    def _shown(self, key):
        """Whether IBKR has shown the order working: an openOrder, or a status recorded or held."""
        return key in self._client_ids or key in self._last or key in self._held

    def on_event(self, event):
        handler = getattr(self, "_on_" + event.name, None)
        if handler is not None:
            handler(*event.args)

    def _on_open_order(self, order_id, contract, order, order_state):
        if order.account != self.account_id:
            if order_id > 0 or order.permId:
                key = order_key(order.clientId, order_id, order.permId)
                self._foreign.add(key)
                self._held.pop(key, None)
            return
        key = order_key(order.clientId, order_id, order.permId)
        record = open_order_record(order_id, contract, order, order_state)
        self._sink(record)
        self._client_ids[key] = record.client_order_id
        self._release(key)

    def _on_order_status(self, *args):
        order_id, perm_id, client_id = args[0], args[5], args[8]
        key = order_key(client_id, order_id, perm_id)
        if key in self._foreign:
            return
        owner_known = key in self._client_ids or key in self._rejections
        if key in self._held or not owner_known:
            # Statuses still held (one whose record failed, for instance) are
            # older than this one, so it queues behind them: a held Submitted
            # must never be recorded after a later Cancelled.
            self._held.setdefault(key, deque()).append(args)
            if owner_known:
                self._release(key)
            return
        self._status(key, *args)

    def _release(self, key):
        """Record the statuses held for an order now known to be this account's, in arrival order.

        Each leaves the hold only once recorded, so a sink failure keeps the
        rest for the next release.
        """
        held = self._held.get(key)
        while held:
            self._status(key, *held[0])
            held.popleft()
        self._held.pop(key, None)

    def _status(self, key, order_id, status, filled, remaining, avg_fill_price, perm_id,
                parent_id, last_fill_price, client_id, why_held, mkt_cap_price):
        code = reason = None
        if status in STATUS_LIFECYCLE:
            lifecycle = STATUS_LIFECYCLE[status]
        elif status == INACTIVE and key in self._rejections:
            lifecycle = "invalid"
            code, reason = self._rejections[key]
        else:
            raise UnrecognisedOrderReport(
                "order {} of client {} reported status {!r}".format(order_id, client_id, status))
        report = OrderStatusReport(
            client_id=client_id, broker_order_id=order_id, perm_id=perm_id,
            client_order_id=self._client_ids.get(key), ibkr_status=status,
            lifecycle=lifecycle, filled=to_decimal(filled, "filled"),
            remaining=to_decimal(remaining, "remaining"),
            avg_fill_price=to_decimal(avg_fill_price, "avgFillPrice"),
            last_fill_price=to_decimal(last_fill_price, "lastFillPrice"),
            ibkr_code=code, reason=reason)
        self._emit(key, report)

    def _on_error(self, req_id, code, message, *_):
        if req_id <= 0 or (code not in REJECTION_CODES and code not in PLACEMENT_REJECTION_CODES):
            return
        key = order_key(self.client_id, req_id, None)
        if key in self._foreign:
            return
        if code in PLACEMENT_REJECTION_CODES and (key not in self._placed or self._shown(key)):
            return
        # Statuses held before the rejection arrived precede it.
        self._release(key)
        # IBKR's rejection is remembered even if sink fails below: it is what
        # classifies the Inactive status that follows it.
        self._rejections[key] = (code, message)
        previous = self._last.get(key)
        self._emit(key, OrderStatusReport(
            client_id=self.client_id, broker_order_id=req_id,
            perm_id=previous.perm_id if previous else None,
            client_order_id=self._client_ids.get(key), ibkr_status=None, lifecycle="invalid",
            filled=previous.filled if previous else None,
            remaining=previous.remaining if previous else None,
            avg_fill_price=None, last_fill_price=None, ibkr_code=code, reason=message))

    def _emit(self, key, report):
        previous = self._last.get(key)
        if previous is not None:
            if report.lifecycle == "invalid" and previous.lifecycle == "invalid":
                return
            if report == previous:
                return
        self._sink(report)
        self._last[key] = report

    def _on_exec_details(self, req_id, contract, execution):
        if execution.acctNumber != self.account_id or execution.execId in self._executions:
            return
        report = execution_report(contract, execution)
        self._sink(report)
        self._executions.add(execution.execId)

    def _on_commission_report(self, report):
        # A commission report carries no account; it belongs to this account
        # only if its execution did. IBKR resends it whenever executions are
        # requested again (reconnect matching), so it is recorded once.
        if report.execId not in self._executions or report.execId in self._commissions:
            return
        self._sink(CommissionReport(report.execId, to_decimal(report.commission, "commission"),
                                    report.currency))
        self._commissions.add(report.execId)


def collect_broker_orders(conn):
    """Read every open order and this session's executions for the configured account.

    Uses reqAllOpenOrders, not reqOpenOrders, so orders placed by hand or by
    another client are seen too (ADR 0019 compares both sets). Executions
    reach back only to midnight through Gateway (ADR 0025 point 5).
    """
    conn.require_connected()
    account = conn.config.account_id
    exec_req = conn.new_req_id()
    conn.api.req_all_open_orders()
    conn.api.req_executions(exec_req, account)
    open_orders = {}
    executions = {}
    orders_done = execs_done = False
    for event in conn.events(conn.config.request_timeout):
        if not conn.connected:
            break
        name, args = event.name, event.args
        if name == "open_order":
            record = open_order_record(*args)
            if record.account_id == account:
                key = order_key(record.client_id, record.broker_order_id, record.perm_id)
                open_orders[key] = record
        elif name == "open_order_end":
            orders_done = True
        elif name == "exec_details" and args[0] == exec_req:
            report = execution_report(args[1], args[2])
            if report.account_id == account:
                executions[report.exec_id] = report
        elif name == "exec_details_end" and args[0] == exec_req:
            execs_done = True
        if orders_done and execs_done:
            break
    missing = []
    if not conn.connected:
        missing.append("connection")
    if not orders_done:
        missing.append("open-orders")
    if not execs_done:
        missing.append("executions")
    if missing:
        raise BrokerOrdersIncomplete(missing)
    return BrokerOrders(
        open_orders=tuple(open_orders[k] for k in sorted(open_orders)),
        executions=tuple(executions[k] for k in sorted(executions)))


def match_on_reconnect(expected_client_order_ids, broker, client_id):
    """Match IBKR's orders and executions to the journal's expected tags; see ReconnectMatch.

    Only an order or execution placed under this engine's own client_id can
    be the journal's: collect_broker_orders also returns other clients'
    orders in the account, and one carrying an expected tag is unknown, not
    a match, so it can never satisfy reconciliation.
    """
    expected = set(expected_client_order_ids)
    # Each expected tag belongs to one IBKR order: the first open order (or,
    # if none is open, the first execution) carrying it. Further executions
    # of that order are its partial fills; another order with the same tag is
    # unknown.
    owner = {}
    matched = {}
    unknown = []
    for record in broker.open_orders:
        cid = record.client_order_id
        if record.client_id == client_id and cid in expected and cid not in matched:
            matched[cid] = record
            owner[cid] = order_key(record.client_id, record.broker_order_id, record.perm_id)
        else:
            unknown.append(record)
    executed = {}
    unknown_execs = []
    for report in broker.executions:
        cid = report.client_order_id
        if report.client_id == client_id and cid in expected:
            key = order_key(report.client_id, report.broker_order_id, report.perm_id)
            if owner.setdefault(cid, key) == key:
                executed.setdefault(cid, []).append(report)
                continue
        unknown_execs.append(report)
    executed = {k: tuple(v) for k, v in executed.items()}
    missing = tuple(sorted(expected - set(matched) - set(executed)))
    return ReconnectMatch(
        matched=matched, executed=executed, missing_at_broker=missing,
        unknown_at_broker=tuple(unknown), unknown_executions=tuple(unknown_execs),
        clean=not missing and not unknown and not unknown_execs)
