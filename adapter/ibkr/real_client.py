"""TWSApi implemented with IBKR's own TWS API Python client, `ibapi` (ADR 0025 point 4).

This is the only module that imports ibapi. It forwards requests to
EClient and EWrapper callbacks to the adapter's Inbox, and does nothing else,
so the logic it carries is the logic the fake-client tests already cover.

ibapi is optional at import: until the pinned, checksummed release is
installed, this module still imports and IbapiClient() raises
IbapiUnavailable, so the rest of the adapter and its tests never need it.
"""
import inspect
import logging
import threading

from connection import ConnectFailed
from sender import LiveSendingDisabled
from tws import TWSApi

try:
    from ibapi.client import EClient
    from ibapi.execution import ExecutionFilter
    from ibapi.wrapper import EWrapper
    IBAPI_AVAILABLE = True
except ImportError:
    IBAPI_AVAILABLE = False


log = logging.getLogger("ibkr")

# How long disconnect waits for the old reader thread to exit once its socket
# is closed. A reader that outlives it can still only reach its own session's
# Inbox, never a newer one.
READER_STOP_TIMEOUT = 5.0


class IbapiUnavailable(ImportError):
    """IBKR's ibapi package is not installed in this environment."""


def _forwarder_class():
    # IBKR changed EWrapper.error to take an errorTime second argument in
    # later 10.x releases; the forwarder follows whichever ibapi is installed
    # and passes the adapter the older, time-free shape.
    takes_error_time = "errorTime" in inspect.signature(EWrapper.error).parameters

    class Forwarder(EWrapper):
        """Bound to one session's Inbox for its whole life, so a late callback stays in that session."""

        def __init__(self, callbacks):
            super().__init__()
            self._callbacks = callbacks

        def _send(self, name, *args):
            getattr(self._callbacks, name)(*args)

        def nextValidId(self, orderId):
            self._send("next_valid_id", orderId)

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

    return Forwarder


class IbapiClient(TWSApi):
    """The real IB Gateway client.

    EClient.run, which delivers callbacks, runs on its own daemon thread;
    callbacks only enqueue into the Inbox (tws.py), so no adapter state is
    touched from it.

    Invariant: each connect is a new session with its own EWrapper and
    EClient, bound to that connect's Inbox. The previous session is
    disconnected and its reader joined first, and even a reader that fails
    to stop can only deliver to its own session's Inbox.
    """

    def __init__(self, start_reader=True):
        if not IBAPI_AVAILABLE:
            raise IbapiUnavailable("ibapi is not installed; see adapter/ibkr/README.md")
        self._start_reader = start_reader
        self._forwarder = _forwarder_class()
        self._wrapper = None
        self._client = None
        self._reader = None

    def connect(self, host, port, client_id, callbacks, timeout):
        self.disconnect()
        self._wrapper = self._forwarder(callbacks)
        self._client = EClient(self._wrapper)
        # EClient.connect opens the socket and runs IBKR's handshake
        # synchronously, with no bound of its own.
        failure = []

        def open_socket(client=self._client):
            try:
                client.connect(host, port, client_id)
            except Exception as err:  # reported to the caller below
                failure.append(err)
        opener = threading.Thread(target=open_socket, name="ibapi-connect", daemon=True)
        opener.start()
        opener.join(timeout)
        if opener.is_alive():
            # Closing the socket unblocks the opener; it starts no reader.
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
        """Close this session's socket and wait for its reader to exit."""
        if self._client is not None:
            self._client.disconnect()
        reader, self._reader = self._reader, None
        if reader is not None and reader is not threading.current_thread():
            reader.join(READER_STOP_TIMEOUT)
            if reader.is_alive():
                log.warning("ibkr reader did not stop within %.1fs of disconnect",
                            READER_STOP_TIMEOUT)

    def is_connected(self):
        return self._client is not None and self._client.isConnected()

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

    def place_order(self, order_id, contract, order):
        """Disabled: no order reaches IBKR before the paper and limited-live gates (AGENTS.md rule 5)."""
        raise LiveSendingDisabled("placeOrder is disabled until the paper and limited-live gates")
