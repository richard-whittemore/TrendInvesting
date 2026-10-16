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
from orders import (OrderTracker, OrderStatusReport, OpenOrderRecord, ExecutionReport,
                    CommissionReport, UnrecognisedOrderReport, collect_broker_orders,
                    match_on_reconnect, parse_execution_time)

STATE = SimpleNamespace(status="Submitted")


def status(cb, order_id, ibkr_status, filled, remaining, avg=0.0, last=0.0, perm_id=9001):
    cb.order_status(order_id, ibkr_status, Decimal(filled), Decimal(remaining), avg, perm_id,
                    0, last, 11, "", 0.0)


class Tracked:
    def __init__(self):
        self.h = Harness()
        self.h.conn.connect()
        self.reports = []
        self.tracker = OrderTracker(ACCOUNT, self.reports.append)
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

    def test_inactive_without_a_rejection_fails_closed(self):
        t = Tracked()
        status(t.cb, 7, "Inactive", "0", "10")
        with self.assertRaises(UnrecognisedOrderReport):
            t.drain()

    def test_unknown_status_fails_closed(self):
        t = Tracked()
        status(t.cb, 7, "Teleported", "0", "10")
        with self.assertRaises(UnrecognisedOrderReport):
            t.drain()

    def test_pending_submit_is_recorded_without_a_lifecycle(self):
        t = Tracked()
        status(t.cb, 7, "PendingSubmit", "0", "10")
        status(t.cb, 7, "PreSubmitted", "0", "10")
        t.drain()
        self.assertEqual([s.lifecycle for s in t.of(OrderStatusReport)], [None, "submitted"])

    def test_identical_repeats_are_collapsed(self):
        t = Tracked()
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

    def test_redelivered_execution_is_reported_once(self):
        t = Tracked()
        t.cb.exec_details(-1, contract(), execution())
        t.cb.exec_details(4, contract(), execution())
        t.drain()
        self.assertEqual(len(t.of(ExecutionReport)), 1)

    def test_execution_time_formats(self):
        self.assertEqual(parse_execution_time("20261016 15:59:59 America/New_York"),
                         datetime(2026, 10, 16, 19, 59, 59, tzinfo=timezone.utc))
        for bad in ("20261016  09:30:01", "yesterday", "20261016 09:30:01 Mars/Olympus"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_execution_time(bad)


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
        match = match_on_reconnect({"tag-1", "tag-2", "tag-3"}, broker)
        self.assertEqual(match.matched, {"tag-1": 7})
        self.assertEqual(sorted(match.executed), ["tag-2"])
        self.assertEqual(match.missing_at_broker, ("tag-3",))
        self.assertEqual([o.broker_order_id for o in match.unknown_at_broker], [9])
        self.assertFalse(match.clean)
        self.assertEqual(h.fake.called("place_order"), [])
        self.assertEqual(h.fake.called("req_executions")[0][2], ACCOUNT)

    def test_clean_when_everything_is_accounted_for(self):
        h = Harness()
        h.conn.connect()
        h.fake.open_orders = [(7, contract(), order(order_ref="tag-1"), STATE)]
        match = match_on_reconnect({"tag-1"}, collect_broker_orders(h.conn))
        self.assertTrue(match.clean)


if __name__ == "__main__":
    unittest.main()
