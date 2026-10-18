"""The order-sending seam: dry-run never sends, paper sends to a paper account only, live is disabled.

AGENTS.md rule 5: no live-order behaviour before the paper-trading and
limited-live gates. plan.md section 5 ("Modes") and ADR 0026 point 3:
dry-run decides and journals with send false, and never sends. Paper sends
each released order to IBKR's paper account; sending is bound to the
configuration's mode and account, as the engine binds them, and refuses
anything else (fail closed): a paper sender exists only on a connection
whose configuration is mode paper on an account whose ID begins "DU", and
it sends only a request for that account. Live refuses to start until the
limited-live gate is passed and this module is changed under review.

The re-check of ADR 0026 point 3's limits before sending is not here yet: it
needs the hashed configuration, which the live engine configuration that
names adapter ibkr will carry (ADR 0025 point 3).
"""
from collections import namedtuple
from decimal import Decimal
from types import SimpleNamespace

from config import is_paper_account
from values import to_decimal

ACTIONS = ("BUY", "SELL")
PRICED_ORDER_TYPES = ("LMT",)
UNPRICED_ORDER_TYPES = ("MKT",)
# A released order is a whole-share limit order for the session (ADR 0026
# point 2, "Order type and price").
RELEASE_ORDER_TYPE = "LMT"
RELEASE_TIF = "DAY"
RELEASE_SIDES = {"buy": "BUY", "sell": "SELL"}
# The one kind of contract the engine trades: a US-dollar stock or ETF
# routed by IBKR's SMART router, identified by IBKR's contract ID.
SEC_TYPE = "STK"
CURRENCY = "USD"
EXCHANGE = "SMART"

OrderRequest = namedtuple("OrderRequest", (
    "client_order_id account_id con_id symbol action quantity order_type limit_price tif"))
"""An order the adapter was asked to place.

client_order_id is the decision's tag, sent to IBKR as orderRef; IBKR's own
order ID is never part of a request (plan.md section 5).
"""

DryRunRecord = namedtuple("DryRunRecord", "request send")

SentRecord = namedtuple("SentRecord", "request send client_id broker_order_id")
"""An order handed to IB Gateway: the request, send=True, and the (client ID, IBKR order ID) it went under.

The IBKR order ID is the one this adapter placed the order under, from
IBKR's nextValidId; it identifies the order in IBKR's reports (orders.order_key).
"""


class LiveSendingDisabled(Exception):
    """Sending to a live account is disabled until the limited-live gate (AGENTS.md rule 5)."""


class SendRefused(LiveSendingDisabled):
    """Paper sending was asked for where it is not bound to a paper configuration and account; nothing is sent."""


class NotReconciled(Exception):
    """The link has not been reconciled since it connected or was restored; no order is sent (ADR 0025 point 5)."""


def validate(request):
    """Refuse a malformed request before anything records or sends it."""
    if not isinstance(request.client_order_id, str) or not request.client_order_id:
        raise ValueError("client_order_id is required")
    if not isinstance(request.account_id, str) or not request.account_id:
        raise ValueError("account_id is required")
    if request.action not in ACTIONS:
        raise ValueError("action must be BUY or SELL")
    if not isinstance(request.quantity, Decimal) or not request.quantity.is_finite() \
            or request.quantity <= 0:
        raise ValueError("quantity must be a positive Decimal")
    if request.order_type in PRICED_ORDER_TYPES:
        if not isinstance(request.limit_price, Decimal) or not request.limit_price.is_finite() \
                or request.limit_price <= 0:
            raise ValueError("{} needs a positive Decimal limit_price".format(request.order_type))
    elif request.order_type in UNPRICED_ORDER_TYPES:
        if request.limit_price is not None:
            raise ValueError("{} takes no limit_price".format(request.order_type))
    else:
        raise ValueError("unsupported order_type {!r}".format(request.order_type))


class DryRunSender:
    """Validates and journals each request with send=False; holds no IBKR client at all."""

    def __init__(self, journal):
        self._journal = journal

    def send(self, request):
        validate(request)
        record = DryRunRecord(request=request, send=False)
        self._journal(record)
        return record


def _require_paper(config):
    if config.mode != "paper":
        raise SendRefused("the connection's configuration is mode {!r}; only paper sends".format(config.mode))
    if not is_paper_account(config.account_id):
        raise SendRefused("account {!r} is not an IBKR paper account (its ID would begin \"DU\"); nothing is "
                          "sent".format(config.account_id))


class PaperSender:
    """Sends each request to IBKR's paper account on conn and journals what it handed over.

    Bound to conn's configuration: it refuses to exist unless that is mode
    paper on a paper account, re-checks the binding before every order, and
    sends only a request for that account. An order goes out only while the
    link is up and reconciled (ADR 0025 point 5), as a whole-share limit
    order (ADR 0026 point 2), under the next IBKR order ID, which is used
    once whatever place_order does: a call that raised may still have
    reached IBKR. journal receives the SentRecord only after place_order
    returns; the write-ahead record of the hand-over is the relay's
    (relay.py), which never hands one client order ID over twice.
    """

    def __init__(self, conn, journal):
        _require_paper(conn.config)
        self._conn = conn
        self._journal = journal

    def check(self, request):
        """Return request if send would take it now, else raise as send would; sends nothing.

        The relay builds each request through this before it marks the order
        about to be sent, so an order refused here never left the process and
        leaves no mark to stop a restart (relay.py). Nothing between this
        check and the send pumps the connection, so the link's state cannot
        change in between.
        """
        config = self._conn.config
        _require_paper(config)
        validate(request)
        if request.account_id != config.account_id:
            raise ValueError("request for account {!r} on the connection to paper account {!r}".format(
                request.account_id, config.account_id))
        if request.order_type != RELEASE_ORDER_TYPE:
            raise ValueError("a released order is a limit order, not {!r}".format(request.order_type))
        if request.quantity != request.quantity.to_integral_value():
            raise ValueError("quantity {} is not whole shares".format(request.quantity))
        if not isinstance(request.con_id, int) or isinstance(request.con_id, bool) or request.con_id <= 0:
            raise ValueError("con_id must be IBKR's positive contract ID")
        self._conn.require_connected()
        if self._conn.needs_reconciliation:
            raise NotReconciled("the link to IB Gateway has not been reconciled since it connected; nothing is "
                                "sent until IBKR's orders, executions and balances are matched to the journal")
        if self._conn.next_order_id is None:
            raise SendRefused("IBKR has not given this session an order ID (nextValidId); nothing is sent")
        return request

    def send(self, request):
        self.check(request)
        config = self._conn.config
        order_id = self._conn.next_order_id
        self._conn.next_order_id = order_id + 1
        contract = SimpleNamespace(conId=request.con_id, symbol=request.symbol, secType=SEC_TYPE,
                                   currency=CURRENCY, exchange=EXCHANGE)
        order = SimpleNamespace(orderRef=request.client_order_id, account=request.account_id,
                                action=request.action, totalQuantity=request.quantity,
                                orderType=request.order_type, lmtPrice=float(request.limit_price),
                                tif=request.tif, transmit=True)
        self._conn.api.place_order(order_id, contract, order)
        record = SentRecord(request=request, send=True, client_id=config.client_id, broker_order_id=order_id)
        self._journal(record)
        return record


def release_to_request(order, mode, con_ids):
    """The OrderRequest for a released order's payload, or ValueError if this adapter must not send it.

    Only a release marked send whose mode is the adapter's own is sent
    (ADR 0026 point 3, "Mode"). con_ids maps the engine's instrument ID to
    IBKR's contract ID; an instrument with none is refused, never looked up
    by name.
    """
    if order.get("send") is not True:
        raise ValueError("order {} is not marked to be sent".format(order.get("client_order_id")))
    if order.get("mode") != mode:
        raise ValueError("order {} was released in mode {!r}; this adapter sends in {!r}".format(
            order.get("client_order_id"), order.get("mode"), mode))
    action = RELEASE_SIDES.get(order.get("side"))
    if action is None:
        raise ValueError("side {!r} is not buy or sell".format(order.get("side")))
    symbol = order.get("instrument_id")
    if symbol not in con_ids:
        raise ValueError("no IBKR contract ID is configured for {!r}".format(symbol))
    quantity = order.get("quantity")
    if not isinstance(quantity, int) or isinstance(quantity, bool) or quantity <= 0:
        raise ValueError("quantity {!r} is not a positive whole number of shares".format(quantity))
    limit = to_decimal(order.get("limit_price"), "limit_price")
    if limit <= 0:
        raise ValueError("limit_price must be positive")
    return OrderRequest(client_order_id=order["client_order_id"], account_id=order["account_id"],
                        con_id=con_ids[symbol], symbol=symbol, action=action, quantity=Decimal(quantity),
                        order_type=RELEASE_ORDER_TYPE, limit_price=limit, tif=RELEASE_TIF)


def make_sender(config, journal, conn=None):
    """Return the sender for a configuration: dry-run journals only, paper sends on conn, live is refused."""
    if config.mode == "dry-run":
        return DryRunSender(journal)
    if config.mode == "paper":
        if conn is None or conn.config != config:
            raise SendRefused("paper sends only on the connection built from this configuration")
        return PaperSender(conn, journal)
    if config.mode == "live":
        raise LiveSendingDisabled("mode 'live' is disabled until the limited-live gate (AGENTS.md rule 5)")
    raise ValueError("unknown mode {!r}".format(config.mode))
