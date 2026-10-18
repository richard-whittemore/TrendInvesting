"""The adapter's main loop against the fake gateway and a logging engine stand-in.

The loop connects, reconciles (snapshot.read_account and IBKR's open orders
and executions) before anything is sent, pumps IB Gateway's callbacks,
checks for overdue commission reports after every pump, and sends the
engine each input it records through the relay, so a released order goes
out once and IBKR's reports on it come back as inputs. Synthetic figures
only; nothing here reaches IBKR.
"""
import json
import sys
import tempfile
import unittest
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from engine_double import CONFIGURATION_HASH, STRATEGY_VERSION, LoggedEngine
from fake_tws import ACCOUNT, FakeTWS
from test_connection import Clock, config
from connection import AccountNotManaged
from loop import AdapterLoop, ReconcileIncomplete, SOURCE
from relay import RELEASED, PendingInput, read_engine_log
from reports import FILL_TYPE, LIFECYCLE_TYPE, UnreportableOrder

TESTDATA = tests_dir / "testdata"
SESSION = date(2026, 10, 16)
SESSION_CLOSED = "2026-10-16T20:00:00Z"
RECEIVED = datetime(2026, 10, 17, 13, 30, 0, 750000, tzinfo=timezone.utc)
COMMISSION_WAIT = 30.0
SUMMARY = [(ACCOUNT, "NetLiquidation", "100000.00", "USD"), (ACCOUNT, "TotalCashValue", "100000.00", "USD"),
           (ACCOUNT, "ExcessLiquidity", "100000.00", "USD")]


def released_order():
    return {"type": RELEASED, "payload": json.loads((TESTDATA / "paper_released.json").read_text())}


class Ids:
    def __init__(self):
        self.n = 0

    def __call__(self, event_type):
        self.n += 1
        return "ibkr:{}:{}".format(event_type, self.n)


class LoopTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.pending = str(Path(self.dir.name) / "pending.json")
        self.engine_log = str(Path(self.dir.name) / "engine.log")
        self.received = RECEIVED
        self.engine = LoggedEngine(self.engine_log, {"session:1": [released_order()]})
        self.fake = self.gateway()

    def gateway(self):
        fake = FakeTWS()
        fake.summary = list(SUMMARY)
        return fake

    def loop(self, fake=None, **overrides):
        values = dict(mode="paper", paper_sending_enabled=True, commission_wait=COMMISSION_WAIT)
        values.update(overrides)
        clock = Clock()

        def sleep(seconds):
            clock.now += seconds
        loop = AdapterLoop(config(**values), fake or self.fake, self.engine, self.pending, self.engine_log,
                           now=lambda: self.received, today=lambda: SESSION, sleep=sleep, monotonic=clock, poll=0,
                           new_id=Ids())
        self.addCleanup(loop.close)
        return loop

    def settle(self, loop, steps=60):
        for _ in range(steps):
            loop.step()

    def session_closed(self, loop):
        loop.submit("market.session.closed", 1, {"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]},
                    SESSION_CLOSED, input_id="session:1")

    def fills(self, fake):
        fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4", commission="1.00",
                  time="20261017 09:30:01 US/Eastern")
        fake.fill(7, shares="5", price=401.3, exec_id="0001f4e8.paper.02.01", cum_qty="9", commission="0.35",
                  time="20261017 09:30:04 US/Eastern")

    def test_one_released_order_is_sent_once_and_its_status_and_fills_reach_the_engine(self):
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        self.fills(self.fake)
        self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)
        order_id, contract, order = self.fake.placed[0]
        self.assertEqual((order_id, contract.conId, order.orderRef, order.orderType, order.tif, order.account),
                         (7, 756733, "tw-paper-1", "LMT", "DAY", ACCOUNT))
        self.assertEqual([e["type"] for e in self.engine.inputs()],
                         ["market.session.closed", LIFECYCLE_TYPE, FILL_TYPE, FILL_TYPE])
        self.assertEqual([e["payload"]["fill_id"] for e in self.engine.inputs(FILL_TYPE)],
                         ["0001f4e8.paper.01.01", "0001f4e8.paper.02.01"])
        # The records are the ones the engine's own test reads (internal/event).
        expected = json.loads((TESTDATA / "paper_reports.json").read_text())
        self.assertEqual([{"event_type": e["type"], "schema_version": e["schema_version"], "payload": e["payload"]}
                          for e in self.engine.inputs()[1:]], expected)
        self.assertIsNone(PendingInput(self.pending).load())
        self.assertEqual(loop.outbox.load(), [])

    def test_every_input_carries_the_runs_identity_the_next_sequence_and_the_adapters_name(self):
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        envelopes = self.engine.inputs()
        self.assertEqual([e["sequence"] for e in envelopes], [1, 2])
        for e in envelopes:
            self.assertEqual((e["configuration_hash"], e["strategy_version"], e["source"], e["envelope_version"]),
                             (CONFIGURATION_HASH, STRATEGY_VERSION, SOURCE, 1))
        lifecycle = envelopes[1]
        self.assertEqual(lifecycle["event_time"], lifecycle["payload"]["occurred_at"])
        self.assertEqual(lifecycle["recorded_at"], "2026-10-17T13:30:00Z")
        self.assertEqual(read_engine_log(self.engine_log).next_sequence, 3)

    def test_nothing_is_sent_before_ibkr_is_read_and_matched(self):
        loop = self.loop()
        loop.start()
        calls = [c[0] for c in self.fake.calls]
        self.assertLess(calls.index("req_account_summary"), len(calls))
        self.assertIn("req_executions", calls)
        self.assertFalse(loop.conn.needs_reconciliation)

    def test_a_read_of_the_account_with_anything_unread_stops_before_any_order(self):
        self.fake.summary = SUMMARY[:1]
        loop = self.loop()
        with self.assertRaises(ReconcileIncomplete) as caught:
            loop.start()
        self.assertIn("total-cash", str(caught.exception))
        self.assertTrue(loop.conn.needs_reconciliation)
        self.assertEqual(self.fake.placed, [])

    def test_a_login_that_does_not_manage_the_account_stops_at_start(self):
        self.fake.managed_accounts = ("DU0000002",)
        loop = self.loop()
        with self.assertRaises(AccountNotManaged):
            loop.start()

    def test_a_missing_commission_report_stops_the_loop_on_time_with_no_further_message(self):
        # The invariant OrderReportJournal states: the loop checks for
        # overdue commission reports after every pump, so the stop does not
        # wait for IBKR's next message.
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        self.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4", commission=None,
                       time="20261017 09:30:01 US/Eastern")
        self.settle(loop)
        self.received += timedelta(seconds=COMMISSION_WAIT)
        with self.assertRaises(UnreportableOrder):
            loop.step()

    def test_a_reconnect_reconciles_again_and_never_resends(self):
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        matched = len(self.fake.called("req_executions"))
        self.fake.drop()
        self.settle(loop)
        self.assertTrue(loop.conn.connected)
        self.assertFalse(loop.conn.needs_reconciliation)
        self.assertEqual(len(self.fake.called("connect")), 2)
        self.assertEqual(len(self.fake.called("req_executions")), matched + 1, "matched again after the reconnect")
        self.fills(self.fake)
        self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)
        self.assertEqual([e["type"] for e in self.engine.inputs()],
                         ["market.session.closed", LIFECYCLE_TYPE, FILL_TYPE, FILL_TYPE])

    def test_a_restart_sends_nothing_twice_and_records_nothing_twice(self):
        first = self.loop()
        self.fake.on_place = self.fake.accept
        first.start()
        self.session_closed(first)
        self.settle(first)
        self.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4", commission="1.00",
                       time="20261017 09:30:01 US/Eastern")
        self.settle(first)
        first.close()
        self.fake.drop()
        second = self.loop()
        second.start()
        self.settle(second)
        self.fake.fill(7, shares="5", price=401.3, exec_id="0001f4e8.paper.02.01", cum_qty="9", commission="0.35",
                       time="20261017 09:30:04 US/Eastern")
        self.settle(second)
        self.assertEqual(len(self.fake.placed), 1)
        self.assertEqual([e["type"] for e in self.engine.inputs()],
                         ["market.session.closed", LIFECYCLE_TYPE, FILL_TYPE, FILL_TYPE])

    def test_an_input_left_awaiting_its_reply_is_asked_for_again_first_byte_for_byte(self):
        loop = self.loop()
        loop.start()
        self.session_closed(loop)
        waiting = loop._envelope(loop.outbox.load()[0], read_engine_log(self.engine_log))
        PendingInput(self.pending).save(waiting)
        loop.close()
        restarted = self.loop()
        self.fake.on_place = self.fake.accept
        restarted.start()
        self.assertEqual(self.engine.received[0], waiting)
        self.settle(restarted)
        self.assertEqual(len(self.fake.placed), 1)
        self.assertEqual([e["id"] for e in self.engine.inputs()][:1], ["session:1"])
        self.assertEqual(restarted.outbox.load(), [])

    def test_an_input_the_engine_already_committed_is_never_sent_again(self):
        # A stop after the engine answered and before the adapter struck the
        # input off its outbox: the engine's log shows it committed.
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        entry = loop.outbox.load()[0]
        self.settle(loop)
        loop.close()
        loop.outbox.append(entry)
        restarted = self.loop()
        restarted.start()
        self.settle(restarted)
        self.assertEqual([e["id"] for e in self.engine.received].count("session:1"), 1)
        self.assertEqual(restarted.outbox.load(), [])

    def test_a_dry_run_sends_nothing(self):
        self.engine = LoggedEngine(self.engine_log, {"session:1": []})
        loop = self.loop(mode="dry-run", paper_sending_enabled=False)
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        self.assertEqual(self.fake.placed, [])
        self.assertEqual([e["id"] for e in self.engine.inputs()], ["session:1"])


if __name__ == "__main__":
    unittest.main()
