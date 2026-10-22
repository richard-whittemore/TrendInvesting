"""The adapter's main loop against the fake gateway and a logging engine stand-in.

The loop connects, reconciles (snapshot.read_account and IBKR's open orders
and executions) before anything is sent, pumps IB Gateway's callbacks,
checks for overdue commission reports after every pump, and sends the
engine each input it records through the relay, so a released order goes
out once and IBKR's reports on it come back as inputs. Synthetic figures
only; nothing here reaches IBKR.
"""
import contextlib
import fcntl
import hashlib
import io
import json
import os
import sys
import tempfile
import threading
import unittest
from unittest import mock
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
from loop import (OUTBOX_SUFFIX, RECONCILE_CUTOFFS, AdapterLoop, Outbox, OutboxInvalid, ReconcileIncomplete, ReconcileMismatch,
                  ReportRecordInvalid, SOURCE)
from relay import (RELEASED, EarlierRunInput, HandoverRecordInvalid, PendingInput, ResolveRefused, main as relay_main,
                   read_engine_log, resolve, retire_order, run_identity, settle_ended_run)
import relay as relay_module
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

    The start's reconciliation reads them three times (the two agreeing
    reads of the order match, then the account read); the fourth read is
    the relay's.
    """

    def __init__(self):
        super().__init__()
        self.reads = 0

    def req_all_open_orders(self):
        self.reads += 1
        if self.reads == 4:
            self.calls.append(("req_all_open_orders",))
            self.drop()
            return
        super().req_all_open_orders()


class LosesIbkrOnTheRelaysOrderRead(DropsOnTheRelaysOrderRead):
    """Gateway loses IBKR's servers as the relay reads IBKR's orders, and restores them on the same socket."""

    def req_all_open_orders(self):
        self.reads += 1
        if self.reads == 4:
            self.calls.append(("req_all_open_orders",))
            self.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
            self.callbacks.error(-1, 1102, "Connectivity between IB and Trader Workstation has been restored - "
                                           "data maintained.")
            return
        FakeTWS.req_all_open_orders(self)


class LosesIbkrAndRestoresLater(DropsOnTheRelaysOrderRead):
    """Gateway loses IBKR's servers as the relay reads IBKR's orders; the restoration comes only when the test sends it."""

    def req_all_open_orders(self):
        self.reads += 1
        if self.reads == 4:
            self.calls.append(("req_all_open_orders",))
            self.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
            return
        FakeTWS.req_all_open_orders(self)


class NeverQuietAfterARead(FakeTWS):
    """Stray open-order ends keep arriving after the reqAllOpenOrders numbered in never_quiet_after, so the link is never quiet.

    Each arrives the next time the adapter waits, after the loop's clock has
    moved on by gap seconds (tick), until the session is dropped.
    """

    def __init__(self):
        super().__init__()
        self.never_quiet_after = set()
        self.tick = None
        # While set, the stream goes on across reconnects: every new
        # session's messages never pause either (an account another client
        # is working, behind a Gateway that keeps the socket up).
        self.never_pauses = False

    def req_all_open_orders(self):
        super().req_all_open_orders()
        if len(self.called("req_all_open_orders")) in self.never_quiet_after:
            self.later_hook = self.tick
            self._later.extend([self.callbacks.open_order_end] * 20)

    def connect(self, *args):
        self.later_hook = None
        super().connect(*args)
        if self.never_pauses and self.connected:
            self.later_hook = self.tick
            self._later.extend([self.callbacks.open_order_end] * 20)


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
        self.assertEqual(len(self.fake.called("req_executions")), matched + 2,
                         "matched again after the reconnect, by two reads that agree")
        self.fills(self.fake)
        self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)
        self.assertEqual([e["type"] for e in self.engine.inputs()],
                         ["market.session.closed", LIFECYCLE_TYPE, FILL_TYPE, FILL_TYPE])

    def test_a_reconnect_with_an_order_working_reads_ibkrs_whole_list_and_stays_reconciled(self):
        # Gateway volunteers this client's orders and an end marker as the
        # new session starts, and sends the list asked for later, in parts.
        loop = self.loop()
        self.fake.on_place = self.fake.accept
        loop.start()
        self.session_closed(loop)
        self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)
        manual = (0, contract(), order(order_ref="", client_id=0, perm_id=4242, order_type="LMT", lmt_price=1.0),
                  SimpleNamespace(status="Submitted"))
        self.fake.volunteer_open_orders = True
        self.fake.open_order_batch = 1
        self.fake.drop()
        with self.assertLogs("ibkr", "INFO") as logged:
            self.settle(loop)
        self.assertTrue(loop.ready(), "matched again: the working order is IBKR's and the engine's")
        self.assertEqual(len(self.fake.called("connect")), 2)
        self.assertEqual(len(self.fake.placed), 1, "never sent again")
        self.assertTrue(any("ibkr reconciled" in line and "open_orders=1 " in line for line in logged.output),
                        logged.output)
        # IBKR's whole list is read: an order there that no run released stops the next reconnect.
        self.fake.open_orders.append(manual)
        self.fake.drop()
        with self.assertRaises(ReconcileMismatch):
            self.settle(loop)
        self.assertEqual(len(self.fake.placed), 1)

    def test_an_order_only_ibkrs_whole_list_shows_stops_the_start_behind_a_volunteered_end(self):
        fake = self.gateway()
        fake.open_orders = [(0, contract(), order(order_ref="", client_id=0, perm_id=4242, order_type="LMT",
                                                  lmt_price=1.0), SimpleNamespace(status="Submitted"))]
        fake.volunteer_open_orders = True
        fake.open_order_batch = 1
        loop = self.loop(fake)
        with self.assertRaises(ReconcileMismatch):
            loop.start()
        self.assertTrue(loop.conn.needs_reconciliation)
        self.assertEqual(fake.placed, [])

    def manual_order(self):
        return (0, contract(), order(order_ref="", client_id=0, perm_id=4242, order_type="LMT", lmt_price=1.0),
                SimpleNamespace(status="Submitted"))

    def test_an_order_only_ibkrs_whole_list_shows_stops_the_start_when_the_volunteered_end_follows_the_request(self):
        fake = self.gateway()
        fake.open_orders = [self.manual_order()]
        fake.volunteer_open_orders = True
        fake.volunteer_after_request = True
        fake.open_order_batch = 1
        loop = self.loop(fake)
        with self.assertRaises(ReconcileMismatch):
            loop.start()
        self.assertTrue(loop.conn.needs_reconciliation)
        self.assertEqual(fake.placed, [])

    def test_the_start_reads_the_open_orders_again_until_two_reads_agree(self):
        # Two reads that disagree, then one that agrees with the read before
        # it, then the account read, which agrees with them: the agreed list
        # is matched.
        fake = self.gateway()
        fake.open_order_answers = [[], [self.manual_order()], [self.manual_order()]]
        fake.open_orders = [self.manual_order()]
        loop = self.loop(fake)
        with self.assertRaises(ReconcileMismatch):
            loop.start()
        self.assertEqual(len(fake.called("req_all_open_orders")), 4)
        self.assertTrue(loop.conn.needs_reconciliation)

    def test_an_account_read_that_never_agrees_with_the_settled_reads_leaves_the_link_not_reconciled(self):
        # Each time two reads agree, the account read after them differs:
        # four account reads, then the link is not reconciled.
        fake = self.gateway()
        fake.open_order_answers = [[self.manual_order()], [self.manual_order()], []] * 4
        loop = self.loop(fake)
        with self.assertRaises(ReconcileIncomplete) as caught:
            loop.start()
        self.assertIn("account read", str(caught.exception))
        self.assertEqual(len(fake.called("req_all_open_orders")), 12)
        self.assertTrue(loop.conn.needs_reconciliation)
        self.assertEqual(fake.placed, [])

    def test_an_order_that_finishes_between_reads_on_a_healthy_link_still_settles(self):
        # A hand-placed order is working, then fills while the reads go on;
        # the account read taken after the two agreeing reads agrees with
        # them, and the link is reconciled.
        fake = self.gateway()
        filled = self.manual_order()[:3] + (SimpleNamespace(status="Filled"),)
        fake.open_order_answers = [[self.manual_order()], [filled], [], []]
        loop = self.loop(fake)
        loop.start()
        self.assertTrue(loop.ready(), "a book that changed and then settled is reconciled")
        self.assertEqual(fake.placed, [])

    def never_quiet(self, fake, *reads):
        fake.never_quiet_after = set(reads)

        def tick():
            self.clock.now += 1.5
        fake.tick = tick

    def test_open_orders_never_quiet_during_the_starts_reconcile_reconnect_and_reconcile_the_new_link(self):
        fake = NeverQuietAfterARead()
        fake.summary = list(SUMMARY)
        loop = self.loop(fake)
        self.never_quiet(fake, 1)
        loop.start()
        self.assertTrue(loop.ready())
        self.assertEqual(len(fake.called("connect")), 2, "the dropped link was made again")
        self.assertEqual(fake.placed, [])

    def test_open_orders_never_quiet_during_a_reconnects_reconcile_reconnect_and_never_end_the_adapter(self):
        fake = NeverQuietAfterARead()
        fake.summary = list(SUMMARY)
        loop = self.loop(fake)
        loop.start()
        self.assertTrue(loop.ready())
        self.never_quiet(fake, len(fake.called("req_all_open_orders")) + 1)
        fake.drop()
        self.settle(loop)
        self.assertTrue(loop.ready())
        self.assertEqual(len(fake.called("connect")), 3, "the dropped link was made again")

    def test_open_orders_that_never_pause_across_reconnects_stop_the_start_for_a_person(self):
        fake = NeverQuietAfterARead()
        fake.summary = list(SUMMARY)
        loop = self.loop(fake)
        self.never_quiet(fake, 1)
        fake.never_pauses = True
        with self.assertRaises(ReconcileIncomplete) as caught:
            loop.start()
        self.assertIn("open-orders-never-quiet", str(caught.exception))
        self.assertEqual(len(fake.called("connect")), RECONCILE_CUTOFFS, "the cut-offs are bounded")
        self.assertTrue(loop.conn.needs_reconciliation)
        self.assertEqual(fake.placed, [])

    def test_open_orders_that_never_pause_after_a_drop_stop_the_adapter_for_a_person(self):
        fake = NeverQuietAfterARead()
        fake.summary = list(SUMMARY)
        loop = self.loop(fake)
        loop.start()
        self.assertTrue(loop.ready())
        self.never_quiet(fake, len(fake.called("req_all_open_orders")) + 1)
        fake.never_pauses = True
        fake.drop()
        with self.assertRaises(ReconcileIncomplete) as caught:
            self.settle(loop)
        self.assertIn("open-orders-never-quiet", str(caught.exception))
        self.assertEqual(len(fake.called("connect")), 1 + RECONCILE_CUTOFFS, "the cut-offs are bounded")
        self.assertEqual(fake.placed, [])

    def test_a_completed_reconcile_resets_the_count_of_cut_off_reconciles(self):
        # Two cut-offs, a completed reconcile, then two more: never three in
        # a row, so the adapter is never stopped (ADR 0025 point 5).
        fake = NeverQuietAfterARead()
        fake.summary = list(SUMMARY)
        loop = self.loop(fake)
        loop.start()
        self.assertTrue(loop.ready())
        for episode in range(2):
            reads = len(fake.called("req_all_open_orders"))
            self.never_quiet(fake, reads + 1, reads + 2)
            fake.drop()
            self.settle(loop)
            self.assertTrue(loop.ready(), "episode {}: the link was not reconciled again".format(episode + 1))
            self.assertEqual(loop._reconcile_cutoffs, 0, "a completed reconcile left the cut-offs counted")
        self.assertEqual(len(fake.called("connect")), 1 + 2 * 3, "each episode: the drop and two cut-offs")
        self.assertEqual(fake.placed, [])

    def test_the_engines_own_fill_during_the_starts_reads_is_read_again_and_the_start_proceeds(self):
        # An order an earlier run released is working at IBKR and fills
        # during the restart's first account read: the reads are taken
        # again, the fill is among the executions re-read, the engine's
        # release explains it, and the start goes on with nothing sent.
        first = self.loop()
        self.fake.on_place = self.fake.accept
        first.start()
        self.session_closed(first)
        self.settle(first)
        first.close()
        self.fake.drop()
        self.fake.on_place = None
        positions = self.fake.req_positions
        filled = []

        def fill_during_the_first_account_read():
            if not filled:
                filled.append(True)
                self.fake.fill(7, shares="9", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="9",
                               commission="1.00", time="20261017 09:30:01 US/Eastern")
            positions()
        self.fake.req_positions = fill_during_the_first_account_read
        executions_asked = len(self.fake.called("req_executions"))
        second = self.loop()
        second.start()
        self.assertTrue(filled, "the fill never came during the account read")
        self.assertTrue(second.ready(), "the engine's own fill stopped the start")
        self.assertGreaterEqual(len(self.fake.called("req_executions")) - executions_asked, 4,
                                "the settled pair was not read again after the fill")
        self.settle(second)
        self.assertEqual(len(self.fake.placed), 1, "nothing is sent again")
        self.assertEqual([e["type"] for e in self.engine.inputs()].count(FILL_TYPE), 1, "the fill is recorded once")

    def test_a_hand_placed_fill_during_the_account_read_is_read_again_and_stops_the_start(self):
        # An order placed by hand fills after the settled pair's executions
        # were read and before the account read ends: it is in neither
        # open-order list, so only the execution shows it. The reads are
        # taken again and the unexplained execution stops the start.
        fake = self.gateway()
        manual = execution(exec_id="0001f4e8.manual.01.01", order_id=0, perm_id=4242, order_ref="", client_id=0)
        positions = fake.req_positions

        def fill_during_the_account_read():
            if not fake.executions:
                fake.executions.append((contract(), manual))
                fake.callbacks.exec_details(-1, contract(), manual)
            positions()
        fake.req_positions = fill_during_the_account_read
        loop = self.loop(fake)
        with self.assertRaises(ReconcileMismatch) as caught:
            loop.start()
        self.assertIn("0001f4e8.manual.01.01", str(caught.exception))
        self.assertTrue(loop.conn.needs_reconciliation)
        self.assertEqual(fake.placed, [])

    def test_open_order_reads_that_disagree_three_times_leave_the_link_not_reconciled(self):
        fake = self.gateway()
        fake.open_order_answers = [[], [self.manual_order()], [], [self.manual_order()]]
        loop = self.loop(fake)
        with self.assertRaises(ReconcileIncomplete) as caught:
            loop.start()
        self.assertIn("did not agree", str(caught.exception))
        self.assertEqual(len(fake.called("req_all_open_orders")), 4)
        self.assertTrue(loop.conn.needs_reconciliation)
        self.assertEqual(fake.placed, [])

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

    def test_an_order_read_cut_off_by_gateway_losing_ibkrs_servers_is_never_followed_on_the_same_link(self):
        # IBKR's open-order list carries no request ID, so its late end
        # would complete the next read on the link Gateway restored.
        fake = LosesIbkrOnTheRelaysOrderRead()
        fake.summary = list(SUMMARY)
        fake.on_place = fake.accept
        loop = self.loop(fake)
        loop.start()
        self.session_closed(loop)
        loop.step()
        self.assertEqual(fake.placed, [])
        self.assertIsNotNone(PendingInput(self.pending).load(), "the input is held on disk")
        self.settle(loop)
        self.assertEqual(len(fake.called("connect")), 2, "the link was made again, not taken back as restored")
        self.assertEqual(len(fake.placed), 1)

    def test_an_order_read_cut_off_by_an_outage_waits_for_the_restoration_before_the_link_is_made_again(self):
        fake = LosesIbkrAndRestoresLater()
        fake.summary = list(SUMMARY)
        fake.on_place = fake.accept
        loop = self.loop(fake, max_connect_attempts=1)
        loop.start()
        self.session_closed(loop)
        loop.step()
        for _ in range(5):
            loop.step()
        self.assertEqual(len(fake.called("connect")), 1, "Gateway is given restore_timeout to restore IBKR")
        self.assertEqual(fake.placed, [])
        fake.callbacks.error(-1, 1102, "Connectivity between IB and Trader Workstation has been restored - "
                                       "data maintained.")
        self.settle(loop)
        self.assertEqual(len(fake.called("connect")), 2, "the restored socket is never read on")
        self.assertEqual(len(fake.placed), 1)

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

    def fresh_run(self, previous_log, name, at=None):
        """A fresh run's engine in the same directory, its Run approval naming previous_log committed first.

        at, when given, is when the run started and was approved.
        """
        engine = LoggedEngine(str(Path(self.dir.name) / name), **({"started_at": at} if at else {}))
        self.approve_run(engine, previous_log, at or "2026-10-17T14:00:00Z")
        return engine

    def approve_run(self, engine, previous_log, at):
        """Commit to engine, at at, a Run approval naming previous_log, the run before it in the same directory."""
        payload = {"account_id": ACCOUNT, "previous_log": previous_log,
                   "previous_log_final_hash": self.final_hash(previous_log),
                   "incident_id": "", "approved_by": "owner", "approved_at": at,
                   "boost": {"latched": False}, "incident_in_force": None}
        raw = json.dumps(payload, separators=(",", ":"))
        engine.decide({"id": "operator-run-approved:" + payload["previous_log_final_hash"],
                       "type": "operator.run.approved", "envelope_version": 1, "schema_version": 2,
                       "event_time": at, "recorded_at": at,
                       "sequence": engine.next_sequence,
                       "source": "engine", "configuration_hash": CONFIGURATION_HASH,
                       "strategy_version": STRATEGY_VERSION, "payload": payload,
                       "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})

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

    def settle_ended_run(self, *extra):
        return relay_main(["settle-ended-run", self.pending, "--journal", self.engine_log, "--by", "owner",
                           "--reason", "the run ended before the adapter sent it"] + list(extra))

    def test_settle_ended_run_records_each_order_the_ended_run_released_and_the_adapter_never_sent(self):
        # The owner's decision of 2026-10-22: an order the ended run released
        # and the adapter never sent is settled for the fresh run's approval,
        # with the adapter's evidence, and marked never sent so no relay ever
        # sends it; settling again writes nothing more (ADR 0019, amended
        # 2026-10-17, reading 4).
        second = self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        settle_ended_run(pending, self.engine_log, "owner", "the run ended before the adapter sent it",
                         now=lambda: datetime(2026, 10, 16, 21, 0, tzinfo=timezone.utc))
        # The line internal/restart ReadSettlements reads (cmd/operator's
        # test reads the same file).
        self.assertEqual(Path(pending.never_sent_path).read_text(encoding="utf-8"),
                         (TESTDATA / "never_sent.jsonl").read_text(encoding="utf-8"))
        self.assertEqual(self.settle_ended_run(), 0, "settling again is not refused")
        lines = pending.never_sent()
        self.assertEqual(len(lines), 1, "settled once, however often the command runs")
        line = lines[0]
        self.assertEqual((line["log"], line["run"], line["input_id"], line["evidence"]),
                         ("engine.log", run_identity(self.engine_log), "session:1", "unmarked"))
        self.assertEqual(line["retirement"], {
            "account_id": "DU0000001", "client_order_id": "tw-paper-1", "instrument_id": "SPY", "side": "buy",
            "quantity": 9, "limit_price": 401.5, "reason": "owner retired",
            "detail": "the run ended before the adapter sent it", "retired_by": "owner",
            "retired_at": line["retirement"]["retired_at"]})
        self.assertRegex(line["retirement"]["retired_at"], r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
        self.assertEqual(pending.handovers(), {"tw-paper-1": "never-sent"})
        # The fresh run's start still settles the input as run ended, and
        # nothing of it is ever sent.
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        settled = self.loop(engine=second, engine_log=second.path)
        settled.start()
        self.settle(settled)
        self.assertEqual(self.fake.placed, [])
        self.assertEqual(len(PendingInput(self.pending).never_sent()), 1, "the settlement is kept as evidence")
        # With nothing awaiting its reply, the run's records still settle the
        # order, once: the line is kept as it was.
        settled.close()
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(Path(pending.never_sent_path).read_text(encoding="utf-8"),
                         (TESTDATA / "never_sent.jsonl").read_text(encoding="utf-8"))

    def test_settle_ended_run_never_settles_an_order_the_adapter_may_have_sent(self):
        # Only an order the record leaves unmarked, or marks never sent, is
        # settled: one marked handed over, sent, or allowed to be sent again
        # may have reached IBKR (fail closed, AGENTS.md).
        # An order the relay recorded IBKR showing (at-broker) was sent, and
        # a never-sent mark no recorded decision of the owner's or the
        # adapter's backs is ambiguous: neither is ever settled.
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        for mark in ("sent", "handing-over", "resend:2026-10-16", "at-broker", "never-sent"):
            with self.subTest(mark=mark):
                pending.mark("tw-paper-1", mark)
                printed = io.StringIO()
                with contextlib.redirect_stdout(printed):
                    self.assertEqual(self.settle_ended_run(), 0)
                self.assertEqual(pending.never_sent(), [])
                self.assertEqual(pending.handovers(), {"tw-paper-1": mark})
                if mark != "sent":
                    self.assertIn("tw-paper-1 not settled", printed.getvalue())
        pending.record_decision({"client_order_id": "tw-paper-1", "decision": "never-sent", "input_id": "session:1",
                                 "reason": "IBKR shows nothing", "at": "2026-10-16T20:30:00Z",
                                 "session_date": "2026-10-16"})
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual([(l["evidence"], l["retirement"]["client_order_id"]) for l in pending.never_sent()],
                         [("never-sent", "tw-paper-1")])

    def test_settle_ended_run_never_settles_an_order_the_relay_found_at_ibkr(self):
        # The relay's stop on a never-sent order IBKR shows (NeverSentAtBroker)
        # records the broker's word durably before it stops; a settlement of
        # the ended run then refuses the order, so the fresh run never sells
        # it a second time (fail closed, AGENTS.md).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        pending.record_refusal("session:1", "tw-paper-1", "no contract ID")
        resolve(pending, "tw-paper-1", "never-sent", "IBKR shows nothing", today=SESSION)
        relay = relay_module.Relay(connect=lambda: self.engine, sender=SimpleNamespace(check=print, send=print),
                                   pending=pending, to_request=lambda order, session: order, today=lambda: SESSION,
                                   broker_orders=lambda: SimpleNamespace(open_orders=[SimpleNamespace(
                                       client_order_id="tw-paper-1")], executions=[]))
        with self.assertRaises(relay_module.NeverSentAtBroker):
            relay.exchange(pending.load())
        relay.close()
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(pending.never_sent(), [])

    def found_at_ibkr_stopped_after(self, write):
        """Retire session:1's order, then stop the relay as IBKR is found showing it, just after one write of the record.

        write names the write the stop follows: "decision" (the at-broker
        decision), "mark" (the at-broker mark) or "strike" (the queued
        retirements struck off). Returns the fresh run's engine.
        """
        second = self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        order = json.loads((TESTDATA / "paper_released.json").read_text())
        retire_order(pending, order, "session over", "its session is over", "adapter",
                     datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc), SESSION, "session:1")
        self.assertEqual(len(self.queued_retirements()), 1)

        def after(real, applies):
            def wrapped(target, *args):
                real(target, *args)
                if applies(*args):
                    raise OSError("stopped after the {} was written".format(write))
            return wrapped
        cls, name, applies = {
            "decision": (PendingInput, "record_decision", lambda d: d.get("decision") == "at-broker"),
            "mark": (PendingInput, "mark", lambda cid, state: state == "at-broker"),
            "strike": (Outbox, "strike_all", lambda ids: bool(list(ids))),
        }[write]
        relay = relay_module.Relay(connect=lambda: self.engine, sender=SimpleNamespace(check=print, send=print),
                                   pending=pending, to_request=lambda order, session: order, today=lambda: SESSION,
                                   broker_orders=lambda: SimpleNamespace(open_orders=[SimpleNamespace(
                                       client_order_id="tw-paper-1")], executions=[]))
        with mock.patch.object(cls, name, after(getattr(cls, name), applies)):
            with self.assertRaises(OSError):
                relay.exchange(pending.load())
        relay.close()
        return second

    def queued_retirements(self):
        return [e for e in Outbox(self.pending + OUTBOX_SUFFIX).load() if e["event_type"] == "execution.order.retired"]

    def test_a_stop_after_any_write_of_the_at_broker_record_leaves_the_order_refused_by_settle_ended_run(self):
        # The broker's word is written durably first (the decision, then the
        # mark) and the queued retirements are struck off last, so a stop or
        # a failed write after any one write leaves the order readable as at
        # the broker: settle-ended-run refuses it, an earlier run's input
        # counts it sent, and the next start, even with IBKR no longer
        # showing it, finishes the record and stops at it with no retirement
        # of it left to reach an engine (fail closed, AGENTS.md; ADR 0026,
        # amended 2026-10-22, "The adapter").
        for write in ("decision", "mark", "strike"):
            with self.subTest(stopped_after=write):
                self.setUp()
                second = self.found_at_ibkr_stopped_after(write)
                pending = PendingInput(self.pending)
                printed = io.StringIO()
                with contextlib.redirect_stdout(printed):
                    self.assertEqual(self.settle_ended_run(), 0)
                self.assertEqual(pending.never_sent(), [])
                self.assertIn("order tw-paper-1 not settled", printed.getvalue())
                ended = relay_module.earlier_run(pending, str(second.path))
                self.assertEqual(ended.unsent, (), "an order the relay found at IBKR was sent")
                restarted = relay_module.Relay(
                    connect=lambda: self.engine, sender=SimpleNamespace(check=print, send=print), pending=pending,
                    to_request=lambda order, session: order, today=lambda: SESSION,
                    broker_orders=lambda: SimpleNamespace(open_orders=[], executions=[]))
                with self.assertRaises(relay_module.NeverSentAtBroker):
                    restarted.exchange(pending.load())
                restarted.close()
                self.assertEqual(pending.handovers(), {"tw-paper-1": "at-broker"})
                self.assertEqual(self.queued_retirements(), [], "no retirement of an order IBKR showed is queued")

    def test_marking_sent_an_order_stopped_at_the_broker_strikes_any_retirement_of_it_still_queued(self):
        # A stop after the at-broker mark and before the strike leaves a
        # retirement queued; the owner's --sent strikes it first, so it never
        # reaches the engine once the order is marked sent (ADR 0026, amended
        # 2026-10-22, "The adapter"; fail closed, AGENTS.md).
        self.found_at_ibkr_stopped_after("mark")
        self.assertEqual(len(self.queued_retirements()), 1)
        self.assertEqual(relay_main(["resolve", self.pending, "tw-paper-1", "--sent", "--reason",
                                     "IBKR shows it working"]), 0)
        self.assertEqual(PendingInput(self.pending).handovers(), {"tw-paper-1": "sent"})
        self.assertEqual(self.queued_retirements(), [])

    def test_marking_sent_a_never_sent_order_records_the_decision_before_it_strikes_any_queued_retirement(self):
        # --sent on an order marked never sent records the owner's finding
        # first and strikes its queued retirements after, mirroring the
        # at-broker record: a failure between the two leaves the order no
        # settlement reads as never sent, and running --sent again finishes
        # (ADR 0026, amended 2026-10-22, "The adapter"; fail closed, AGENTS.md).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        order = json.loads((TESTDATA / "paper_released.json").read_text())
        retire_order(pending, order, "session over", "its session is over", "adapter",
                     datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc), SESSION, "session:1")
        self.assertEqual(len(self.queued_retirements()), 1)
        real = Outbox.strike_all

        def failing(outbox, ids):
            if list(ids):
                raise OSError("the disk failed before the strike")
            real(outbox, ids)
        with mock.patch.object(Outbox, "strike_all", failing):
            with self.assertRaises(OSError):
                resolve(pending, "tw-paper-1", "sent", "IBKR shows it working", today=SESSION)
        self.assertEqual(pending.decisions()[-1]["decision"], "sent")
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(pending.never_sent(), [])
        self.assertIn("order tw-paper-1 not settled", printed.getvalue())
        self.assertEqual(relay_main(["resolve", self.pending, "tw-paper-1", "--sent", "--reason",
                                     "IBKR shows it working"]), 0)
        self.assertEqual(pending.handovers(), {"tw-paper-1": "sent"})
        self.assertEqual(self.queued_retirements(), [])

    def test_a_sent_decision_for_an_earlier_input_does_not_stop_sent_recording_one_for_the_input_awaiting(self):
        # --sent skips its own decision only when the last decision for the
        # order is one recorded for the input awaiting its reply (_last_decided):
        # a sent decision of input A does not stand for input B's, so B's
        # decision is recorded whatever the mark it was settled under, and
        # settle-ended-run reads each input's own decisions (ADR 0026, amended
        # 2026-10-22, "The adapter"; fail closed, AGENTS.md).
        for mark in ("never-sent", "at-broker"):
            with self.subTest(mark=mark):
                self.setUp()
                self.left_waiting_by_the_first_run("session:B", commits=True)
                pending = PendingInput(self.pending)
                order = json.loads((TESTDATA / "paper_released.json").read_text())
                retire_order(pending, order, "session over", "its session is over", "adapter",
                             datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc), SESSION, "session:B")
                pending.record_decision({"client_order_id": order["client_order_id"], "decision": "sent",
                                         "reason": "IBKR shows it working", "at": "2026-10-16T20:31:00+00:00",
                                         "session_date": SESSION.isoformat(), "input_id": "session:A"})
                pending.mark(order["client_order_id"], mark)
                resolve(pending, order["client_order_id"], "sent", "IBKR shows it working", today=SESSION)
                decided = [(d["decision"], d.get("input_id")) for d in pending.decisions()
                           if d["client_order_id"] == order["client_order_id"]]
                self.assertEqual(decided[-1], ("sent", "session:B"), "the sent decision for the awaiting input is recorded")
                self.assertEqual(pending.handovers(), {order["client_order_id"]: "sent"})

    def test_marking_sent_a_never_sent_order_stopped_after_any_step_delivers_no_retirement_on_restart(self):
        # --sent on an order marked never sent marks it at the broker first,
        # then records the owner's finding, strikes its queued retirements and
        # marks it sent last, so a stop after any step leaves the order the
        # restarted relay stops at, whatever IBKR shows by then, with no
        # retirement of it left to reach the engine (ADR 0026, amended
        # 2026-10-22, "The adapter": an order IBKR shows is never retired;
        # fail closed, AGENTS.md).
        steps = {
            "at-broker mark": (PendingInput, "mark", lambda cid, state: state == "at-broker"),
            "decision": (PendingInput, "record_decision", lambda d: d.get("decision") == "sent"),
            "strike": (Outbox, "strike_all", lambda ids: bool(list(ids))),
        }
        for step, (cls, name, applies) in steps.items():
            with self.subTest(stopped_after=step):
                self.setUp()
                self.left_waiting_by_the_first_run("session:1", commits=True)
                pending = PendingInput(self.pending)
                order = json.loads((TESTDATA / "paper_released.json").read_text())
                retire_order(pending, order, "session over", "its session is over", "adapter",
                             datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc), SESSION, "session:1")
                self.assertEqual(len(self.queued_retirements()), 1)
                real = getattr(cls, name)

                def stopped(target, *args, real=real, applies=applies, step=step):
                    real(target, *args)
                    if applies(*args):
                        raise OSError("stopped after the {}".format(step))
                with mock.patch.object(cls, name, stopped):
                    with self.assertRaises(OSError):
                        resolve(pending, "tw-paper-1", "sent", "IBKR shows it working", today=SESSION)
                restarted = relay_module.Relay(
                    connect=lambda: self.engine, sender=SimpleNamespace(check=print, send=print), pending=pending,
                    to_request=lambda order, session: order, today=lambda: SESSION,
                    broker_orders=lambda: SimpleNamespace(open_orders=[], executions=[]))
                with self.assertRaises(relay_module.NeverSentAtBroker):
                    restarted.exchange(pending.load())
                restarted.close()
                self.assertEqual(self.queued_retirements(), [], "no retirement of an order IBKR showed is delivered")
                self.assertEqual(pending.load()["id"], "session:1", "the input stays awaiting its reply")
                self.assertEqual(relay_main(["resolve", self.pending, "tw-paper-1", "--sent", "--reason",
                                             "IBKR shows it working"]), 0)
                self.assertEqual(pending.handovers(), {"tw-paper-1": "sent"})
                self.assertEqual(self.queued_retirements(), [])
                # Running --sent again finishes the stop without recording the
                # owner's finding twice, and the restart between records no
                # finding of its own over the owner's (fail closed, AGENTS.md).
                self.assertEqual([d["decision"] for d in pending.decisions()], {
                    "at-broker mark": ["retired", "at-broker", "sent"],
                    "decision": ["retired", "sent"],
                    "strike": ["retired", "sent"]}[step])

    def test_an_order_marked_at_the_broker_is_never_read_as_settled_never_sent_whatever_its_last_decision_says(self):
        # --sent marks the order at the broker before it records the owner's
        # finding, so a stop between the two leaves the last decision still
        # reading never sent or retired; the mark wins for every reader of
        # the adapter's records: the order is not one the records settle
        # (settled_orders, which bounds the walk back through the runs), an
        # earlier run's input counts it sent, and settle-ended-run refuses
        # it (fail closed, AGENTS.md; ADR 0026, amended 2026-10-22, "The
        # adapter": an order IBKR shows is never retired).
        for settled_as in ("retired", "never-sent"):
            with self.subTest(settled_as=settled_as):
                self.setUp()
                second = self.left_waiting_by_the_first_run("session:1", commits=True)
                pending = PendingInput(self.pending)
                if settled_as == "retired":
                    order = json.loads((TESTDATA / "paper_released.json").read_text())
                    retire_order(pending, order, "session over", "its session is over", "adapter",
                                 datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc), SESSION, "session:1")
                else:
                    pending.record_refusal("session:1", "tw-paper-1", "no contract ID")
                    resolve(pending, "tw-paper-1", "never-sent", "IBKR shows nothing", today=SESSION)
                self.assertIn("tw-paper-1", pending.settled_orders().ended)
                real = PendingInput.mark

                def stopped(target, cid, state):
                    real(target, cid, state)
                    if state == "at-broker":
                        raise OSError("stopped after the at-broker mark")
                with mock.patch.object(PendingInput, "mark", stopped):
                    with self.assertRaises(OSError):
                        resolve(pending, "tw-paper-1", "sent", "IBKR shows it working", today=SESSION)
                self.assertEqual(pending.handovers(), {"tw-paper-1": "at-broker"})
                self.assertEqual(pending.decisions()[-1]["decision"], settled_as)
                self.assertNotIn("tw-paper-1", pending.settled_orders().ended)
                self.assertEqual(relay_module.earlier_run(pending, str(second.path)).unsent, ())
                printed = io.StringIO()
                with contextlib.redirect_stdout(printed):
                    self.assertEqual(self.settle_ended_run(), 0)
                self.assertEqual(pending.never_sent(), [])
                self.assertIn("order tw-paper-1 not settled", printed.getvalue())

    def test_run_ended_reads_no_log_older_than_the_bounded_walk_and_refuses_a_missing_log_the_walk_reads(self):
        # A queued retirement is struck by what the outbox itself holds: a
        # retirement of an order the fresh run never released is an earlier
        # run's, however old, so only the fresh run's log is read for it, and
        # an archived log older than the session-bounded walk (run_logs)
        # never blocks the settlement. A log the bounded walk reads that is
        # missing refuses it, with nothing changed (fail closed, AGENTS.md;
        # ADR 0026, amended 2026-10-22, "The adapter").
        for missing in ("ancient.log", "old.log"):
            with self.subTest(missing=missing):
                self.setUp()
                # ancient.log -> old.log (an earlier day, nothing open) ->
                # engine.log (the run that ended) -> run-2.log (the fresh run).
                self.fresh_run("ancient.log", "old.log", at="2026-10-09T14:00:00Z")
                self.approve_run(self.engine, "old.log", "2026-10-16T13:45:00Z")
                second = self.left_waiting_by_the_first_run("session:1", commits=True)
                pending = PendingInput(self.pending)
                order = json.loads((TESTDATA / "paper_released.json").read_text())
                retire_order(pending, order, "session over", "its session is over", "adapter",
                             datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc), SESSION, "session:1")
                self.assertFalse((Path(self.dir.name) / "ancient.log").exists())
                if missing == "old.log":
                    (Path(self.dir.name) / "old.log").unlink()
                before = [Outbox(self.pending + OUTBOX_SUFFIX).load(), pending.load(), pending.run_ended()]
                code = relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                   str(second.path), "--reason", "a fresh run after a reboot"])
                if missing == "ancient.log":
                    self.assertEqual(code, 0)
                    self.assertEqual(self.queued_retirements(), [])
                    [settled] = pending.run_ended()
                    self.assertEqual([e["id"] for e in settled["outbox"]], ["order-retired:tw-paper-1"])
                else:
                    self.assertEqual(code, 1)
                    self.assertEqual([Outbox(self.pending + OUTBOX_SUFFIX).load(), pending.load(),
                                      pending.run_ended()], before, "nothing is changed")

    def test_run_ended_keeps_a_queued_retirement_of_an_order_the_fresh_run_released_and_it_is_delivered(self):
        # A retirement of an order the fresh run's own log released is the
        # fresh run's, so it stays in the outbox and reaches the fresh run's
        # engine; only an earlier run's are settled with the input (fail
        # closed, AGENTS.md rule 5; ADR 0026, amended 2026-10-22, "The
        # adapter").
        second = self.left_waiting_by_the_first_run("session:1", commits=True)
        own = json.loads((TESTDATA / "paper_released.json").read_text())
        own["client_order_id"] = "tw-paper-2"
        second.decisions["fresh:1"] = [{"type": RELEASED, "payload": own}]
        raw = json.dumps({"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]}, separators=(",", ":"))
        second.decide({"id": "fresh:1", "type": "market.session.closed", "envelope_version": 1,
                       "schema_version": 1, "event_time": SESSION_CLOSED, "recorded_at": "2026-10-17T14:05:00Z",
                       "sequence": second.next_sequence, "source": SOURCE, "configuration_hash": CONFIGURATION_HASH,
                       "strategy_version": STRATEGY_VERSION,
                       "payload": json.loads(raw), "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})
        pending = PendingInput(self.pending)
        order = json.loads((TESTDATA / "paper_released.json").read_text())
        at = datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc)
        retire_order(pending, order, "session over", "its session is over", "adapter", at, SESSION, "session:1")
        retire_order(pending, own, "session over", "its session is over", "adapter", at, SESSION, "fresh:1")
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        self.assertEqual([e["id"] for e in self.queued_retirements()], ["order-retired:tw-paper-2"])
        [settled] = pending.run_ended()
        self.assertEqual([e["id"] for e in settled["outbox"]], ["order-retired:tw-paper-1"])
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.settle(loop)
        loop.close()
        self.assertEqual([(e["type"], e["payload"].get("client_order_id")) for e in second.received
                          if e["type"] == "execution.order.retired"], [("execution.order.retired", "tw-paper-2")])

    def test_run_ended_strikes_a_queued_retirement_of_an_order_the_relay_found_at_ibkr(self):
        # A stop after the at-broker decision or mark, before the strike,
        # leaves a retirement queued; settling the input as run ended
        # records it with the settlement and strikes it off, so it never
        # reaches the fresh run's engine, which never released the order
        # (ADR 0026, amended 2026-10-22, "The adapter"; fail closed, AGENTS.md).
        for write in ("decision", "mark"):
            with self.subTest(stopped_after=write):
                self.setUp()
                second = self.found_at_ibkr_stopped_after(write)
                self.assertEqual(len(self.queued_retirements()), 1)
                self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                             str(second.path), "--reason", "a fresh run after a reboot"]), 0)
                self.assertEqual(self.queued_retirements(), [])
                [settled] = PendingInput(self.pending).run_ended()
                self.assertEqual([e["id"] for e in settled["outbox"]], ["order-retired:tw-paper-1"])

    def test_run_ended_strikes_a_queued_retirement_of_any_order_an_earlier_run_of_the_chain_released(self):
        # A retirement queued for an order an earlier run released, which
        # that run's engine never read, is one the fresh run's engine refuses
        # (it did not release the order), so it is settled with the input and
        # struck off, never drained into the fresh run, even when the
        # session-bounded walk (run_logs) leaves out the run that released it
        # because the retirement settles the order (ADR 0026, amended
        # 2026-10-22, "The adapter"; fail closed, AGENTS.md).
        for walk in ("bounded walk reaches the run", "bounded walk leaves the run out"):
            with self.subTest(walk):
                self.setUp()
                second = self.left_waiting_by_the_first_run("session:1", commits=True)
                pending = PendingInput(self.pending)
                order = json.loads((TESTDATA / "paper_released.json").read_text())
                retire_order(pending, order, "session over", "its session is over", "adapter",
                             datetime(2026, 10, 16, 20, 30, tzinfo=timezone.utc), SESSION, "session:1")
                self.assertEqual(len(self.queued_retirements()), 1)
                omitted = mock.patch.object(relay_module, "_earlier_logs", return_value=[]) \
                    if walk.endswith("out") else contextlib.nullcontext()
                with omitted:
                    self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:1", "--journal",
                                                 str(second.path), "--reason", "a fresh run after a reboot"]), 0)
                self.assertEqual(self.queued_retirements(), [])
                [settled] = PendingInput(self.pending).run_ended()
                self.assertEqual([e["id"] for e in settled["outbox"]], ["order-retired:tw-paper-1"])
                loop = self.loop(engine=second, engine_log=second.path)
                loop.start()
                self.settle(loop)
                loop.close()
                self.assertEqual([e["type"] for e in second.received], ["operator.run.approved"])

    def committed_report(self, event_type, version, payload, input_id=None):
        """A report input engine.log commits after the waiting input, as the ended run's engine read it."""
        raw = json.dumps(payload, separators=(",", ":"))
        self.engine.decide({"id": input_id or "later:" + event_type, "type": event_type, "envelope_version": 1,
                            "schema_version": version, "event_time": SESSION_CLOSED,
                            "recorded_at": SESSION_CLOSED, "sequence": self.engine.next_sequence,
                            "source": SOURCE, "configuration_hash": CONFIGURATION_HASH,
                            "strategy_version": STRATEGY_VERSION, "payload": payload,
                            "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})

    def test_settle_ended_run_prints_only_the_orders_the_owner_must_act_on_and_counts_the_sent_ones(self):
        # An ordinary sent order the ended log reports on needs nothing of the
        # owner: it is counted in one line, never listed as not settled, so
        # the refusals the owner must act on (at-broker here) stand out, and
        # the command closes by saying their sale stays attempted.
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        pending.mark("tw-paper-1", "sent")
        self.committed_report(LIFECYCLE_TYPE, 2, {
            "account_id": "DU0000001", "client_order_id": "tw-paper-1", "order_id": "order:11:7",
            "instrument_id": "SPY", "side": "buy", "status": "submitted", "quantity": 9, "limit_price": 401.5,
            "occurred_at": "2026-10-16T20:30:00Z", "message": ""})
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(printed.getvalue().splitlines(), [
            "1 order(s) the ended run sent or its log already retires: nothing to settle",
            "0 order(s) settled as never sent in {} (0 already settled); approve the fresh run without "
            "-never-sent".format(pending.never_sent_path)])
        pending.mark("tw-paper-1", "at-broker")
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        lines = printed.getvalue().splitlines()
        self.assertEqual(len(lines), 3, lines)
        self.assertTrue(lines[0].startswith("order tw-paper-1 not settled: the relay found IBKR showing it"), lines)
        self.assertEqual(lines[1:], [
            "1 order(s) not settled above: each one's sale stays attempted in the fresh run until you act on it as "
            "its line says",
            "0 order(s) settled as never sent in {} (0 already settled); approve the fresh run without "
            "-never-sent".format(pending.never_sent_path)])

    def test_settle_ended_run_never_settles_a_client_order_id_released_twice_or_decided_in_another_run(self):
        # A client order ID the ended log released more than once may have
        # been sent under one release, and a last decision another run
        # recorded is not this run's evidence: neither is ever settled, so a
        # re-released ID is never retired though sent (fail closed, AGENTS.md).
        self.engine.decisions["session:0"] = [released_order()]
        pending = PendingInput(self.pending)
        self.committed_by_the_first_run("session:0")
        pending.mark("tw-paper-1", "sent")
        pending.clear()
        self.committed_by_the_first_run("session:1")
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(pending.never_sent(), [])
        self.assertIn("order tw-paper-1 not settled: the ended log released it more than once", printed.getvalue())

        self.setUp()
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        pending.mark("tw-paper-1", "never-sent")
        pending.record_decision({"client_order_id": "tw-paper-1", "decision": "never-sent", "input_id": "session:1",
                                 "run": "operator-run-approved:another-run", "reason": "IBKR shows nothing",
                                 "at": "2026-10-16T20:30:00Z", "session_date": "2026-10-16"})
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(pending.never_sent(), [])
        self.assertIn("order tw-paper-1 not settled: its last decision was recorded in run "
                      "operator-run-approved:another-run", printed.getvalue())

    def test_settle_ended_run_with_no_input_waiting_is_refused_for_a_log_that_is_not_the_run_that_ended(self):
        # With nothing awaiting its reply, the run the adapter's decisions
        # last record must be the log's run, so --journal can only name the
        # run that ended (fail closed, AGENTS.md).
        pending = PendingInput(self.pending)
        self.committed_by_the_first_run("session:1")
        pending.record_refusal("session:1", "tw-paper-1", "no contract ID")
        resolve(pending, "tw-paper-1", "never-sent", "IBKR shows nothing under tw-paper-1", today=SESSION)
        pending.clear()
        second = self.fresh_run("engine.log", "run-2.log")
        printed = io.StringIO()
        with contextlib.redirect_stderr(printed):
            self.assertEqual(relay_main(["settle-ended-run", self.pending, "--journal", str(second.path), "--by",
                                         "owner", "--reason", "the wrong log"]), 1)
        self.assertIn("not the run that ended", printed.getvalue())
        self.assertEqual(pending.never_sent(), [])
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual([l["retirement"]["client_order_id"] for l in pending.never_sent()], ["tw-paper-1"])
        # Decisions that record no run cannot say which run ended: refused,
        # saying what is missing and what the owner does, never that the run
        # left nothing to settle (fail closed, AGENTS.md).
        self.setUp()
        pending = PendingInput(self.pending)
        self.committed_by_the_first_run("session:1")
        pending.clear()
        pending.record_decision({"client_order_id": "tw-paper-1", "decision": "never-sent", "input_id": "session:1",
                                 "reason": "IBKR shows nothing", "at": "2026-10-16T20:30:00Z",
                                 "session_date": "2026-10-16"})
        printed = io.StringIO()
        with contextlib.redirect_stderr(printed):
            self.assertEqual(self.settle_ended_run(), 1)
        message = printed.getvalue()
        for part in ("none of the adapter's decisions", "records the run", "sale stays attempted", "by hand",
                     "without -never-sent"):
            self.assertIn(part, message)
        self.assertNotIn("left no order to settle", message)
        self.assertEqual(pending.never_sent(), [])

    def test_settle_ended_run_with_no_input_waiting_refuses_damaged_decisions_naming_the_line(self):
        # With nothing awaiting its reply the decisions are read strictly
        # before the run that ended is found from them: a damaged line is a
        # refusal naming it (HandoverRecordInvalid), never a traceback or a
        # line read past (fail closed, AGENTS.md).
        for damaged in ("[]", "5", '{"client_order_id": "tw-paper-1", "decis'):
            with self.subTest(line=damaged):
                self.setUp()
                pending = PendingInput(self.pending)
                self.committed_by_the_first_run("session:1")
                pending.clear()
                Path(pending.decisions_path).write_text(damaged + "\n", encoding="utf-8")
                printed = io.StringIO()
                with contextlib.redirect_stderr(printed):
                    self.assertEqual(self.settle_ended_run(), 1)
                self.assertIn("refused: {} line 1".format(pending.decisions_path), printed.getvalue())
                self.assertEqual(pending.never_sent(), [])

    def committed_by_the_first_run(self, input_id):
        """An input engine.log commits, as the relay sent it, its reply's orders as the engine scripts them."""
        payload = {"period_end": SESSION_CLOSED, "instrument_ids": ["SPY"]}
        raw = json.dumps(payload, separators=(",", ":"))
        envelope = {"id": input_id, "type": "market.session.closed", "envelope_version": 1, "schema_version": 1,
                    "event_time": SESSION_CLOSED, "recorded_at": SESSION_CLOSED, "sequence": self.engine.next_sequence,
                    "source": SOURCE, "configuration_hash": CONFIGURATION_HASH, "strategy_version": STRATEGY_VERSION,
                    "payload": payload, "payload_hash": hashlib.sha256(raw.encode()).hexdigest()}
        pending = PendingInput(self.pending)
        self.engine.decide(envelope)
        pending.record_run(input_id, run_identity(self.engine_log), "engine.log")
        pending.save(envelope)
        return envelope

    def test_settle_ended_run_settles_a_never_sent_sale_an_earlier_input_of_the_run_released(self):
        # The owner settled the sale never sent at an earlier input, which the
        # relay then cleared; the run ended later, at another input or none.
        # The run's own records still show the sale never sent, so it is
        # settled under the input that released it (the owner's decision of
        # 2026-10-22; ADR 0019, amended 2026-10-17, reading 4).
        for waiting in (True, False):
            with self.subTest(waiting=waiting):
                self.setUp()
                pending = PendingInput(self.pending)
                self.committed_by_the_first_run("session:1")
                pending.record_refusal("session:1", "tw-paper-1", "no contract ID")
                resolve(pending, "tw-paper-1", "never-sent", "IBKR shows nothing under tw-paper-1", today=SESSION)
                pending.clear()
                self.committed_by_the_first_run("session:2")
                if not waiting:
                    pending.clear()
                self.assertEqual(self.settle_ended_run(), 0)
                [line] = pending.never_sent()
                self.assertEqual((line["input_id"], line["evidence"], line["retirement"]["client_order_id"]),
                                 ("session:1", "never-sent", "tw-paper-1"))

    def test_settle_ended_run_strikes_every_retirement_queued_for_the_ended_run(self):
        # A retirement the relay queued for the ended run's engine and never
        # delivered would reach the fresh run, which must refuse it for an
        # order it never released: every one, in either ID form, is struck
        # off with the settlement and recorded with it (ADR 0026, amended
        # 2026-10-22, "The adapter"; ADR 0027, amended 2026-10-17).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        order = json.loads((TESTDATA / "paper_released.json").read_text())
        retire_order(pending, order, "session over", "its session is over", "adapter",
                     datetime(2026, 10, 17, 13, 30, tzinfo=timezone.utc), date(2026, 10, 17), "session:1")
        outbox = Outbox(self.pending + OUTBOX_SUFFIX)
        queued = [e for e in outbox.load() if e["event_type"] == "execution.order.retired"]
        self.assertEqual([e["id"] for e in queued], ["order-retired:tw-paper-1"])
        outbox.append(dict(queued[0], id="order-retired:tw-paper-1:session:1"))
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual([l["retirement"]["client_order_id"] for l in pending.never_sent()], ["tw-paper-1"])
        self.assertEqual([e for e in outbox.load() if e["event_type"] == "execution.order.retired"], [])
        recorded = [d for d in pending.decisions() if d.get("outbox")]
        self.assertEqual([[e["id"] for e in d["outbox"]] for d in recorded],
                         [["order-retired:tw-paper-1", "order-retired:tw-paper-1:session:1"]])

    def test_a_newline_terminated_damaged_settlement_line_is_refused_never_dropped_as_torn(self):
        # Only the file's unterminated last bytes can be a write cut short; a
        # whole damaged line followed by blank space is evidence, so the
        # command refuses and changes nothing (fail closed, AGENTS.md rule 6).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        Path(pending.never_sent_path).write_text("not json\n   ", encoding="utf-8")
        self.assertEqual(self.settle_ended_run(), 1)
        self.assertEqual(Path(pending.never_sent_path).read_text(encoding="utf-8"), "not json\n   ")
        self.assertEqual(pending.handovers(), {})

    def test_a_decision_cut_short_mid_write_leaves_the_decisions_readable_and_settle_ended_run_runs_again(self):
        # A decision is written whole or not at all (a temporary file renamed
        # over the decisions file), so a stop mid-write never leaves a torn
        # line that would stop every later settlement and resolve (fail
        # closed, AGENTS.md).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        real_open = open

        class CutShort:
            def __init__(self, f):
                self.f = f

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                self.f.close()

            def write(self, text):
                self.f.write(text[:len(text) // 2])
                self.f.flush()
                raise OSError("the power went mid-write")

            def __getattr__(self, name):
                return getattr(self.f, name)

        def cut_short(path, mode="r", *args, **kwargs):
            f = real_open(path, mode, *args, **kwargs)
            if str(path).startswith(pending.decisions_path) and mode in ("a", "w", "ab", "wb"):
                return CutShort(f)
            return f
        with mock.patch.object(relay_module, "open", cut_short, create=True):
            with self.assertRaises(OSError):
                settle_ended_run(pending, self.engine_log, "owner", "the run ended")
        pending.settled_orders()
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual([l["retirement"]["client_order_id"] for l in pending.never_sent()], ["tw-paper-1"])

    def test_settle_ended_run_never_settles_on_a_decision_recorded_with_no_input_or_no_run(self):
        # A decision recorded with no input or no run (by a relay before the
        # fields, or edited by hand) cannot say whose release, or which run,
        # it settled, so it is never settlement evidence: the order is listed
        # as not settled with that reason, and its sale stays attempted for
        # the owner to sell by hand (fail closed, AGENTS.md).
        decision = {"client_order_id": "tw-paper-1", "decision": "never-sent", "reason": "IBKR shows nothing",
                    "at": "2026-10-16T20:30:00Z", "session_date": "2026-10-16"}
        for label, recorded, missing in (
                ("no input", decision, "no input"),
                ("no run", dict(decision, input_id="session:1"), "no run")):
            with self.subTest(label):
                self.setUp()
                pending = PendingInput(self.pending)
                self.committed_by_the_first_run("session:1")
                pending.clear()
                Path(pending.decisions_path).write_text(json.dumps(recorded) + "\n", encoding="utf-8")
                self.committed_by_the_first_run("session:2")
                printed = io.StringIO()
                with contextlib.redirect_stdout(printed):
                    self.assertEqual(self.settle_ended_run(), 0)
                self.assertEqual(pending.never_sent(), [])
                self.assertIn("order tw-paper-1 not settled: its last decision (never-sent) records " + missing,
                              printed.getvalue())
                self.assertIn("sale stays attempted", printed.getvalue())
        # The waiting input's never-sent mark backed only by such a decision
        # is refused the same way.
        self.setUp()
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        pending.mark("tw-paper-1", "never-sent")
        Path(pending.decisions_path).write_text(json.dumps(decision) + "\n", encoding="utf-8")
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(pending.never_sent(), [])
        self.assertIn("order tw-paper-1 not settled: its last decision (never-sent) records no input",
                      printed.getvalue())

    def test_settle_ended_run_refuses_an_order_whose_last_decision_records_a_malformed_run(self):
        # A run field that is present but not a run's identifier is damage,
        # never a decision from before the field: the order is refused,
        # never read past the check that the decision is this run's (fail
        # closed, AGENTS.md).
        for run in (42, None, "", " ", ["operator-run-approved:x"]):
            with self.subTest(run=run):
                self.setUp()
                self.left_waiting_by_the_first_run("session:1", commits=True)
                pending = PendingInput(self.pending)
                pending.mark("tw-paper-1", "never-sent")
                Path(pending.decisions_path).write_text(json.dumps({
                    "client_order_id": "tw-paper-1", "decision": "never-sent", "input_id": "session:1", "run": run,
                    "reason": "IBKR shows nothing", "at": "2026-10-16T20:30:00Z",
                    "session_date": "2026-10-16"}) + "\n", encoding="utf-8")
                printed = io.StringIO()
                with contextlib.redirect_stdout(printed):
                    self.assertEqual(self.settle_ended_run(), 0)
                self.assertEqual(pending.never_sent(), [])
                self.assertIn("order tw-paper-1 not settled: its last decision records run {!r}".format(run),
                              printed.getvalue())

    def test_settle_ended_run_is_refused_while_the_ended_runs_engine_still_holds_its_instance_lock(self):
        # The engine holds an exclusive lock on one file in its state folder
        # for as long as it runs, whatever log it writes (cmd/engine
        # lockInstance): here it has resumed the run into another log, so
        # the ended log itself is not held, and the run has still not ended.
        # Nothing is settled for it (fail closed, AGENTS.md).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        lock = str(Path(self.dir.name) / "engine.lock")
        held = os.open(lock, os.O_RDWR)
        fcntl.flock(held, fcntl.LOCK_EX)
        try:
            printed = io.StringIO()
            with contextlib.redirect_stderr(printed):
                self.assertEqual(self.settle_ended_run(), 1)
            self.assertIn("still runs", printed.getvalue())
            self.assertIn(lock, printed.getvalue())
        finally:
            os.close(held)
        self.assertEqual(PendingInput(self.pending).never_sent(), [])
        # With no instance lock at all, no engine of this build wrote the
        # folder, so whether its run ended cannot be proved: refused too.
        os.remove(lock)
        printed = io.StringIO()
        with contextlib.redirect_stderr(printed):
            self.assertEqual(self.settle_ended_run(), 1)
        self.assertIn(lock, printed.getvalue())
        self.assertEqual(PendingInput(self.pending).never_sent(), [])
        Path(lock).touch()
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(len(PendingInput(self.pending).never_sent()), 1)

    def test_settle_ended_run_repairs_a_torn_last_line(self):
        # A write cut short (a power loss under an appending writer) leaves a
        # last line that is not whole; the command drops it, says so, and
        # writes the file whole again, atomically, so the approval can read
        # it (fail closed, AGENTS.md: only a torn tail, never another line).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        Path(pending.never_sent_path).write_text('{"evidence":"unmarked","input_id":"sess', encoding="utf-8")
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        self.assertIn("torn", printed.getvalue())
        self.assertEqual([l["retirement"]["client_order_id"] for l in pending.never_sent()], ["tw-paper-1"])
        self.assertFalse(Path(pending.never_sent_path + ".tmp").exists())
        # A damaged line that is not the torn tail is refused, nothing changed.
        Path(pending.never_sent_path).write_text("not json\n" + Path(pending.never_sent_path).read_text())
        self.assertEqual(self.settle_ended_run(), 1)

    def test_a_settled_order_found_at_ibkr_is_marked_sent_and_its_settlement_withdrawn(self):
        # The owner who finds a settled order at IBKR marks it sent; settling
        # again then withdraws its line, so no approval retires an order that
        # was sent (fail closed, AGENTS.md).
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(len(pending.never_sent()), 1)
        self.assertEqual(relay_main(["resolve", self.pending, "tw-paper-1", "--sent", "--reason",
                                     "IBKR shows it filled"]), 0)
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(pending.never_sent(), [])
        self.assertIn("withdrawn", printed.getvalue())

    def test_settle_ended_run_is_refused_as_root_or_any_user_but_the_owner_of_the_adapters_files(self):
        self.left_waiting_by_the_first_run("session:1", commits=True)
        owner = os.stat(self.pending).st_uid
        for user in (owner + 1, 0):
            with self.subTest(user=user), mock.patch.object(relay_module, "_effective_user", return_value=user):
                with self.assertRaises(ResolveRefused) as refused:
                    settle_ended_run(PendingInput(self.pending), self.engine_log, "owner", "the run ended")
                self.assertIn("owner", str(refused.exception))
        self.assertEqual(PendingInput(self.pending).never_sent(), [])
        self.assertEqual(PendingInput(self.pending).handovers(), {})

    def test_settle_ended_run_names_the_order_the_adapter_recorded_it_refused(self):
        self.left_waiting_by_the_first_run("session:1", commits=True)
        pending = PendingInput(self.pending)
        pending.record_refusal("session:1", "tw-paper-1", "no contract ID")
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual([l["evidence"] for l in pending.never_sent()], ["refused"])

    def test_settle_ended_run_never_settles_an_order_the_ended_log_shows_retired_or_reported(self):
        # The ended run's engine already read the order's retirement, or a
        # venue's report on it (so it was sent): neither is settled again.
        for event_type, version, payload in (
                ("execution.order.retired", 1, {"account_id": "DU0000001", "client_order_id": "tw-paper-1",
                                                "instrument_id": "SPY", "side": "buy", "quantity": 9,
                                                "limit_price": 401.5, "reason": "session over",
                                                "detail": "session over", "retired_by": "adapter",
                                                "retired_at": "2026-10-16T20:30:00Z"}),
                (LIFECYCLE_TYPE, 2, {"account_id": "DU0000001", "client_order_id": "tw-paper-1",
                                     "order_id": "order:11:7", "instrument_id": "SPY", "side": "buy",
                                     "status": "submitted", "quantity": 9, "limit_price": 401.5,
                                     "occurred_at": "2026-10-16T20:30:00Z", "message": ""})):
            with self.subTest(event_type=event_type):
                self.setUp()
                self.left_waiting_by_the_first_run("session:1", commits=True)
                raw = json.dumps(payload, separators=(",", ":"))
                self.engine.decide({"id": "later:" + event_type, "type": event_type, "envelope_version": 1,
                                    "schema_version": version, "event_time": SESSION_CLOSED,
                                    "recorded_at": SESSION_CLOSED, "sequence": self.engine.next_sequence,
                                    "source": SOURCE, "configuration_hash": CONFIGURATION_HASH,
                                    "strategy_version": STRATEGY_VERSION, "payload": payload,
                                    "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})
                self.assertEqual(self.settle_ended_run(), 0)
                self.assertEqual(PendingInput(self.pending).never_sent(), [])

    def test_settle_ended_run_is_refused_for_a_log_that_is_not_the_run_the_input_was_sent_to(self):
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        # engine.log never committed session:2, so it released nothing for it:
        # nothing is settled, and nothing refused, for the run it was sent to.
        self.assertEqual(self.settle_ended_run(), 0)
        self.assertEqual(relay_main(["settle-ended-run", self.pending, "--journal", str(second.path), "--by", "owner",
                                     "--reason", "the wrong log"]), 1)
        self.assertEqual(PendingInput(self.pending).never_sent(), [])
        self.assertEqual(PendingInput(self.pending).load()["id"], "session:2")

    def test_settle_ended_run_is_refused_without_a_person_and_a_reason_and_while_the_adapter_runs(self):
        self.left_waiting_by_the_first_run("session:1", commits=True)
        for by, reason in (("", "a reason"), ("adapter", "a reason"), ("owner", " ")):
            with self.subTest(by=by, reason=reason):
                self.assertEqual(relay_main(["settle-ended-run", self.pending, "--journal", self.engine_log,
                                             "--by", by, "--reason", reason]), 1)
        # An adapter holds the pending file's lock while it runs.
        held = os.open(PendingInput(self.pending).lock_path, os.O_RDWR | os.O_CREAT, 0o600)
        fcntl.flock(held, fcntl.LOCK_EX)
        try:
            self.assertEqual(self.settle_ended_run(), 1)
        finally:
            os.close(held)
        self.assertEqual(PendingInput(self.pending).never_sent(), [])
        self.assertEqual(PendingInput(self.pending).handovers(), {})

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

    def struck_status_then_cancelled(self, struck_at):
        """Run one's working status of tw-paper-1, recorded at struck_at, struck at a settlement; run two commits IBKR's cancellation.

        A third run seeds the journal in the records' durable order (each
        run of the chain, then the settlements of the runs it ended), so the
        cancellation, not the older struck status, is the order's latest
        whatever the struck status's time, and IBKR's replay of it is never
        recorded again.
        """
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
            "schema_version": working["schema_version"], "event_time": struck_at,
            "payload": dict(working["payload"], occurred_at=struck_at)})
        self.assertEqual(relay_main(["resolve", "--run-ended", self.pending, "session:2", "--journal",
                                     str(second.path), "--reason", "a fresh run after a reboot"]), 0)
        self.assertEqual([[o["id"] for o in r["outbox"]] for r in PendingInput(self.pending).run_ended()],
                         [["ibkr:execution.order.lifecycle:replayed"]])
        loop = self.loop(engine=second, engine_log=second.path)
        loop.start()
        self.fake.status(7, "Cancelled", filled="0", remaining="9")
        self.settle(loop)
        loop.close()
        cancelled = second.inputs(LIFECYCLE_TYPE)
        self.assertEqual([e["payload"]["status"] for e in cancelled], ["canceled"])
        self.assertEqual(cancelled[0]["payload"]["occurred_at"], "2026-10-17T13:30:00Z")
        third = self.fresh_run("run-2.log", "run-3.log")
        loop = self.loop(engine=third, engine_log=third.path)
        loop.start()
        self.fake.status(7, "Cancelled", filled="0", remaining="9")
        self.settle(loop)
        self.assertEqual([e["type"] for e in third.received], ["operator.run.approved"])
        self.assertEqual(loop.outbox.load(), [])

    def test_a_struck_working_status_never_outlives_a_later_runs_cancellation_of_the_same_order(self):
        self.struck_status_then_cancelled("2026-10-17T13:29:00Z")

    def test_a_struck_status_of_the_same_second_as_a_later_runs_cancellation_never_outlives_it(self):
        self.struck_status_then_cancelled("2026-10-17T13:30:00Z")

    def test_a_struck_status_stamped_after_a_later_runs_cancellation_by_a_clock_set_back_never_outlives_it(self):
        # The adapter stamps a status with its own clock as it receives it
        # (IBKR's orderStatus carries no time), so a clock set back between
        # the two can stamp the older status the later time.
        self.struck_status_then_cancelled("2026-10-17T13:31:00Z")

    def test_the_input_awaiting_its_reply_is_seeded_in_its_place_at_the_outboxs_head(self):
        # A stop mid-exchange leaves the input awaiting its reply and still
        # at the outbox's head, with IBKR's later reports behind it. Of the
        # same second, the later report is the order's latest status.
        loop = self.loop()
        status = {"account_id": ACCOUNT, "order_id": "order:1:7", "client_order_id": "tw-paper-1",
                  "occurred_at": "2026-10-17T13:30:00Z"}
        submitted = {"id": "ibkr:execution.order.lifecycle:1", "event_type": LIFECYCLE_TYPE, "schema_version": 2,
                     "event_time": "2026-10-17T13:30:00Z", "payload": dict(status, status="submitted")}
        loop.outbox.append(submitted)
        loop.outbox.append(dict(submitted, id="ibkr:execution.order.lifecycle:2",
                                payload=dict(status, status="canceled")))
        PendingInput(self.pending).save({"id": submitted["id"], "type": LIFECYCLE_TYPE, "schema_version": 2,
                                         "recorded_at": "2026-10-17T13:30:00Z", "payload": submitted["payload"]})
        records = loop._journalled([("run-1", SimpleNamespace(inputs=[]))])
        self.assertEqual([r.payload["status"] for r in records], ["submitted", "canceled"])

    def test_a_settlement_is_seeded_after_the_run_it_ended_and_before_the_runs_after_it(self):
        # One of a run older than the chain was made before every run of it.
        loop = self.loop()
        pending = PendingInput(self.pending)

        def status(name, n):
            return {"account_id": ACCOUNT, "order_id": "order:1:{}".format(n), "client_order_id": name,
                    "status": name, "occurred_at": "2026-10-17T13:30:00Z"}

        def settle(run, name, n):
            pending.record_run_ended({
                "input": {"id": "session:" + name, "type": LIFECYCLE_TYPE, "schema_version": 2,
                          "payload": status(name, n)},
                "refused": "run ended", "reason": "a fresh run", "run": run, "unsent": [], "outbox": [],
                "at": "2026-10-17T13:30:00+00:00", "session_date": "2026-10-17"})

        def log(*names):
            return SimpleNamespace(inputs=[SimpleNamespace(id="logged:" + name, event_type=LIFECYCLE_TYPE,
                                                           schema_version=2, payload=status(name, n))
                                           for name, n in names])
        settle("run-0", "older-than-the-chain", 1)
        settle("run-1", "ended-run-1", 2)
        chain = [("run-3", log(("run-3", 4))), ("run-2", log(("run-2", 5))), ("run-1", log(("run-1", 6)))]
        self.assertEqual([r.payload["status"] for r in loop._journalled(chain)],
                         ["older-than-the-chain", "run-1", "ended-run-1", "run-2", "run-3"])

    def test_a_settlement_with_no_recorded_run_stops_the_adapter_at_start(self):
        # Its place among the runs cannot be known, so its statuses are never
        # seeded at a guessed one: a person checks the file.
        PendingInput(self.pending).record_run_ended({
            "input": {"id": "session:9", "type": LIFECYCLE_TYPE, "schema_version": 2,
                      "payload": {"account_id": ACCOUNT, "order_id": "order:1:7", "client_order_id": "tw-paper-1",
                                  "status": "submitted", "occurred_at": "2026-10-17T13:30:00Z"}},
            "refused": "run ended", "reason": "a fresh run", "run": None, "unsent": [], "outbox": [],
            "at": "2026-10-17T13:30:00+00:00", "session_date": "2026-10-17"})
        loop = self.loop()
        try:
            with self.assertRaises(HandoverRecordInvalid) as raised:
                loop.start()
        finally:
            loop.close()
        self.assertIn(self.pending + ".run-ended", str(raised.exception))
        self.assertEqual(self.fake.placed, [])

    def test_a_report_whose_event_time_cannot_be_read_stops_the_adapter_at_start(self):
        # Read strictly, like every other record the adapter keeps: a report
        # the adapter did not write as it writes one is never seeded.
        status = {"account_id": ACCOUNT, "order_id": "order:1:7", "client_order_id": "tw-paper-1",
                  "status": "submitted"}
        for occurred_at in (None, 7, "", "not a time", "2026-10-17T13:31:00"):
            with self.subTest(occurred_at=occurred_at):
                Path(self.pending + OUTBOX_SUFFIX).unlink(missing_ok=True)
                Outbox(self.pending + OUTBOX_SUFFIX).append({
                    "id": "ibkr:execution.order.lifecycle:odd", "event_type": LIFECYCLE_TYPE, "schema_version": 2,
                    "event_time": "2026-10-17T13:31:00Z", "payload": dict(status, occurred_at=occurred_at)})
                loop = self.loop()
                try:
                    with self.assertRaises(ReportRecordInvalid) as raised:
                        loop.start()
                finally:
                    loop.close()
                for part in ("ibkr:execution.order.lifecycle:odd", self.pending + OUTBOX_SUFFIX, "occurred_at"):
                    self.assertIn(part, str(raised.exception))
                self.assertEqual(self.fake.placed, [])

    def test_an_outbox_entry_recorded_at_a_settlement_is_never_sent_even_if_the_strike_was_cut_short(self):
        # A stop between the settlement's record and its strike: the record
        # alone keeps the entry from the fresh run.
        self.filled_under_the_first_run()
        second = self.left_waiting_by_the_first_run("session:2", commits=False)
        outbox = Outbox(self.pending + OUTBOX_SUFFIX)
        behind = {"id": "ibkr:execution.order.lifecycle:late", "event_type": LIFECYCLE_TYPE, "schema_version": 2,
                  "event_time": "2026-10-16T20:00:01Z",
                  "payload": {"client_order_id": "tw-paper-1", "occurred_at": "2026-10-16T20:00:01Z"}}
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
