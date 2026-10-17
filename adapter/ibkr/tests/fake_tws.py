"""A scripted stand-in for IB Gateway behind the adapter's TWSApi interface.

It answers each request with synthetic callbacks in the shapes ibapi delivers
them (attribute objects for contracts, orders and executions), so the adapter
code under test is exactly the code that runs against the real client. Every
figure here is invented; none is a real price or account.
"""
import sys
from pathlib import Path
from types import SimpleNamespace

adapter_dir = str(Path(__file__).resolve().parents[1])
if adapter_dir not in sys.path:
    sys.path.insert(0, adapter_dir)

from connection import ConnectFailed
from tws import TWSApi

ACCOUNT = "DU0000001"
OTHER_ACCOUNT = "DU0000002"
SERVER_TIME = 1_800_000_000


def contract(con_id=756733, symbol="SPY", sec_type="STK", currency="USD", exchange="SMART"):
    return SimpleNamespace(conId=con_id, symbol=symbol, secType=sec_type,
                           currency=currency, exchange=exchange, primaryExchange="ARCA")


def order(order_ref="tag-1", account=ACCOUNT, action="BUY", total_quantity="10",
          order_type="MKT", lmt_price=None, aux_price=None, tif="DAY", perm_id=9001,
          client_id=11):
    return SimpleNamespace(orderRef=order_ref, account=account, action=action,
                           totalQuantity=total_quantity, orderType=order_type,
                           lmtPrice=lmt_price, auxPrice=aux_price, tif=tif, permId=perm_id,
                           clientId=client_id)


def execution(exec_id="0001f4e8.6512a1b2.01.01", order_id=7, perm_id=9001, order_ref="tag-1",
              account=ACCOUNT, side="BOT", shares="4", price=101.25, cum_qty="4",
              avg_price=101.25, time="20261016 09:30:01 US/Eastern", client_id=11):
    return SimpleNamespace(execId=exec_id, orderId=order_id, permId=perm_id, orderRef=order_ref,
                           clientId=client_id,
                           acctNumber=account, side=side, shares=shares, price=price,
                           cumQty=cum_qty, avgPrice=avg_price, time=time)


class FakeTWS(TWSApi):
    """Records every call and answers from a script.

    connect_behaviours is consumed one entry per connect attempt:
    "ok", "client-id-in-use", "refused", "silent" (no answer at all),
    "hang" (the socket work overran its timeout, as IbapiClient reports it)
    or "socket-error". connect_hook, if set, runs first, so a test can let
    the clock move while the socket is being opened.
    """

    def __init__(self):
        self.calls = []
        self.callbacks = None
        self.connected = False
        self.connect_behaviours = []
        self.connect_hook = None
        self.connect_timeouts = []
        self.next_order_id = 7
        self.server_time = SERVER_TIME
        self.answer_current_time = True
        self.positions = []
        self.summary = []
        self.end_positions = True
        self.end_summary = True
        self.summary_error = None
        self.drop_during_summary = False
        self.open_orders = []
        self.executions = []

    # -- connection ------------------------------------------------------
    def connect(self, host, port, client_id, callbacks, timeout):
        self.calls.append(("connect", host, port, client_id))
        self.connect_timeouts.append(timeout)
        self.callbacks = callbacks
        if self.connect_hook is not None:
            self.connect_hook()
        behaviour = self.connect_behaviours.pop(0) if self.connect_behaviours else "ok"
        if behaviour == "hang":
            raise ConnectFailed("socket connect did not finish within {}s".format(timeout))
        if behaviour == "socket-error":
            raise ConnectionRefusedError("synthetic connection refused")
        if behaviour == "ok":
            self.connected = True
            callbacks.next_valid_id(self.next_order_id)
        elif behaviour == "client-id-in-use":
            callbacks.error(-1, 326, "Unable to connect as the client id is already in use. "
                                     "Retry with a unique client id.")
            callbacks.connection_closed()
        elif behaviour == "refused":
            callbacks.error(-1, 502, "Couldn't connect to TWS. Confirm that API is enabled.")
        elif behaviour != "silent":
            raise AssertionError("unknown behaviour " + behaviour)

    def disconnect(self):
        self.calls.append(("disconnect",))
        self.connected = False

    def is_connected(self):
        return self.connected

    def drop(self):
        """The socket closes underneath the adapter, as a Gateway restart does."""
        self.connected = False
        self.callbacks.connection_closed()

    # -- account ---------------------------------------------------------
    def req_current_time(self):
        self.calls.append(("req_current_time",))
        if self.answer_current_time:
            self.callbacks.current_time(self.server_time)
            self.server_time += 1

    def req_positions(self):
        self.calls.append(("req_positions",))
        for account, con, pos, avg in self.positions:
            self.callbacks.position(account, con, pos, avg)
        if self.end_positions:
            self.callbacks.position_end()

    def cancel_positions(self):
        self.calls.append(("cancel_positions",))

    def req_account_summary(self, req_id, group, tags):
        self.calls.append(("req_account_summary", req_id, group, tags))
        if self.summary_error is not None:
            self.callbacks.error(req_id, self.summary_error, "synthetic summary failure")
            return
        for i, (account, tag, value, currency) in enumerate(self.summary):
            if self.drop_during_summary and i == 1:
                self.drop()
                return
            self.callbacks.account_summary(req_id, account, tag, value, currency)
        if self.end_summary:
            self.callbacks.account_summary_end(req_id)

    def cancel_account_summary(self, req_id):
        self.calls.append(("cancel_account_summary", req_id))

    # -- orders ----------------------------------------------------------
    def req_all_open_orders(self):
        self.calls.append(("req_all_open_orders",))
        for order_id, con, ord_, state in self.open_orders:
            self.callbacks.open_order(order_id, con, ord_, state)
        self.callbacks.open_order_end()

    def req_executions(self, req_id, account_id):
        self.calls.append(("req_executions", req_id, account_id))
        for con, ex in self.executions:
            self.callbacks.exec_details(req_id, con, ex)
        self.callbacks.exec_details_end(req_id)

    def place_order(self, order_id, contract_, order_):
        self.calls.append(("place_order", order_id))

    def called(self, name):
        return [c for c in self.calls if c[0] == name]
