"""The post-close reading counts as the close: the owner's decision of 2026-10-18 (ADR 0019, ADR 0025 point 5, ADR 0026 point 1, amended).

After a session's close and before the next open, with no order working or
pending at IBKR for the account, no execution since the close, and the
holdings and cash those of the previous close adjusted by the run's fills
(the cash within the drift bound), the account does not change, so one
account read is the account as of that close, valued at the close: each
holding at the bar's official close and the net liquidation the cash plus
those holdings, IBKR's own net liquidation beside it (one price basis per
rule). From that one read the adapter delivers, ahead of the day's stream
lines: the engine's account.snapshot stamped as of the close, then the
broker snapshot with the close as its as-of time and the true read time
beside it as evidence; then the file's lines, with market.mark.recorded for
each instrument the last session delivers a bar for, from that bar's
official close (IBKR's last price beside it), just before that session's
close.

When a condition fails, both snapshots are stamped at the true read time,
IBKR's own reading, and the broker snapshot says why, so the engine
withholds the session as stale (ADR 0019's snapshot rule) and the owner is
paged with the reason.

Every batch carries a reconciliation of the same read (ADR 0025 point 5,
ADR 0019): in a run's first batch behind its account snapshot, the engine
having no book before it; in every later one ahead of its snapshots, so the
engine classifies the reading against the book it holds before the batch
replaces it. Once the engine holds an account snapshot, the adapter also
records a reconciliation at every start and reconnect, only once every
replayed execution's fill is recorded.

Synthetic figures only; nothing here reaches IBKR.
"""
import json
import sys
import tempfile
import unittest
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from engine_double import LoggedEngine
from fake_tws import ACCOUNT, FakeTWS, contract, execution, order
from test_connection import Clock, config
from test_delivery import BAR, CLOSED, FRIDAY, RUN, SPY_CON_ID, session_envelopes, write_stream
from test_loop import COMMISSION_WAIT, RECEIVED, SUMMARY, Ids
from loop import AdapterLoop
from relay import RELEASED
from reports import FILL_TYPE
from snapshot import BROKER_SNAPSHOT_TYPE

ACCOUNT_SNAPSHOT = "account.snapshot"
MARK = "market.mark.recorded"
RECONCILIATION = "account.reconciliation"
# FRIDAY's close (4 p.m. New York, 21:00 UTC in January) and IBKR's clock an
# hour and five minutes after it, the evening the data job has run.
CLOSE = "2027-01-14T21:00:00Z"
EVENING = int(datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc).timestamp())
# The bar's official close in session_envelopes, and IBKR's last price.
OFFICIAL = 400.5
IBKR_LAST = 401.17
# The account valued at that close: its $100,000 cash and 147 SPY at the
# official close (ADR 0019, amended 2026-10-18: one price basis per rule);
# IBKR's own net liquidation at the read is SUMMARY's $100,000.
AT_CLOSE = 100000 + 147 * OFFICIAL
THURSDAY = date(2027, 1, 13)


class PostCloseTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        root = Path(self.dir.name)
        self.pending = str(root / "pending.json")
        self.engine_log = str(root / (RUN + ".log"))
        self.streams = root / "streams"
        self.folder = self.streams / RUN
        self.folder.mkdir(parents=True)
        self.engine = LoggedEngine(self.engine_log)
        self.fake = FakeTWS()
        self.fake.summary = list(SUMMARY)
        self.fake.positions = [(ACCOUNT, contract(), Decimal("147"), 312.5)]
        self.fake.marks = {SPY_CON_ID: IBKR_LAST}
        self.fake.server_time = EVENING
        self.new_id = Ids()

    def loop(self, engine=None):
        clock = Clock()

        def sleep(seconds):
            clock.now += seconds
        loop = AdapterLoop(config(mode="paper", paper_sending_enabled=True, commission_wait=COMMISSION_WAIT),
                           self.fake, engine or self.engine, self.pending, self.engine_log,
                           now=lambda: RECEIVED, today=lambda: FRIDAY, sleep=sleep, monotonic=clock, poll=0,
                           new_id=self.new_id, streams=str(self.streams))
        self.addCleanup(loop.close)
        return loop

    def deliver(self, sessions=(FRIDAY,)):
        write_stream(self.folder, sessions[-1], session_envelopes(list(sessions)))
        loop = self.loop()
        loop.start()
        for _ in range(40):
            loop.step()
        return loop

    def sent(self, event_type):
        return [e for e in self.engine.inputs() if e["type"] == event_type]

    def test_the_post_close_reading_is_the_account_as_of_the_close(self):
        self.deliver()
        self.assertEqual([e["type"] for e in self.engine.inputs()],
                         [ACCOUNT_SNAPSHOT, BROKER_SNAPSHOT_TYPE, RECONCILIATION, BAR, MARK, CLOSED])
        account, broker = self.sent(ACCOUNT_SNAPSHOT)[0], self.sent(BROKER_SNAPSHOT_TYPE)[0]
        self.assertEqual((account["schema_version"], account["event_time"]), (3, CLOSE))
        self.assertEqual(account["payload"], {"account_id": ACCOUNT, "as_of": CLOSE, "net_liquidation": AT_CLOSE,
                                              "currency": "USD",
                                              "positions": [{"instrument_id": "SPY", "quantity": 147}]})
        payload = broker["payload"]
        self.assertEqual(broker["schema_version"], 2)
        self.assertEqual((payload["as_of"], payload["read_at"], payload["as_of_close"], payload["not_as_of_close"]),
                         (CLOSE, "2027-01-14T22:05:03Z", True, ""), "IBKR's clock after the start's read and both of this one's")
        self.assertIs(payload["market_open"], False)
        # The holding's mark is the delivered bar's official close, not
        # IBKR's last price.
        self.assertEqual(payload["positions"], [{"instrument_id": "SPY", "quantity": 147, "mark": OFFICIAL,
                                                 "mark_set": True}])

    def test_a_reading_as_of_the_close_is_valued_at_the_official_close(self):
        self.deliver()
        account = self.sent(ACCOUNT_SNAPSHOT)[0]["payload"]
        broker = self.sent(BROKER_SNAPSHOT_TYPE)[0]["payload"]
        self.assertEqual((account["net_liquidation"], broker["net_liquidation"]), (AT_CLOSE, AT_CLOSE),
                         "cash plus each holding times the bar's official close, the marks' own basis")
        self.assertEqual((broker["read_net_liquidation"], broker["excess_liquidity"], broker["cash_drift"]),
                         (100000.0, 100000.0, 0), "IBKR's own figures at the read, beside it")

    def test_a_reading_at_its_read_time_is_ibkrs_own_valuation(self):
        self.fake.server_time = int(datetime(2027, 1, 15, 8, 0, tzinfo=timezone.utc).timestamp())
        self.deliver()
        account = self.sent(ACCOUNT_SNAPSHOT)[0]["payload"]
        broker = self.sent(BROKER_SNAPSHOT_TYPE)[0]["payload"]
        self.assertFalse(broker["as_of_close"])
        self.assertEqual((account["net_liquidation"], broker["net_liquidation"], broker["read_net_liquidation"]),
                         (100000.0, 100000.0, 100000.0))
        self.assertEqual(broker["positions"][0]["mark"], IBKR_LAST, "marked at IBKR's price at the read, its basis")

    def test_a_holding_with_no_bar_in_the_batch_cannot_be_valued_at_the_close(self):
        self.fake.positions.append((ACCOUNT, contract(con_id=8314, symbol="IEF"), Decimal("10"), 95.1))
        self.deliver()
        self.assert_stamped_at_read_time("IEF", "official close")

    # -- the account since the previous close (ADR 0019, amended 2026-10-18) --
    def two_closes(self, between=lambda loop: None):
        """THURSDAY's batch read that evening, then FRIDAY's that evening, with between run in the day between."""
        envelopes = session_envelopes([THURSDAY, FRIDAY])
        write_stream(self.folder, THURSDAY, [dict(e, sequence=n) for n, e in enumerate(envelopes[:2], 1)])
        self.fake.server_time = EVENING - 86400
        loop = self.loop()
        loop.start()
        for _ in range(40):
            loop.step()
        self.assertTrue(self.sent(BROKER_SNAPSHOT_TYPE)[0]["payload"]["as_of_close"], "setup: THURSDAY as of its close")
        between(loop)
        self.fake.server_time = EVENING
        write_stream(self.folder, FRIDAY, [dict(e, sequence=n) for n, e in enumerate(envelopes[2:], 1)])
        for _ in range(40):
            loop.step()
        return loop

    def cash(self, amount):
        self.fake.summary = [(a, t, amount if t == "TotalCashValue" else v, c) for a, t, v, c in self.fake.summary]

    def test_an_account_unchanged_since_the_previous_close_is_as_of_this_close(self):
        self.two_closes()
        broker = self.sent(BROKER_SNAPSHOT_TYPE)[-1]["payload"]
        self.assertEqual((broker["as_of"], broker["as_of_close"], broker["cash_drift"]), (CLOSE, True, 0))

    def test_holdings_that_no_fill_of_this_run_explains_stamp_the_read_time(self):
        # Shares that arrive with no execution this adapter can see: another
        # client's trade under a client ID that is not the Master API client
        # ID, a transfer, or a split booked overnight.
        def transferred(loop):
            self.fake.positions = [(ACCOUNT, contract(), Decimal("150"), 312.5)]
        self.two_closes(transferred)
        why = self.assert_stamped_at_read_time("SPY", "150", "147")
        self.assertIn("previous close", why)

    def test_a_fill_of_this_run_explains_the_holdings_and_the_cash(self):
        def filled(loop):
            payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.t.01.01",
                       "instrument_id": "SPY", "side": "buy", "quantity": 9, "price": 401.25, "commission": 1.0,
                       "filled_at": "2027-01-14T15:00:00Z"}
            loop.submit(FILL_TYPE, 1, payload, payload["filled_at"])
            self.fake.positions = [(ACCOUNT, contract(), Decimal("156"), 312.5)]
            self.cash("96387.75")
        self.two_closes(filled)
        broker = self.sent(BROKER_SNAPSHOT_TYPE)[-1]["payload"]
        self.assertEqual((broker["as_of"], broker["cash_drift"]), (CLOSE, 0), broker["not_as_of_close"])

    def test_a_small_cash_posting_is_recorded_as_drift(self):
        # A day's interest: no order, no execution, within the bound.
        self.two_closes(lambda loop: self.cash("100012.50"))
        broker = self.sent(BROKER_SNAPSHOT_TYPE)[-1]["payload"]
        self.assertEqual((broker["as_of"], broker["cash_drift"]), (CLOSE, 12.5), broker["not_as_of_close"])

    def test_cash_beyond_the_drift_bound_stamps_the_read_time(self):
        # 0.5% of the close's net liquidation is about $770 here.
        self.two_closes(lambda loop: self.cash("95000.00"))
        why = self.assert_stamped_at_read_time("cash", "-5000")
        self.assertIn("0.5%", why)

    # -- the final reads (ADR 0019: one cutoff) -------------------------------
    def test_an_order_in_the_last_open_order_list_stamps_the_read_time(self):
        class OrderAtTheLastRead(FakeTWS):
            lists = 0
            armed = False

            def req_all_open_orders(self):
                if self.armed:
                    self.lists += 1
                    if self.lists == 3:
                        # Placed by another client after both account reads,
                        # shown only by the last list.
                        self.open_orders = [(0, contract(), order(order_ref="", action="BUY",
                                                                   total_quantity=Decimal("3"), perm_id=4343,
                                                                   client_id=0), SimpleNamespace(status="Submitted"))]
                super().req_all_open_orders()
        fake = OrderAtTheLastRead()
        fake.summary, fake.positions, fake.marks, fake.server_time = (self.fake.summary, self.fake.positions,
                                                                      self.fake.marks, self.fake.server_time)
        self.fake = fake
        loop = self.loop()
        loop.start()
        fake.armed = True
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        for _ in range(40):
            loop.step()
        self.assert_stamped_at_read_time("open order", "perm:4343")

    def test_an_execution_arriving_during_the_last_read_makes_the_adapter_read_again(self):
        class ExecutionAtTheLastRead(FakeTWS):
            armed = False
            positions_read = 0

            def req_positions(self):
                self.positions_read += 1
                super().req_positions()

            def req_executions(self, req_id, account_id):
                if self.armed:
                    self.armed = False
                    ex = execution(exec_id="0004.late.01.01", order_id=0, perm_id=5353, order_ref="", client_id=0,
                                   time="20270114 15:59:59 US/Eastern")
                    self.executions.append((contract(), ex))
                super().req_executions(req_id, account_id)
        fake = ExecutionAtTheLastRead()
        fake.summary, fake.positions, fake.marks, fake.server_time = (self.fake.summary, self.fake.positions,
                                                                      self.fake.marks, self.fake.server_time)
        self.fake = fake
        loop = self.loop()
        loop.start()
        fake.armed, fake.positions_read = True, 0
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        for _ in range(40):
            loop.step()
        self.assertEqual(fake.positions_read, 4, "two reads, then two again after the execution arrived")
        self.assertEqual(self.sent(BROKER_SNAPSHOT_TYPE)[0]["payload"]["as_of"], CLOSE,
                         "an execution before the close, replayed on the second attempt, counts once")

    def test_the_mark_is_the_bars_official_close_with_ibkrs_last_price_beside_it(self):
        self.deliver()
        mark = self.sent(MARK)[0]
        self.assertEqual((mark["schema_version"], mark["event_time"], mark["source"]), (2, CLOSE, "ibkr"))
        self.assertEqual(mark["payload"], {"instrument_id": "SPY", "as_of": CLOSE, "price": OFFICIAL,
                                           "source": "marketdata", "broker_last": IBKR_LAST,
                                           "broker_last_set": True})

    def test_no_market_data_permission_at_ibkr_changes_nothing_decided(self):
        self.fake.marks = {}
        self.fake.mark_errors = {SPY_CON_ID: (354, "synthetic: requested market data is not subscribed")}
        self.deliver()
        broker = self.sent(BROKER_SNAPSHOT_TYPE)[0]["payload"]
        self.assertEqual((broker["as_of"], broker["positions"][0]["mark"]), (CLOSE, OFFICIAL))
        mark = self.sent(MARK)[0]["payload"]
        self.assertEqual((mark["price"], mark["broker_last"], mark["broker_last_set"]), (OFFICIAL, 0, False))

    def test_a_catch_up_batch_marks_and_stamps_only_its_last_session(self):
        self.deliver([date(2027, 1, 13), FRIDAY])
        self.assertEqual([e["type"] for e in self.engine.inputs()],
                         [ACCOUNT_SNAPSHOT, BROKER_SNAPSHOT_TYPE, RECONCILIATION, BAR, CLOSED, BAR, MARK, CLOSED])
        self.assertEqual(self.sent(ACCOUNT_SNAPSHOT)[0]["payload"]["as_of"], CLOSE)
        self.assertEqual([e["payload"]["as_of"] for e in self.sent(MARK)], [CLOSE])

    def assert_stamped_at_read_time(self, *why, mark_close=CLOSE):
        account, broker = self.sent(ACCOUNT_SNAPSHOT)[-1], self.sent(BROKER_SNAPSHOT_TYPE)[-1]["payload"]
        read = broker["read_at"]
        self.assertEqual((account["payload"]["as_of"], broker["as_of"], broker["as_of_close"]), (read, read, False))
        self.assertNotEqual(read, CLOSE)
        for text in why:
            self.assertIn(text, broker["not_as_of_close"])
        # The mark is still the official close, as of the close.
        self.assertEqual(self.sent(MARK)[-1]["payload"]["as_of"], mark_close)
        return broker["not_as_of_close"]

    def test_an_order_working_at_ibkr_after_the_close_stamps_the_read_time(self):
        loop = self.loop()
        # An order placed by hand is no order the engine released, so a start
        # that saw it would stop for a person; this one appears after it.
        loop.start()
        self.fake.open_orders = [(0, contract(), order(order_ref="", action="BUY", total_quantity=Decimal("3"),
                                                        perm_id=4242, client_id=0),
                                  SimpleNamespace(status="Submitted"))]
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        for _ in range(40):
            loop.step()
        self.assert_stamped_at_read_time("open order", "perm:4242")

    def test_an_execution_since_the_close_stamps_the_read_time(self):
        loop = self.loop()
        loop.start()
        # Another client's trade in the account, after the close.
        ex = execution(exec_id="0002.after.01.01", order_id=0, perm_id=5151, order_ref="", client_id=0,
                       time="20270114 16:30:01 US/Eastern")
        self.fake.executions.append((contract(), ex))
        self.fake.commissions[ex.execId] = SimpleNamespace(execId=ex.execId, commission="1.00", currency="USD")
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        for _ in range(40):
            loop.step()
        self.assert_stamped_at_read_time("execution", "0002.after.01.01")

    def test_an_execution_before_the_close_does_not(self):
        ex = execution(exec_id="0003.before.01.01", order_id=0, perm_id=5252, order_ref="", client_id=0,
                       time="20270114 15:59:59 US/Eastern")
        loop = self.loop()
        loop.start()
        self.fake.executions.append((contract(), ex))
        write_stream(self.folder, FRIDAY, session_envelopes([FRIDAY]))
        for _ in range(40):
            loop.step()
        self.assertEqual(self.sent(BROKER_SNAPSHOT_TYPE)[0]["payload"]["as_of"], CLOSE)

    def test_a_reading_on_a_later_date_stamps_the_read_time(self):
        # 3 a.m. New York the next day: IBKR's executions reach back only to
        # midnight, so an execution since the close cannot be ruled out.
        self.fake.server_time = int(datetime(2027, 1, 15, 8, 0, tzinfo=timezone.utc).timestamp())
        self.deliver()
        self.assert_stamped_at_read_time("midnight")

    def test_a_reading_before_the_close_stamps_the_read_time(self):
        self.fake.server_time = int(datetime(2027, 1, 14, 20, 0, tzinfo=timezone.utc).timestamp())
        self.deliver()
        self.assert_stamped_at_read_time("not after the close")

    def test_a_later_batch_follows_the_last_stamp_the_engine_holds(self):
        # The first batch, read on a later date, is stamped at its read time,
        # after the next session's close; that session's batch cannot be
        # stamped as of its close, which would not follow.
        sessions = [date(2027, 1, 13), FRIDAY]
        envelopes = session_envelopes(sessions)
        write_stream(self.folder, sessions[0], [dict(e, sequence=n) for n, e in enumerate(envelopes[:2], 1)])
        self.fake.server_time = EVENING + 120
        loop = self.loop()
        loop.start()
        for _ in range(40):
            loop.step()
        self.assert_stamped_at_read_time("midnight", mark_close="2027-01-13T21:00:00Z")
        write_stream(self.folder, FRIDAY, [dict(e, sequence=n) for n, e in enumerate(envelopes[2:], 1)])
        for _ in range(40):
            loop.step()
        why = self.assert_stamped_at_read_time("already holds")
        stamps = [e["payload"]["as_of"] for e in self.sent(ACCOUNT_SNAPSHOT)]
        self.assertLess(stamps[0], stamps[1], why)

    def test_the_first_batch_carries_the_start_of_run_reconciliation(self):
        self.deliver()
        reconciliation = self.sent(RECONCILIATION)
        self.assertEqual(len(reconciliation), 1)
        payload = reconciliation[0]["payload"]
        self.assertEqual((payload["trigger"], payload["positions"], payload["total_cash"], payload["unreadable"]),
                         ("morning", [{"instrument_id": "SPY", "quantity": 147}], 100000.0, []))
        self.assertEqual(payload["as_of"], self.sent(BROKER_SNAPSHOT_TYPE)[0]["payload"]["read_at"], "at the true read time, after the book")

    def test_a_start_and_a_reconnect_reconcile_once_the_engine_holds_a_book(self):
        loop = self.deliver()
        self.assertEqual(len(self.sent(RECONCILIATION)), 1)
        self.fake.drop()
        for _ in range(40):
            loop.step()
        self.assertEqual(len(self.sent(RECONCILIATION)), 2, "the reconnect reconciles")
        loop.close()
        again = self.loop()
        again.start()
        for _ in range(10):
            again.step()
        reconciliations = self.sent(RECONCILIATION)
        self.assertEqual(len(reconciliations), 3, "the restart reconciles")
        self.assertEqual(len(self.sent(ACCOUNT_SNAPSHOT)), 1, "and delivers nothing again")
        self.assertEqual(len({e["payload"]["reconciliation_id"] for e in reconciliations}), 3)

    def test_a_fresh_start_with_no_book_sends_no_reconciliation_until_its_first_batch(self):
        loop = self.loop()
        loop.start()
        for _ in range(10):
            loop.step()
        self.assertEqual(self.engine.inputs(), [])

    def test_every_later_batch_reconciles_the_reading_before_its_snapshot_replaces_the_book(self):
        # The engine classifies the reading against the book it holds and
        # this run's fills since (ADR 0019): shares no fill explains are
        # unexplained, never adopted by the next account snapshot.
        self.deliver([date(2027, 1, 13)]).close()
        loop = self.loop()
        loop.start()
        write_stream(self.folder, FRIDAY, [dict(e, sequence=n) for n, e in enumerate(session_envelopes([FRIDAY]), 1)])
        for _ in range(40):
            loop.step()
        types = [e["type"] for e in self.engine.inputs()]
        self.assertEqual(types.count(ACCOUNT_SNAPSHOT), 2)
        self.assertEqual(types[-6:], [RECONCILIATION, ACCOUNT_SNAPSHOT, BROKER_SNAPSHOT_TYPE, BAR, MARK, CLOSED])
        reconciliation = self.sent(RECONCILIATION)[-1]["payload"]
        self.assertEqual(reconciliation["as_of"], self.sent(BROKER_SNAPSHOT_TYPE)[-1]["payload"]["read_at"])

    # -- a reconnect while an execution awaits its commission (ADR 0019) -----
    def test_a_reconnect_reconciles_only_after_every_replayed_fill_is_recorded(self):
        released = json.loads((tests_dir / "testdata" / "paper_released.json").read_text())
        self.engine.decisions = {"marketdata:session-closed:{}".format(FRIDAY): [{"type": RELEASED,
                                                                               "payload": released}]}
        self.fake.on_place = self.fake.accept
        loop = self.deliver()
        self.assertEqual(len(self.fake.placed), 1, "setup: the day's order is placed")
        self.fake.fill(7, shares="9", price=401.25, exec_id="0001f4e8.t.01.01", cum_qty="9", commission=None,
                       time="20270114 15:59:00 US/Eastern")
        self.fake.positions = [(ACCOUNT, contract(), Decimal("156"), 312.5)]
        self.fake.drop()
        for _ in range(40):
            loop.step()
        before = len(self.sent(RECONCILIATION))
        self.assertEqual(before, 1, "no reconciliation while the replayed fill awaits its commission")
        self.fake.callbacks.commission_report(mock.Mock(execId="0001f4e8.t.01.01", commission=Decimal("1.00"),
                                                        currency="USD"))
        for _ in range(40):
            loop.step()
        types = [e["type"] for e in self.engine.inputs()]
        self.assertEqual(types.count(RECONCILIATION), 2)
        self.assertLess(types.index(FILL_TYPE), len(types) - 1 - types[::-1].index(RECONCILIATION),
                        "the fill reaches the engine before the reconciliation that shows its shares")


if __name__ == "__main__":
    unittest.main()
