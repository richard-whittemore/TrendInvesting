"""TWSApi implemented with IBKR's own TWS API Python client, `ibapi` (ADR 0025 point 4).

This is the only module that imports ibapi. It forwards requests to
EClient and EWrapper callbacks to the adapter's Inbox, so the logic it
carries is the logic the fake-client tests already cover. The one call it
does not simply forward is place_order: it builds ibapi's Contract and Order
from the adapter's request shape (tws.TWSApi.place_order; ibapi_contract,
ibapi_order) and places it only while can_place_orders() holds, which needs
a paper configuration on a paper account that this session's IBKR login
manages, with paper_sending_enabled switched on (AGENTS.md rule 5), and
only while ibapi's own socket is open, before and after the call: a socket
IB Gateway has closed makes EClient.placeOrder return without sending, so
an order is never taken as sent unless the socket was open throughout.

The installed ibapi's Contract and Order are checked at construction for
every field the adapter sets, so an ibapi without one refuses to start
rather than refuse an order after the relay has marked it.

ibapi is optional at import: until the pinned, checksummed release is
installed, this module still imports and IbapiClient() raises
IbapiUnavailable, so the rest of the adapter and its tests never need it.
"""
import inspect
import logging
import math
import sys
import threading
import time
from decimal import Decimal

from config import is_paper_account
from connection import ConnectFailed
from relay import HandoverUnconfirmed
from sender import ACTIONS, CURRENCY, EXCHANGE, RELEASE_ORDER_TYPE, RELEASE_TIF, SEC_TYPE, SendRefused
from tws import TWSApi

try:
    from ibapi.client import EClient
    from ibapi.contract import Contract
    from ibapi.execution import ExecutionFilter
    from ibapi.order import Order
    from ibapi.wrapper import EWrapper
    IBAPI_AVAILABLE = True
except ImportError:
    IBAPI_AVAILABLE = False


log = logging.getLogger("ibkr")

# The longest disconnect waits for the old reader thread to exit once its
# socket is closed (connect waits no longer than its own deadline). A reader
# that outlives it is kept, blocks every new session until it stops, and can
# only reach its own session's Inbox.
READER_STOP_TIMEOUT = 5.0


class IbapiUnavailable(ImportError):
    """IBKR's ibapi package is not installed in this environment."""


class OrderShapeRefused(SendRefused):
    """A request is not in the adapter's order shape, or ibapi lacks a field the adapter sets; nothing is sent."""


class IbapiMismatch(Exception):
    """The installed ibapi's Contract or Order lacks a field the adapter sets; the client does not start."""


class HandoverNotConfirmed(HandoverUnconfirmed):
    """IB Gateway's socket was closed as an order was to be placed: before placeOrder, or during it.

    Never taken as sent and never sent again by itself: the relay's mark
    stays "about to send", and the adapter stops for a person. Closed
    before placeOrder ("before"), nothing was handed to IB Gateway, so
    IBKR cannot have the order: handed_over is False, which the relay
    records beside its mark, and the owner settles it with a resend in the
    session it was committed, or, on any day, as not handed over once
    IBKR's order history shows nothing under its tag (relay.resolve).
    Closed during it ("during"), the order may or may not have reached
    IBKR (handed_over None), and a person checks IBKR as for any hand-over
    a stop interrupted.
    """

    def __init__(self, client_order_id, when):
        if when == "before":
            text = ("order {0}: IB Gateway's socket was closed before placeOrder, so nothing was handed to IB "
                    "Gateway and IBKR cannot have the order; it is not taken as sent and nothing else is sent. "
                    "With the adapter stopped, settle it with python3 adapter/ibkr/relay.py resolve PENDING_FILE "
                    "{0} --resend --reason \"socket closed before placeOrder\" in the session the order was "
                    "committed (New York date), and it goes out once the link is back on that same day; on a "
                    "later day the adapter stops again (the permission is dated) and the order is settled with "
                    "--not-handed-over. On a later day, or if it is not to go out, check IBKR's order history "
                    "shows no order whose orderRef is {0}, then settle it with the same command and "
                    "--not-handed-over in place of --resend "
                    "(docs/manual/paper-trading.md)")
        else:
            text = ("order {0}: IB Gateway's socket was closed {1} placeOrder, so whether IBKR has the order is "
                    "not known; it is not taken as sent, it is not sent again and nothing else is sent: check "
                    "IBKR for an order whose orderRef is {0}, then settle it with relay.py resolve "
                    "(docs/manual/redelivery.md)")
        Exception.__init__(self, text.format(client_order_id, when))
        self.client_order_id = client_order_id
        self.handed_over = False if when == "before" else None


# ibapi marks an unset price with sys.float_info.max (ibapi.const.UNSET_DOUBLE;
# reports.UNSET_FIGURE reads the same marker back).
UNSET_DOUBLE = sys.float_info.max

# The request shape tws.TWSApi.place_order promises, attribute for attribute.
CONTRACT_REQUEST_FIELDS = ("conId", "symbol", "secType", "currency", "exchange")
ORDER_REQUEST_FIELDS = ("orderRef", "account", "action", "totalQuantity", "orderType", "lmtPrice", "tif",
                        "transmit")

# The Order fields that change what IBKR is asked to do and that the adapter
# sets to their plain value rather than leave to the installed ibapi's
# default: regular hours only and not the overnight session, no parent, OCA
# group or conditions, a real order (not whatIf), no stop or trail price,
# not hidden, not all-or-none, no start or end time beyond DAY, no IBKR algo
# and no price-management algo (explicitly off, not ibapi's None, which
# means the account's own setting) (ADR 0026 point 2: a whole-share DAY
# limit order and nothing more).
ORDER_FIELDS_SET = (
    ("outsideRth", False), ("parentId", 0), ("ocaGroup", ""), ("conditions", []), ("whatIf", False),
    ("auxPrice", UNSET_DOUBLE), ("hidden", False), ("allOrNone", False), ("goodAfterTime", ""),
    ("goodTillDate", ""), ("algoStrategy", ""), ("trailStopPrice", UNSET_DOUBLE), ("includeOvernight", False),
    ("usePriceMgmtAlgo", False),
)

# The defaults the paper test confirms. ibapi's Order has more fields than
# the adapter sets, and this list is not all of them: it names those the
# paper-account test checks from IBKR's own view of the order (its order
# window and the openOrder echo) to show the order is plain: no iceberg
# display size, no minimum quantity, no percent offset or discretion, no
# sweep or block, not "not held", no e-trade or firm-quote flag, no cash
# quantity. Every field the adapter does set is in ORDER_REQUEST_FIELDS or
# ORDER_FIELDS_SET.
DEFAULTS_TO_CONFIRM = ("displaySize", "minQty", "percentOffset", "discretionaryAmt", "sweepToFill", "blockOrder",
                       "notHeld", "eTradeOnly", "firmQuoteOnly", "cashQty")


def _fields(request, names, what):
    values = vars(request) if hasattr(request, "__dict__") else {}
    if tuple(sorted(values)) != tuple(sorted(names)):
        raise OrderShapeRefused("the {} request carries {}, not exactly {}; nothing is sent".format(
            what, ", ".join(sorted(values)) or "nothing", ", ".join(names)))
    return values


def _assign(target, name, value):
    # ibapi's encoder sends only the attributes it knows: one the installed
    # ibapi lacks would be dropped silently, so it is refused instead.
    if not hasattr(target, name):
        raise OrderShapeRefused("the installed ibapi's {} has no field {!r}; nothing is sent".format(
            type(target).__name__, name))
    setattr(target, name, value)


def _is_text(value):
    return isinstance(value, str) and bool(value)


def ibapi_contract(request, contract_class):
    """A contract_class (ibapi.contract.Contract) for the request's contract: a USD stock on SMART, by IBKR's conId.

    Raises OrderShapeRefused for anything but tws.TWSApi.place_order's
    contract shape, and for an ibapi Contract without one of its fields.
    """
    values = _fields(request, CONTRACT_REQUEST_FIELDS, "contract")
    con_id = values["conId"]
    if not isinstance(con_id, int) or isinstance(con_id, bool) or con_id <= 0:
        raise OrderShapeRefused("conId must be IBKR's positive contract ID")
    if not _is_text(values["symbol"]):
        raise OrderShapeRefused("the contract names no symbol")
    if (values["secType"], values["currency"], values["exchange"]) != (SEC_TYPE, CURRENCY, EXCHANGE):
        raise OrderShapeRefused("the contract is not a {} {} contract on {}".format(CURRENCY, SEC_TYPE, EXCHANGE))
    contract = contract_class()
    for name in CONTRACT_REQUEST_FIELDS:
        _assign(contract, name, values[name])
    return contract


def ibapi_order(request, order_class):
    """An order_class (ibapi.order.Order) for the request's order: a whole-share DAY limit order, transmitted.

    The request's fields are copied as they are (orderRef is the client
    order ID; totalQuantity a whole Decimal, as ibapi 10.x takes it;
    lmtPrice a float), and every field in ORDER_FIELDS_SET is set to its
    plain value; the fields in DEFAULTS_TO_CONFIRM keep ibapi's default.
    Raises OrderShapeRefused for anything but tws.TWSApi.place_order's order
    shape, and for an ibapi Order without one of the fields set here.
    """
    values = _fields(request, ORDER_REQUEST_FIELDS, "order")
    if not _is_text(values["orderRef"]) or not _is_text(values["account"]):
        raise OrderShapeRefused("an order carries its client order ID (orderRef) and its account")
    if values["action"] not in ACTIONS:
        raise OrderShapeRefused("action {!r} is not BUY or SELL".format(values["action"]))
    quantity = values["totalQuantity"]
    if (not isinstance(quantity, Decimal) or not quantity.is_finite() or quantity <= 0
            or quantity != quantity.to_integral_value()):
        raise OrderShapeRefused("totalQuantity must be a positive whole number of shares as a Decimal")
    price = values["lmtPrice"]
    if not isinstance(price, float) or not math.isfinite(price) or price <= 0:
        raise OrderShapeRefused("lmtPrice must be a positive finite float")
    if values["orderType"] != RELEASE_ORDER_TYPE or values["tif"] != RELEASE_TIF:
        raise OrderShapeRefused("an order is a {} {} order, not {} {}".format(
            RELEASE_TIF, RELEASE_ORDER_TYPE, values["tif"], values["orderType"]))
    if values["transmit"] is not True:
        raise OrderShapeRefused("an order the adapter sends is transmitted")
    order = order_class()
    for name in ORDER_REQUEST_FIELDS:
        _assign(order, name, values[name])
    for name, value in ORDER_FIELDS_SET:
        # A fresh list each time: ibapi's conditions list is the order's own.
        _assign(order, name, list(value) if isinstance(value, list) else value)
    return order


def _forwarder_class():
    # IBKR changed EWrapper.error to take an errorTime second argument in
    # later 10.x releases; the forwarder follows whichever ibapi is installed
    # and passes the adapter the older, time-free shape.
    takes_error_time = "errorTime" in inspect.signature(EWrapper.error).parameters

    class Forwarder(EWrapper):
        """Bound to one session's Inbox for its whole life, so a late callback stays in that session.

        managed holds the accounts this session's login manages, as IBKR
        last named them (managedAccounts); it is this session's alone.
        """

        def __init__(self, callbacks):
            super().__init__()
            self._callbacks = callbacks
            self.managed = frozenset()

        def _send(self, name, *args):
            getattr(self._callbacks, name)(*args)

        def nextValidId(self, orderId):
            self._send("next_valid_id", orderId)

        def managedAccounts(self, accountsList):
            self.managed = _account_list(accountsList)
            self._send("managed_accounts", accountsList)

        def connectionClosed(self):
            self._send("connection_closed")

        if takes_error_time:
            def error(self, reqId, errorTime, errorCode, errorString, advancedOrderRejectJson=""):
                self._send("error", reqId, errorCode, errorString, advancedOrderRejectJson)
        else:
            def error(self, reqId, errorCode, errorString, advancedOrderRejectJson=""):
                self._send("error", reqId, errorCode, errorString, advancedOrderRejectJson)

        def currentTime(self, time):
            self._send("current_time", time)

        def position(self, account, contract, position, avgCost):
            self._send("position", account, contract, position, avgCost)

        def positionEnd(self):
            self._send("position_end")

        def accountSummary(self, reqId, account, tag, value, currency):
            self._send("account_summary", reqId, account, tag, value, currency)

        def accountSummaryEnd(self, reqId):
            self._send("account_summary_end", reqId)

        def openOrder(self, orderId, contract, order, orderState):
            self._send("open_order", orderId, contract, order, orderState)

        def openOrderEnd(self):
            self._send("open_order_end")

        def orderStatus(self, orderId, status, filled, remaining, avgFillPrice, permId, parentId,
                        lastFillPrice, clientId, whyHeld, mktCapPrice):
            self._send("order_status", orderId, status, filled, remaining, avgFillPrice, permId,
                       parentId, lastFillPrice, clientId, whyHeld, mktCapPrice)

        def execDetails(self, reqId, contract, execution):
            self._send("exec_details", reqId, contract, execution)

        def execDetailsEnd(self, reqId):
            self._send("exec_details_end", reqId)

        # Renamed commissionAndFeesReport in later 10.x releases; both reach
        # the same callback.
        def commissionReport(self, commissionReport):
            self._send("commission_report", commissionReport)

        def commissionAndFeesReport(self, commissionAndFeesReport):
            self._send("commission_report", commissionAndFeesReport)

        def tickPrice(self, reqId, tickType, price, attrib):
            self._send("tick_price", reqId, tickType, price, attrib)

        def tickSnapshotEnd(self, reqId):
            self._send("tick_snapshot_end", reqId)

    return Forwarder


def _account_list(text):
    """IBKR's managedAccounts list ("DU1,DU2,") as a set of account IDs."""
    return frozenset(a.strip() for a in str(text).split(",") if a.strip())


def check_ibapi_fields(contract_class, order_class):
    """Raise IbapiMismatch unless a new contract_class and order_class carry every field the adapter sets.

    ibapi's encoder sends only the attributes it knows, so a field the
    installed ibapi lacks would be dropped from every order; checked once,
    before any order can be marked (relay.py marks before place_order).
    """
    for cls, names in ((contract_class, CONTRACT_REQUEST_FIELDS),
                       (order_class, ORDER_REQUEST_FIELDS + tuple(n for n, _ in ORDER_FIELDS_SET))):
        instance = cls()
        missing = [n for n in names if not hasattr(instance, n)]
        if missing:
            raise IbapiMismatch("the installed ibapi's {} has no field {}: the adapter would send orders without "
                                "it, so it does not start; install the pinned TWS API build with `sudo "
                                "deploy/provision.sh twsapi`".format(cls.__name__, ", ".join(missing)))


class IbapiClient(TWSApi):
    """The real IB Gateway client.

    EClient.run, which delivers callbacks, runs on its own daemon thread;
    callbacks only enqueue into the Inbox (tws.py), so no adapter state is
    touched from it.

    Invariant: each connect is a new session with its own EWrapper and
    EClient, bound to that connect's Inbox. The previous session is
    disconnected and its reader joined first. A reader that fails to stop is
    kept, and no new session starts until it has stopped; even meanwhile it
    can only deliver to its own session's Inbox.

    Invariant: an attempt abandoned at its timeout never keeps a connection.
    Its opener disconnects its EClient if the socket connect completes late,
    and no new attempt starts while that opener may still claim the client
    ID, so a stray session cannot make IBKR refuse the next one (code 326).

    config is the adapter's configuration (config.AdapterConfig); without
    one the client places no order. With one, can_place_orders holds only
    on its terms (see can_place_orders).
    """

    def __init__(self, start_reader=True, monotonic=time.monotonic, config=None):
        if not IBAPI_AVAILABLE:
            raise IbapiUnavailable("ibapi is not installed; see adapter/ibkr/README.md")
        check_ibapi_fields(Contract, Order)
        self._config = config
        self._start_reader = start_reader
        self._monotonic = monotonic
        self._forwarder = _forwarder_class()
        self._wrapper = None
        self._client = None
        self._reader = None
        self._opener = None

    def connect(self, host, port, client_id, callbacks, timeout):
        # One deadline bounds the whole call: waiting out an abandoned attempt
        # spends the same timeout the new attempt then has left.
        deadline = self._monotonic() + timeout
        previous = self._opener
        if previous is not None and previous.is_alive():
            # An abandoned attempt is still inside EClient.connect and may yet
            # take this client ID; wait for it to finish and clean up.
            previous.join(max(deadline - self._monotonic(), 0))
            if previous.is_alive():
                raise ConnectFailed("an abandoned connect attempt has not finished")
        # The previous reader must have stopped before a new session starts;
        # waiting for it spends the same deadline.
        if not self._end_session(min(READER_STOP_TIMEOUT,
                                     max(deadline - self._monotonic(), 0))):
            raise ConnectFailed("the previous session's reader has not stopped")
        remaining = deadline - self._monotonic()
        if remaining <= 0:
            raise ConnectFailed("no time left within {}s for a new connect attempt".format(timeout))
        self._wrapper = self._forwarder(callbacks)
        self._client = EClient(self._wrapper)
        # EClient.connect opens the socket and runs IBKR's handshake
        # synchronously, with no bound of its own.
        failure = []
        lock = threading.Lock()
        attempt = {"abandoned": False}

        def open_socket(client=self._client):
            try:
                client.connect(host, port, client_id)
            except Exception as err:  # reported to the caller below
                failure.append(err)
            with lock:
                if attempt["abandoned"]:
                    # Completed after its timeout: it must not hold the ID.
                    client.disconnect()
        opener = threading.Thread(target=open_socket, name="ibapi-connect", daemon=True)
        self._opener = opener
        opener.start()
        opener.join(remaining)
        with lock:
            timed_out = opener.is_alive()
            attempt["abandoned"] = timed_out
        if timed_out:
            # Closing the socket unblocks the opener; it starts no reader, and
            # disconnects its client itself if the connect completes later.
            self.disconnect()
            raise ConnectFailed("socket connect did not finish within {}s".format(timeout))
        if failure:
            self.disconnect()
            raise ConnectFailed("socket connect failed: {}".format(type(failure[0]).__name__))
        if self._start_reader:
            self._reader = threading.Thread(target=self._client.run, name="ibapi-reader",
                                            daemon=True)
            self._reader.start()

    def disconnect(self):
        """Close this session's socket and wait for its reader to exit.

        Returns False if the reader is still running. It is kept, so the next
        connect checks the same reader again and refuses to start a session
        beside it.
        """
        return self._end_session(READER_STOP_TIMEOUT)

    def _end_session(self, wait):
        if self._client is not None:
            self._client.disconnect()
        reader = self._reader
        if reader is not None and reader is not threading.current_thread():
            reader.join(wait)
            if reader.is_alive():
                log.warning("ibkr reader did not stop within %.2fs of disconnect", wait)
                return False
        self._reader = None
        return True

    def is_connected(self):
        """Whether EClient's state and ibapi's own socket are both open.

        IB Gateway closing the socket (its nightly restart, a crash) closes
        ibapi's connection (conn) without changing EClient's own state,
        which only the adapter's disconnect does; both are read.
        """
        client = self._client
        if client is None or not client.isConnected():
            return False
        conn = getattr(client, "conn", None)
        return conn is not None and conn.isConnected()

    def req_current_time(self):
        self._client.reqCurrentTime()

    def req_positions(self):
        self._client.reqPositions()

    def cancel_positions(self):
        self._client.cancelPositions()

    def req_account_summary(self, req_id, group, tags):
        self._client.reqAccountSummary(req_id, group, tags)

    def cancel_account_summary(self, req_id):
        self._client.cancelAccountSummary(req_id)

    def req_all_open_orders(self):
        self._client.reqAllOpenOrders()

    def req_executions(self, req_id, account_id):
        flt = ExecutionFilter()
        flt.acctCode = account_id
        self._client.reqExecutions(req_id, flt)

    def req_mkt_data(self, req_id, contract):
        """One market-data snapshot of the contract, by IBKR's conId (snapshot.read_marks).

        A snapshot (IBKR ends it with tickSnapshotEnd) with no generic ticks,
        which IBKR refuses on a snapshot, and not a regulatory snapshot,
        which IBKR charges for. The contract must be in place_order's
        contract shape (OrderShapeRefused otherwise).
        """
        self._client.reqMktData(req_id, ibapi_contract(contract, Contract), "", True, False, [])

    def cancel_mkt_data(self, req_id):
        self._client.cancelMktData(req_id)

    def can_place_orders(self):
        """Whether this client may place orders: the sending switch, the paper gate's last check (AGENTS.md rule 5).

        True only with a configuration in mode paper on a paper account (its
        ID begins "DU", config.is_paper_account), that switches
        paper_sending_enabled on, and only once this session's IBKR login has
        named that account among the accounts it manages (managedAccounts).
        The link's state is not part of it: the sender checks the link first
        and reports it down as NotConnected, which holds the input, and
        place_order checks ibapi's socket itself (sender.PaperSender.check).
        """
        cfg = self._config
        if cfg is None or cfg.mode != "paper" or cfg.paper_sending_enabled is not True:
            return False
        if not is_paper_account(cfg.account_id):
            return False
        return self._wrapper is not None and cfg.account_id in self._wrapper.managed

    def place_order(self, order_id, contract, order):
        """Place the request on IB Gateway as ibapi's Contract and Order, or raise SendRefused and send nothing.

        Refused unless can_place_orders() holds and the order is for the
        configured account; the request must be in tws.TWSApi.place_order's
        shape (OrderShapeRefused otherwise). IBKR answers on the Inbox.
        ibapi's socket is checked immediately before and after
        EClient.placeOrder: closed at either moment, the order may or may
        not have reached IBKR, and HandoverNotConfirmed stops the adapter
        for a person rather than let it be taken as sent.
        """
        if not self.can_place_orders():
            raise SendRefused("the IBKR client places no order: it needs mode paper on a paper account this login "
                              "manages, with paper_sending_enabled on (docs/manual/paper-trading.md)")
        if getattr(order, "account", None) != self._config.account_id:
            raise SendRefused("an order for another account than the configured paper account is never placed")
        built_contract = ibapi_contract(contract, Contract)
        built_order = ibapi_order(order, Order)
        client_order_id = getattr(order, "orderRef", None)
        if not self.is_connected():
            raise HandoverNotConfirmed(client_order_id, "before")
        self._client.placeOrder(order_id, built_contract, built_order)
        if not self.is_connected():
            raise HandoverNotConfirmed(client_order_id, "during")
