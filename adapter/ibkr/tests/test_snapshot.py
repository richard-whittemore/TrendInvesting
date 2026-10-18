"""account.broker.snapshot and account.reconciliation payloads from one IBKR read.

The engine refuses a broker snapshot that leaves positions, open_orders or
market_open unstated (fail closed), so every field is stated here: [] for
none, and the payload's unreadable marker for a list or value that could not
be read, never an omitted or null field.
"""
import json
import sys
import unittest
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from fake_tws import ACCOUNT, OTHER_ACCOUNT, contract, order
from test_account import connected, full_summary
from snapshot import (BROKER_SNAPSHOT_TYPE, BROKER_SNAPSHOT_VERSION, LAST_PRICE_TICK, RECONCILIATION_TYPE,
                      RECONCILIATION_VERSION, broker_snapshot_record, read_account, read_marks,
                      reconciliation_record)

TESTDATA = tests_dir / "testdata"


def state(status):
    return SimpleNamespace(status=status)


def scripted():
    """A boosted joint account: 147 SPY, one working sell of ours, one manual sell, one filled order."""
    h = connected()
    h.fake.positions = [
        (ACCOUNT, contract(), Decimal("147"), 312.5),
        (OTHER_ACCOUNT, contract(), Decimal("999"), 300.0),
        (ACCOUNT, contract(con_id=8314, symbol="IEF"), Decimal("0"), 95.1),
    ]
    h.fake.summary = [
        (ACCOUNT, "NetLiquidation", "8530.00", "USD"),
        (ACCOUNT, "TotalCashValue", "-4700.00", "USD"),
        (ACCOUNT, "ExcessLiquidity", "5222.50", "USD"),
    ]
    h.fake.open_orders = [
        (7, contract(), order(order_ref="tw-1", action="SELL", total_quantity=Decimal("10"),
                              order_type="LMT", lmt_price=85.5), state("Submitted")),
        (0, contract(), order(order_ref="", action="SELL", total_quantity=Decimal("5"), perm_id=4242,
                              client_id=0), state("PreSubmitted")),
        (8, contract(), order(order_ref="tw-0", action="BUY", total_quantity=Decimal("3")), state("Filled")),
        (9, contract(), order(order_ref="x", account=OTHER_ACCOUNT, action="SELL"), state("Submitted")),
    ]
    return h


def fixture(name):
    return json.loads((TESTDATA / name).read_text())


class BrokerSnapshotTests(unittest.TestCase):
    def test_every_field_is_stated_from_one_read(self):
        h = scripted()
        record = broker_snapshot_record(read_account(h.conn), market_open=True, marks={"SPY": Decimal("90")})
        self.assertEqual((record.event_type, record.schema_version), (BROKER_SNAPSHOT_TYPE, BROKER_SNAPSHOT_VERSION))
        # The fixture is the contract: internal/event's test decodes and
        # validates this same file as the engine would.
        self.assertEqual(record.payload, fixture("broker_snapshot.json"))
        self.assertEqual(json.loads(json.dumps(record.payload)), record.payload)

    def test_none_held_and_none_working_are_stated_as_empty_lists(self):
        h = connected()
        h.fake.summary = full_summary()
        payload = broker_snapshot_record(read_account(h.conn), market_open=False).payload
        self.assertEqual(payload["positions"], [])
        self.assertEqual(payload["open_orders"], [])
        self.assertEqual(payload["unreadable"], [])
        self.assertIs(payload["market_open"], False)

    def test_a_list_that_did_not_arrive_is_marked_unreadable(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.end_positions = False
        h.fake.end_open_orders = False
        payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
        self.assertEqual(payload["unreadable"], ["open-orders", "positions"])
        self.assertEqual(payload["positions"], [])
        self.assertEqual(payload["open_orders"], [])

    def test_a_failed_summary_marks_both_balances_unreadable_and_carries_no_value(self):
        h = connected()
        h.fake.summary_error = 321
        payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity", "net-liquidation"])
        self.assertEqual((payload["net_liquidation"], payload["excess_liquidity"]), (0, 0))

    def test_a_missing_or_foreign_currency_balance_is_unreadable(self):
        h = connected()
        h.fake.summary = [(ACCOUNT, "NetLiquidation", "8530.00", "EUR"), (ACCOUNT, "TotalCashValue", "1", "USD")]
        payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity", "net-liquidation"])

    def test_a_position_the_engine_cannot_name_makes_positions_unreadable(self):
        for name, row in {
            "an option": (ACCOUNT, contract(sec_type="OPT"), Decimal("1"), 1.0),
            "another currency": (ACCOUNT, contract(currency="CAD"), Decimal("1"), 1.0),
            "a fraction": (ACCOUNT, contract(), Decimal("1.5"), 1.0),
            "a duplicate symbol": (ACCOUNT, contract(con_id=1), Decimal("1"), 1.0),
        }.items():
            with self.subTest(name):
                h = connected()
                h.fake.summary = full_summary()
                h.fake.positions = [(ACCOUNT, contract(), Decimal("2"), 1.0), row] if name == "a duplicate symbol" else [row]
                payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
                self.assertIn("positions", payload["unreadable"])
                self.assertEqual(payload["positions"], [])

    def test_an_open_order_the_engine_cannot_read_makes_open_orders_unreadable(self):
        for name, row in {
            "an unknown status": (7, contract(), order(action="SELL"), state("Mystery")),
            "no status": (7, contract(), order(action="SELL"), state(None)),
            "an unknown action": (7, contract(), order(action="SSHORT"), state("Submitted")),
            "a fraction": (7, contract(), order(action="SELL", total_quantity="0.5"), state("Submitted")),
            "an unnamed order": (0, contract(), order(action="SELL", perm_id=0), state("Submitted")),
        }.items():
            with self.subTest(name):
                h = connected()
                h.fake.summary = full_summary()
                h.fake.open_orders = [row]
                payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
                self.assertIn("open-orders", payload["unreadable"])
                self.assertEqual(payload["open_orders"], [])

    def test_held_and_cancel_pending_orders_count_as_able_to_execute(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.open_orders = [
            (7, contract(), order(action="SELL"), state("PendingCancel")),
            (8, contract(), order(action="SELL"), state("Inactive")),
            (9, contract(), order(action="SELL"), state("PendingSubmit")),
        ]
        payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
        self.assertEqual([o["status"] for o in payload["open_orders"]], ["cancel-pending", "pending", "pending"])

    def test_a_dropped_link_reads_nothing_and_says_so(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.drop()
        reading = read_account(h.conn)
        payload = broker_snapshot_record(reading, market_open=True).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity", "net-liquidation", "open-orders", "positions"])
        self.assertRegex(payload["as_of"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        rec = reconciliation_record(reading, "morning:1").payload
        self.assertEqual(rec["unreadable"], ["positions", "total-cash"])
        self.assertEqual((rec["total_cash"], rec["currency"]), (0, ""))

    def test_no_server_time_after_the_read_leaves_it_unbounded_and_unreadable(self):
        h = scripted()
        h.fake.answer_current_time = False
        payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity", "net-liquidation", "open-orders", "positions"])
        self.assertEqual((payload["positions"], payload["open_orders"], payload["net_liquidation"]), ([], [], 0))

    def test_a_balance_reported_twice_with_different_values_is_unreadable(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "ExcessLiquidity", "1.00", "USD")]
        payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity"])
        self.assertEqual(payload["excess_liquidity"], 0)

    def test_market_open_must_be_stated_as_a_boolean(self):
        h = scripted()
        with self.assertRaises(TypeError):
            broker_snapshot_record(read_account(h.conn), market_open=None)

    def test_a_mark_not_supplied_is_not_set(self):
        h = scripted()
        payload = broker_snapshot_record(read_account(h.conn), market_open=True).payload
        self.assertEqual(payload["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": 0, "mark_set": False}])


class MarkTests(unittest.TestCase):
    """IBKR's mark for each holding: the last price of a one-off market-data snapshot of its contract."""

    def test_each_symbol_is_asked_for_by_its_contract_and_its_last_price_is_its_mark(self):
        h = connected()
        h.fake.marks = {756733: 401.17, 8314: 95.5}
        marks = read_marks(h.conn, {"SPY": 756733, "IEF": 8314})
        self.assertEqual(marks, {"IEF": Decimal("95.5"), "SPY": Decimal("401.17")})
        asked = h.fake.called("req_mkt_data")
        self.assertEqual(sorted(c[2].conId for c in asked), [8314, 756733])
        self.assertEqual(h.fake.called("cancel_mkt_data"), [], "a finished snapshot needs no cancel")

    def test_only_the_last_price_counts_as_the_mark(self):
        self.assertEqual(LAST_PRICE_TICK, 4, "IBKR's tick type 4 is LAST")
        h = connected()
        h.fake.marks = {756733: 401.17}
        h.fake.mark_tick = 9  # the previous close, not the last price
        self.assertEqual(read_marks(h.conn, {"SPY": 756733}), {})

    def test_an_error_a_non_positive_price_or_no_answer_gives_no_mark(self):
        for setup in (lambda f: setattr(f, "mark_errors", {756733: (354, "synthetic: not subscribed")}),
                      lambda f: setattr(f, "marks", {756733: -1.0}),
                      lambda f: setattr(f, "marks", {756733: float("nan")}),
                      lambda f: setattr(f, "answer_marks", False)):
            with self.subTest(setup=setup):
                h = connected()
                h.fake.marks = {756733: 401.17}
                setup(h.fake)
                self.assertEqual(read_marks(h.conn, {"SPY": 756733}), {})

    def test_a_later_last_price_that_is_not_valid_leaves_no_mark(self):
        # A valid price and then an invalid one under the same request: the
        # request did not end with a valid price, so it gives no mark.
        for bad in (-1.0, 0.0, float("nan"), float("inf")):
            with self.subTest(bad=bad):
                h = connected()
                h.fake.mark_ticks = {756733: [(LAST_PRICE_TICK, 401.17), (LAST_PRICE_TICK, bad)]}
                self.assertEqual(read_marks(h.conn, {"SPY": 756733}), {})

    def test_a_price_whose_snapshot_never_ended_is_no_mark(self):
        # IBKR sent a price but not the snapshot's end within the timeout:
        # the request did not complete, so its price is not the mark.
        h = connected()
        h.fake.marks = {756733: 401.17, 8314: 95.5}
        h.fake.end_marks = False
        self.assertEqual(read_marks(h.conn, {"SPY": 756733, "IEF": 8314}), {})
        self.assertEqual(sorted(c[1] for c in h.fake.called("cancel_mkt_data")),
                         sorted(c[1] for c in h.fake.called("req_mkt_data")))

    def test_a_request_left_unanswered_is_cancelled(self):
        h = connected()
        h.fake.answer_marks = False
        read_marks(h.conn, {"SPY": 756733})
        self.assertEqual([c[1] for c in h.fake.called("cancel_mkt_data")],
                         [c[1] for c in h.fake.called("req_mkt_data")])

    def test_a_dropped_link_gives_no_marks(self):
        h = connected()
        h.fake.drop()
        self.assertEqual(read_marks(h.conn, {"SPY": 756733}), {})

    def test_the_marks_reach_the_snapshot(self):
        h = scripted()
        h.fake.marks = {756733: 401.17}
        reading = read_account(h.conn)
        payload = broker_snapshot_record(reading, market_open=False, marks=read_marks(h.conn, {"SPY": 756733})).payload
        self.assertEqual(payload["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": 401.17,
                                                 "mark_set": True}])


class ReconciliationTests(unittest.TestCase):
    def test_positions_and_cash_from_the_same_read(self):
        h = scripted()
        record = reconciliation_record(read_account(h.conn), "morning:2027-01-15")
        self.assertEqual((record.event_type, record.schema_version), (RECONCILIATION_TYPE, RECONCILIATION_VERSION))
        self.assertEqual(record.payload, fixture("reconciliation.json"))

    def test_cash_that_did_not_arrive_is_unreadable(self):
        h = connected()
        h.fake.summary = full_summary()[:1]
        payload = reconciliation_record(read_account(h.conn), "morning:1").payload
        self.assertEqual(payload["unreadable"], ["total-cash"])
        self.assertEqual(payload["positions"], [])

    def test_cash_in_another_currency_is_unreadable(self):
        # The engine's book is in USD; a TotalCashValue in another currency
        # is not a figure it can compare, so the adapter marks it unreadable
        # rather than pass it on.
        h = connected()
        h.fake.summary = [row if row[1] != "TotalCashValue" else (row[0], row[1], row[2], "EUR") for row in full_summary()]
        payload = reconciliation_record(read_account(h.conn), "morning:1").payload
        self.assertIn("total-cash", payload["unreadable"])
        self.assertEqual((payload["total_cash"], payload["currency"]), (0, ""))

    def test_a_reconciliation_names_its_trigger(self):
        h = scripted()
        with self.assertRaises(ValueError):
            reconciliation_record(read_account(h.conn), "r", trigger="hourly")
        with self.assertRaises(ValueError):
            reconciliation_record(read_account(h.conn), "")


if __name__ == "__main__":
    unittest.main()
