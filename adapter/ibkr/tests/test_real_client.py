"""The ibapi-backed client: importable without ibapi, and a thin forwarder when it is present.

ibapi is not installed in CI (ADR 0025 pins and vendors it; until then it is
absent). A stand-in ibapi package, injected into sys.modules, checks that the
forwarding is wired to the adapter's callback names, including both
signatures IBKR has shipped for EWrapper.error.
"""
import importlib
import sys
import threading
import time
import types
import unittest
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
adapter_dir = str(Path(__file__).resolve().parents[1])
for p in (tests_dir, adapter_dir):
    if p not in sys.path:
        sys.path.insert(0, p)

from tws import Inbox
from datetime import date
from decimal import Decimal
from test_connection import config
from relay import HandoverUnconfirmed
from sender import LiveSendingDisabled, OrderRequest, SendRefused, request_shape

PAPER = "DU0000001"


def paper_request(**overrides):
    values = dict(client_order_id="tw-paper-1", account_id=PAPER, con_id=756733, symbol="SPY", action="BUY",
                  quantity=Decimal(9), order_type="LMT", limit_price=Decimal("401.50"), tif="DAY",
                  session=date(2026, 10, 16))
    values.update(overrides)
    return OrderRequest(**values)


def paper_config(**overrides):
    values = dict(mode="paper", account_id=PAPER, paper_sending_enabled=True)
    values.update(overrides)
    return config(**values)


class _Conn:
    """ibapi.connection.Connection: its own socket, which IB Gateway can close without telling EClient."""

    def __init__(self):
        self.socket = object()

    def isConnected(self):
        return self.socket is not None


class _EClient:
    # Called as placeOrder(orderId, contract, order) runs, so a test can close
    # the socket underneath it (ibapi's sendMsg then returns 0, sending nothing).
    on_place = None
    # Set to a threading.Event to make connect block until it is set, as a
    # stalled socket connect or handshake does.
    connect_gate = None
    # Called once a connect stops blocking, so a test can move its clock then.
    after_connect = None
    # Set to a threading.Event to make the reader ignore its socket closing
    # until the event is set, as a reader stuck in a callback does.
    run_gate = None

    def __init__(self, wrapper):
        self.wrapper = wrapper
        self.requests = []
        self.conn = None
        self._stopped = threading.Event()

    def connect(self, host, port, clientId):
        self.requests.append(("connect", host, port, clientId))
        if _EClient.connect_gate is not None:
            _EClient.connect_gate.wait(5)
        if _EClient.after_connect is not None:
            _EClient.after_connect()
        # The socket is open now, whatever happened while connect was blocked.
        self.conn = _Conn()
        self._stopped.clear()

    def run(self):
        # The real reader loops until the socket closes.
        if _EClient.run_gate is not None:
            _EClient.run_gate.wait(5)
            return
        self._stopped.wait(5)

    def isConnected(self):
        return not self._stopped.is_set()

    def disconnect(self):
        self.requests.append(("disconnect",))
        if self.conn is not None:
            self.conn.socket = None
        self.conn = None
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

    def placeOrder(self, orderId, contract, order):
        self.requests.append(("placeOrder", orderId, contract, order))
        if _EClient.on_place is not None:
            _EClient.on_place(orderId, contract, order)


# ibapi marks an unset price with sys.float_info.max (ibapi.const.UNSET_DOUBLE).
UNSET_DOUBLE = sys.float_info.max


class _Contract:
    """ibapi.contract.Contract's attributes the adapter names, at ibapi's defaults."""

    def __init__(self):
        self.conId = 0
        self.symbol = ""
        self.secType = ""
        self.currency = ""
        self.exchange = ""
        self.primaryExchange = ""


class _Order:
    """ibapi.order.Order's attributes the adapter names, at ibapi's defaults, and some it leaves alone."""

    def __init__(self):
        self.orderId = 0
        self.clientId = 0
        self.permId = 0
        self.action = ""
        self.totalQuantity = None
        self.orderType = ""
        self.lmtPrice = UNSET_DOUBLE
        self.auxPrice = UNSET_DOUBLE
        self.tif = ""
        self.account = ""
        self.orderRef = ""
        self.transmit = True
        self.parentId = 0
        self.ocaGroup = ""
        self.outsideRth = False
        self.hidden = False
        self.allOrNone = False
        self.goodAfterTime = ""
        self.goodTillDate = ""
        self.algoStrategy = ""
        self.trailStopPrice = UNSET_DOUBLE
        self.whatIf = False
        self.conditions = []
        self.includeOvernight = False
        self.usePriceMgmtAlgo = None
        self.displaySize = 0


def _install_stub(new_error_signature, order_class=_Order, contract_class=_Contract):
    pkg = types.ModuleType("ibapi")
    client = types.ModuleType("ibapi.client")
    wrapper = types.ModuleType("ibapi.wrapper")
    execution = types.ModuleType("ibapi.execution")
    contract_module = types.ModuleType("ibapi.contract")
    order_module = types.ModuleType("ibapi.order")
    contract_module.Contract = contract_class
    order_module.Order = order_class

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
                        "ibapi.execution": execution, "ibapi.contract": contract_module,
                        "ibapi.order": order_module})


def _uninstall_stub():
    for name in ("ibapi", "ibapi.client", "ibapi.wrapper", "ibapi.execution", "ibapi.contract", "ibapi.order"):
        sys.modules.pop(name, None)


class Clock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now


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
        _EClient.after_connect = None
        if _EClient.run_gate is not None:
            _EClient.run_gate.set()
        _EClient.run_gate = None
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

    def test_a_reader_that_outlives_disconnect_is_kept_and_blocks_every_retry(self):
        real_client = self.module()
        from connection import ConnectFailed
        real_client.READER_STOP_TIMEOUT = 0.01
        _EClient.run_gate = threading.Event()
        client = real_client.IbapiClient(start_reader=True)
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        stuck, old_eclient = client._reader, client._client
        self.assertFalse(client.disconnect())
        self.assertIs(client._reader, stuck)
        for _ in range(2):  # each retry re-checks the same live reader
            with self.assertRaises(ConnectFailed):
                client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
            self.assertIs(client._client, old_eclient)  # no new EClient was opened
            self.assertIs(client._reader, stuck)
        _EClient.run_gate.set()
        stuck.join(1.0)
        _EClient.run_gate = None
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        self.assertIsNot(client._client, old_eclient)
        self.assertIsNot(client._reader, stuck)
        self.assertTrue(client.disconnect())
        self.assertIsNone(client._reader)

    def test_waiting_for_a_stuck_reader_spends_the_connect_timeout_not_more(self):
        real_client = self.module()
        from connection import ConnectFailed
        self.assertEqual(real_client.READER_STOP_TIMEOUT, 5.0)
        _EClient.run_gate = threading.Event()
        client = real_client.IbapiClient(start_reader=True)
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        started = time.monotonic()
        with self.assertRaises(ConnectFailed):
            client.connect("127.0.0.1", 4002, 11, Recorder(), 0.05)
        self.assertLess(time.monotonic() - started, 2.0)  # not the 5s reader wait

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

    def _abandon_one_attempt(self, clock):
        real_client = self.module()
        from connection import ConnectFailed
        gate = threading.Event()
        _EClient.connect_gate = gate
        client = real_client.IbapiClient(start_reader=False, monotonic=clock)
        with self.assertRaises(ConnectFailed):
            client.connect("127.0.0.1", 4002, 11, Recorder(), 0.05)
        return client, gate, ConnectFailed

    def test_waiting_for_an_abandoned_attempt_spends_the_same_timeout(self):
        # The abandoned opener finishes just before the deadline; the new
        # attempt may only have what is left of that one timeout.
        clock = Clock()
        client, gate, ConnectFailed = self._abandon_one_attempt(clock)
        abandoned = client._client

        def late_finish():
            clock.now += 0.99
            _EClient.after_connect = None
        _EClient.after_connect = late_finish
        _EClient.connect_gate = threading.Event()  # the new attempt stalls too
        threading.Timer(0.02, gate.set).start()
        started = time.monotonic()
        try:
            with self.assertRaises(ConnectFailed):
                client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
            self.assertLess(time.monotonic() - started, 0.5)  # not another full second
            self.assertIsNot(client._client, abandoned)
        finally:
            _EClient.connect_gate.set()

    def test_no_attempt_starts_when_the_wait_used_the_whole_timeout(self):
        clock = Clock()
        client, gate, ConnectFailed = self._abandon_one_attempt(clock)
        abandoned = client._client

        def finish_at_the_deadline():
            clock.now += 1.0
            _EClient.after_connect = None
        _EClient.after_connect = finish_at_the_deadline
        threading.Timer(0.02, gate.set).start()
        with self.assertRaises(ConnectFailed):
            client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        self.assertIs(client._client, abandoned)  # no new EClient was opened
        self.assertFalse(abandoned.isConnected())

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

    def test_without_a_configuration_the_real_client_never_places_an_order(self):
        client, _ = self.build(False)
        client._wrapper.managedAccounts("DU0000001")
        self.assertFalse(client.can_place_orders())
        with self.assertRaises(LiveSendingDisabled):
            client.place_order(7, *request_shape(paper_request()))
        self.assertNotIn("placeOrder", [r[0] for r in client._client.requests])

    def test_callbacks_reach_the_inbox_under_both_error_signatures(self):
        for new_sig in (False, True):
            with self.subTest(new_error_signature=new_sig):
                client, inbox = self.build(new_sig)
                w = client._wrapper
                w.nextValidId(7)
                w.managedAccounts("DU0000001,DU0000002")
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
                    "next_valid_id", "managed_accounts", "error", "current_time", "position", "position_end",
                    "account_summary", "account_summary_end", "open_order", "open_order_end",
                    "order_status", "exec_details", "exec_details_end", "commission_report",
                    "commission_report", "connection_closed"])


class OrderMappingTests(unittest.TestCase):
    """The adapter's request shape (sender.request_shape) onto ibapi's Contract and Order, field by field."""

    def tearDown(self):
        _uninstall_stub()
        import real_client
        importlib.reload(real_client)

    def module(self, order_class=_Order):
        _install_stub(False, order_class)
        import real_client
        return importlib.reload(real_client)

    def test_the_contract_is_exactly_the_request_contract(self):
        real_client = self.module()
        contract, _ = request_shape(paper_request())
        built = real_client.ibapi_contract(contract, _Contract)
        self.assertIsInstance(built, _Contract)
        self.assertEqual((built.conId, built.symbol, built.secType, built.currency, built.exchange),
                         (756733, "SPY", "STK", "USD", "SMART"))
        self.assertEqual(built.primaryExchange, "", "the adapter names the contract by its ID, nothing else")

    def test_the_order_is_a_whole_share_day_limit_order_for_the_account_tagged_with_the_client_order_id(self):
        real_client = self.module()
        _, order = request_shape(paper_request())
        built = real_client.ibapi_order(order, _Order)
        self.assertIsInstance(built, _Order)
        self.assertEqual(built.orderRef, "tw-paper-1")
        self.assertEqual(built.account, PAPER)
        self.assertEqual(built.action, "BUY")
        self.assertEqual(built.totalQuantity, Decimal(9))
        self.assertIsInstance(built.totalQuantity, Decimal)
        self.assertEqual(built.orderType, "LMT")
        self.assertEqual(built.lmtPrice, 401.5)
        self.assertIsInstance(built.lmtPrice, float)
        self.assertEqual(built.tif, "DAY")
        self.assertIs(built.transmit, True)

    def test_every_order_field_that_changes_what_is_sent_is_set_never_left_to_ibapi(self):
        # tws.TWSApi promises these; each is set to its plain value whatever
        # the installed ibapi defaults it to.
        real_client = self.module()

        class Odd(_Order):
            """An ibapi whose defaults are all the wrong way round."""

            def __init__(self):
                super().__init__()
                self.outsideRth = True
                self.parentId = 12
                self.ocaGroup = "group"
                self.conditions = ["a condition"]
                self.whatIf = True
                self.auxPrice = 1.0
                self.hidden = True
                self.allOrNone = True
                self.goodAfterTime = "20261016 09:30:00"
                self.goodTillDate = "20261016 16:00:00"
                self.algoStrategy = "Adaptive"
                self.trailStopPrice = 2.0
                self.includeOvernight = True
                self.usePriceMgmtAlgo = True
                self.transmit = False
        built = real_client.ibapi_order(request_shape(paper_request())[1], Odd)
        expected = {"outsideRth": False, "parentId": 0, "ocaGroup": "", "conditions": [], "whatIf": False,
                    "auxPrice": UNSET_DOUBLE, "hidden": False, "allOrNone": False, "goodAfterTime": "",
                    "goodTillDate": "", "algoStrategy": "", "trailStopPrice": UNSET_DOUBLE,
                    "includeOvernight": False, "usePriceMgmtAlgo": False, "transmit": True}
        self.assertEqual({k: getattr(built, k) for k in expected}, expected)
        self.assertIs(built.usePriceMgmtAlgo, False, "explicitly off, not ibapi's None (the account's setting)")
        self.assertEqual(dict(real_client.ORDER_FIELDS_SET), {k: v for k, v in expected.items() if k != "transmit"})
        self.assertEqual(real_client.ORDER_REQUEST_FIELDS,
                         ("orderRef", "account", "action", "totalQuantity", "orderType", "lmtPrice", "tif",
                          "transmit"))
        self.assertEqual(real_client.CONTRACT_REQUEST_FIELDS, ("conId", "symbol", "secType", "currency", "exchange"))

    def test_the_defaults_left_to_ibapi_are_listed_for_the_paper_account_test(self):
        # Every other Order field keeps ibapi's default; the paper-account
        # test confirms from IBKR's own view of the order that these are
        # plain: no iceberg, no minimum, no discretion, no sweep or block.
        real_client = self.module()
        self.assertEqual(real_client.DEFAULTS_TO_CONFIRM,
                         ("displaySize", "minQty", "percentOffset", "discretionaryAmt", "sweepToFill", "blockOrder",
                          "notHeld", "eTradeOnly", "firmQuoteOnly", "cashQty"))
        self.assertFalse(set(real_client.DEFAULTS_TO_CONFIRM) & set(dict(real_client.ORDER_FIELDS_SET)))

    def test_an_ibapi_without_a_field_the_adapter_sets_is_refused_rather_than_sent_without_it(self):
        real_client = self.module()

        class NoConditions(_Order):
            def __init__(self):
                super().__init__()
                del self.conditions
        with self.assertRaises(real_client.OrderShapeRefused) as caught:
            real_client.ibapi_order(request_shape(paper_request())[1], NoConditions)
        self.assertIn("conditions", str(caught.exception))

    def test_an_ibapi_lacking_a_field_the_adapter_sets_refuses_at_construction_before_any_order_is_marked(self):
        # The relay marks an order "about to send" before place_order builds
        # ibapi's objects, so a missing field must refuse before then.
        class NoOvernight(_Order):
            def __init__(self):
                super().__init__()
                del self.includeOvernight

        class NoConditions(_Order):
            def __init__(self):
                super().__init__()
                del self.conditions

        class NoExchange(_Contract):
            def __init__(self):
                super().__init__()
                del self.exchange

        class NoOrderRef(_Order):
            def __init__(self):
                super().__init__()
                del self.orderRef
        for order_class, contract_class, name in ((NoOvernight, _Contract, "includeOvernight"),
                                                  (NoConditions, _Contract, "conditions"),
                                                  (NoOrderRef, _Contract, "orderRef"),
                                                  (_Order, NoExchange, "exchange")):
            with self.subTest(missing=name):
                _install_stub(False, order_class, contract_class)
                import real_client
                real_client = importlib.reload(real_client)
                with self.assertRaises(real_client.IbapiMismatch) as caught:
                    real_client.IbapiClient(start_reader=False, config=paper_config())
                self.assertIn(name, str(caught.exception))
        _install_stub(False)
        import real_client
        real_client = importlib.reload(real_client)
        real_client.IbapiClient(start_reader=False, config=paper_config())

    def test_a_request_not_in_the_adapters_shape_is_refused(self):
        real_client = self.module()
        contract, order = request_shape(paper_request())
        bad_orders = [
            dict(orderType="MKT"), dict(tif="GTC"), dict(transmit=False), dict(totalQuantity=Decimal("9.5")),
            dict(totalQuantity=Decimal(0)), dict(totalQuantity=9), dict(lmtPrice=0.0), dict(lmtPrice=float("nan")),
            dict(lmtPrice="401.5"), dict(action="SSHORT"), dict(account=""), dict(orderRef=""),
            dict(outsideRth=True),
        ]
        for change in bad_orders:
            with self.subTest(order=change), self.assertRaises(real_client.OrderShapeRefused):
                shaped = types.SimpleNamespace(**dict(vars(order), **change))
                real_client.ibapi_order(shaped, _Order)
        missing = types.SimpleNamespace(**{k: v for k, v in vars(order).items() if k != "tif"})
        with self.assertRaises(real_client.OrderShapeRefused):
            real_client.ibapi_order(missing, _Order)
        for change in (dict(conId=0), dict(conId=True), dict(conId="756733"), dict(secType="OPT"),
                       dict(currency="EUR"), dict(exchange="ARCA"), dict(symbol=""), dict(primaryExchange="ARCA")):
            with self.subTest(contract=change), self.assertRaises(real_client.OrderShapeRefused):
                real_client.ibapi_contract(types.SimpleNamespace(**dict(vars(contract), **change)), _Contract)


class PlacingOrdersTests(unittest.TestCase):
    """can_place_orders: paper mode, a DU account the session's login manages, and the configuration's switch."""

    def tearDown(self):
        _EClient.on_place = None
        _uninstall_stub()
        import real_client
        importlib.reload(real_client)

    def client(self, cfg, accounts="DU0000001,DU0000002"):
        _install_stub(False)
        import real_client
        real_client = importlib.reload(real_client)
        client = real_client.IbapiClient(start_reader=False, config=cfg)
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        if accounts is not None:
            client._wrapper.managedAccounts(accounts)
        return client

    def test_a_paper_configuration_with_sending_switched_on_places_the_order_it_was_asked_for(self):
        client = self.client(paper_config())
        self.assertTrue(client.can_place_orders())
        client.place_order(7, *request_shape(paper_request()))
        placed = [r for r in client._client.requests if r[0] == "placeOrder"]
        self.assertEqual(len(placed), 1)
        _, order_id, contract, order = placed[0]
        self.assertEqual(order_id, 7)
        self.assertIsInstance(contract, _Contract)
        self.assertIsInstance(order, _Order)
        self.assertEqual((contract.conId, order.orderRef, order.account, order.totalQuantity, order.lmtPrice),
                         (756733, "tw-paper-1", PAPER, Decimal(9), 401.5))

    def test_sending_stays_off_without_each_of_mode_account_login_and_switch(self):
        cases = {
            "switch off": (paper_config(paper_sending_enabled=False), "DU0000001"),
            "switch left out": (config(mode="paper", account_id=PAPER), "DU0000001"),
            "dry-run": (config(mode="dry-run", account_id=PAPER), "DU0000001"),
            "login does not manage the account": (paper_config(), "DU0000002"),
            "no managedAccounts yet": (paper_config(), None),
        }
        for name, (cfg, accounts) in cases.items():
            with self.subTest(name):
                client = self.client(cfg, accounts)
                self.assertFalse(client.can_place_orders())
                with self.assertRaises(SendRefused):
                    client.place_order(7, *request_shape(paper_request()))
                self.assertNotIn("placeOrder", [r[0] for r in client._client.requests])
                _uninstall_stub()

    def test_an_account_not_beginning_du_never_places_an_order_whatever_the_configuration_says(self):
        # A configuration refuses a paper mode on a live account; this is the
        # client's own check, against a configuration that got past it.
        cfg = paper_config()._replace(account_id="U0000001")
        client = self.client(cfg, "U0000001")
        self.assertFalse(client.can_place_orders())

    def test_a_request_for_another_account_is_refused(self):
        client = self.client(paper_config())
        with self.assertRaises(SendRefused):
            client.place_order(7, *request_shape(paper_request(account_id="DU0000002")))
        self.assertNotIn("placeOrder", [r[0] for r in client._client.requests])

    def test_the_login_is_read_from_the_current_session_only(self):
        client = self.client(paper_config())
        old = client._wrapper
        client.disconnect()
        client.connect("127.0.0.1", 4002, 11, Recorder(), 1.0)
        self.assertFalse(client.can_place_orders(), "a new session starts with no managed accounts")
        old.managedAccounts("DU0000001")
        self.assertFalse(client.can_place_orders(), "a late callback from the old session does not count")
        client._wrapper.managedAccounts("DU0000002,DU0000001")
        self.assertTrue(client.can_place_orders())
        # IBKR may restate the list; the latest stands.
        client._wrapper.managedAccounts("DU0000002")
        self.assertFalse(client.can_place_orders())

    def test_no_order_is_placed_while_the_link_is_down(self):
        # The relay has marked the order "about to send" by now, so a link
        # found down here leaves the hand-over for a person, never "sent".
        client = self.client(paper_config())
        client.disconnect()
        self.assertFalse(client.is_connected())
        with self.assertRaises(HandoverUnconfirmed):
            client.place_order(7, *request_shape(paper_request()))
        self.assertNotIn("placeOrder", [r[0] for r in client._client.requests])

    def test_the_sending_switch_is_the_configuration_and_the_login_not_the_link(self):
        # The link's state is the connection's to report (NotConnected, which
        # holds the input), never a refusal of the switch.
        client = self.client(paper_config())
        client._client.conn.socket = None
        self.assertFalse(client.is_connected())
        self.assertTrue(client.can_place_orders())

    def test_a_socket_ibapi_closed_underneath_is_a_link_down_whatever_eclient_says(self):
        # IB Gateway closing the socket (its nightly restart, a crash) leaves
        # EClient's own state CONNECTED; only ibapi's connection knows.
        client = self.client(paper_config())
        client._client.conn.socket = None
        self.assertTrue(client._client.isConnected())
        self.assertFalse(client.is_connected())
        with self.assertRaises(HandoverUnconfirmed):
            client.place_order(7, *request_shape(paper_request()))
        self.assertNotIn("placeOrder", [r[0] for r in client._client.requests])
        client._client.conn = None
        self.assertFalse(client.is_connected())

    def test_an_order_whose_socket_closed_during_placeorder_is_unconfirmed_never_taken_as_sent(self):
        client = self.client(paper_config())

        def gateway_closes(order_id, contract, order):
            # ibapi's sendMsg returns 0 on a closed socket and raises nothing.
            client._client.conn.socket = None
        _EClient.on_place = gateway_closes
        with self.assertRaises(HandoverUnconfirmed) as caught:
            client.place_order(7, *request_shape(paper_request()))
        self.assertIn("tw-paper-1", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
