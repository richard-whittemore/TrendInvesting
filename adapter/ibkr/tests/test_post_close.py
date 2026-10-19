"""The post-close reading counts as the close: the owner's decision of 2026-10-18 (ADR 0019, ADR 0025 point 5, ADR 0026 point 1, amended).

After a session's close and before the next open, with no order working or
pending at IBKR for the account and no execution since the close, the
account does not change, so one account read is the account as of that
close. From that one read the adapter delivers, ahead of the day's stream
lines: the engine's account.snapshot stamped as of the close, then the
broker snapshot with the close as its as-of time and the true read time
beside it as evidence; then the file's lines, with market.mark.recorded for
each instrument the last session delivers a bar for, from that bar's
official close (IBKR's last price beside it, never decided from), just
before that session's close. The engine therefore reads account.snapshot,
the bar, the mark, then the close, which decides.

When the condition fails, both snapshots are stamped at the true read time
and the broker snapshot says why, so the engine withholds the session as
stale (ADR 0019's snapshot rule) and the owner is paged with the reason.

The first batch of a run also carries the start-of-run reconciliation
(ADR 0025 point 5, ADR 0019), from the same read, right behind its account
snapshot: the engine has no book before it. Once the engine holds an
account snapshot, the adapter records a reconciliation at every start and
reconnect from the read that reconciles the link.

Synthetic figures only; nothing here reaches IBKR.
"""
import sys
import tempfile
import unittest
from datetime import date, datetime, timezone
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from engine_double import LoggedEngine
from fake_tws import ACCOUNT, FakeTWS, contract, execution, order
from test_connection import Clock, config
from test_delivery import BAR, CLOSED, FRIDAY, RUN, SPY_CON_ID, session_envelopes, write_stream
from test_loop import COMMISSION_WAIT, RECEIVED, SUMMARY, Ids
from loop import AdapterLoop
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
        self.assertEqual(account["payload"], {"account_id": ACCOUNT, "as_of": CLOSE, "net_liquidation": 100000.0,
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

    def test_a_later_batch_carries_no_reconciliation(self):
        self.deliver([date(2027, 1, 13)]).close()
        loop = self.loop()
        loop.start()
        write_stream(self.folder, FRIDAY, [dict(e, sequence=n) for n, e in enumerate(session_envelopes([FRIDAY]), 1)])
        for _ in range(40):
            loop.step()
        types = [e["type"] for e in self.engine.inputs()]
        self.assertEqual(types.count(ACCOUNT_SNAPSHOT), 2)
        self.assertEqual(types[-4:], [BROKER_SNAPSHOT_TYPE, BAR, MARK, CLOSED])


if __name__ == "__main__":
    unittest.main()
