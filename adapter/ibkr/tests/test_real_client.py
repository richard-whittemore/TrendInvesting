"""The ibapi-backed client: importable without ibapi, and a thin forwarder when it is present.

ibapi is not installed in CI (ADR 0025 pins and vendors it; until then it is
absent). A stand-in ibapi package, injected into sys.modules, checks that the
forwarding is wired to the adapter's callback names, including both
signatures IBKR has shipped for EWrapper.error.
"""
import importlib
import sys
import threading
import types
import unittest
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
adapter_dir = str(Path(__file__).resolve().parents[1])
for p in (tests_dir, adapter_dir):
    if p not in sys.path:
        sys.path.insert(0, p)

from tws import Inbox
from sender import LiveSendingDisabled


class _EClient:
    # Set to a threading.Event to make connect block until it is set, as a
    # stalled socket connect or handshake does.
    connect_gate = None

    def __init__(self, wrapper):
        self.wrapper = wrapper
        self.requests = []
        self._stopped = threading.Event()

    def connect(self, host, port, clientId):
        self.requests.append(("connect", host, port, clientId))
        if _EClient.connect_gate is not None:
            _EClient.connect_gate.wait(5)
        # The socket is open now, whatever happened while connect was blocked.
        self._stopped.clear()

    def run(self):
        # The real reader loops until the socket closes.
        self._stopped.wait(5)

    def isConnected(self):
        return not self._stopped.is_set()

    def disconnect(self):
        self.requests.append(("disconnect",))
        self._stopped.set()

    def reqCurrentTime(self):
        self.requests.append(("reqCurrentTime",))

    def reqPositions(self):
        self.requests.append(("reqPositions",))

    def cancelPositions(self):
        self.requests.append(("cancelPositions",))

    def reqAccountSummary(self, reqId, group, tags):
        self.requests.append(("reqAccountSummary", reqId, group, tags))

    def cancelAccountSummary(self, reqId):
        self.requests.append(("cancelAccountSummary", reqId))

    def reqAllOpenOrders(self):
        self.requests.append(("reqAllOpenOrders",))

    def reqExecutions(self, reqId, flt):
        self.requests.append(("reqExecutions", reqId, flt.acctCode))

    def placeOrder(self, *args):
        raise AssertionError("placeOrder must never be reached")


def _install_stub(new_error_signature):
    pkg = types.ModuleType("ibapi")
    client = types.ModuleType("ibapi.client")
    wrapper = types.ModuleType("ibapi.wrapper")
    execution = types.ModuleType("ibapi.execution")

    class EWrapper:
        pass

    if new_error_signature:
        def error(self, reqId, errorTime, errorCode, errorString, advancedOrderRejectJson=""):
            pass
    else:
        def error(self, reqId, errorCode, errorString, advancedOrderRejectJson=""):
            pass
    EWrapper.error = error

    class ExecutionFilter:
        acctCode = ""

    client.EClient = _EClient
    wrapper.EWrapper = EWrapper
    execution.ExecutionFilter = ExecutionFilter
    sys.modules.update({"ibapi": pkg, "ibapi.client": client, "ibapi.wrapper": wrapper,
                        "ibapi.execution": execution})


def _uninstall_stub():
    for name in ("ibapi", "ibapi.client", "ibapi.wrapper", "ibapi.execution"):
        sys.modules.pop(name, None)


class Recorder(Inbox):
    def __init__(self):
        super().__init__()

    def events(self):
        out = []
        while True:
            ev = self.get(0)
            if ev is None:
                return out
            out.append(ev)


class WithoutIbapiTests(unittest.TestCase):
    def test_module_imports_and_construction_explains_the_absence(self):
        _uninstall_stub()
        import real_client
        real_client = importlib.reload(real_client)
        if real_client.IBAPI_AVAILABLE:
            self.skipTest("a real ibapi is installed on this machine")
        with self.assertRaises(real_client.IbapiUnavailable):
            real_client.IbapiClient()


class ForwardingTests(unittest.TestCase):
    def tearDown(self):
        _EClient.connect_gate = None
        _uninstall_stub()
        import real_client
        importlib.reload(real_client)

    def module(self, new_error_signature=False):
        _install_stub(new_error_signature)
        import real_client
        real_client = importlib.reload(real_client)
        self.assertTrue(real_client.IBAPI_AVAILABLE)
        return real_client

    def build(self, new_error_signature, start_reader=False):
        client = self.module(new_error_signature).IbapiClient(start_reader=start_reader)
        inbox = Recorder()
        client.connect("127.0.0.1", 4002, 11, inbox, 1.0)
        return client, inbox

    def test_a_late_callback_from_an_old_session_never_reaches_the_new_one(self):
        client, old_inbox = self.build(False)
        old_wrapper = client._wrapper
        client.disconnect()
        new_inbox = Recorder()
        client.connect("127.0.0.1", 4002, 11, new_inbox, 1.0)
        old_wrapper.connectionClosed()
        old_wrapper.nextValidId(99)
        self.assertEqual(new_inbox.events(), [])
        self.assertEqual([e.name for e in old_inbox.events()],
                         ["connection_closed", "next_valid_id"])
        self.assertIsNot(client._wrapper, old_wrapper)

    def test_disconnect_stops_and_joins_the_reader_before_a_reconnect(self):
        client, _ = self.build(False, start_reader=True)
        old_reader = client._reader
        self.assertTrue(old_reader.is_alive())
        client.disconnect()
        self.assertFalse(old_reader.is_alive())
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        self.assertIsNot(client._reader, old_reader)
        client.disconnect()

    def test_a_reconnect_without_a_disconnect_still_ends_the_old_session_first(self):
        client, _ = self.build(False, start_reader=True)
        old_reader, old_eclient = client._reader, client._client
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        self.assertFalse(old_reader.is_alive())
        self.assertIn(("disconnect",), old_eclient.requests)
        client.disconnect()

    def test_a_stalled_socket_connect_is_bounded_by_the_timeout(self):
        real_client = self.module()
        from connection import ConnectFailed
        _EClient.connect_gate = threading.Event()
        client = real_client.IbapiClient(start_reader=True)
        try:
            with self.assertRaises(ConnectFailed):
                client.connect("127.0.0.1", 4002, 11, Recorder(), 0.05)
            self.assertIn(("disconnect",), client._client.requests)
            self.assertIsNone(client._reader)
        finally:
            _EClient.connect_gate.set()

    def test_an_abandoned_attempt_that_connects_late_is_disconnected(self):
        real_client = self.module()
        _EClient.connect_gate = threading.Event()
        client = real_client.IbapiClient(start_reader=True)
        from connection import ConnectFailed
        with self.assertRaises(ConnectFailed):
            client.connect("127.0.0.1", 4002, 11, Recorder(), 0.05)
        abandoned, opener = client._client, client._opener
        _EClient.connect_gate.set()  # the stalled connect completes after the timeout
        opener.join(1.0)
        self.assertFalse(opener.is_alive())
        self.assertFalse(abandoned.isConnected())
        self.assertIsNone(client._reader)

    def test_no_new_attempt_starts_while_an_abandoned_one_may_still_claim_the_id(self):
        real_client = self.module()
        _EClient.connect_gate = threading.Event()
        client = real_client.IbapiClient(start_reader=False)
        from connection import ConnectFailed
        with self.assertRaises(ConnectFailed):
            client.connect("127.0.0.1", 4002, 11, Recorder(), 0.05)
        abandoned = client._client
        with self.assertRaises(ConnectFailed):
            client.connect("127.0.0.1", 4002, 11, Recorder(), 0.05)
        self.assertIs(client._client, abandoned)  # no second EClient was opened
        _EClient.connect_gate.set()
        _EClient.connect_gate = None
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        self.assertFalse(abandoned.isConnected())
        self.assertTrue(client.is_connected())

    def test_requests_reach_eclient(self):
        client, _ = self.build(False)
        client.req_current_time()
        client.req_positions()
        client.cancel_positions()
        client.req_account_summary(3, "All", "NetLiquidation")
        client.cancel_account_summary(3)
        client.req_all_open_orders()
        client.req_executions(4, "DU0000001")
        self.assertTrue(client.is_connected())
        client.disconnect()
        self.assertEqual(client._client.requests, [
            ("connect", "127.0.0.1", 4002, 11), ("reqCurrentTime",), ("reqPositions",),
            ("cancelPositions",), ("reqAccountSummary", 3, "All", "NetLiquidation"),
            ("cancelAccountSummary", 3), ("reqAllOpenOrders",), ("reqExecutions", 4, "DU0000001"),
            ("disconnect",)])
        self.assertFalse(client.is_connected())

    def test_place_order_is_disabled(self):
        client, _ = self.build(False)
        with self.assertRaises(LiveSendingDisabled):
            client.place_order(7, object(), object())

    def test_callbacks_reach_the_inbox_under_both_error_signatures(self):
        for new_sig in (False, True):
            with self.subTest(new_error_signature=new_sig):
                client, inbox = self.build(new_sig)
                w = client._wrapper
                w.nextValidId(7)
                if new_sig:
                    w.error(-1, 1_800_000_000_000, 1100, "lost", "")
                else:
                    w.error(-1, 1100, "lost", "")
                w.currentTime(5)
                w.position("A", "c", 1, 2.0)
                w.positionEnd()
                w.accountSummary(3, "A", "NetLiquidation", "1", "USD")
                w.accountSummaryEnd(3)
                w.openOrder(7, "c", "o", "s")
                w.openOrderEnd()
                w.orderStatus(7, "Submitted", 0, 1, 0.0, 9, 0, 0.0, 11, "", 0.0)
                w.execDetails(4, "c", "e")
                w.execDetailsEnd(4)
                w.commissionReport("r")
                w.commissionAndFeesReport("r2")
                w.connectionClosed()
                names = [e.name for e in inbox.events()]
                self.assertEqual(names, [
                    "next_valid_id", "error", "current_time", "position", "position_end",
                    "account_summary", "account_summary_end", "open_order", "open_order_end",
                    "order_status", "exec_details", "exec_details_end", "commission_report",
                    "commission_report", "connection_closed"])


if __name__ == "__main__":
    unittest.main()
