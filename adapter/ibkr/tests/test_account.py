"""Assembling one account snapshot from IBKR's separate position and summary callbacks."""
import sys
import unittest
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from fake_tws import ACCOUNT, OTHER_ACCOUNT, SERVER_TIME, contract
from test_connection import Harness
from account import read_snapshot, Money, SnapshotIncomplete, SnapshotInvalid


def full_summary(account=ACCOUNT):
    return [
        (account, "NetLiquidation", "25012.34", "USD"),
        (account, "TotalCashValue", "512.30", "USD"),
        (account, "ExcessLiquidity", "15800.00", "USD"),
    ]


def connected(**overrides):
    h = Harness(**overrides)
    h.conn.connect()
    return h


class SnapshotTests(unittest.TestCase):
    def test_assembles_positions_and_balances_for_the_configured_account_only(self):
        h = connected()
        h.fake.positions = [
            (ACCOUNT, contract(), Decimal("120"), 312.5),
            (OTHER_ACCOUNT, contract(), Decimal("999"), 300.0),
            (ACCOUNT, contract(con_id=8314, symbol="IEF"), Decimal("0"), 95.1),
        ]
        h.fake.summary = full_summary() + full_summary(OTHER_ACCOUNT)
        snap = read_snapshot(h.conn)
        self.assertEqual(snap.account_id, ACCOUNT)
        self.assertEqual([(p.con_id, p.symbol, p.quantity) for p in snap.positions],
                         [(8314, "IEF", Decimal("0")), (756733, "SPY", Decimal("120"))])
        self.assertEqual(snap.positions[1].avg_cost, Decimal("312.5"))
        self.assertEqual(snap.net_liquidation, Money(Decimal("25012.34"), "USD"))
        self.assertEqual(snap.total_cash, Money(Decimal("512.30"), "USD"))
        self.assertEqual(snap.excess_liquidity, Money(Decimal("15800.00"), "USD"))
        self.assertEqual(snap.server_time_requested, SERVER_TIME)
        self.assertEqual(snap.server_time_completed, SERVER_TIME + 1)
        self.assertIsNotNone(snap.received_at.tzinfo)
        self.assertIn(("cancel_positions",), h.fake.calls)
        self.assertEqual(len(h.fake.called("cancel_account_summary")), 1)

    def test_summary_requests_exactly_the_three_balances(self):
        h = connected()
        h.fake.summary = full_summary()
        read_snapshot(h.conn)
        _, _, group, tags = h.fake.called("req_account_summary")[0]
        self.assertEqual(group, "All")
        self.assertEqual(set(tags.split(",")), {"NetLiquidation", "TotalCashValue", "ExcessLiquidity"})

    def test_a_missing_balance_is_not_zero(self):
        h = connected()
        h.fake.summary = full_summary()[:2]
        with self.assertRaises(SnapshotIncomplete) as caught:
            read_snapshot(h.conn)
        self.assertIn("ExcessLiquidity", caught.exception.missing)

    def test_no_end_marker_within_the_deadline_is_incomplete(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.end_positions = False
        with self.assertRaises(SnapshotIncomplete) as caught:
            read_snapshot(h.conn)
        self.assertIn("positions", caught.exception.missing)

    def test_a_drop_mid_collection_is_incomplete(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.drop_during_summary = True
        with self.assertRaises(SnapshotIncomplete):
            read_snapshot(h.conn)
        self.assertFalse(h.conn.connected)

    def test_an_error_on_the_summary_request_is_incomplete(self):
        h = connected()
        h.fake.summary_error = 321
        with self.assertRaises(SnapshotIncomplete):
            read_snapshot(h.conn)

    def test_no_server_time_is_incomplete(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.answer_current_time = False
        with self.assertRaises(SnapshotIncomplete) as caught:
            read_snapshot(h.conn)
        self.assertIn("server-time", caught.exception.missing)

    def test_a_time_answer_lost_in_an_outage_does_not_block_reads_after_restoration(self):
        # IBKR answers reqCurrentTime in order; an answer lost while Gateway
        # had no link to IBKR's servers must not leave every later ticket
        # waiting for one answer too many.
        h = connected(request_timeout=0.05)
        h.fake.summary = full_summary()
        h.fake.answer_current_time = False
        h.conn.request_server_time()  # its answer is lost in the outage
        h.fake.callbacks.error(-1, 1100, "lost")
        h.fake.callbacks.error(-1, 1102, "Connectivity restored - data maintained.")
        h.conn.pump(0.001)
        h.conn.pump(0.001)
        h.fake.answer_current_time = True
        # Until request_timeout has passed since the restoration, an answer
        # may still be the lost one arriving late, so a read fails closed.
        with self.assertRaises(SnapshotIncomplete) as caught:
            read_snapshot(h.conn)
        self.assertIn("server-time", caught.exception.missing)
        h.clock.now += 1.0  # well past request_timeout
        # The answers can no longer be matched to their requests, so the
        # adapter's loop drops the link and reconnects, and reads resume.
        h.conn.maintain()
        self.assertEqual(len(h.fake.called("connect")), 2)
        snap = read_snapshot(h.conn)
        self.assertEqual(snap.server_time_completed, snap.server_time_requested + 1)

    def test_a_late_answer_from_before_the_outage_is_not_read_as_fresh(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.answer_current_time = False
        h.conn.request_server_time()
        h.fake.callbacks.error(-1, 1100, "lost")
        h.fake.callbacks.error(-1, 1102, "Connectivity restored - data maintained.")
        h.conn.pump(0.001)
        h.conn.pump(0.001)
        h.fake.callbacks.current_time(SERVER_TIME - 100)  # the old request's answer, late
        h.fake.answer_current_time = True
        snap = read_snapshot(h.conn)
        self.assertEqual(snap.server_time_requested, SERVER_TIME)
        self.assertEqual(snap.server_time_completed, SERVER_TIME + 1)

    def test_an_old_answer_arriving_after_the_restore_deadline_is_not_read_as_fresh(self):
        # Past request_timeout an unanswered request's real answer can still
        # arrive, and IBKR's answers carry no request ID, so the adapter can
        # no longer tell which answer belongs to which request: it drops the
        # link, and the reconnected session starts its count afresh.
        h = connected(request_timeout=0.05)
        h.fake.summary = full_summary()
        h.fake.answer_current_time = False
        h.conn.request_server_time()
        h.fake.callbacks.error(-1, 1100, "lost")
        h.fake.callbacks.error(-1, 1102, "Connectivity restored - data maintained.")
        h.conn.pump(0.001)
        h.conn.pump(0.001)
        h.clock.now += 1.0  # well past request_timeout
        h.fake.callbacks.current_time(SERVER_TIME - 100)  # the old request's answer, very late
        h.fake.answer_current_time = True
        with self.assertRaises(SnapshotIncomplete) as caught:
            read_snapshot(h.conn)
        self.assertIn("connection", caught.exception.missing)
        self.assertIn("time-answers-unmatched", [s.reason for s in h.statuses])
        h.conn.maintain()
        snap = read_snapshot(h.conn)
        self.assertEqual(snap.server_time_requested, SERVER_TIME)
        self.assertEqual(snap.server_time_completed, SERVER_TIME + 1)

    def test_conflicting_duplicate_balance_is_invalid(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "NetLiquidation", "1.00", "USD")]
        with self.assertRaises(SnapshotInvalid):
            read_snapshot(h.conn)

    def test_unparseable_balance_is_invalid(self):
        h = connected()
        h.fake.summary = [(ACCOUNT, "NetLiquidation", "n/a", "USD")] + full_summary()[1:]
        with self.assertRaises(SnapshotInvalid):
            read_snapshot(h.conn)

    def test_duplicate_position_row_is_invalid(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.positions = [(ACCOUNT, contract(), Decimal("1"), 1.0)] * 2
        with self.assertRaises(SnapshotInvalid):
            read_snapshot(h.conn)

    def test_disconnected_refuses_to_read(self):
        h = Harness()
        with self.assertRaises(SnapshotIncomplete):
            read_snapshot(h.conn)


if __name__ == "__main__":
    unittest.main()
