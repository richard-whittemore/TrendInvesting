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
from datetime import date, datetime
from decimal import Decimal
from types import SimpleNamespace
from zoneinfo import ZoneInfo

from config import is_paper_account
from quantities import quantity_scale, shares, units
from connection import NotConnected
from spelling import ibkr_symbol
from values import to_decimal

ACTIONS = ("BUY", "SELL")
PRICED_ORDER_TYPES = ("LMT",)
UNPRICED_ORDER_TYPES = ("MKT",)
# A released order is a limit order for the session (ADR 0026
# point 2, "Order type and price").
RELEASE_ORDER_TYPE = "LMT"
RELEASE_TIF = "DAY"
RELEASE_SIDES = {"buy": "BUY", "sell": "SELL"}
# The one kind of contract the engine trades: a US-dollar stock or ETF
# routed by IBKR's SMART router, identified by IBKR's contract ID.
SEC_TYPE = "STK"
CURRENCY = "USD"
EXCHANGE = "SMART"
# A session's date is its New York date, the calendar relay.py dates
# sessions by.
SESSION_ZONE = ZoneInfo("America/New_York")

OrderRequest = namedtuple("OrderRequest", (
    "client_order_id account_id con_id symbol action quantity order_type limit_price tif session"),
    defaults=(None,))
"""An order the adapter was asked to place.

client_order_id is the decision's tag, sent to IBKR as orderRef; IBKR's own
order ID is never part of a request (plan.md section 5). session is the New
York date of the session the order was released in (its input's event
time); a released order is a limit order for that session only (ADR 0026
point 2), so it is never sent in another.
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
    """A paper send was refused; nothing is sent.

    Either the connection is not bound to a paper configuration and account,
    or the order cannot go out now: it was released for another session, the
    login does not manage the account, IBKR has given no order ID, or the
    client cannot place orders. A subclass of LiveSendingDisabled, so a
    caller that catches that catches these refusals too.
    """


class SessionOver(SendRefused):
    """The order was released for a session that is over; nothing is sent.

    A released order is a DAY limit order for its own session only (ADR
    0026 point 2), so one asked for again on a later day is never sent. The
    owner's decision of 2026-10-22: the relay retires such an order itself,
    as a refusal for "session over", never a cancel of an order at IBKR, and
    goes on to its next input (relay.Relay).
    """


class UnsendableOrder(ValueError):
    """The order can never be sent as it stands: not a limit order, a fraction of a share, or no IBKR contract ID.

    The owner's decision of 2026-10-22: the adapter stops at such an order
    for a person, who finds the cause and then retires it with the relay's
    retire command (relay.retire). A subclass of ValueError, the sender's
    refusal of a request out of shape.
    """


def _session_today():
    return datetime.now(SESSION_ZONE).date()


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

    def check(self, request):
        """Return request if it is well formed, else raise as send would; journals nothing."""
        validate(request)
        return request

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
    link is up and reconciled (ADR 0025 point 5), for an account the
    session's login manages (IBKR's managedAccounts), through a client that
    can place orders, as a whole-share DAY limit order released in today's
    session (ADR 0026 point 2), under the next IBKR order ID, which is used
    once whatever place_order does: a call that raised may still have
    reached IBKR. journal receives the SentRecord only after place_order
    returns; the write-ahead record of the hand-over is the relay's
    (relay.py), which never hands one client order ID over twice. today
    returns the current session's New York date (by default, today's).
    """

    def __init__(self, conn, journal, today=None):
        _require_paper(conn.config)
        self._conn = conn
        self._journal = journal
        self._today = today or _session_today

    def check(self, request):
        """Return request if send would take it now, else raise as send would; sends nothing.

        The relay calls this itself before it marks the order about to be
        sent, whatever its mapper does, so an order refused here never left
        the process and leaves no mark to stop a restart (relay.py). It is
        the one link check before the hand-over. Nothing between this check
        and the send pumps the connection, so the state the main thread
        keeps (the link's flag, its reconciliation, the next order ID)
        cannot change in between; ibapi's socket can, from its reader
        thread, so send does not test the link again: the client's own
        socket test inside place_order is the one after the mark, and
        reports a socket closed in between as a hand-over not confirmed
        (real_client.HandoverNotConfirmed).
        """
        config = self._check_shape(request)
        # A late order needs a person: its limit and the engine's checks
        # behind it were made for its own session.
        today = self._today()
        if _is_date(request.session) and request.session < today:
            raise SessionOver("order {} was released for the session of {}, not today's ({}); its session is "
                              "over, so it is never sent (docs/manual/paper-trading.md)".format(
                                  request.client_order_id, request.session, today))
        if not _is_date(request.session) or request.session != today:
            raise SendRefused("order {} was released for the session of {}, not today's ({}); it is never sent, "
                              "and the adapter stops here until a person settles it with relay.py resolve "
                              "--never-sent (docs/manual/paper-trading.md)".format(
                                  request.client_order_id, request.session, today))
        # The link first: a link down or not yet reconciled holds the input
        # until it is back (NotConnected, NotReconciled), where the switch's
        # refusal below stops the adapter for a person.
        self._conn.require_connected()
        if not self._conn.api.is_connected():
            raise NotConnected("IB Gateway's socket is closed; nothing is sent")
        if self._conn.needs_reconciliation:
            raise NotReconciled("the link to IB Gateway has not been reconciled since it connected; nothing is "
                                "sent until IBKR's open orders and executions are matched to the journal and its "
                                "account read in full")
        if not self._conn.api.can_place_orders():
            raise SendRefused("the IBKR client on this connection does not place orders; nothing is sent")
        if config.account_id not in self._conn.managed_accounts:
            raise SendRefused("account {!r} is not among the accounts this IBKR login manages; nothing is "
                              "sent".format(config.account_id))
        if self._conn.next_order_id is None:
            raise SendRefused("IBKR has not given this session an order ID (nextValidId); nothing is sent")
        return request

    def _check_shape(self, request):
        """Refuse a request out of the released-order shape or for another account; returns the configuration."""
        config = self._conn.config
        _require_paper(config)
        validate(request)
        if request.account_id != config.account_id:
            raise ValueError("request for account {!r} on the connection to paper account {!r}".format(
                request.account_id, config.account_id))
        if request.order_type != RELEASE_ORDER_TYPE:
            raise UnsendableOrder("a released order is a limit order, not {!r}".format(request.order_type))
        if request.tif != RELEASE_TIF:
            raise ValueError("a released order is a {} order, not {!r}".format(RELEASE_TIF, request.tif))
        try:
            units(request.quantity, quantity_scale(config), "order quantity")
        except ValueError as err:
            raise UnsendableOrder(str(err)) from None
        if not isinstance(request.con_id, int) or isinstance(request.con_id, bool) or request.con_id <= 0:
            raise UnsendableOrder("con_id must be IBKR's positive contract ID")
        return config

    def send(self, request):
        """Place a request check has passed: only its shape is checked again, never the link (see check)."""
        config = self._check_shape(request)
        order_id = self._conn.next_order_id
        self._conn.next_order_id = order_id + 1
        self._conn.placing(order_id)
        self._conn.api.place_order(order_id, *request_shape(request))
        record = SentRecord(request=request, send=True, client_id=config.client_id, broker_order_id=order_id)
        self._journal(record)
        return record


def request_shape(request):
    """The (contract, order) a request is handed to TWSApi.place_order as: plain objects with ibapi's attribute names.

    Exactly the attributes tws.TWSApi.place_order promises, which the real
    client maps onto ibapi's Contract and Order (real_client.ibapi_contract,
    real_client.ibapi_order).
    """
    contract = SimpleNamespace(conId=request.con_id, symbol=ibkr_symbol(request.symbol), secType=SEC_TYPE,
                               currency=CURRENCY, exchange=EXCHANGE)
    order = SimpleNamespace(orderRef=request.client_order_id, account=request.account_id,
                            action=request.action, totalQuantity=request.quantity,
                            orderType=request.order_type, lmtPrice=float(request.limit_price),
                            tif=request.tif, transmit=True)
    return contract, order


def release_to_request(order, mode, con_ids, session=None):
    """The OrderRequest for a released order's payload, or ValueError if this adapter must not send it.

    Only a release marked send whose mode is the adapter's own is sent
    (ADR 0026 point 3, "Mode"). con_ids maps the engine's instrument ID to
    IBKR's contract ID; an instrument with none is refused, never looked up
    by name. That refusal, and a noninteger wire quantity, raise
    UnsendableOrder: the order can never be sent as it stands. session is the New York date of the input whose reply released
    the order; without one the order is refused.
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
        raise UnsendableOrder("no IBKR contract ID is configured for {!r}".format(symbol))
    quantity = order.get("quantity")
    if _is_fraction(quantity):
        raise UnsendableOrder("quantity {!r} is not integer units at its declared quantity_scale".format(quantity))
    if not isinstance(quantity, int) or isinstance(quantity, bool) or quantity <= 0:
        raise ValueError("quantity {!r} is not a positive integer quantity".format(quantity))
    quantity = shares(quantity, order.get("quantity_scale", 0))
    limit = to_decimal(order.get("limit_price"), "limit_price")
    if limit <= 0:
        raise ValueError("limit_price must be positive")
    if not _is_date(session):
        raise ValueError("order {} has no session it was released in".format(order.get("client_order_id")))
    return OrderRequest(client_order_id=order["client_order_id"], account_id=order["account_id"],
                        con_id=con_ids[symbol], symbol=symbol, action=action, quantity=quantity,
                        order_type=RELEASE_ORDER_TYPE, limit_price=limit, tif=RELEASE_TIF,
                        session=session)


def _is_fraction(value):
    """Whether value is a positive, finite number of shares that is not whole."""
    if isinstance(value, bool) or not isinstance(value, (int, float, Decimal)):
        return False
    try:
        number = Decimal(value)
    except (ValueError, ArithmeticError):
        return False
    return number.is_finite() and number > 0 and number != number.to_integral_value()


def _is_date(value):
    return isinstance(value, date) and not isinstance(value, datetime)


def make_sender(config, journal, conn=None, today=None):
    """Return the sender for a configuration: dry-run journals only, paper sends on conn, live is refused."""
    if config.mode == "dry-run":
        return DryRunSender(journal)
    if config.mode == "paper":
        if conn is None or conn.config != config:
            raise SendRefused("paper sends only on the connection built from this configuration")
        return PaperSender(conn, journal, today)
    if config.mode == "live":
        raise LiveSendingDisabled("mode 'live' is disabled until the limited-live gate (AGENTS.md rule 5)")
    raise ValueError("unknown mode {!r}".format(config.mode))
