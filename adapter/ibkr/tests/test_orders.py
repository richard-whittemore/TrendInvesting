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

from fake_tws import ACCOUNT, OTHER_ACCOUNT, contract, order, execution
from test_connection import Harness
from connection import REQUEST_ID_BASE
from orders import (OrderTracker, OrderStatusReport, OpenOrderRecord, ExecutionReport,
                    CommissionReport, UnrecognisedOrderReport, collect_broker_orders,
                    match_on_reconnect, parse_execution_time)

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


if __name__ == "__main__":
    unittest.main()
