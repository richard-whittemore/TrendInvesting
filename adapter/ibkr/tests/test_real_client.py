"""The ibapi-backed client: importable without ibapi, and a thin forwarder when it is present.

ibapi is not installed in CI (ADR 0025 pins and vendors it; until then it is
absent). A stand-in ibapi package, injected into sys.modules, checks that the
forwarding is wired to the adapter's callback names, including both
signatures IBKR has shipped for EWrapper.error.
"""
import importlib
import sys
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
    def __init__(self, wrapper):
        self.wrapper = wrapper
        self.requests = []

    def connect(self, host, port, clientId):
        self.requests.append(("connect", host, port, clientId))

    def run(self):
        pass

    def isConnected(self):
        return True

    def disconnect(self):
        self.requests.append(("disconnect",))

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
        _uninstall_stub()
        import real_client
        importlib.reload(real_client)

    def build(self, new_error_signature):
        _install_stub(new_error_signature)
        import real_client
        real_client = importlib.reload(real_client)
        self.assertTrue(real_client.IBAPI_AVAILABLE)
        client = real_client.IbapiClient(start_reader=False)
        inbox = Recorder()
        client.connect("127.0.0.1", 4002, 11, inbox)
        return client, inbox

    def test_requests_reach_eclient(self):
        client, _ = self.build(False)
        client.req_current_time()
        client.req_positions()
        client.cancel_positions()
        client.req_account_summary(3, "All", "NetLiquidation")
        client.cancel_account_summary(3)
        client.req_all_open_orders()
        client.req_executions(4, "DU0000001")
        client.disconnect()
        self.assertEqual(client._client.requests, [
            ("connect", "127.0.0.1", 4002, 11), ("reqCurrentTime",), ("reqPositions",),
            ("cancelPositions",), ("reqAccountSummary", 3, "All", "NetLiquidation"),
            ("cancelAccountSummary", 3), ("reqAllOpenOrders",), ("reqExecutions", 4, "DU0000001"),
            ("disconnect",)])
        self.assertTrue(client.is_connected())

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
