"""The adapter's main loop against the fake gateway and a logging engine stand-in.

The loop connects, reconciles (snapshot.read_account and IBKR's open orders
and executions) before anything is sent, pumps IB Gateway's callbacks,
checks for overdue commission reports after every pump, and sends the
engine each input it records through the relay, so a released order goes
out once and IBKR's reports on it come back as inputs. Synthetic figures
only; nothing here reaches IBKR.
"""
import fcntl
import hashlib
import json
import os
import sys
import tempfile
import threading
import unittest
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from engine_double import CONFIGURATION_HASH, STRATEGY_VERSION, LoggedEngine
from types import SimpleNamespace

from fake_tws import ACCOUNT, FakeTWS, contract, execution, order
from test_connection import Clock, config
from connection import AccountNotManaged
from engine_client import EngineRejected
from relay import EngineUnavailable
from loop import OUTBOX_SUFFIX, AdapterLoop, Outbox, OutboxInvalid, ReconcileIncomplete, ReconcileMismatch, SOURCE
from relay import (RELEASED, EarlierRunInput, HandoverRecordInvalid, PendingInput, ResolveRefused, main as relay_main,
                   read_engine_log, run_identity)
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


class SwitchFollowsLink(FakeTWS):
    """A client whose sending switch reads false while its socket is closed, as one folding the link into it would."""

    def can_place_orders(self):
        return self.can_place and self.connected


class DropsOnTheRelaysOrderRead(FakeTWS):
    """IB Gateway closes the connection as the relay reads IBKR's orders for an order about to be sent.

    The start's reconciliation reads them twice (the account read and the
    order match); the third read is the relay's.
    """

    def __init__(self):
        super().__init__()
        self.reads = 0

    def req_all_open_orders(self):
        self.reads += 1
        if self.reads == 3:
            self.calls.append(("req_all_open_orders",))
            self.drop()
            return
        super().req_all_open_orders()


class Interposed:
    """The engine, with act() run once, inside the next exchange, before the reply returns."""

    def __init__(self, engine, act):
        self.engine = engine
        self.act = act

    def __call__(self):
        self.engine()
        return self

    def decide(self, envelope):
        if self.act is not None:
            act, self.act = self.act, None
            act()
        return self.engine.decide(envelope)


class Unanswered:
    """The engine stops before the adapter has its reply: after committing the input when commits is true, else before."""

    def __init__(self, engine, commits):
        self.engine = engine
        self.commits = commits

    def __call__(self):
        return self

    def decide(self, envelope):
        if self.commits:
            self.engine.decide(envelope)
        raise EngineUnavailable("the engine stopped before it replied")


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

    def loop(self, fake=None, engine=None, engine_log=None, **overrides):
        values = dict(mode="paper", paper_sending_enabled=True, commission_wait=COMMISSION_WAIT)
        values.update(overrides)
        clock = self.clock = Clock()

        def sleep(seconds):
            clock.now += seconds
        loop = AdapterLoop(config(**values), fake or self.fake, engine or self.engine, self.pending,
                           engine_log or self.engine_log,
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
        # The engine's configuration is its log's first input (sequence 1).
        self.assertEqual([e["sequence"] for e in envelopes], [2, 3])
        for e in envelopes:
            self.assertEqual((e["configuration_hash"], e["strategy_version"], e["source"], e["envelope_version"]),
                             (CONFIGURATION_HASH, STRATEGY_VERSION, SOURCE, 1))
        lifecycle = envelopes[1]
        self.assertEqual(lifecycle["event_time"], lifecycle["payload"]["occurred_at"])
        self.assertEqual(lifecycle["recorded_at"], "2026-10-17T13:30:00Z")
        self.assertEqual(read_engine_log(self.engine_log).next_sequence, 4)

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
        # A crash: the engine stopped before it committed the input, and is
        # resumed as the same run.
        loop = self.loop(engine=Unanswered(self.engine, commits=False))
        loop.start()
        self.session_closed(loop)
        with self.assertRaises(EngineUnavailable):
            loop.step()
        waiting = PendingInput(self.pending).load()
        self.assertEqual(waiting["id"], "session:1")
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

    def test_a_link_lost_while_the_engine_decides_holds_the_order_and_sends_it_once_the_link_is_back(self):
        # The socket closes underneath during the exchange, before any
        # callback says so: the order waits, the adapter does not stop.
        fake = SwitchFollowsLink()
        fake.summary = list(SUMMARY)
        fake.on_place = fake.accept
        loop = self.loop(fake, engine=Interposed(self.engine, lambda: setattr(fake, "connected", False)))
        loop.start()
        self.session_closed(loop)
        loop.step()
        self.assertEqual(fake.placed, [])
        self.assertIsNotNone(PendingInput(self.pending).load(), "the input is held on disk")
        fake.connected = True
        self.settle(loop)
        self.assertEqual(len(fake.placed), 1)
        self.assertIsNone(PendingInput(self.pending).load())

    def test_a_link_lost_while_ibkr_is_read_for_a_due_order_holds_it_and_sends_it_once_reconnected(self):
        fake = DropsOnTheRelaysOrderRead()
        fake.summary = list(SUMMARY)
        fake.on_place = fake.accept
        loop = self.loop(fake)
        loop.start()
        self.session_closed(loop)
        loop.step()
        self.assertEqual(fake.placed, [])
        self.assertIsNotNone(PendingInput(self.pending).load(), "the input is held on disk")
        self.settle(loop)
        self.assertEqual(len(fake.called("connect")), 2)
        self.assertEqual(len(fake.placed), 1)
        self.assertIsNone(PendingInput(self.pending).load())

    def test_the_engines_refusal_during_an_exchange_stops_the_adapter_and_keeps_the_input(self):
        class Refusing:
            def __call__(self):
                return self

            def decide(self, envelope):
                raise EngineRejected("invalid", "synthetic refusal")
        loop = self.loop(engine=Refusing())
        loop.start()
        self.session_closed(loop)
        with self.assertRaises(EngineRejected):
            loop.step()
        self.assertEqual(PendingInput(self.pending).load()["id"], "session:1")

    def test_an_open_order_or_trade_at_ibkr_the_journal_does_not_know_stops_before_anything_is_sent(self):
        manual = (0, contract(), order(order_ref="", client_id=0, perm_id=4242, order_type="LMT", lmt_price=1.0),
                  SimpleNamespace(status="Submitted"))
        other_tag = (contract(), execution(order_ref="not-the-engines", order_id=99, perm_id=4343, client_id=5))
        for name, orders, executions in (("open order", [manual], []), ("execution", [], [other_tag])):
            with self.subTest(name):
                fake = self.gateway()
                fake.open_orders = orders
                fake.executions = executions
                loop = self.loop(fake)
                try:
                    with self.assertRaises(ReconcileMismatch) as caught:
                        loop.start()
                finally:
                    loop.close()
                self.assertTrue(loop.conn.needs_reconciliation, "the link is not marked reconciled")
                self.assertIn("unexplained", str(caught.exception))
                self.assertNotIn(ACCOUNT, str(caught.exception))
                self.assertEqual(fake.placed, [])

    def fresh_run(self, previous_log, name):
        """A fresh run's engine in the same directory, its Run approval naming previous_log committed first."""
        engine = LoggedEngine(str(Path(self.dir.name) / name))
        payload = {"account_id": ACCOUNT, "previous_log": previous_log,
                   "previous_log_final_hash": self.final_hash(previous_log),
                   "incident_id": "", "approved_by": "owner", "approved_at": "2026-10-17T14:00:00Z",
                   "boost": {"latched": False}, "incident_in_force": None}
        raw = json.dumps(payload, separators=(",", ":"))
        engine.decide({"id": "operator-run-approved:" + payload["previous_log_final_hash"],
                       "type": "operator.run.approved", "envelope_version": 1, "schema_version": 2,
                       "event_time": "2026-10-17T14:00:00Z", "recorded_at": "2026-10-17T14:00:00Z",
                       "sequence": engine.next_sequence,
                       "source": "engine", "configuration_hash": CONFIGURATION_HASH,
                       "strategy_version": STRATEGY_VERSION, "payload": payload,
                       "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})
        return engine

    def final_hash(self, name):
        """The record hash of the last commit in the log called name beside the others, as the engine approves it."""
        path = Path(self.dir.name) / name
        if not name or not path.is_file():
            return "sha256:" + "2" * 64
        commits = [json.loads(line)["commit"] for line in path.read_text(encoding="utf-8").splitlines()
                   if line.startswith('{"commit"')]
        return commits[-1]["record_hash"]

    def filled_under_the_first_run(self):
        """Run engine.log until its order is sent and filled, then stop the adapter, as before a reboot."""
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        self.fills(self.fake)
        self.settle(loop)
        loop.close()
        self.assertEqual(len(self.engine.inputs(FILL_TYPE)), 2)

    def test_a_fresh_run_after_a_same_day_fill_starts_and_records_nothing_twice(self):
        # IBKR replays the day's executions under this client ID and the
        # first run's orderRef. The fresh run's Run approval names the run
        # before it, whose log released that order and holds its reports.
        self.filled_under_the_first_run()
        second = self.fresh_run("engine.log", "run-2.log")
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.assertFalse(loop.conn.needs_reconciliation)
        self.settle(loop)
        self.assertEqual([e["type"] for e in second.inputs()], ["operator.run.approved"])
        # And a run after that one: the approvals chain back to the order.
        loop.close()
        third = self.fresh_run("run-2.log", "run-3.log")
        loop = self.loop(engine=third, engine_log=third.path)
        loop.start()
        self.settle(loop)
        self.assertFalse(loop.conn.needs_reconciliation)
        self.assertEqual([e["type"] for e in third.inputs()], ["operator.run.approved"])
        self.assertEqual(len(self.fake.placed), 1)

    def test_the_adapter_reads_nothing_and_records_nothing_until_the_engine_is_listening(self):
        # The engine records a fresh run's Run approval before it opens its
        # socket. An adapter that read the log first would see a chain of one
        # run, and IBKR's replay of the earlier run's fills would be journalled
        # into the outbox as new inputs before the approval named that run.
        self.filled_under_the_first_run()
        starting = LoggedEngine(str(Path(self.dir.name) / "run-2.log"))

        def not_listening():
            raise EngineUnavailable("cannot connect to the engine's socket: not listening yet")
        loop = self.loop(engine=not_listening, engine_log=str(starting.path))
        with self.assertRaises(EngineUnavailable):
            loop.start()
        self.assertEqual(loop.outbox.load(), [])
        self.assertEqual(self.fake.placed[1:], [])
        self.assertTrue(loop.conn is None or loop.conn.needs_reconciliation)

    def test_an_approval_whose_final_hash_is_not_the_named_logs_stops_the_adapter(self):
        self.filled_under_the_first_run()
        second = self.fresh_run("engine.log", "run-2.log")
        # The earlier run's log holds another commit than the one approved.
        payload = {"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]}
        raw = json.dumps(payload, separators=(",", ":"))
        self.engine.decide({"id": "session:late", "type": "market.session.closed", "envelope_version": 1,
                            "schema_version": 1, "event_time": SESSION_CLOSED, "recorded_at": SESSION_CLOSED,
                            "sequence": self.engine.next_sequence, "source": SOURCE,
                            "configuration_hash": CONFIGURATION_HASH, "strategy_version": STRATEGY_VERSION,
                            "payload": payload, "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})
        loop = self.loop(engine=second, engine_log=second.path)
        with self.assertRaises(ResolveRefused) as caught:
            loop.start()
        self.assertIn("final record hash", str(caught.exception))
        self.assertEqual(loop.outbox.load(), [])

    def test_a_fresh_run_still_stops_on_an_execution_no_run_released(self):
        self.filled_under_the_first_run()
        self.fake.executions.append((contract(), execution(exec_id="0001f4e8.other.01.01", order_ref="tw-elsewhere",
                                                           order_id=40, perm_id=4444)))
        second = self.fresh_run("engine.log", "run-2.log")
        loop = self.loop(engine=second, engine_log=second.path)
        with self.assertRaises(ReconcileMismatch) as caught:
            loop.start()
        self.assertIn("tw-elsewhere", str(caught.exception))
        self.assertTrue(loop.conn.needs_reconciliation)

    def test_a_run_approval_naming_a_log_that_cannot_be_read_stops_the_adapter(self):
        self.filled_under_the_first_run()
        for name in ("missing.log", "../engine.log", "/tmp/engine.log", "sub/engine.log", ""):
            with self.subTest(previous_log=name):
                second = self.fresh_run(name, "run-{}.log".format(abs(hash(name))))
                loop = self.loop(engine=second, engine_log=second.path)
                try:
                    with self.assertRaises(ResolveRefused):
                        loop.start()
                finally:
                    loop.close()
                self.assertTrue(loop.conn is None or loop.conn.needs_reconciliation)

    def left_waiting_by_the_first_run(self, input_id, commits):
        """The first run (engine.log) stops before the adapter has its reply to input_id; then a fresh run is approved."""
        loop = self.loop(engine=Unanswered(self.engine, commits=commits))
        self.fake.on_place = self.fake.accept
        loop.start()
        loop.submit("market.session.closed", 1, {"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]},
                    SESSION_CLOSED, input_id=input_id)
        with self.assertRaises(EngineUnavailable):
            loop.step()
        loop.close()
        self.assertEqual(PendingInput(self.pending).load()["id"], input_id)
        return self.fresh_run("engine.log", "run-2.log")

    def test_an_earlier_runs_uncommitted_input_stops_a_fresh_run_for_a_person_and_is_never_sent_to_it(self):
        self.filled_under_the_first_run()
        placed = len(self.fake.placed)
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        loop = self.loop(engine=second, engine_log=second.path)
        with self.assertRaises(EarlierRunInput) as caught:
            loop.start()
        message = str(caught.exception)
        for part in ("session:2", "engine.log", "did not commit", "relay.py resolve --run-ended", self.pending,
                     str(second.path)):
            self.assertIn(part, message)
        loop.close()
        self.assertEqual([e["type"] for e in second.received], ["operator.run.approved"])
        self.assertEqual(len(self.fake.placed), placed)

    def test_an_earlier_runs_committed_reply_with_an_unsent_order_stops_naming_it_and_settles_without_sending(self):
        second = self.left_waiting_by_the_first_run("session:1", commits=True)
        loop = self.loop(engine=second, engine_log=second.path)
        with self.assertRaises(EarlierRunInput) as caught:
            loop.start()
        message = str(caught.exception)
        for part in ("session:1", "engine.log", "committed", "tw-paper-1", "never sent"):
            self.assertIn(part, message)
        loop.close()
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        self.assertIsNone(PendingInput(self.pending).load())
        settled = self.loop(engine=second, engine_log=second.path)
        settled.start()
        self.settle(settled)
        self.assertEqual(self.fake.placed, [], "an earlier run's released order is never sent by a fresh run")
        self.assertEqual([e["type"] for e in second.received], ["operator.run.approved"])
        self.assertEqual(settled.outbox.load(), [])
        recorded = PendingInput(self.pending).run_ended()
        self.assertEqual([(r["input"]["id"], r["refused"], r["unsent"]) for r in recorded],
                         [("session:1", "run ended", ["tw-paper-1"])])

    def test_run_ended_is_refused_for_an_input_of_the_current_run(self):
        loop = self.loop(engine=Unanswered(self.engine, commits=False))
        loop.start()
        self.session_closed(loop)
        with self.assertRaises(EngineUnavailable):
            loop.step()
        loop.close()
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                     self.engine_log, "--reason", "not a fresh run"]), 1)
        self.assertEqual(PendingInput(self.pending).load()["id"], "session:1")
        self.assertEqual(loop.outbox.load()[0]["id"], "session:1")
        self.assertEqual(PendingInput(self.pending).run_ended(), [])

    def test_run_ended_is_refused_for_an_input_other_than_the_one_awaiting_its_reply(self):
        self.filled_under_the_first_run()
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:9", "--journal",
                                     str(second.path), "--reason", "a typo"]), 1)
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--reason",
                                     "no log named"]), 1)
        self.assertEqual(PendingInput(self.pending).load()["id"], "session:2")

    def test_after_an_earlier_runs_input_is_settled_the_fresh_runs_own_first_input_goes_through(self):
        self.filled_under_the_first_run()
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        loop.submit("market.session.closed", 1, {"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]},
                    SESSION_CLOSED, input_id="session:3")
        self.settle(loop)
        self.assertEqual([e["id"] for e in second.inputs()][1:], ["session:3"])
        self.assertNotIn("session:2", [e["id"] for e in second.received])
        self.assertEqual(loop.outbox.load(), [])

    def test_run_ended_settles_each_report_on_an_earlier_runs_order_so_none_drains_into_the_fresh_run(self):
        # While the earlier run's engine decided, IBKR's report on an order
        # it released was recorded behind the input awaiting its reply.
        self.filled_under_the_first_run()
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        behind = {"id": "ibkr:execution.order.lifecycle:late", "event_type": LIFECYCLE_TYPE, "schema_version": 2,
                  "event_time": "2026-10-16T20:00:01Z",
                  "payload": {"client_order_id": "tw-paper-1", "status": "canceled",
                              "occurred_at": "2026-10-16T20:00:01Z"}}
        Outbox(self.pending + OUTBOX_SUFFIX).append(behind)
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        self.assertEqual(Outbox(self.pending + OUTBOX_SUFFIX).load(), [], "struck off at the settlement")
        recorded = PendingInput(self.pending).run_ended()
        self.assertEqual([[e["id"] for e in r["outbox"]] for r in recorded], [[behind["id"]]])
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.settle(loop)
        self.assertEqual([e["type"] for e in second.received], ["operator.run.approved"])
        self.assertEqual(loop.outbox.load(), [])

    def test_run_ended_keeps_an_input_recorded_for_the_fresh_run_and_it_is_delivered(self):
        # The session's close, a producer of its own, records its input for
        # the fresh run while the adapter sits stopped on the earlier run's
        # input. Only what provably belongs to the earlier run is struck: a
        # report on an order that run released.
        self.filled_under_the_first_run()
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        outbox = Outbox(self.pending + OUTBOX_SUFFIX)
        report = {"id": "ibkr:execution.order.lifecycle:late", "event_type": LIFECYCLE_TYPE, "schema_version": 2,
                  "event_time": "2026-10-16T20:00:01Z",
                  "payload": {"client_order_id": "tw-paper-1", "status": "canceled",
                              "occurred_at": "2026-10-16T20:00:01Z"}}
        fresh = {"id": "session:3", "event_type": "market.session.closed", "schema_version": 1,
                 "event_time": SESSION_CLOSED, "payload": {"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]}}
        outbox.append(report)
        outbox.append(fresh)
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        self.assertEqual([e["id"] for e in outbox.load()], ["session:3"])
        self.assertEqual([[e["id"] for e in r["outbox"]] for r in PendingInput(self.pending).run_ended()],
                         [[report["id"]]])
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.settle(loop)
        self.assertEqual([e["id"] for e in second.inputs()][1:], ["session:3"])
        self.assertEqual(loop.outbox.load(), [])

    def test_a_stop_as_an_answered_input_is_struck_off_leaves_it_awaiting_so_a_fresh_run_never_takes_it(self):
        # The entry is struck off before the input awaiting its reply is
        # cleared: a stop between the two leaves the input awaiting, which a
        # fresh run refuses (EarlierRunInput), never an outbox entry it would
        # take for its own.
        class Stopped(Exception):
            pass
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        strike = loop.outbox.strike

        def stop_once(input_id):
            loop.outbox.strike = strike
            raise Stopped("the adapter stopped as it struck the input off")
        loop.outbox.strike = stop_once
        with self.assertRaises(Stopped):
            self.settle(loop)
        loop.close()
        placed = len(self.fake.placed)
        second = self.fresh_run("engine.log", "run-2.log")
        restarted = self.loop(engine=second, engine_log=second.path)
        with self.assertRaises(EarlierRunInput):
            restarted.start()
        restarted.close()
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        settled = self.loop(engine=second, engine_log=second.path)
        settled.start()
        self.settle(settled)
        self.assertEqual([e["type"] for e in second.received], ["operator.run.approved"])
        self.assertEqual(settled.outbox.load(), [])
        self.assertEqual(len(self.fake.placed), placed)

    def test_a_restart_seeds_each_orders_status_from_the_oldest_run_to_the_newest(self):
        # Run one journals tw-paper-1 working; run two journals IBKR's
        # cancellation of it. A third run reads both: the later status is the
        # one the journal holds, so IBKR's replay of the cancellation is
        # never recorded again.
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        loop.close()
        second = self.fresh_run("engine.log", "run-2.log")
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.fake.status(7, "Cancelled", filled="0", remaining="9")
        self.settle(loop)
        loop.close()
        self.assertEqual([e["payload"]["status"] for e in second.inputs(LIFECYCLE_TYPE)], ["canceled"])
        third = self.fresh_run("run-2.log", "run-3.log")
        loop = self.loop(engine=third, engine_log=third.path)
        loop.start()
        self.settle(loop)
        self.assertEqual([e["type"] for e in third.received], ["operator.run.approved"])
        self.assertEqual(loop.outbox.load(), [])

    def test_a_struck_working_status_never_outlives_a_later_runs_cancellation_of_the_same_order(self):
        # Run one releases tw-paper-1 and IBKR's working status of it is
        # recorded in the outbox while that run decides; the run stops and
        # resolve --run-ended strikes the status off with its input. Run two
        # commits IBKR's cancellation. A third run seeds the journal by event
        # time across every source, so the cancellation, not the older
        # struck status, is the order's latest, and IBKR's replay of it is
        # never recorded again.
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        loop.close()
        working = self.engine.inputs(LIFECYCLE_TYPE)[-1]
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        Outbox(self.pending + OUTBOX_SUFFIX).append({
            "id": "ibkr:execution.order.lifecycle:replayed", "event_type": LIFECYCLE_TYPE,
            "schema_version": working["schema_version"], "event_time": "2026-10-17T13:29:00Z",
            "payload": dict(working["payload"], occurred_at="2026-10-17T13:29:00Z")})
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        self.assertEqual([[o["id"] for o in r["outbox"]] for r in PendingInput(self.pending).run_ended()],
                         [["ibkr:execution.order.lifecycle:replayed"]])
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.fake.status(7, "Cancelled", filled="0", remaining="9")
        self.settle(loop)
        loop.close()
        self.assertEqual([e["payload"]["status"] for e in second.inputs(LIFECYCLE_TYPE)], ["canceled"])
        third = self.fresh_run("run-2.log", "run-3.log")
        loop = self.loop(engine=third, engine_log=third.path)
        loop.start()
        self.fake.status(7, "Cancelled", filled="0", remaining="9")
        self.settle(loop)
        self.assertEqual([e["type"] for e in third.received], ["operator.run.approved"])
        self.assertEqual(loop.outbox.load(), [])

    def test_a_report_whose_event_time_cannot_be_read_is_seeded_before_every_other(self):
        # It never stands as an order's latest status, and it never stops
        # the seed from ordering the rest by time.
        loop = self.loop()
        status = {"account_id": ACCOUNT, "order_id": "order:1:7", "client_order_id": "tw-paper-1"}
        logs = [SimpleNamespace(inputs=[SimpleNamespace(event_type=LIFECYCLE_TYPE, schema_version=2, payload=dict(
            status, status="canceled", occurred_at="2026-10-17T13:30:00Z"))])]
        for occurred_at in (None, 7, "", "not a time", "2026-10-17T13:31:00"):
            with self.subTest(occurred_at=occurred_at):
                Path(self.pending + OUTBOX_SUFFIX).unlink(missing_ok=True)
                loop.outbox.append({"id": "ibkr:execution.order.lifecycle:odd", "event_type": LIFECYCLE_TYPE,
                                    "schema_version": 2, "event_time": "2026-10-17T13:31:00Z",
                                    "payload": dict(status, status="submitted", occurred_at=occurred_at)})
                self.assertEqual([r.payload["status"] for r in loop._journalled(logs)], ["submitted", "canceled"])

    def test_an_outbox_entry_recorded_at_a_settlement_is_never_sent_even_if_the_strike_was_cut_short(self):
        # A stop between the settlement's record and its strike: the record
        # alone keeps the entry from the fresh run.
        self.filled_under_the_first_run()
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        outbox = Outbox(self.pending + OUTBOX_SUFFIX)
        behind = {"id": "ibkr:execution.order.lifecycle:late", "event_type": LIFECYCLE_TYPE, "schema_version": 2,
                  "event_time": "2026-10-16T20:00:01Z", "payload": {"client_order_id": "tw-paper-1"}}
        outbox.append(behind)
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        if behind["id"] not in {e["id"] for e in outbox.load()}:
            outbox.append(behind)
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.settle(loop)
        self.assertEqual([e["type"] for e in second.received], ["operator.run.approved"])
        self.assertEqual(loop.outbox.load(), [])

    def test_a_settlement_record_whose_outbox_entries_are_not_as_the_adapter_writes_them_is_refused(self):
        self.filled_under_the_first_run()
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        path = Path(self.pending + ".run-ended")
        good = json.loads(path.read_text(encoding="utf-8"))
        for bad in ("not a list", [{"id": "x"}], [7]):
            with self.subTest(outbox=bad):
                path.write_text(json.dumps(dict(good, outbox=bad)) + "\n", encoding="utf-8")
                with self.assertRaises(HandoverRecordInvalid):
                    PendingInput(self.pending).run_ended()

    def test_two_unapproved_starts_are_two_runs_and_the_first_ones_input_stops_the_second(self):
        # Neither run holds a Run approval; each is named by its log's first
        # committed input, the configuration, which a resume copies.
        loop = self.loop(engine=Unanswered(self.engine, commits=False))
        loop.start()
        self.session_closed(loop)
        with self.assertRaises(EngineUnavailable):
            loop.step()
        loop.close()
        other = LoggedEngine(str(Path(self.dir.name) / "other.log"), started_at="2026-10-17T13:30:00Z")
        restarted = self.loop(engine=other, engine_log=str(other.path))
        with self.assertRaises(EarlierRunInput) as caught:
            restarted.start()
        self.assertIn("session:1", str(caught.exception))
        self.assertEqual(other.received, [])

    def test_two_unapproved_starts_with_the_same_configuration_and_as_of_share_one_identity(self):
        # The known residual: with no Run approval, a run is named by its
        # configuration and as-of time, which a second start from the same
        # run.env repeats, so the second start is taken for a resume of the
        # first and is asked for the first one's waiting input. The manual
        # forbids starting a first run again the same day without an
        # approval (a crashed first run is resumed instead).
        loop = self.loop(engine=Unanswered(self.engine, commits=False))
        loop.start()
        self.session_closed(loop)
        with self.assertRaises(EngineUnavailable):
            loop.step()
        loop.close()
        again = LoggedEngine(str(Path(self.dir.name) / "again.log"))
        self.assertEqual(run_identity(str(again.path)), run_identity(self.engine_log))
        restarted = self.loop(engine=again, engine_log=str(again.path))
        restarted.start()
        self.assertEqual([e["id"] for e in again.received], ["session:1"])

    def test_an_approved_fresh_run_resumed_after_a_crash_takes_its_own_input_awaiting_its_reply(self):
        self.filled_under_the_first_run()
        second = self.fresh_run("engine.log", "run-2.log")
        loop = self.loop(engine=Unanswered(second, commits=False), engine_log=second.path)
        loop.start()
        loop.submit("market.session.closed", 1, {"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]},
                    SESSION_CLOSED, input_id="session:3")
        with self.assertRaises(EngineUnavailable):
            loop.step()
        loop.close()
        # The resume copies the crashed run's records, its Run approval
        # included, into a log of its own.
        resumed_path = Path(self.dir.name) / "run-2-resumed.log"
        resumed_path.write_text(Path(second.path).read_text(encoding="utf-8"), encoding="utf-8")
        resumed = LoggedEngine(str(resumed_path))
        restarted = self.loop(engine=resumed, engine_log=str(resumed_path))
        restarted.start()
        self.settle(restarted)
        self.assertEqual([e["id"] for e in resumed.received], ["session:3"])
        self.assertIsNone(PendingInput(self.pending).load())
        self.assertEqual(restarted.outbox.load(), [])

    def test_ibkr_refusing_the_order_in_read_only_mode_reaches_the_engine_as_invalid_with_its_reason(self):
        def read_only(order_id, contract_, order_):
            self.fake.callbacks.error(order_id, 321, "Error validating request.-'bW' : cause - The API interface is "
                                                     "currently in Read-Only mode.")
        self.fake.on_place = read_only
        loop = self.loop()
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        lifecycle = self.engine.inputs(LIFECYCLE_TYPE)
        self.assertEqual(len(lifecycle), 1)
        self.assertEqual((lifecycle[0]["payload"]["status"], lifecycle[0]["payload"]["client_order_id"]),
                         ("invalid", "tw-paper-1"))
        self.assertIn("Read-Only", lifecycle[0]["payload"]["message"])

    def test_an_outbox_entry_under_a_committed_id_with_other_content_stops_the_adapter(self):
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        loop.close()
        changed = {"id": "session:1", "event_type": "market.session.closed", "schema_version": 1,
                   "event_time": SESSION_CLOSED, "payload": {"period_end": SESSION_CLOSED, "instrument_ids": ["QQQ"]}}
        loop.outbox.append(changed)
        restarted = self.loop()
        restarted.start()
        with self.assertRaises(OutboxInvalid) as caught:
            restarted.step()
        self.assertIn("session:1", str(caught.exception))
        self.assertEqual(restarted.outbox.load(), [changed], "the entry is kept for a person")

    def test_the_outbox_is_changed_only_under_its_exclusive_lock(self):
        # A producer in another process takes the same lock (Outbox.append).
        outbox = Outbox(self.pending + OUTBOX_SUFFIX)
        entry = {"id": "session:9", "event_type": "market.session.closed", "schema_version": 1,
                 "event_time": SESSION_CLOSED, "payload": {}}
        fd = os.open(self.pending + OUTBOX_SUFFIX + ".lock", os.O_RDWR | os.O_CREAT, 0o600)
        fcntl.flock(fd, fcntl.LOCK_EX)
        writer = threading.Thread(target=outbox.append, args=(entry,), daemon=True)
        try:
            writer.start()
            writer.join(0.3)
            self.assertTrue(writer.is_alive(), "append waits for the lock")
            self.assertEqual(outbox.load(), [])
        finally:
            fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)
        writer.join(2)
        self.assertFalse(writer.is_alive())
        self.assertEqual(outbox.load(), [entry])

    def test_a_slow_engine_reply_does_not_drop_a_healthy_link(self):
        # A 15 s reply with a 10 s heartbeat timeout: IBKR's answer to the
        # probe arrives during the wait and is read then, not left unread.
        self.engine = LoggedEngine(self.engine_log, {"session:1": []})

        def slow():
            self.fake.callbacks.current_time(self.fake.server_time)
            self.clock.now += 15.0
        loop = self.loop(engine=Interposed(self.engine, slow), heartbeat_interval=30.0, heartbeat_timeout=10.0)
        loop.start()
        self.fake.answer_current_time = False
        loop.step()
        self.session_closed(loop)
        loop.step()
        loop.step()
        self.assertEqual(self.fake.called("disconnect"), [])
        self.assertTrue(loop.conn.connected)
        self.assertEqual([e["id"] for e in self.engine.inputs()], ["session:1"])

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
