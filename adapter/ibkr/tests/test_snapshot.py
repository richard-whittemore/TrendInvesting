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
from account import Money
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
        # The positions and the account values, asked behind the list, were
        # cut off with it.
        self.assertEqual(reading.unended, frozenset({"open-orders", "positions", "account-values"}))

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


class ReadsOnOneConnectionTests(unittest.TestCase):
    """Every read on one connection is complete, however many came before it.

    On 2026-10-25 IB Gateway 10.50 answered exactly two account summaries a
    connection and refused every later one with error 322 for minutes,
    although each was cancelled once read (the fake gateway does the same).
    Rule: ADR 0019 (a fresh reading; an unavailable balance is not zero):
    the balances never depend on IBKR honouring a cancel.
    """

    def test_the_third_and_later_reads_on_one_connection_read_every_balance(self):
        h = connected()
        h.fake.summary = full_summary()
        for n in range(1, 6):
            reading = read_account(h.conn)
            self.assertEqual(reading.unread, frozenset(), "read {}: {}".format(n, reading.summary))
            self.assertEqual(reading.balances["NetLiquidation"], Money(Decimal("25012.34"), "USD"))
            self.assertEqual(reading.balances["TotalCashValue"], Money(Decimal("512.30"), "USD"))
            self.assertEqual(reading.balances["ExcessLiquidity"], Money(Decimal("15800.00"), "USD"))
        self.assertEqual(h.fake.called("req_account_summary"), [], "no account summary is asked for")

    def test_each_read_is_a_fresh_download_of_the_account_values_and_ends_its_subscription(self):
        h = connected()
        h.fake.summary = full_summary()
        read_account(h.conn)
        h.fake.summary = [(a, t, "26000.00" if t == "NetLiquidation" else v, c) for a, t, v, c in full_summary()]
        reading = read_account(h.conn)
        self.assertEqual(reading.balances["NetLiquidation"], Money(Decimal("26000.00"), "USD"))
        self.assertEqual(h.fake.called("req_account_updates"),
                         [("req_account_updates", True, ACCOUNT), ("req_account_updates", False, ACCOUNT)] * 2)


class AccountValuesTests(unittest.TestCase):
    """IBKR's account values (reqAccountUpdates): why a read of them failed is logged, and nothing is guessed.

    Rule: ADR 0019 (an unavailable balance is not zero; collection failures
    with explicit missing components).
    """

    def test_an_error_during_the_download_is_logged_with_ibkrs_code_and_message_and_the_account_masked(self):
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
        self.assertTrue({"net-liquidation", "excess-liquidity", "total-cash"} <= reading.unread)

    def test_ibkrs_unsubscribed_notice_during_the_download_fails_it_closed(self):
        # 2100: IBKR unsubscribed this client from the account's data.
        h = connected()
        h.fake.summary_error = 2100
        h.fake.summary_error_message = "API client has been unsubscribed from account data."
        reading = read_account(h.conn)
        self.assertTrue({"net-liquidation", "excess-liquidity", "total-cash"} <= reading.unread)
        self.assertIn("2100", reading.summary)

    def test_a_market_data_farm_notice_during_the_download_does_not_fail_it(self):
        h = connected()
        h.fake.summary = full_summary()
        ask = h.fake.req_account_updates

        def with_notice(subscribe, account_id):
            if subscribe:
                h.fake.callbacks.error(-1, 2104, "Market data farm connection is OK:usfarm")
            ask(subscribe, account_id)
        h.fake.req_account_updates = with_notice
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)

    def test_values_that_never_end_are_logged_as_timed_out_and_leave_the_link_unread_on(self):
        h = connected()
        h.fake.summary = full_summary()
        h.fake.end_summary = False
        with self.assertLogs("ibkr", level="INFO") as logs:
            reading = read_account(h.conn)
        self.assertIn("did not end", "\n".join(logs.output))
        self.assertIn("did not end", reading.summary)
        self.assertIn("net-liquidation", reading.unread)
        # Its end carries no request ID: a late one would end a later read's.
        self.assertIn("account-values", reading.unended)
        self.assertIn(("req_account_updates", False, ACCOUNT), h.fake.calls, "unsubscribed all the same")

    def test_values_that_ended_are_logged_as_ended_with_any_balance_missing_or_in_another_currency(self):
        h = connected()
        h.fake.summary = [(ACCOUNT, "NetLiquidation", "8530.00", "EUR"), (ACCOUNT, "TotalCashValue", "1", "USD")]
        with self.assertLogs("ibkr", level="INFO") as logs:
            reading = read_account(h.conn)
        text = "\n".join(logs.output)
        self.assertIn("ended", text)
        self.assertIn("ExcessLiquidity missing", text)
        self.assertIn("NetLiquidation in EUR", text)
        self.assertIn("ExcessLiquidity missing", reading.summary)

    def test_the_total_cash_is_ibkrs_total_cash_value_in_the_base_currency_beside_its_cash_ledger(self):
        # IBKR's key list (EWrapper::updateAccountValue) gives TotalCashValue
        # as the account's total, in the currency the value is expressed in,
        # with -S and -C segment variants; the per-currency cash ledger is
        # CashBalance and TotalCashBalance, with their "BASE" totals. The
        # fake sends all of these (ledger_rows): the cash is TotalCashValue.
        h = connected()
        h.fake.summary = full_summary()
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)
        self.assertEqual(reading.balances["TotalCashValue"], Money(Decimal("512.30"), "USD"))

    def test_a_total_cash_base_row_that_disagrees_with_the_base_currencys_is_unreadable(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "TotalCashValue", "600.00", "BASE")]
        reading = read_account(h.conn)
        self.assertIn("total-cash", reading.unread)
        self.assertNotIn("TotalCashValue", reading.balances)

    def test_total_cash_given_in_two_currencies_is_ambiguous_and_unreadable_even_with_a_base_row(self):
        h = connected()
        h.fake.ledger_rows = False
        h.fake.summary = [(ACCOUNT, "NetLiquidation", "25012.34", "USD"), (ACCOUNT, "ExcessLiquidity", "15800", "USD"),
                          (ACCOUNT, "TotalCashValue", "400.00", "USD"), (ACCOUNT, "TotalCashValue", "100.00", "EUR"),
                          (ACCOUNT, "TotalCashValue", "512.30", "BASE")]
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset({"total-cash"}), reading.summary)
        self.assertNotIn("TotalCashValue", reading.balances)

    def test_cash_in_two_currencies_without_ibkrs_base_total_is_unreadable(self):
        h = connected()
        h.fake.ledger_rows = False
        h.fake.summary = [(ACCOUNT, "NetLiquidation", "25012.34", "USD"), (ACCOUNT, "ExcessLiquidity", "15800", "USD"),
                          (ACCOUNT, "TotalCashValue", "400.00", "USD"), (ACCOUNT, "TotalCashValue", "100.00", "EUR")]
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset({"total-cash"}))
        self.assertNotIn("TotalCashValue", reading.balances)
        self.assertIn("total-cash unreadable", reading.summary)

    def test_every_row_of_a_balance_is_validated_not_only_the_one_taken(self):
        for row in [(ACCOUNT, "NetLiquidation", "not-a-number", "BASE"),
                    (ACCOUNT, "ExcessLiquidity", "", "BASE"),
                    (ACCOUNT, "TotalCashValue", "n/a", "BASE")]:
            h = connected()
            h.fake.summary = full_summary() + [row]
            reading = read_account(h.conn)
            marker = {"NetLiquidation": "net-liquidation", "ExcessLiquidity": "excess-liquidity",
                      "TotalCashValue": "total-cash"}[row[1]]
            self.assertIn(marker, reading.unread, row)
            self.assertNotIn(row[1], reading.balances, row)

    def test_cash_held_in_another_currency_is_unreadable_since_the_engine_books_us_dollars_only(self):
        for key in ("CashBalance", "TotalCashBalance"):
            h = connected()
            h.fake.summary = full_summary() + [(ACCOUNT, key, "250.00", "EUR")]
            reading = read_account(h.conn)
            self.assertEqual(reading.unread, frozenset({"total-cash"}), key)
            self.assertIn("cash in EUR", reading.summary)

    def test_a_zero_cash_row_in_another_currency_is_no_holding(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "CashBalance", "0.00", "EUR"),
                                           (ACCOUNT, "TotalCashBalance", "0", "EUR")]
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)

    def test_a_cash_row_in_another_currency_that_is_not_a_number_is_unreadable(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "CashBalance", "?", "EUR")]
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset({"total-cash"}), reading.summary)

    def test_the_same_amount_written_differently_in_one_download_is_no_conflict(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "ExcessLiquidity", "15800.000", "USD"),
                                           (ACCOUNT, "TotalCashValue", "512.3", "USD")]
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)
        self.assertEqual(reading.balances["ExcessLiquidity"], Money(Decimal("15800.00"), "USD"))

    def test_only_ibkrs_documented_informational_notices_leave_the_download_standing(self):
        # TWS API "Message Codes": 2104, 2106 and 2158 are notifications on
        # connecting, 2107 and 2108 an inactive farm "available upon demand".
        for code, fails in ((2104, False), (2106, False), (2107, False), (2108, False), (2158, False),
                            (2103, True), (2105, True), (2150, True), (2199, True)):
            h = connected()
            h.fake.summary = full_summary()
            ask = h.fake.req_account_updates

            def with_notice(subscribe, account_id, ask=ask, h=h, code=code):
                if subscribe:
                    h.fake.callbacks.error(-1, code, "synthetic notice")
                ask(subscribe, account_id)
            h.fake.req_account_updates = with_notice
            reading = read_account(h.conn)
            if fails:
                self.assertIn("total-cash", reading.unread, code)
                self.assertIn(str(code), reading.summary)
            else:
                self.assertEqual(reading.unread, frozenset(), (code, reading.summary))

    def test_a_value_restated_with_another_figure_in_one_download_is_unreadable(self):
        h = connected()
        h.fake.summary = full_summary() + [(ACCOUNT, "ExcessLiquidity", "1.00", "USD")]
        reading = read_account(h.conn)
        self.assertIn("excess-liquidity", reading.unread)
        self.assertNotIn("ExcessLiquidity", reading.balances)

    def test_values_for_another_account_and_other_keys_are_skipped(self):
        h = connected()
        h.fake.summary = (full_summary(OTHER_ACCOUNT) + [(ACCOUNT, "NetLiquidation-S", "1", "USD")]
                          + full_summary())
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)
        self.assertEqual(reading.balances["NetLiquidation"], Money(Decimal("25012.34"), "USD"))

    def test_a_link_lost_and_restored_during_the_download_leaves_the_whole_reading_unread(self):
        # Gateway loses IBKR's servers and restores the same socket while
        # the values download: the positions and open orders read before the
        # outage are not combined with values read after it (ADR 0019: one
        # cutoff; ADR 0025 point 5: a link lost is reconciled again), so
        # nothing of the reading stands.
        h = connected()
        h.fake.summary = full_summary()
        h.fake.positions = [(ACCOUNT, contract(), Decimal("147"), 312.5)]
        ask = h.fake.req_account_updates

        def with_outage(subscribe, account_id):
            if subscribe:
                h.fake.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
                h.fake.callbacks.error(-1, 1101, "Connectivity between IB and Trader Workstation has been "
                                                 "restored - data lost.")
            ask(subscribe, account_id)
        h.fake.req_account_updates = with_outage
        reading = read_account(h.conn)
        while h.conn.pump(0) is not None:
            pass
        self.assertTrue(h.conn.connected, "restored on the same socket")
        self.assertTrue({"positions", "open-orders", "net-liquidation", "excess-liquidity", "total-cash"}
                        <= reading.unread, reading.unread)
        self.assertEqual(reading.positions, {})
        self.assertEqual(reading.open_orders, [])
        self.assertEqual(reading.balances, {})
        self.assertIn("link", reading.summary)


def waits_land_in_flight(h):
    """The adapter's sleeps let IBKR's callbacks in flight land (FakeTWS.in_flight)."""
    def sleep(seconds):
        h.sleeps.append(seconds)
        h.clock.now += seconds
        h.fake.waited()
    h.conn._sleep = sleep


class SeparateDownloadsTests(unittest.TestCase):
    """Each read takes only its own download's rows: none from before its subscription, none after its end.

    IBKR's account values carry no request ID, so nothing in a row tells one
    download from another. Rule: ADR 0019 (one coherent cutoff; an
    unavailable balance is not zero) and ADR 0025 point 5 (a late answer
    must not complete a later read).
    """

    def test_rows_queued_before_the_subscription_are_not_the_downloads(self):
        h = connected()
        h.fake.summary = full_summary()
        ask = h.fake.req_positions

        def then_stale_values():
            # Queued after the positions are asked, before the values are.
            ask()
            h.fake.callbacks.account_value("ExcessLiquidity", "1.00", "USD", ACCOUNT)
            h.fake.callbacks.account_download_end(ACCOUNT)
        h.fake.req_positions = then_stale_values
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)
        self.assertEqual(reading.balances["ExcessLiquidity"], Money(Decimal("15800.00"), "USD"))

    def test_rows_after_the_downloads_end_are_ignored_while_the_rest_of_the_read_finishes(self):
        h = connected()
        h.fake.summary = [r for r in full_summary() if r[1] != "TotalCashValue"]
        h.fake.end_positions = False
        ask = h.fake.req_account_updates

        def then_updates(subscribe, account_id):
            ask(subscribe, account_id)
            if subscribe:
                h.fake.callbacks.account_value("TotalCashValue", "999.00", "USD", ACCOUNT)
                h.fake.callbacks.account_value("ExcessLiquidity", "1.00", "USD", ACCOUNT)
                h.fake.callbacks.position_end()
        h.fake.req_account_updates = then_updates
        reading = read_account(h.conn)
        self.assertNotIn("positions", reading.unread)
        self.assertIn("total-cash", reading.unread, "a later update does not fill the ended download")
        self.assertNotIn("TotalCashValue", reading.balances)
        self.assertNotIn("excess-liquidity", reading.unread, "nor does it unsettle what the download read")
        self.assertEqual(reading.balances["ExcessLiquidity"], Money(Decimal("15800.00"), "USD"))

    def test_rows_ibkr_sent_before_the_last_unsubscription_took_effect_are_not_the_next_reads(self):
        h = connected()
        waits_land_in_flight(h)
        h.fake.summary = full_summary()
        read_account(h.conn)
        h.fake.in_flight.append(lambda: h.fake.callbacks.account_value("ExcessLiquidity", "1.00", "USD", ACCOUNT))
        h.fake.in_flight.append(lambda: h.fake.callbacks.account_download_end(ACCOUNT))
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)
        self.assertEqual(reading.balances["ExcessLiquidity"], Money(Decimal("15800.00"), "USD"))

    def test_ibkrs_2100_answering_the_adapters_own_unsubscription_does_not_fail_the_next_read(self):
        h = connected()
        waits_land_in_flight(h)
        h.fake.summary = full_summary()
        h.fake.unsubscribe_notice = True
        for n in range(3):
            reading = read_account(h.conn)
            self.assertEqual(reading.unread, frozenset(), "read {}: {}".format(n + 1, reading.summary))

    def test_a_download_left_unfinished_is_waited_out_before_the_next_subscription(self):
        h = connected()
        waits_land_in_flight(h)
        h.fake.summary = full_summary()
        h.fake.end_summary = False
        self.assertIn("account-values", read_account(h.conn).unended)
        h.fake.end_summary = True
        h.fake.in_flight.append(lambda: h.fake.callbacks.account_download_end(ACCOUNT))
        reading = read_account(h.conn)
        self.assertEqual(reading.unread, frozenset(), reading.summary)
        self.assertTrue(h.conn.connected)

    def test_a_download_whose_end_never_comes_drops_the_link_before_another_is_asked(self):
        h = connected()
        waits_land_in_flight(h)
        h.fake.summary = full_summary()
        h.fake.end_summary = False
        read_account(h.conn)
        h.fake.end_summary = True
        reading = read_account(h.conn)
        self.assertFalse(h.conn.connected)
        self.assertTrue({"net-liquidation", "excess-liquidity", "total-cash"} <= reading.unread)
        self.assertEqual(len(h.fake.called("req_account_updates")), 2, "no second subscription on that link")
