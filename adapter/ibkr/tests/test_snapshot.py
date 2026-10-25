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
from datetime import datetime, timezone

from snapshot import (ACCOUNT_SNAPSHOT_TYPE, ACCOUNT_SNAPSHOT_VERSION, BROKER_SNAPSHOT_TYPE, BROKER_SNAPSHOT_VERSION,
                      LAST_PRICE_TICK, MARK_TYPE, MARK_VERSION, RECONCILIATION_TYPE, RECONCILIATION_VERSION,
                      account_snapshot_record, broker_snapshot_record, close_valuation, mark_record, read_account,
                      read_marks, reconciliation_record)

TESTDATA = tests_dir / "testdata"
# The close of the session 2027-01-14, which the scripted read (IBKR's clock
# 2027-01-15 08:00 UTC) follows.
CLOSE = datetime(2027, 1, 14, 21, 0, tzinfo=timezone.utc)
# Why a reading stamped at its read time is not as of the close (the tests
# below that do not concern the stamp).
WHY = "the reading is not on the session's own New York date"


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
        record = broker_snapshot_record(read_account(h.conn), market_open=True, marks={"SPY": Decimal("90")}, why=WHY)
        self.assertEqual((record.event_type, record.schema_version), (BROKER_SNAPSHOT_TYPE, BROKER_SNAPSHOT_VERSION))
        # The fixture is the contract: internal/event's test decodes and
        # validates this same file as the engine would.
        self.assertEqual(record.payload, fixture("broker_snapshot.json"))
        self.assertEqual(json.loads(json.dumps(record.payload)), record.payload)

    def test_none_held_and_none_working_are_stated_as_empty_lists(self):
        h = connected()
        h.fake.summary = full_summary()
        payload = broker_snapshot_record(read_account(h.conn), market_open=False, why=WHY).payload
        self.assertEqual(payload["positions"], [])
        self.assertEqual(payload["open_orders"], [])
        self.assertEqual(payload["unreadable"], [])
        self.assertIs(payload["market_open"], False)

    def test_a_list_that_did_not_arrive_is_marked_unreadable(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.end_positions = False
        h.fake.end_open_orders = False
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
        self.assertEqual(payload["unreadable"], ["open-orders", "positions"])
        self.assertEqual(payload["positions"], [])
        self.assertEqual(payload["open_orders"], [])

    def test_the_end_gateway_volunteers_on_a_reconnect_does_not_cut_the_order_list_short(self):
        # The list's end carries no request ID: only the end of the list this
        # read asked for ends it, however late IBKR sends the list.
        h = scripted()
        h.fake.volunteer_open_orders = True
        h.conn.connect()
        h.fake.open_order_batch = 1
        record = broker_snapshot_record(read_account(h.conn), market_open=True, marks={"SPY": Decimal("90")}, why=WHY)
        self.assertEqual(record.payload, fixture("broker_snapshot.json"))

    def test_a_stray_end_before_the_read_does_not_cut_the_order_list_short(self):
        h = scripted()
        h.fake.callbacks.open_order_end()
        h.fake.open_order_batch = 2
        record = broker_snapshot_record(read_account(h.conn), market_open=True, marks={"SPY": Decimal("90")}, why=WHY)
        self.assertEqual(record.payload, fixture("broker_snapshot.json"))

    def test_a_failed_summary_marks_both_balances_unreadable_and_carries_no_value(self):
        h = connected()
        h.fake.summary_error = 321
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity", "net-liquidation"])
        self.assertEqual((payload["net_liquidation"], payload["excess_liquidity"]), (0, 0))

    def test_a_missing_or_foreign_currency_balance_is_unreadable(self):
        h = connected()
        h.fake.summary = [(ACCOUNT, "NetLiquidation", "8530.00", "EUR"), (ACCOUNT, "TotalCashValue", "1", "USD")]
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
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
                payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
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
                payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
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
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
        self.assertEqual([o["status"] for o in payload["open_orders"]], ["cancel-pending", "pending", "pending"])

    def test_a_dropped_link_reads_nothing_and_says_so(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.drop()
        reading = read_account(h.conn)
        payload = broker_snapshot_record(reading, market_open=True, why=WHY).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity", "net-liquidation", "open-orders", "positions"])
        self.assertRegex(payload["as_of"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        rec = reconciliation_record(reading, "morning:1").payload
        self.assertEqual(rec["unreadable"], ["positions", "total-cash"])
        self.assertEqual((rec["total_cash"], rec["currency"]), (0, ""))

    def test_a_list_cut_off_by_gateway_losing_ibkrs_servers_is_unended(self):
        # Gateway restores the same socket itself (1101/1102) after losing
        # IBKR's servers (1100), so the abandoned list's late end could
        # complete a later read on it, as after a timeout.
        h = scripted()
        h.fake.end_open_orders = False
        asked = h.fake.req_all_open_orders

        def lost():
            asked()
            h.fake.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
        h.fake.req_all_open_orders = lost
        reading = read_account(h.conn)
        self.assertFalse(h.conn.connected)
        # The positions, asked behind the list, were cut off with it.
        self.assertEqual(reading.unended, frozenset({"open-orders", "positions"}))

    def test_no_server_time_after_the_read_leaves_it_unbounded_and_unreadable(self):
        h = scripted()
        h.fake.answer_current_time = False
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity", "net-liquidation", "open-orders", "positions"])
        self.assertEqual((payload["positions"], payload["open_orders"], payload["net_liquidation"]), ([], [], 0))

    def test_a_balance_reported_twice_with_different_values_is_unreadable(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "ExcessLiquidity", "1.00", "USD")]
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
        self.assertEqual(payload["unreadable"], ["excess-liquidity"])
        self.assertEqual(payload["excess_liquidity"], 0)

    def test_market_open_must_be_stated_as_a_boolean(self):
        h = scripted()
        with self.assertRaises(TypeError):
            broker_snapshot_record(read_account(h.conn), market_open=None, why=WHY)

    def test_a_mark_not_supplied_is_not_set(self):
        h = scripted()
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
        self.assertEqual(payload["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": 0, "mark_set": False}])


class PostCloseReadingTests(unittest.TestCase):
    """The owner's decision of 2026-10-18 (ADR 0019, ADR 0025 point 5, ADR 0026 point 1, amended).

    One reading after the close, with nothing working at IBKR and no
    execution since, is the account as of that close: the broker snapshot
    (version 2) states the close as its as-of time and the true read time
    beside it, or, when the reading cannot stand for the close, its read time
    and why. From the same reading come the engine's account.snapshot
    (version 3) and the mark (version 2), the delivered bar's official close
    with IBKR's last price beside it as evidence only.
    """

    def test_a_snapshot_as_of_the_close_states_its_read_time_beside_it(self):
        h = scripted()
        reading = read_account(h.conn)
        record = broker_snapshot_record(reading, market_open=False, marks={"SPY": Decimal("90")}, close=CLOSE,
                                        net_liquidation=close_valuation(reading, {"SPY": Decimal("90")}))
        self.assertEqual((record.event_type, record.schema_version), (BROKER_SNAPSHOT_TYPE, 2))
        payload = record.payload
        self.assertEqual((payload["as_of"], payload["read_at"], payload["as_of_close"], payload["not_as_of_close"]),
                         ("2027-01-14T21:00:00Z", "2027-01-15T08:00:00Z", True, ""))
        # The contract with the engine: internal/event's test validates this
        # file at version 2.
        self.assertEqual(payload, fixture("broker_snapshot_close.json"))

    def test_a_snapshot_as_of_the_close_is_valued_at_the_official_close(self):
        # ADR 0019, amended 2026-10-18: one price basis per rule. ADR 0019's
        # worked example, 147 SPY and $4,700 borrowed, read after SPY traded
        # below its $90 close: IBKR's own net liquidation is $8,397.70 at
        # the read, the close's $8,530.
        h = scripted()
        h.fake.summary = [(a, t, "8397.70" if t == "NetLiquidation" else v, c) for a, t, v, c in h.fake.summary]
        reading = read_account(h.conn)
        at_close = close_valuation(reading, {"SPY": 90})
        self.assertEqual(at_close, Decimal("8530.00"), "cash plus each holding times its official close")
        payload = broker_snapshot_record(reading, market_open=False, marks={"SPY": Decimal("90")}, close=CLOSE,
                                         net_liquidation=at_close, cash_drift=Decimal("-0.25")).payload
        self.assertEqual((payload["net_liquidation"], payload["read_net_liquidation"], payload["excess_liquidity"],
                          payload["cash_drift"]), (8530.0, 8397.7, 5222.5, -0.25))
        account = account_snapshot_record(reading, CLOSE, net_liquidation=at_close).payload
        self.assertEqual(account["net_liquidation"], 8530.0)

    def test_a_snapshot_at_its_read_time_is_ibkrs_own_valuation(self):
        h = scripted()
        h.fake.summary = [(a, t, "8397.70" if t == "NetLiquidation" else v, c) for a, t, v, c in h.fake.summary]
        reading = read_account(h.conn)
        payload = broker_snapshot_record(reading, market_open=True, why=WHY).payload
        self.assertEqual((payload["net_liquidation"], payload["read_net_liquidation"], payload["cash_drift"]),
                         (8397.7, 8397.7, 0))
        for kwargs in ({"net_liquidation": Decimal("8530")}, {"cash_drift": Decimal("1")},
                       {"unexplained_cash": Decimal("1")}):
            with self.assertRaises(ValueError, msg=kwargs):
                broker_snapshot_record(reading, market_open=True, why=WHY, **kwargs)
        with self.assertRaises(ValueError, msg="as of the close, its valuation is stated"):
            broker_snapshot_record(reading, market_open=False, close=CLOSE)

    def test_a_holding_with_no_official_close_cannot_be_valued_at_the_close(self):
        h = scripted()
        reading = read_account(h.conn)
        with self.assertRaises(ValueError):
            close_valuation(reading, {})
        h = scripted()
        h.fake.summary = [row for row in h.fake.summary if row[1] != "TotalCashValue"]
        with self.assertRaises(ValueError):
            close_valuation(read_account(h.conn), {"SPY": 90})

    def test_a_snapshot_at_its_read_time_says_why(self):
        h = scripted()
        payload = broker_snapshot_record(read_account(h.conn), market_open=True, why=WHY).payload
        self.assertEqual((payload["as_of"], payload["read_at"], payload["as_of_close"], payload["not_as_of_close"]),
                         ("2027-01-15T08:00:00Z", "2027-01-15T08:00:00Z", False, WHY))

    def test_a_snapshot_is_either_as_of_a_close_or_says_why_not(self):
        h = scripted()
        reading = read_account(h.conn)
        for kwargs in ({}, {"close": CLOSE, "why": WHY}, {"close": datetime(2027, 1, 15, 8, 0, tzinfo=timezone.utc)},
                       {"close": datetime(2027, 1, 14, 21, 0)}):
            with self.assertRaises(ValueError, msg=kwargs):
                broker_snapshot_record(reading, market_open=False, **kwargs)

    def test_the_engines_account_snapshot_is_the_same_reading_as_of_its_stamp(self):
        h = scripted()
        record = account_snapshot_record(read_account(h.conn), CLOSE)
        self.assertEqual((record.event_type, record.schema_version), (ACCOUNT_SNAPSHOT_TYPE, ACCOUNT_SNAPSHOT_VERSION))
        self.assertEqual((ACCOUNT_SNAPSHOT_TYPE, ACCOUNT_SNAPSHOT_VERSION), ("account.snapshot", 3))
        self.assertEqual(record.payload, fixture("account_snapshot.json"))

    def test_an_account_snapshot_needs_its_positions_and_net_liquidation(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.end_positions = False
        with self.assertRaises(ValueError):
            account_snapshot_record(read_account(h.conn), CLOSE)
        h = connected()
        h.fake.summary = [row for row in full_summary() if row[1] != "NetLiquidation"]
        with self.assertRaises(ValueError):
            account_snapshot_record(read_account(h.conn), CLOSE)

    def test_the_mark_is_the_official_close_with_ibkrs_last_price_beside_it(self):
        record = mark_record("SPY", CLOSE, 401.5, "marketdata", Decimal("401.17"))
        self.assertEqual((record.event_type, record.schema_version, MARK_TYPE, MARK_VERSION),
                         ("market.mark.recorded", 2, "market.mark.recorded", 2))
        self.assertEqual(record.payload, fixture("mark.json"))
        self.assertEqual(mark_record("SPY", CLOSE, 401.5, "marketdata").payload,
                         dict(fixture("mark.json"), broker_last=0, broker_last_set=False))
        with self.assertRaises(ValueError):
            mark_record("SPY", CLOSE, 0, "marketdata")


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

    def test_a_snapshot_ibkr_ends_at_the_end_of_its_window_gives_its_mark(self):
        # IBKR ends a snapshot at the end of its 11-second window, after the
        # account reads' request_timeout: the marks wait mark_timeout.
        h = connected(request_timeout=0.05, mark_timeout=15.0)
        h.fake.marks = {756733: 401.17}
        h.fake.mark_end_delay = 11.0
        h.conn.subscribe(h.fake.late_end(h.clock))
        self.assertEqual(read_marks(h.conn, {"SPY": 756733}), {"SPY": Decimal("401.17")})
        self.assertEqual(h.fake.called("cancel_mkt_data"), [], "the snapshot ended, so it needs no cancel")

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
        payload = broker_snapshot_record(reading, market_open=False, marks=read_marks(h.conn, {"SPY": 756733}),
                                         why=WHY).payload
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


class CloseStampedFiguresTests(unittest.TestCase):
    """A snapshot as of the close states the drift added up and IBKR's own net liquidation (ADR 0019, amended 2026-10-18)."""

    def test_the_unexplained_cash_is_stated_beside_the_drift(self):
        h = scripted()
        reading = read_account(h.conn)
        payload = broker_snapshot_record(reading, market_open=False, marks={"SPY": Decimal("90")}, close=CLOSE,
                                         net_liquidation=close_valuation(reading, {"SPY": 90}),
                                         cash_drift=Decimal("12.50"), unexplained_cash=Decimal("22.50")).payload
        self.assertEqual((payload["cash_drift"], payload["unexplained_cash"]), (12.5, 22.5))

    def test_a_non_positive_net_liquidation_at_the_read_is_never_stamped_as_of_the_close(self):
        # The margin alarm's basis; the engine refuses a close-stamped
        # snapshot without it, so the adapter never writes one.
        h = scripted()
        h.fake.summary = [(a, t, "0.00" if t == "NetLiquidation" else v, c) for a, t, v, c in h.fake.summary]
        reading = read_account(h.conn)
        with self.assertRaises(ValueError):
            broker_snapshot_record(reading, market_open=False, close=CLOSE, net_liquidation=Decimal("8530"))


class AccountSummaryTests(unittest.TestCase):
    """IBKR's account summary: why a read of it failed is logged, and requests are paced (IBKR's 322 limit).

    IBKR allows two account-summary subscriptions at a time and a cancel
    takes a moment to count (TWS API, reqAccountSummary; "Message Codes"
    322). On 2026-10-25 IB Gateway 10.50 refused every summary the adapter
    asked for within a second of cancelling the one before it, and the log
    said only which balances were missing.
    """

    def paced(self, grace=2.0):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.clock = h.clock
        h.fake.summary_cancel_grace = grace
        return h

    def test_an_error_under_the_summary_is_logged_with_ibkrs_code_and_message_and_the_account_masked(self):
        h = connected()
        h.fake.summary_error = 321
        h.fake.summary_error_message = "Error validating request: account {} not found".format(ACCOUNT)
        with self.assertLogs("ibkr", level="INFO") as logs:
            reading = read_account(h.conn)
        text = "\n".join(logs.output)
        self.assertIn("errored", text)
        self.assertIn("321", text)
        self.assertIn("Error validating request: account <account> not found", text)
        self.assertNotIn(ACCOUNT, text)
        self.assertIn("321", reading.summary)
        self.assertNotIn(ACCOUNT, reading.summary)
        self.assertEqual(len(h.fake.called("req_account_summary")), 1, "an error other than 322 is not asked again")

    def test_a_summary_that_never_ends_is_logged_as_timed_out(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.end_summary = False
        with self.assertLogs("ibkr", level="INFO") as logs:
            reading = read_account(h.conn)
        self.assertIn("did not end", "\n".join(logs.output))
        self.assertIn("did not end", reading.summary)
        self.assertIn("net-liquidation", reading.unread)

    def test_a_summary_that_ended_is_logged_as_ended_with_any_balance_missing_or_in_another_currency(self):
        h = connected()
        h.fake.summary = [(ACCOUNT, "NetLiquidation", "8530.00", "EUR"), (ACCOUNT, "TotalCashValue", "1", "USD")]
        with self.assertLogs("ibkr", level="INFO") as logs:
            reading = read_account(h.conn)
        text = "\n".join(logs.output)
        self.assertIn("ended", text)
        self.assertIn("ExcessLiquidity missing", text)
        self.assertIn("NetLiquidation in EUR", text)
        self.assertIn("ExcessLiquidity missing", reading.summary)

    def test_a_summary_asked_for_soon_after_a_cancel_waits_until_ibkr_counts_the_cancel(self):
        h = self.paced()
        self.assertEqual(read_account(h.conn).unread, frozenset())
        second = read_account(h.conn)
        self.assertEqual(second.unread, frozenset(), second.summary)
        self.assertGreaterEqual(sum(h.sleeps), 2.0)
        self.assertEqual(len(h.fake.called("req_account_summary")), 2, "paced, it is asked once")

    def test_ibkrs_limit_refusal_is_waited_out_and_the_summary_asked_again(self):
        # IBKR still counts the cancel longer than the pacing allows: its
        # 322 is waited out, never read as a missing balance.
        h = self.paced(grace=4.0)
        read_account(h.conn)
        with self.assertLogs("ibkr", level="INFO") as logs:
            reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)
        self.assertEqual(reading.balances["NetLiquidation"].amount, Decimal("25012.34"))
        self.assertEqual(len(h.fake.called("req_account_summary")), 3)
        text = "\n".join(logs.output)
        self.assertIn("322", text)
        self.assertIn("Maximum number of account summary requests exceeded", text)

    def test_ibkrs_limit_refusal_that_persists_leaves_the_balances_unread_naming_322(self):
        h = self.paced(grace=1000.0)
        read_account(h.conn)
        reading = read_account(h.conn)
        self.assertTrue({"net-liquidation", "excess-liquidity", "total-cash"} <= reading.unread)
        self.assertIn("322", reading.summary)
        # Asked a bounded number of times, each after a wait.
        self.assertLessEqual(len(h.fake.called("req_account_summary")), 5)
        self.assertEqual(len(h.fake.called("req_account_summary")), len(h.fake.called("cancel_account_summary")))
