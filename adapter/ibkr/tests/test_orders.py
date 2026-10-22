"""Turning IBKR's order status, open-order and execution callbacks into plain records."""
import sys
import unittest
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from fake_tws import ACCOUNT, OTHER_ACCOUNT, FakeTWS, contract, order, execution
from test_connection import Harness
from connection import REQUEST_ID_BASE, NotConnected
from orders import (BrokerOrdersIncomplete, OrderTracker, OrderStatusReport, OpenOrderRecord, ExecutionReport,
                    CommissionReport, OpenOrdersUnsettled, UnrecognisedOrderReport, collect_broker_orders,
                    collect_settled_broker_orders, match_on_reconnect, parse_execution_time)

STATE = SimpleNamespace(status="Submitted")


def status(cb, order_id, ibkr_status, filled, remaining, avg=0.0, last=0.0, perm_id=9001,
           client_id=11):
    cb.order_status(order_id, ibkr_status, Decimal(filled), Decimal(remaining), avg, perm_id,
                    0, last, client_id, "", 0.0)


class Tracked:
    def __init__(self, sink=None):
        self.h = Harness()
        self.h.conn.connect()
        self.reports = []
        self.tracker = OrderTracker(ACCOUNT, 11, sink or self.reports.append)
        self.h.conn.subscribe(self.tracker.on_event)
        self.cb = self.h.fake.callbacks

    def drain(self):
        while self.h.conn.pump(0.001) is not None:
            pass

    def of(self, kind):
        return [r for r in self.reports if isinstance(r, kind)]


class OrderStatusTests(unittest.TestCase):
    def test_partial_fill_then_fill(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(order_ref="tag-1", total_quantity=Decimal("10")), STATE)
        status(t.cb, 7, "Submitted", "0", "10")
        status(t.cb, 7, "Submitted", "4", "6", avg=101.25, last=101.25)
        t.cb.exec_details(-1, contract(), execution(shares=Decimal("4"), cum_qty=Decimal("4")))
        status(t.cb, 7, "Filled", "10", "0", avg=101.3, last=101.333)
        t.drain()
        statuses = t.of(OrderStatusReport)
        self.assertEqual([(s.lifecycle, s.filled, s.remaining) for s in statuses],
                         [("submitted", Decimal("0"), Decimal("10")),
                          ("submitted", Decimal("4"), Decimal("6")),
                          (None, Decimal("10"), Decimal("0"))])
        self.assertTrue(statuses[1].partially_filled)
        self.assertFalse(statuses[2].partially_filled)
        self.assertEqual(statuses[2].ibkr_status, "Filled")
        self.assertTrue(all(s.client_order_id == "tag-1" for s in statuses))
        self.assertTrue(all(s.broker_order_id == 7 for s in statuses))
        self.assertEqual(statuses[2].avg_fill_price, Decimal("101.3"))
        fills = t.of(ExecutionReport)
        self.assertEqual(len(fills), 1)
        self.assertEqual((fills[0].client_order_id, fills[0].shares, fills[0].price, fills[0].side),
                         ("tag-1", Decimal("4"), Decimal("101.25"), "BOT"))
        self.assertEqual(fills[0].occurred_at, datetime(2026, 10, 16, 13, 30, 1, tzinfo=timezone.utc))
        self.assertEqual(fills[0].time, "20261016 09:30:01 US/Eastern")

    def test_open_order_record_carries_ibkr_ids_and_the_tag(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(order_type="LMT", lmt_price=100.5,
                                             total_quantity=Decimal("3")), STATE)
        t.drain()
        rec = t.of(OpenOrderRecord)[0]
        self.assertEqual((rec.broker_order_id, rec.perm_id, rec.client_order_id, rec.con_id,
                          rec.action, rec.quantity, rec.order_type, rec.limit_price),
                         (7, 9001, "tag-1", 756733, "BUY", Decimal("3"), "LMT", Decimal("100.5")))

    def test_cancel_pending_then_cancelled(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(), STATE)
        status(t.cb, 7, "Submitted", "0", "10")
        status(t.cb, 7, "PendingCancel", "0", "10")
        status(t.cb, 7, "Cancelled", "0", "10")
        t.drain()
        self.assertEqual([s.lifecycle for s in t.of(OrderStatusReport)],
                         ["submitted", "cancel-pending", "canceled"])

    def test_rejection_is_invalid_with_ibkr_reason_and_inactive_is_not_repeated(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(), STATE)
        t.cb.error(7, 201, "Order rejected - reason:synthetic insufficient funds")
        status(t.cb, 7, "Inactive", "0", "10")
        t.drain()
        statuses = t.of(OrderStatusReport)
        self.assertEqual(len(statuses), 1)
        self.assertEqual(statuses[0].lifecycle, "invalid")
        self.assertEqual(statuses[0].ibkr_code, 201)
        self.assertIn("insufficient funds", statuses[0].reason)
        self.assertEqual(statuses[0].client_order_id, "tag-1")

    def test_an_order_level_refusal_of_an_order_this_client_placed_is_invalid_with_ibkr_reason(self):
        # IBKR answers an order it will not take (the Read-Only API, code 321)
        # with an error under the order's ID and no openOrder: the order never
        # worked, and the engine is told so.
        t = Tracked()
        t.tracker.placed(7)
        t.cb.error(7, 321, "Error validating request.-'bW' : cause - The API interface is currently in Read-Only mode.")
        status(t.cb, 7, "Inactive", "0", "10")
        t.drain()
        statuses = t.of(OrderStatusReport)
        self.assertEqual([(s.lifecycle, s.ibkr_code, s.broker_order_id) for s in statuses], [("invalid", 321, 7)])
        self.assertIn("Read-Only", statuses[0].reason)

    def test_an_order_level_code_on_anything_but_an_order_placed_and_not_yet_shown_is_no_order_report(self):
        # Code 321 also answers other requests, whose IDs may equal an order's.
        t = Tracked()
        t.cb.error(7, 321, "Error validating request.-'bW' : cause - synthetic")
        t.tracker.placed(8)
        t.cb.open_order(8, contract(), order(order_ref="tag-8"), STATE)
        status(t.cb, 8, "Submitted", "0", "10")
        t.cb.error(8, 321, "Error validating request.-'bW' : cause - synthetic")
        t.drain()
        self.assertEqual([s.lifecycle for s in t.of(OrderStatusReport)], ["submitted"])

    def test_inactive_without_a_rejection_fails_closed(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(), STATE)
        status(t.cb, 7, "Inactive", "0", "10")
        with self.assertRaises(UnrecognisedOrderReport):
            t.drain()

    def test_unknown_status_fails_closed(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(), STATE)
        status(t.cb, 7, "Teleported", "0", "10")
        with self.assertRaises(UnrecognisedOrderReport):
            t.drain()

    def test_pending_submit_is_recorded_without_a_lifecycle(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(), STATE)
        status(t.cb, 7, "PendingSubmit", "0", "10")
        status(t.cb, 7, "PreSubmitted", "0", "10")
        t.drain()
        self.assertEqual([s.lifecycle for s in t.of(OrderStatusReport)], [None, "submitted"])

    def test_identical_repeats_are_collapsed(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(), STATE)
        status(t.cb, 7, "Submitted", "0", "10")
        status(t.cb, 7, "Submitted", "0", "10")
        t.drain()
        self.assertEqual(len(t.of(OrderStatusReport)), 1)

    def test_other_accounts_are_ignored(self):
        t = Tracked()
        t.cb.open_order(8, contract(), order(account=OTHER_ACCOUNT), STATE)
        t.cb.exec_details(-1, contract(), execution(account=OTHER_ACCOUNT, order_id=8))
        t.drain()
        self.assertEqual(t.reports, [])

    def test_commission_report(self):
        t = Tracked()
        t.cb.exec_details(-1, contract(), execution(exec_id="x.01"))
        t.cb.commission_report(SimpleNamespace(execId="x.01", commission=1.0, currency="USD"))
        t.cb.commission_report(SimpleNamespace(execId="foreign.01", commission=2.0,
                                               currency="USD"))
        t.drain()
        self.assertEqual(t.of(CommissionReport),
                         [CommissionReport("x.01", Decimal("1.0"), "USD")])

    def test_redelivered_commission_report_is_reported_once(self):
        # Reconnect matching re-requests executions, and IBKR then resends each
        # execution's commission report.
        t = Tracked()
        t.cb.exec_details(-1, contract(), execution(exec_id="x.01"))
        t.cb.commission_report(SimpleNamespace(execId="x.01", commission=1.0, currency="USD"))
        t.cb.exec_details(4, contract(), execution(exec_id="x.01"))
        t.cb.commission_report(SimpleNamespace(execId="x.01", commission=1.0, currency="USD"))
        t.drain()
        self.assertEqual(t.of(CommissionReport),
                         [CommissionReport("x.01", Decimal("1.0"), "USD")])

    def test_a_commission_report_whose_record_failed_is_recorded_on_redelivery(self):
        failures = [RuntimeError("journal write failed")]
        reports = []

        def sink(record):
            if isinstance(record, CommissionReport) and failures:
                raise failures.pop()
            reports.append(record)
        t = Tracked(sink)
        t.cb.exec_details(-1, contract(), execution(exec_id="x.01"))
        t.cb.commission_report(SimpleNamespace(execId="x.01", commission=1.0, currency="USD"))
        with self.assertRaises(RuntimeError):
            t.drain()
        t.cb.commission_report(SimpleNamespace(execId="x.01", commission=1.0, currency="USD"))
        t.drain()
        self.assertEqual([r for r in reports if isinstance(r, CommissionReport)],
                         [CommissionReport("x.01", Decimal("1.0"), "USD")])

    def test_redelivered_execution_is_reported_once(self):
        t = Tracked()
        t.cb.exec_details(-1, contract(), execution())
        t.cb.exec_details(4, contract(), execution())
        t.drain()
        self.assertEqual(len(t.of(ExecutionReport)), 1)

    def test_another_clients_order_with_the_same_id_does_not_hide_this_engines_order(self):
        # IBKR's orderId is unique only within one client ID; reqAllOpenOrders
        # also reports other clients' orders, which may reuse it.
        t = Tracked()
        t.cb.open_order(7, contract(), order(account=OTHER_ACCOUNT, client_id=12, perm_id=9100),
                        STATE)
        status(t.cb, 7, "Submitted", "0", "10", perm_id=9100, client_id=12)
        t.cb.open_order(7, contract(), order(order_ref="tag-1"), STATE)
        status(t.cb, 7, "Submitted", "0", "10")
        t.drain()
        statuses = t.of(OrderStatusReport)
        self.assertEqual([(s.client_id, s.perm_id, s.client_order_id) for s in statuses],
                         [(11, 9001, "tag-1")])

    def test_same_account_orders_from_two_clients_keep_their_own_tags(self):
        t = Tracked()
        t.cb.open_order(7, contract(), order(order_ref="tag-1"), STATE)
        t.cb.open_order(7, contract(), order(order_ref="tag-9", client_id=12, perm_id=9002), STATE)
        status(t.cb, 7, "Submitted", "0", "10")
        status(t.cb, 7, "Submitted", "0", "10", perm_id=9002, client_id=12)
        t.drain()
        statuses = t.of(OrderStatusReport)
        self.assertEqual([(s.client_id, s.client_order_id) for s in statuses],
                         [(11, "tag-1"), (12, "tag-9")])

    def test_a_rejection_belongs_to_this_engines_order_only(self):
        # error(reqId=orderId) reaches only the client that placed the order,
        # so a 201 never explains another client's Inactive order.
        t = Tracked()
        t.cb.open_order(7, contract(), order(), STATE)
        t.cb.error(7, 201, "Order rejected - reason:synthetic")
        t.cb.open_order(7, contract(), order(client_id=12, perm_id=9002), STATE)
        status(t.cb, 7, "Inactive", "0", "10", perm_id=9002, client_id=12)
        with self.assertRaises(UnrecognisedOrderReport):
            t.drain()

    def test_manual_orders_without_an_order_id_are_told_apart_by_perm_id(self):
        t = Tracked()
        t.cb.open_order(0, contract(), order(order_ref="", client_id=0, perm_id=9101), STATE)
        t.cb.open_order(0, contract(), order(order_ref="", client_id=0, perm_id=9102), STATE)
        status(t.cb, 0, "Submitted", "0", "10", perm_id=9101, client_id=0)
        status(t.cb, 0, "Submitted", "0", "10", perm_id=9102, client_id=0)
        t.drain()
        self.assertEqual([s.perm_id for s in t.of(OrderStatusReport)], [9101, 9102])

    def test_an_order_with_no_identity_fails_closed(self):
        t = Tracked()
        status(t.cb, 0, "Submitted", "0", "10", perm_id=0, client_id=0)
        with self.assertRaises(UnrecognisedOrderReport):
            t.drain()

    def test_a_status_waits_until_an_open_order_says_whose_account_it_is(self):
        # orderStatus carries no account. Another account's status that
        # arrives before its openOrder must not be recorded, and an
        # unclassifiable one must not stop this account's reader.
        t = Tracked()
        status(t.cb, 8, "Submitted", "0", "10", perm_id=9100, client_id=12)
        status(t.cb, 9, "Teleported", "0", "10", perm_id=9200, client_id=12)
        status(t.cb, 7, "Submitted", "0", "10")
        t.drain()
        self.assertEqual(t.reports, [])
        t.cb.open_order(8, contract(), order(account=OTHER_ACCOUNT, client_id=12, perm_id=9100),
                        STATE)
        t.cb.open_order(9, contract(), order(account=OTHER_ACCOUNT, client_id=12, perm_id=9200),
                        STATE)
        t.cb.open_order(7, contract(), order(order_ref="tag-1"), STATE)
        t.drain()
        self.assertEqual([type(r) for r in t.reports], [OpenOrderRecord, OrderStatusReport])
        report = t.reports[1]
        self.assertEqual((report.client_id, report.broker_order_id, report.client_order_id,
                          report.lifecycle), (11, 7, "tag-1", "submitted"))

    def test_a_rejection_shows_the_order_is_this_engines(self):
        # error(reqId=orderId) reaches only the client that placed the order.
        t = Tracked()
        t.cb.error(7, 201, "Order rejected - reason:synthetic")
        status(t.cb, 7, "Inactive", "0", "10")
        t.drain()
        self.assertEqual([s.lifecycle for s in t.of(OrderStatusReport)], ["invalid"])

    def test_a_status_the_sink_failed_to_record_is_recorded_on_redelivery(self):
        failures = [RuntimeError("synthetic journal write failure")]
        reports = []

        def sink(record):
            if isinstance(record, OrderStatusReport) and failures:
                raise failures.pop()
            reports.append(record)
        t = Tracked(sink)
        t.cb.open_order(7, contract(), order(), STATE)
        status(t.cb, 7, "Submitted", "0", "10")
        with self.assertRaises(RuntimeError):
            t.drain()
        status(t.cb, 7, "Submitted", "0", "10")
        t.drain()
        self.assertEqual(len([r for r in reports if isinstance(r, OrderStatusReport)]), 1)

    def test_a_held_status_the_sink_failed_to_record_stays_ahead_of_later_ones(self):
        # The owner is known once the openOrder arrives, but a held status
        # that failed to record is still older than anything after it.
        failures = [RuntimeError("synthetic journal write failure")]
        reports = []

        def sink(record):
            if isinstance(record, OrderStatusReport) and failures:
                raise failures.pop()
            reports.append(record)
        t = Tracked(sink)
        status(t.cb, 7, "Submitted", "0", "10")
        t.cb.open_order(7, contract(), order(), STATE)
        with self.assertRaises(RuntimeError):
            t.drain()
        status(t.cb, 7, "Submitted", "0", "10")
        status(t.cb, 7, "Cancelled", "0", "10")
        t.cb.open_order(7, contract(), order(), STATE)
        t.drain()
        self.assertEqual([r.lifecycle for r in reports if isinstance(r, OrderStatusReport)],
                         ["submitted", "canceled"])

    def test_an_execution_the_sink_failed_to_record_is_recorded_on_redelivery(self):
        failures = [RuntimeError("synthetic journal write failure")]
        reports = []

        def sink(record):
            if isinstance(record, ExecutionReport) and failures:
                raise failures.pop()
            reports.append(record)
        t = Tracked(sink)
        t.cb.exec_details(-1, contract(), execution())
        with self.assertRaises(RuntimeError):
            t.drain()
        t.cb.exec_details(4, contract(), execution())
        t.drain()
        self.assertEqual(len([r for r in reports if isinstance(r, ExecutionReport)]), 1)

    def test_an_execution_that_cannot_be_read_is_not_marked_seen(self):
        t = Tracked()
        t.cb.exec_details(-1, contract(), execution(exec_id="x.01", time="20261016 09:30:01"))
        with self.assertRaises(ValueError):
            t.drain()
        t.cb.commission_report(SimpleNamespace(execId="x.01", commission=1.0, currency="USD"))
        t.drain()
        self.assertEqual(t.reports, [])

    def test_execution_time_formats(self):
        self.assertEqual(parse_execution_time("20261016 15:59:59 America/New_York"),
                         datetime(2026, 10, 16, 19, 59, 59, tzinfo=timezone.utc))
        for bad in ("20261016  09:30:01", "yesterday", "20261016 09:30:01 Mars/Olympus"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_execution_time(bad)


class RequestIdTests(unittest.TestCase):
    def test_an_error_on_a_request_is_never_read_as_an_orders_rejection(self):
        # IBKR answers a request's error under its request ID and an order's
        # rejection under its order ID, with codes shared by both (321).
        # Request IDs start far above any order ID, so an error on
        # reqExecutions cannot name an order this client placed.
        h = Harness()
        # A fresh paper account's first valid order ID is 1.
        h.fake.next_order_id = 1
        h.conn.connect()
        reports = []
        tracker = OrderTracker(ACCOUNT, 11, reports.append)
        h.conn.subscribe(tracker.on_event)
        tracker.placed(h.conn.next_order_id)
        h.fake.executions_error = (321, "Error validating request")
        collect_broker_orders(h.conn)
        self.assertEqual([r for r in reports if isinstance(r, OrderStatusReport)], [])
        self.assertGreaterEqual(h.fake.called("req_executions")[0][1], REQUEST_ID_BASE)

    def test_request_ids_start_above_the_order_id_space_and_never_repeat(self):
        h = Harness()
        h.conn.connect()
        ids = [h.conn.new_req_id() for _ in range(3)]
        self.assertEqual(ids, [REQUEST_ID_BASE, REQUEST_ID_BASE + 1, REQUEST_ID_BASE + 2])
        self.assertEqual(REQUEST_ID_BASE, 1 << 30)


class ReconnectMatchTests(unittest.TestCase):
    def test_matches_by_client_order_id_and_never_resends(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = [
            (7, contract(), order(order_ref="tag-1"), STATE),
            (9, contract(), order(order_ref="", perm_id=9003), STATE),
            (12, contract(), order(order_ref="tag-x", account=OTHER_ACCOUNT), STATE),
        ]
        h.fake.executions = [(contract(), execution(order_id=8, perm_id=9002, order_ref="tag-2"))]
        broker = collect_broker_orders(h.conn)
        self.assertEqual([o.broker_order_id for o in broker.open_orders], [7, 9])
        match = match_on_reconnect({"tag-1", "tag-2", "tag-3"}, broker, 11)
        self.assertEqual({k: (r.client_id, r.broker_order_id, r.perm_id)
                          for k, r in match.matched.items()}, {"tag-1": (11, 7, 9001)})
        self.assertEqual(sorted(match.executed), ["tag-2"])
        self.assertEqual(match.missing_at_broker, ("tag-3",))
        self.assertEqual([o.broker_order_id for o in match.unknown_at_broker], [9])
        self.assertFalse(match.clean)
        self.assertEqual(h.fake.called("place_order"), [])
        self.assertEqual(h.fake.called("req_executions")[0][2], ACCOUNT)

    def test_same_id_orders_from_two_clients_are_both_collected(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = [
            (7, contract(), order(order_ref="tag-1"), STATE),
            (7, contract(), order(order_ref="", client_id=12, perm_id=9002), STATE),
            (0, contract(), order(order_ref="", client_id=0, perm_id=9101), STATE),
            (0, contract(), order(order_ref="", client_id=0, perm_id=9102), STATE),
        ]
        broker = collect_broker_orders(h.conn)
        self.assertEqual(sorted(o.perm_id for o in broker.open_orders),
                         [9001, 9002, 9101, 9102])
        match = match_on_reconnect({"tag-1"}, broker, 11)
        self.assertEqual(match.matched["tag-1"].client_id, 11)
        self.assertEqual(len(match.unknown_at_broker), 3)

    def test_another_clients_order_or_execution_with_an_expected_tag_is_not_this_engines(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = [
            (7, contract(), order(order_ref="tag-1", client_id=12, perm_id=9002), STATE)]
        h.fake.executions = [(contract(), execution(order_id=8, perm_id=9003, order_ref="tag-2",
                                                    client_id=12))]
        match = match_on_reconnect({"tag-1", "tag-2"}, collect_broker_orders(h.conn), 11)
        self.assertEqual(match.matched, {})
        self.assertEqual(match.executed, {})
        self.assertEqual(match.missing_at_broker, ("tag-1", "tag-2"))
        self.assertEqual(len(match.unknown_at_broker), 1)
        self.assertEqual(len(match.unknown_executions), 1)
        self.assertFalse(match.clean)

    def test_a_second_order_carrying_an_expected_tag_is_unknown_even_when_executed(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = [(7, contract(), order(order_ref="tag-1"), STATE)]
        h.fake.executions = [
            # Two partial fills of one order: both are that tag's executions.
            (contract(), execution(exec_id="x.01", order_id=8, perm_id=9002, order_ref="tag-2")),
            (contract(), execution(exec_id="x.02", order_id=8, perm_id=9002, order_ref="tag-2")),
            # A second order under this client ID with the same tag.
            (contract(), execution(exec_id="x.03", order_id=10, perm_id=9004, order_ref="tag-2")),
            # An execution of another order carrying the open order's tag.
            (contract(), execution(exec_id="x.04", order_id=11, perm_id=9005, order_ref="tag-1")),
        ]
        match = match_on_reconnect({"tag-1", "tag-2"}, collect_broker_orders(h.conn), 11)
        self.assertEqual(match.matched["tag-1"].broker_order_id, 7)
        self.assertEqual([e.exec_id for e in match.executed["tag-2"]], ["x.01", "x.02"])
        self.assertEqual([e.exec_id for e in match.unknown_executions], ["x.03", "x.04"])
        self.assertEqual(match.missing_at_broker, ())
        self.assertFalse(match.clean)

    def test_clean_when_everything_is_accounted_for(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = [(7, contract(), order(order_ref="tag-1"), STATE)]
        match = match_on_reconnect({"tag-1"}, collect_broker_orders(h.conn), 11)
        self.assertTrue(match.clean)


class EndsEachListOnAnotherEnd(FakeTWS):
    """Each list asked for ends at once on an end that is not its own, and its rows follow that end.

    The first list's is an end nothing asked for; each later list's is the
    end of the list before it, delayed across this request. A list's own
    end comes only behind the next request, so every read is cut short in
    the same way.
    """

    def req_all_open_orders(self):
        self.calls.append(("req_all_open_orders",))
        self.callbacks.open_order_end()
        self._send_open_orders(self.callbacks, self.open_orders)


class OpenOrderLists:
    """The orders IBKR's open-order list shows in these tests, and what a whole read of it holds."""

    def working(self):
        # One order of this engine's, one placed by hand (no order ID), one of another client's.
        return [
            (7, contract(), order(order_ref="tag-1"), STATE),
            (0, contract(), order(order_ref="", client_id=0, perm_id=9101), STATE),
            (5, contract(), order(order_ref="", client_id=12, perm_id=9102), STATE),
        ]

    def assert_whole(self, broker):
        self.assertEqual(sorted(o.perm_id for o in broker.open_orders), [9001, 9101, 9102])
        match = match_on_reconnect({"tag-1"}, broker, 11)
        self.assertEqual(match.missing_at_broker, ())
        self.assertEqual(sorted(match.matched), ["tag-1"])


class WholeOpenOrderListTests(OpenOrderLists, unittest.TestCase):
    """IBKR's open-order list and its end carry no request ID; a read ends only on its own request's end."""

    def test_the_end_gateway_volunteers_as_the_session_starts_does_not_end_the_read(self):
        h = Harness()
        h.fake.open_orders = self.working()
        h.fake.volunteer_open_orders = True
        h.fake.open_order_batch = 1
        h.conn.connect()
        self.assert_whole(collect_broker_orders(h.conn))

    def test_a_list_in_several_batches_is_read_to_its_end_behind_a_stray_end(self):
        h = Harness()
        h.conn.connect()
        h.fake.callbacks.open_order_end()
        h.fake.open_orders = self.working()
        h.fake.open_order_batch = 2
        self.assert_whole(collect_broker_orders(h.conn))

    def test_an_end_nothing_asked_for_between_reads_ends_no_later_read(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = self.working()
        self.assert_whole(collect_broker_orders(h.conn))
        h.fake.callbacks.open_order_end()
        h.fake.open_order_batch = 1
        self.assert_whole(collect_broker_orders(h.conn))

    def test_the_late_end_of_a_read_that_gave_up_ends_that_read_not_the_next(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = self.working()
        h.fake.end_open_orders = False
        with self.assertRaises(BrokerOrdersIncomplete) as caught:
            collect_broker_orders(h.conn)
        self.assertEqual(caught.exception.missing, ("open-orders",))
        # The abandoned list's end arrives late, on the same link.
        h.fake.callbacks.open_order_end()
        h.fake.end_open_orders = True
        h.fake.open_order_batch = 1
        self.assert_whole(collect_broker_orders(h.conn))

    def test_an_order_reported_before_the_list_was_asked_for_is_not_read_as_part_of_it(self):
        h = Harness()
        h.conn.connect()
        # Shown working earlier, and ended since: IBKR's list no longer has it.
        h.fake.callbacks.open_order(8, contract(), order(order_ref="tag-0", perm_id=9008), STATE)
        h.fake.open_orders = self.working()
        self.assert_whole(collect_broker_orders(h.conn))

    def test_a_list_whose_end_never_comes_is_incomplete(self):
        h = Harness()
        h.conn.connect()
        h.fake.callbacks.open_order_end()
        h.fake.open_orders = self.working()
        h.fake.end_open_orders = False
        h.fake.open_order_batch = 1
        with self.assertRaises(BrokerOrdersIncomplete) as caught:
            collect_broker_orders(h.conn)
        self.assertEqual(caught.exception.missing, ("open-orders",))


    def test_connect_takes_the_volunteered_rows_until_gateway_stops_sending_them(self):
        # No end comes after the volunteered rows: connect waits only until
        # Gateway has sent none for a while, and the first list is asked
        # behind every row it volunteered.
        h = Harness()
        h.fake.open_orders = self.working()
        h.fake.volunteer_open_orders = True
        h.fake.volunteer_end = False
        h.conn.connect()
        self.assertTrue(h.conn.connected)
        self.assertEqual(len(h.fake._later), 0, "every volunteered row was taken before connect returned")
        h.fake.open_order_batch = 1
        self.assert_whole(collect_broker_orders(h.conn))

    def test_connect_stops_waiting_for_the_volunteered_end_after_ten_seconds(self):
        h = Harness()
        h.fake.open_orders = [(n, contract(), order(order_ref="tag-{}".format(n), perm_id=9000 + n), STATE)
                              for n in range(1, 11)]
        h.fake.volunteer_open_orders = True

        def three_seconds():
            h.clock.now += 3.0
        h.fake.later_hook = three_seconds
        started = h.clock.now
        h.conn.connect()
        self.assertTrue(h.conn.connected)
        self.assertGreaterEqual(h.clock.now - started, 10.0)
        self.assertLess(h.clock.now - started, 13.0)
        self.assertTrue(h.fake._later, "connect stopped waiting with volunteered rows still to come")

    def test_delayed_rows_of_an_abandoned_list_are_not_read_as_the_next_lists(self):
        # The first list's answer arrives only after its read gave up: an
        # order it shows (cancelled since) is no part of the second list.
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = [(8, contract(), order(order_ref="tag-0", perm_id=9008), STATE)]
        h.fake.hold_open_orders = True
        with self.assertRaises(BrokerOrdersIncomplete):
            collect_broker_orders(h.conn)
        h.fake.hold_open_orders = False
        h.fake.release_held()
        h.fake.open_orders = []
        self.assertEqual(collect_broker_orders(h.conn).open_orders, ())
        self.assertEqual(len(h.fake.called("req_all_open_orders")), 2)

    def test_open_orders_still_arriving_at_ten_seconds_drop_the_link_however_short_the_last_gap(self):
        # A stray end every 1.5 seconds for 9 seconds: at 10 seconds the
        # link has been silent for 1 second, not the 2 a quiet link needs.
        h = Harness(request_timeout=5.0)
        h.fake.volunteer_open_orders = True
        h.conn.connect()
        start = h.clock.now
        strays = [start + 1.5 * n for n in range(1, 7)]
        inbox = h.conn.inbox
        waiting = inbox.get

        def get(timeout):
            if not inbox._queue.empty():
                return waiting(0)
            if strays and strays[0] <= h.clock.now + timeout:
                h.clock.now = max(h.clock.now, strays.pop(0))
                inbox.open_order_end()
                return waiting(0)
            h.clock.now += timeout
            return None
        inbox.get = get
        with self.assertRaises(NotConnected):
            h.conn.resync_open_orders()
        self.assertFalse(h.conn.connected)
        self.assertEqual(h.statuses[-1].reason, "open-orders-never-quiet")
        self.assertLessEqual(h.clock.now - start, 10.0 + 1e-9)

    def test_the_relays_read_after_an_empty_list_that_ended_cleanly_does_not_wait_for_quiet(self):
        # No open order at IBKR: every list ends before any row. With no
        # stray since, the link is level and the next read is asked at once.
        h = Harness()
        h.fake.volunteer_open_orders = True
        h.conn.connect()
        self.assertEqual(collect_broker_orders(h.conn).open_orders, ())
        quiet_pumps = []
        resync = h.conn.resync_open_orders
        h.conn.resync_open_orders = lambda: (quiet_pumps.append(True), resync())[1]
        self.assertEqual(collect_broker_orders(h.conn).open_orders, ())
        self.assertEqual(quiet_pumps, [], "the read waited for a quiet link it already had")
        # A stray since the last list still makes the next read wait.
        h.fake.callbacks.open_order_end()
        h.conn.pump(0)
        collect_broker_orders(h.conn)
        self.assertEqual(quiet_pumps, [True])

    def test_a_list_whose_end_never_comes_drops_the_link_before_another_is_asked(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = self.working()
        h.fake.hold_open_orders = True
        with self.assertRaises(BrokerOrdersIncomplete):
            collect_broker_orders(h.conn)
        with self.assertRaises(NotConnected):
            collect_broker_orders(h.conn)
        self.assertEqual(len(h.fake.called("req_all_open_orders")), 1, "never two lists outstanding")
        self.assertFalse(h.conn.connected)
        self.assertEqual(h.statuses[-1].reason, "open-order-list-unended")


class SettledOpenOrderListTests(OpenOrderLists, unittest.TestCase):
    """A start's or reconnect's read of IBKR's open orders stands only once two reads in a row agree."""

    def test_a_volunteered_end_after_the_first_request_never_passes_a_short_list_as_whole(self):
        h = Harness()
        h.fake.open_orders = self.working()
        h.fake.volunteer_open_orders = True
        h.fake.volunteer_after_request = True
        h.conn.connect()
        self.assert_whole(collect_settled_broker_orders(h.conn))

    def test_two_reads_that_disagree_are_read_again_until_two_agree(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_order_answers = [[], self.working(), self.working()]
        self.assert_whole(collect_settled_broker_orders(h.conn))
        self.assertEqual(len(h.fake.called("req_all_open_orders")), 3)

    def test_an_order_whose_state_changed_between_reads_is_read_again(self):
        h = Harness()
        h.conn.connect()
        pending = [(7, contract(), order(order_ref="tag-1"), SimpleNamespace(status="PreSubmitted"))]
        h.fake.open_order_answers = [pending, self.working()[:1], self.working()[:1]]
        broker = collect_settled_broker_orders(h.conn)
        self.assertEqual([o.ibkr_status for o in broker.open_orders], ["Submitted"])
        self.assertEqual(len(h.fake.called("req_all_open_orders")), 3)

    def test_two_reads_cut_short_the_same_way_never_settle(self):
        h = Harness()
        h.fake = EndsEachListOnAnotherEnd()
        h.conn.api = h.fake
        h.fake.open_orders = self.working()
        h.conn.connect()
        with self.assertRaises(OpenOrdersUnsettled):
            collect_settled_broker_orders(h.conn)
        self.assertEqual(len(h.fake.called("req_all_open_orders")), 4)

    def test_a_link_one_list_behind_is_resynced_and_the_next_read_is_current(self):
        # Gateway's unasked end lands inside the first list asked for; that
        # list's own rows and end then arrive behind the next request. Each
        # request's answer differs, so a read one list behind shows the
        # previous request's answer, not its own.
        h = Harness()
        h.fake.open_orders = self.working()
        h.fake.volunteer_open_orders = True
        h.fake.volunteer_after_request = True
        h.fake.open_order_batch = 1
        h.fake.open_order_answers = [self.working()[:1], self.working(), self.working()]
        h.conn.connect()
        self.assert_whole(collect_settled_broker_orders(h.conn))
        self.assertEqual(len(h.fake.called("req_all_open_orders")), 3)

    def test_three_disagreements_in_a_row_leave_the_list_unsettled(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_order_answers = [[], self.working(), [], self.working(), []]
        with self.assertRaises(OpenOrdersUnsettled):
            collect_settled_broker_orders(h.conn)
        self.assertEqual(len(h.fake.called("req_all_open_orders")), 4)


if __name__ == "__main__":
    unittest.main()
