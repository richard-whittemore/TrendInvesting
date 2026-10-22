"""Paper trading against the fake gateway: one released order goes out once, and IBKR's reports come back as inputs.

The sending interface (sender.PaperSender behind relay.Relay) is exercised
here against tests/fake_tws.py, never a real IB Gateway. The released order
is testdata/paper_released.json, and the records journalled from IBKR's
answers are testdata/paper_reports.json and paper_rejected.json, which the
engine's own test reads by the rule it reads a venue's reports by
(internal/event, TestTheIBKRAdaptersPaperReportsAreReadByOrderReports). The
retirement of that order once its session is over is
testdata/paper_retired.json, which the engine's test retires the order by
(TestTheIBKRAdaptersRetirementIsReadByOrderReports).
"""
import functools
import json
import sys
import tempfile
import unittest
from unittest import mock
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from fake_tws import ACCOUNT, contract, execution, order
from test_connection import Harness
from test_relay import Restarts, log_group, log_header
from connection import AccountNotManaged
from orders import (CommissionReport, OrderStatusReport, OrderTracker, collect_broker_orders, execution_report,
                    open_order_record)
from relay import (OUTBOX_SUFFIX, RELEASED, HandoverUnconfirmed, OrderUnsendable, Outbox, PendingInput, PendingInUse,
                   Relay, main as relay_main)
from reports import FILL_TYPE, FILL_VERSION, LIFECYCLE_TYPE, LIFECYCLE_VERSION, OrderReportJournal, UnreportableOrder
from sender import NotReconciled, PaperSender, SendRefused, SentRecord, release_to_request

TESTDATA = tests_dir / "testdata"
CON_IDS = {"SPY": 756733}
# The session the order is released in, at its close (16:00 in New York),
# and its time as the input the engine answers with the release.
SESSION = date(2026, 10, 16)
SESSION_CLOSED = "2026-10-16T20:00:00Z"
# The adapter's receipt time of an order status, which IBKR does not stamp:
# the next session's open, when the order works and fills.
RECEIVED = datetime(2026, 10, 17, 13, 30, 0, 750000, tzinfo=timezone.utc)
# How long an execution may wait for its commission report (seconds).
COMMISSION_WAIT = 30.0


def fixture(name):
    return json.loads((TESTDATA / name).read_text())


def as_json(records):
    return [{"event_type": r.event_type, "schema_version": r.schema_version, "payload": r.payload}
            for r in records]


class Died(Exception):
    """The adapter process stopping at this point."""


class PaperAdapter:
    """The paper adapter's parts, wired as its loop wires them, over one fake gateway.

    The engine (test_relay.Restarts) answers the input session:1 with the
    released order in paper_released.json. Its decisions survive a restart
    of this adapter, as the engine's durable log does.
    """

    def __init__(self, directory, harness=None, engine=None, inputs=None):
        self.h = harness or Harness(mode="paper", commission_wait=COMMISSION_WAIT)
        self.conn = self.h.conn
        self.journal = []
        # The records journalled for the engine, durable across a restart:
        # a restarted adapter is handed the ones written before it.
        self.inputs = [] if inputs is None else inputs
        self.received = RECEIVED
        self.today = SESSION
        self.pending = PendingInput(str(Path(directory) / "pending.json"))
        # The engine's durable log, as the owner's commands read it.
        self.engine_journal = str(Path(directory) / "engine.log")
        # A lifecycle status whose journal write fails, as a stop would.
        self.fail_on = None
        self.die_after_place = False
        self.sender = PaperSender(self.conn, self._journal_sent, today=lambda: self.today)
        self.engine = engine or Restarts({}, {"session:1": [{"type": RELEASED, "payload": fixture("paper_released.json")}]})
        # The relay takes the adapter's exclusive lock first (PendingInUse if
        # another adapter has it), and asks the sender's check before it marks
        # an order about to be sent, so an order the sender would refuse
        # leaves no mark.
        self.relay = Relay(self.engine, self.sender, self.pending, lambda: collect_broker_orders(self.conn),
                           lambda o, session: release_to_request(o, "paper", self.conn.config.con_ids, session),
                           now=lambda: self.received)
        self.outbox = Outbox(self.pending.path + OUTBOX_SUFFIX)
        # Only then are the held reports read, and journalled if their fills
        # already are: the held file is this adapter's alone.
        try:
            self.reports = OrderReportJournal(
                self.conn.config.account_id, self.conn.config.client_id, self._journal_input,
                now=lambda: self.received, commission_wait=self.conn.config.commission_wait,
                held_path=str(Path(directory) / "journal.held"), owns_lock=self.relay.holds,
                ready=lambda: self.conn.connected and not self.conn.needs_reconciliation,
                journalled=list(self.inputs), acknowledged=self.pending.acknowledged_corrections(),
                late_fills=self.pending.acknowledged_late_fills())
        except BaseException:
            self.relay.close()
            raise
        self.tracker = OrderTracker(self.conn.config.account_id, self.conn.config.client_id,
                                    self.reports.on_record)
        self.conn.subscribe(self.tracker.on_event)

    def _journal_input(self, record):
        if self.fail_on is not None and record.payload.get("status") == self.fail_on:
            raise OSError("the engine did not take the {} report".format(self.fail_on))
        self.inputs.append(record)
        # The engine's durable log streams each input as it is committed
        # (ADR 0027), so the owner's commands read it while the engine runs.
        lines = [] if Path(self.engine_journal).exists() else [log_header()]
        lines += log_group(len(self.inputs), [("input", record.event_type, record.schema_version, record.payload)])
        with open(self.engine_journal, "a", encoding="utf-8") as f:
            f.write("".join(lines))

    def _journal_sent(self, record):
        if self.die_after_place:
            raise Died("stopped after IBKR took order {}".format(record.request.client_order_id))
        self.journal.append(record)
        self.reports.sent(record)

    def start(self):
        if not self.conn.connected:
            self.conn.connect()
        collect_broker_orders(self.conn)
        self.conn.mark_reconciled()

    def exchange(self):
        return self.relay.exchange({"id": "session:1", "type": "market.session.closed", "sequence": 3,
                                    "event_time": SESSION_CLOSED})

    def drain(self):
        while self.conn.pump(0) is not None:
            pass

    def placed(self):
        return len(self.h.fake.called("place_order"))


class PaperSendingTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)

    def adapter(self, **kw):
        a = PaperAdapter(self.dir.name, **kw)
        self.addCleanup(a.relay.close)
        return a

    def restart(self, a):
        """A new adapter process on a's gateway, pending file, engine log and journalled records."""
        restarted = Harness(mode="paper", commission_wait=COMMISSION_WAIT)
        restarted.fake = a.h.fake
        restarted.conn.api = a.h.fake
        return self.adapter(harness=restarted, engine=a.engine, inputs=a.inputs)

    def sent_and_working(self):
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.start()
        a.exchange()
        a.drain()
        return a

    def test_a_restart_after_reports_and_fills_were_journalled_writes_none_of_them_again(self):
        # IBKR replays each working order's status and each execution's
        # commission on the restarted adapter's reads; what was journalled
        # before the restart is not journalled again.
        a = self.sent_and_working()
        a.h.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4", commission="1.00",
                      time="20261017 09:30:01 US/Eastern")
        a.drain()
        self.assertEqual(len(a.inputs), 2)
        a.relay.close()
        b = self.restart(a)
        b.start()
        b.exchange()
        b.drain()
        self.assertEqual(len(b.inputs), 2, "no record is written twice across the restart")
        b.h.fake.fill(7, shares="5", price=401.3, exec_id="0001f4e8.paper.02.01", cum_qty="9", commission="0.35",
                      time="20261017 09:30:04 US/Eastern")
        b.drain()
        self.assertEqual(as_json(b.inputs), fixture("paper_reports.json"))
        self.assertEqual(b.placed(), 1)

    def test_a_corrected_execution_stops_for_a_person_and_is_never_a_second_fill(self):
        # IBKR corrects an execution by reporting it again under the same
        # execId base with a later last segment; the original stands.
        a = self.sent_and_working()
        a.h.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4",
                      time="20261017 09:30:01 US/Eastern")
        a.drain()
        a.h.fake.fill(7, shares="4", price=401.2, exec_id="0001f4e8.paper.01.02", cum_qty="4",
                      time="20261017 09:30:01 US/Eastern")
        with self.assertRaises(UnreportableOrder):
            a.drain()
        self.assertEqual([r.payload["fill_id"] for r in a.inputs if r.event_type == FILL_TYPE],
                         ["0001f4e8.paper.01.01"])

    def test_an_execution_whose_commission_never_arrives_stops_for_a_person(self):
        a = self.sent_and_working()
        a.h.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4", commission=None,
                      time="20261017 09:30:01 US/Eastern")
        a.drain()
        a.received += timedelta(seconds=COMMISSION_WAIT - 1)
        a.reports.check_overdue()
        a.received += timedelta(seconds=1)
        with self.assertRaises(UnreportableOrder):
            a.reports.check_overdue()
        self.assertEqual([r.event_type for r in a.inputs], [LIFECYCLE_TYPE])

    def test_a_release_left_waiting_past_its_session_is_retired_and_never_sent(self):
        # Refused yesterday for the link, the input is asked again today: its
        # order was for yesterday's session, so it is refused before the mark
        # and, by the owner's decision of 2026-10-22, retired by the adapter
        # itself: journalled for the engine as a refusal for "session over"
        # (testdata/paper_retired.json), never a cancel of an order at IBKR,
        # and the adapter goes on past the input without the owner.
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.conn.connect()
        with self.assertRaises(NotReconciled):
            a.exchange()
        a.today = date(2026, 10, 17)
        a.start()
        a.exchange()
        a.drain()
        self.assertEqual(a.placed(), 0)
        self.assertEqual(a.conn.next_order_id, 7)
        self.assertIsNone(a.pending.load(), "the adapter went on past the input")
        self.assertEqual([{"event_type": e["event_type"], "schema_version": e["schema_version"],
                           "payload": e["payload"]} for e in a.outbox.load()], fixture("paper_retired.json"))
        self.assertEqual(a.inputs, [], "IBKR never received it, so it reports nothing")
        self.assertEqual([d["decision"] for d in a.pending.decisions()], ["retired"])

    def test_an_order_ibkr_can_never_accept_stops_until_the_owner_retires_it(self):
        # No IBKR contract ID is configured for the stock: the order can never
        # be sent as it stands, so the adapter stops at it, every time, until
        # the owner retires it; the retirement is journalled for the engine
        # with the owner's reason, and the adapter then goes on.
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.conn.config = a.conn.config._replace(con_ids={})
        a.start()
        for _ in range(2):
            with self.assertRaises(OrderUnsendable) as stopped:
                a.exchange()
        self.assertIn("relay.py retire", str(stopped.exception))
        self.assertEqual(a.placed(), 0)
        self.assertEqual(a.outbox.load(), [])
        a.relay.close()
        self.assertEqual(relay_main(["retire", a.pending.path, "tw-paper-1", "--by", "richard", "--reason",
                                     "no IBKR contract for SPY configured"], today=SESSION), 0)
        [entry] = a.outbox.load()
        self.assertEqual((entry["event_type"], entry["payload"]["reason"], entry["payload"]["retired_by"],
                          entry["payload"]["detail"]),
                         ("execution.order.retired", "owner retired", "richard", "no IBKR contract for SPY configured"))
        b = self.restart(a)
        b.conn.config = b.conn.config._replace(con_ids={})
        b.start()
        b.exchange()
        b.drain()
        self.assertEqual(b.placed(), 0)
        self.assertIsNone(b.pending.load(), "the adapter went on past the input")

    def test_the_relay_checks_with_the_sender_before_the_mark_whatever_the_mapper(self):
        # A composition whose mapper does no check of its own still refuses
        # before the mark: the relay asks the sender itself.
        a = self.adapter()
        a.relay.close()
        relay = Relay(a.engine, a.sender, a.pending, lambda: collect_broker_orders(a.conn),
                      lambda o, session: release_to_request(o, "paper", a.conn.config.con_ids, session))
        self.addCleanup(relay.close)
        a.conn.connect()
        with self.assertRaises(NotReconciled):
            relay.exchange({"id": "session:1", "type": "market.session.closed", "sequence": 3,
                            "event_time": SESSION_CLOSED})
        self.assertEqual(a.pending.handovers(), {})
        self.assertEqual(a.placed(), 0)

    def test_an_acknowledged_correction_lets_the_adapter_start_again_and_is_never_a_fill(self):
        a = self.sent_and_working()
        a.h.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4",
                      time="20261017 09:30:01 US/Eastern")
        a.drain()
        a.h.fake.fill(7, shares="4", price=401.2, exec_id="0001f4e8.paper.01.02", cum_qty="4",
                      time="20261017 09:30:01 US/Eastern")
        with self.assertRaises(UnreportableOrder):
            a.drain()
        a.relay.close()
        # IBKR replays the day's executions on every start, so the correction
        # stops the adapter again until the owner acknowledges it.
        b = self.restart(a)
        with self.assertRaises(UnreportableOrder):
            b.start()
        self.assertEqual(relay_main(["acknowledge-correction", b.pending.path, "--journal", b.engine_journal, "0001f4e8.paper.01.02", "--corrects",
                                     "0001f4e8.paper.01.01", "--reason", "IBKR corrected the price to 401.20"]), 1,
                         "refused while an adapter holds the pending file")
        b.relay.close()
        self.assertEqual(relay_main(["acknowledge-correction", b.pending.path, "--journal", b.engine_journal, "0001f4e8.paper.01.02", "--corrects",
                                     "0001f4e8.paper.01.01", "--reason", "IBKR corrected the price to 401.20"]), 0)
        c = self.restart(b)
        c.start()
        c.exchange()
        c.drain()
        self.assertEqual([r.payload["fill_id"] for r in c.inputs if r.event_type == FILL_TYPE],
                         ["0001f4e8.paper.01.01"], "the engine keeps the original fill; the correction is no fill")
        self.assertEqual(c.placed(), 1)
        # Another correction of the same execution is not acknowledged.
        c.h.fake.fill(7, shares="4", price=401.1, exec_id="0001f4e8.paper.01.03", cum_qty="4",
                      time="20261017 09:30:01 US/Eastern")
        with self.assertRaises(UnreportableOrder):
            c.drain()

    def test_a_correction_of_an_execution_never_recorded_as_a_fill_is_not_acknowledged(self):
        # IBKR's correction arrives while the original still waits for its
        # commission: the original was never journalled, so the engine has no
        # fill to keep, and acknowledging the correction would leave the
        # shares in no fill record. The owner is sent to reconcile instead.
        a = self.sent_and_working()
        a.h.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4", commission=None,
                      time="20261017 09:30:01 US/Eastern")
        a.h.fake.fill(7, shares="4", price=401.2, exec_id="0001f4e8.paper.01.02", cum_qty="4", commission=None,
                      time="20261017 09:30:01 US/Eastern")
        with self.assertRaises(UnreportableOrder):
            a.drain()
        a.relay.close()
        self.assertEqual(relay_main(["acknowledge-correction", a.pending.path, "--journal", a.engine_journal,
                                     "0001f4e8.paper.01.02", "--corrects", "0001f4e8.paper.01.01", "--reason",
                                     "IBKR corrected the price to 401.20"]), 1)
        self.assertEqual(a.pending.acknowledged_corrections(), {})

    def test_a_cancellation_held_for_a_fill_survives_a_stop_before_the_fills_commission(self):
        # IBKR cancels the order while a partial fill waits for its
        # commission, and the adapter stops before the commission arrives. On
        # restart IBKR replays the execution and its commission but not the
        # status of an order no longer open, so the held cancellation must
        # have been kept on disk: the fill, then the cancellation, once.
        a = self.sent_and_working()
        exec_id = "0001f4e8.paper.01.01"
        a.h.fake.fill(7, shares="4", price=401.25, exec_id=exec_id, cum_qty="4", commission=None,
                      time="20261017 09:30:01 US/Eastern")
        a.h.fake.open_orders = [o for o in a.h.fake.open_orders if o[0] != 7]
        a.h.fake.status(7, "Cancelled", filled="4", remaining="5")
        a.drain()
        self.assertEqual([r.payload.get("status") for r in a.inputs], ["submitted"], "the cancellation is held")
        a.relay.close()
        # IBKR's commission arrives after the stop, so only its replay brings it.
        a.h.fake.commissions[exec_id] = SimpleNamespace(execId=exec_id, commission="1.00", currency="USD")
        b = self.restart(a)
        b.start()
        b.exchange()
        b.drain()
        self.assertEqual([(r.event_type, r.payload.get("status")) for r in b.inputs],
                         [(LIFECYCLE_TYPE, "submitted"), (FILL_TYPE, None), (LIFECYCLE_TYPE, "canceled")])
        b.relay.close()
        c = self.restart(b)
        c.start()
        c.exchange()
        c.drain()
        self.assertEqual(len(c.inputs), 3, "the cancellation is journalled once, across every restart")
        self.assertEqual(c.placed(), 1)

    def test_an_execution_reported_after_a_held_cancellation_is_recorded_before_it_across_a_restart(self):
        # IBKR cancels the order while its first execution waits for its
        # commission, then reports a second execution before the adapter
        # stops. The first's commission arrives; the second's only with
        # IBKR's replay after the restart. The cancellation waits for both:
        # the two fills, then the cancellation, once each.
        a = self.sent_and_working()
        first, second = "0001f4e8.paper.01.01", "0001f4e8.paper.02.01"
        a.h.fake.fill(7, shares="4", price=401.25, exec_id=first, cum_qty="4", commission=None,
                      time="20261017 09:30:01 US/Eastern")
        a.h.fake.status(7, "Cancelled", filled="4", remaining="5")
        a.drain()
        a.h.fake.fill(7, shares="1", price=401.3, exec_id=second, cum_qty="5", commission=None,
                      time="20261017 09:30:02 US/Eastern")
        a.h.fake.open_orders = [o for o in a.h.fake.open_orders if o[0] != 7]
        report = SimpleNamespace(execId=first, commission="1.00", currency="USD")
        a.h.fake.commissions[first] = report
        a.h.fake.callbacks.commission_report(report)
        a.drain()
        self.assertEqual([(r.event_type, r.payload.get("fill_id")) for r in a.inputs],
                         [(LIFECYCLE_TYPE, None), (FILL_TYPE, first)], "the cancellation waits for the second")
        a.relay.close()
        a.h.fake.commissions[second] = SimpleNamespace(execId=second, commission="0.35", currency="USD")
        b = self.restart(a)
        b.start()
        b.exchange()
        b.drain()
        self.assertEqual([(r.event_type, r.payload.get("fill_id"), r.payload.get("status")) for r in b.inputs],
                         [(LIFECYCLE_TYPE, None, "submitted"), (FILL_TYPE, first, None), (FILL_TYPE, second, None),
                          (LIFECYCLE_TYPE, None, "canceled")])
        b.relay.close()
        c = self.restart(b)
        c.start()
        c.exchange()
        c.drain()
        self.assertEqual(len(c.inputs), 4, "each is journalled once, across every restart")

    def test_a_trade_after_a_recorded_cancellation_stops_until_the_owner_acknowledges_it(self):
        # IBKR reports a trade on an order after its cancellation was
        # journalled (no execution was waiting, so nothing was held). The
        # engine reads no fill on an ended order, so the adapter stops; IBKR
        # replays the day's trades on every start, so it stops again at every
        # start until the owner acknowledges the trade, which is then left to
        # reconciliation and never recorded as a fill.
        a = self.sent_and_working()
        late = "0001f4e8.paper.01.01"
        a.h.fake.status(7, "Cancelled", filled="0", remaining="9")
        a.drain()
        self.assertEqual([r.payload.get("status") for r in a.inputs], ["submitted", "canceled"])
        a.h.fake.fill(7, shares="4", price=401.25, exec_id=late, cum_qty="4", time="20261017 09:30:01 US/Eastern")
        a.h.fake.open_orders = [o for o in a.h.fake.open_orders if o[0] != 7]
        with self.assertRaises(UnreportableOrder) as raised:
            a.drain()
        message = str(raised.exception)
        self.assertIn("relay.py acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG {} --after tw-paper-1".format(
            late), message)
        self.assertIn("every start", message)
        a.relay.close()
        b = self.restart(a)
        with self.assertRaises(UnreportableOrder):
            b.start()
        ack = ["acknowledge-late-fill", b.pending.path, "--journal", b.engine_journal, late, "--after", "tw-paper-1",
               "--reason", "IBKR shows 4 SPY bought at 401.25 after the cancel"]
        self.assertEqual(relay_main(ack), 1, "refused while an adapter holds the pending file")
        b.relay.close()
        self.assertEqual(relay_main(ack), 0)
        c = self.restart(b)
        c.start()
        c.exchange()
        c.drain()
        self.assertEqual([(r.event_type, r.payload.get("status")) for r in c.inputs],
                         [(LIFECYCLE_TYPE, "submitted"), (LIFECYCLE_TYPE, "canceled")],
                         "the trade is never a fill: the engine refuses one on an ended order")
        self.assertEqual(c.placed(), 1)

    def test_a_second_adapter_journals_nothing_held_while_the_first_holds_the_lock(self):
        # The first adapter holds a cancellation whose fill is journalled but
        # whose own write failed; a second adapter started beside it must be
        # refused before it reads the held reports, or it would journal the
        # cancellation behind the first's back.
        a = self.sent_and_working()
        exec_id = "0001f4e8.paper.01.01"
        a.h.fake.fill(7, shares="4", price=401.25, exec_id=exec_id, cum_qty="4", commission=None,
                      time="20261017 09:30:01 US/Eastern")
        a.h.fake.open_orders = [o for o in a.h.fake.open_orders if o[0] != 7]
        a.h.fake.status(7, "Cancelled", filled="4", remaining="5")
        a.drain()
        a.fail_on = "canceled"
        report = SimpleNamespace(execId=exec_id, commission="1.00", currency="USD")
        a.h.fake.commissions[exec_id] = report
        a.h.fake.callbacks.commission_report(report)
        with self.assertRaises(OSError):
            a.drain()
        held = Path(self.dir.name, "journal.held").read_bytes()
        with self.assertRaises(PendingInUse):
            self.restart(a)
        self.assertEqual([r.event_type for r in a.inputs], [LIFECYCLE_TYPE, FILL_TYPE])
        self.assertEqual(Path(self.dir.name, "journal.held").read_bytes(), held)

    def test_a_client_that_cannot_place_orders_leaves_no_mark(self):
        a = self.adapter()
        a.h.fake.can_place = False
        a.start()
        with self.assertRaises(SendRefused):
            a.exchange()
        self.assertEqual(a.placed(), 0)
        self.assertEqual(a.pending.handovers(), {})
        self.assertEqual(a.conn.next_order_id, 7)

    def test_a_login_that_does_not_manage_the_account_is_never_reconciled_or_sent_on(self):
        a = self.adapter()
        a.h.fake.managed_accounts = ("DU0000002",)
        with self.assertRaises(AccountNotManaged):
            a.start()
        with self.assertRaises(NotReconciled):
            a.exchange()
        self.assertEqual(a.placed(), 0)
        self.assertEqual(a.pending.handovers(), {})

    def test_one_spy_order_is_sent_and_its_status_and_fills_are_journalled_as_the_engine_reads_them(self):
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.start()
        a.exchange()
        self.assertEqual(a.placed(), 1)
        self.assertEqual(len(a.journal), 1)
        self.assertTrue(a.journal[0].send)
        a.drain()
        a.h.fake.fill(7, shares="4", price=401.25, exec_id="0001f4e8.paper.01.01", cum_qty="4", commission="1.00",
                      time="20261017 09:30:01 US/Eastern")
        a.drain()
        a.h.fake.fill(7, shares="5", price=401.3, exec_id="0001f4e8.paper.02.01", cum_qty="9", commission="0.35",
                      time="20261017 09:30:04 US/Eastern")
        a.drain()
        self.assertEqual(as_json(a.inputs), fixture("paper_reports.json"))
        self.assertEqual([(r.event_type, r.schema_version) for r in a.inputs],
                         [(LIFECYCLE_TYPE, LIFECYCLE_VERSION), (FILL_TYPE, FILL_VERSION), (FILL_TYPE, FILL_VERSION)])

    def test_a_rejection_is_journalled_as_invalid_with_ibkrs_reason(self):
        a = self.adapter()
        a.h.fake.on_place = functools.partial(a.h.fake.reject, message="Order rejected - reason: synthetic margin")
        a.start()
        a.exchange()
        a.drain()
        self.assertEqual(as_json(a.inputs), fixture("paper_rejected.json"))

    def test_a_cancellation_is_journalled_as_cancel_pending_then_canceled(self):
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.start()
        a.exchange()
        a.drain()
        a.h.fake.status(7, "PendingCancel", filled="0", remaining="9")
        a.h.fake.status(7, "Cancelled", filled="0", remaining="9")
        a.drain()
        self.assertEqual([r.payload["status"] for r in a.inputs], ["submitted", "cancel-pending", "canceled"])

    def test_a_crash_between_the_send_and_its_mark_never_resends(self):
        # IBKR took the order, and the adapter stopped before it could record
        # that it had sent it: the hand-over mark is still "about to send".
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.start()
        a.die_after_place = True
        with self.assertRaises(Died):
            a.exchange()
        a.relay.close()
        self.assertEqual(a.pending.handovers(), {"tw-paper-1": "handing-over"})
        # The restarted adapter: a new process on the same gateway, pending
        # file and engine log, which reconnects and asks for the reply again.
        b = self.restart(a)
        b.start()
        b.exchange()
        b.drain()
        self.assertEqual(b.placed(), 1)
        self.assertEqual(len(a.h.fake.placed), 1)
        self.assertEqual(b.pending.handovers(), {})
        # IBKR's replay of the working order on the restart is journalled
        # once, and a second restart writes nothing again.
        self.assertEqual([r.payload["status"] for r in b.inputs], ["submitted"])
        b.relay.close()
        c = self.restart(b)
        c.start()
        c.drain()
        self.assertEqual([r.payload["status"] for r in c.inputs], ["submitted"])

    def test_a_crash_before_ibkr_shows_the_order_stops_rather_than_resend(self):
        a = self.adapter()
        a.h.fake.place_error = ConnectionError("synthetic socket failure mid-send")
        a.start()
        with self.assertRaises(ConnectionError):
            a.exchange()
        a.relay.close()
        a.h.fake.place_error = None
        b = self.restart(a)
        b.start()
        with self.assertRaises(HandoverUnconfirmed):
            b.exchange()
        self.assertEqual(len(a.h.fake.called("place_order")), 1)
        self.assertEqual(a.h.fake.placed, [])

    def test_an_order_the_link_cannot_take_yet_leaves_no_mark_and_goes_out_once_reconciled(self):
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.conn.connect()
        with self.assertRaises(NotReconciled):
            a.exchange()
        self.assertEqual(a.placed(), 0)
        self.assertEqual(a.pending.handovers(), {})
        a.start()
        a.exchange()
        self.assertEqual(a.placed(), 1)

    def test_a_reconnect_never_resends_an_order_already_sent(self):
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.start()
        a.exchange()
        a.drain()
        a.h.fake.drop()
        a.drain()
        self.assertFalse(a.conn.connected)
        a.conn.maintain()
        self.assertTrue(a.conn.connected and a.conn.needs_reconciliation)
        # IBKR's next valid order ID after the reconnect is past the one used.
        self.assertEqual(a.conn.next_order_id, 8)
        a.start()
        # The same input asked again after the reconnect: its order is
        # already sent, so nothing goes out a second time.
        a.exchange()
        a.drain()
        self.assertEqual(a.placed(), 1)
        # IBKR restating the working order on reconnect is not a new report.
        self.assertEqual([r.payload["status"] for r in a.inputs], ["submitted"])


class OrderReportJournalTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.held_path = str(Path(self.dir.name) / "journal.held")

    def journal(self, now=lambda: RECEIVED, journalled=(), sink=None, owns_lock=lambda: True, ready=lambda: True,
                late_fills=None):
        inputs = []
        j = OrderReportJournal(ACCOUNT, 11, sink or inputs.append, now=now, commission_wait=COMMISSION_WAIT,
                               held_path=self.held_path, owns_lock=owns_lock, ready=ready, journalled=journalled,
                               late_fills=late_fills)
        return j, inputs

    def held_cancellation(self, sink=None):
        """A journal holding order 7's cancellation for a partial fill still waiting for its commission."""
        j, inputs = self.journal(sink=sink)
        self.sent(j)
        j.on_record(self.status("submitted"))
        ex = execution_report(contract(), execution(order_ref="tw-1", side="SLD", shares="1", cum_qty="1", price=401.5))
        j.on_record(ex)
        j.on_record(self.status("canceled"))
        return j, inputs, ex

    def test_a_failed_write_of_a_held_cancellation_stops_and_keeps_it_for_the_restart(self):
        # The cancellation's journal write fails after the fill's: the run
        # stops with the cause, the held cancellation stays on disk, and a
        # repeated commission or a restart writes it once.
        written = []
        fail = {"canceled": True}

        def sink(record):
            if record.payload.get("status") == "canceled" and fail["canceled"]:
                raise OSError("journal disk full")
            written.append(record)

        j, _, ex = self.held_cancellation(sink=sink)
        with self.assertRaises(OSError):
            j.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        self.assertEqual([r.event_type for r in written], [LIFECYCLE_TYPE, FILL_TYPE])
        with self.assertRaises(OSError):
            j.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        fail["canceled"] = False
        j.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        self.assertEqual([r.payload.get("status") for r in written], ["submitted", None, "canceled"],
                         "a repeated commission retries the held write")
        # A restart after the write finds nothing held to write again.
        after, inputs = self.journal(journalled=list(written))
        after.on_record(ex)
        after.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        self.assertEqual(inputs, [])

    def test_a_held_cancellation_whose_fill_was_written_before_a_stop_is_written_at_restart(self):
        written = []

        def sink(record):
            if record.payload.get("status") == "canceled":
                raise OSError("adapter stopped")
            written.append(record)

        j, _, ex = self.held_cancellation(sink=sink)
        with self.assertRaises(OSError):
            j.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        after, inputs = self.journal(journalled=list(written))
        self.assertEqual([r.payload.get("status") for r in inputs], ["canceled"], "written as the journal starts")
        again, more = self.journal(journalled=list(written) + inputs)
        again.on_record(ex)
        again.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        self.assertEqual(more, [])

    def test_a_held_cancellation_that_cannot_be_kept_on_disk_stops_for_a_person(self):
        self.held_path = str(Path(self.dir.name) / "missing" / "journal.held")
        j, inputs = self.journal()
        self.sent(j)
        j.on_record(self.status("submitted"))
        j.on_record(execution_report(contract(), execution(order_ref="tw-1", side="SLD", shares="1", cum_qty="1",
                                                           price=401.5)))
        with self.assertRaises(UnreportableOrder) as raised:
            j.on_record(self.status("canceled"))
        self.assertIn("tw-1", str(raised.exception))
        self.assertEqual([r.payload.get("status") for r in inputs], ["submitted"])

    def test_the_held_reports_file_is_read_strictly(self):
        self.held_cancellation()
        good = json.loads(Path(self.held_path).read_text(encoding="utf-8"))
        entry = good[0]
        for bad in ("{not json", json.dumps({}), json.dumps([dict(entry, extra=1)]),
                    json.dumps([dict(entry, status="submitted")]), json.dumps([dict(entry, awaits="x")]),
                    json.dumps([dict(entry, record=dict(entry["record"], event_type=FILL_TYPE))]),
                    json.dumps([dict(entry, order_id=8)]),
                    # Another client's ID, though the payload agrees with it.
                    json.dumps([dict(entry, client_id=12, record=dict(entry["record"], payload=dict(
                        entry["record"]["payload"], order_id="order:12:7")))]),
                    json.dumps([entry, entry])) + tuple(
                    json.dumps([dict(entry, record=dict(entry["record"], payload=payload))])
                    for payload in self.incomplete_payloads(entry["record"]["payload"])):
            with self.subTest(bad=bad):
                Path(self.held_path).write_text(bad, encoding="utf-8")
                with self.assertRaises(UnreportableOrder):
                    self.journal()

    @staticmethod
    def incomplete_payloads(payload):
        """Lifecycle payloads this journal would never write: a field missing, malformed or extra."""
        for field in payload:
            yield {k: v for k, v in payload.items() if k != field}
        for field, value in (("client_order_id", ""), ("instrument_id", 7), ("side", "short"), ("quantity", "1"),
                             ("quantity", 0), ("quantity", True), ("limit_price", 0), ("limit_price", "401.5"),
                             ("occurred_at", "2026-10-17 13:30:00"), ("occurred_at", "2026-10-17T13:30:00.5Z"),
                             ("message", None), ("extra", 1)):
            yield dict(payload, **{field: value})

    def test_a_later_execution_of_a_held_order_is_kept_on_disk_before_it_is_accepted(self):
        j, inputs, ex = self.held_cancellation()
        later = execution_report(contract(), execution(exec_id="0001f4e8.6512a1b3.01.01", order_ref="tw-1",
                                                       side="SLD", shares="1", cum_qty="2", price=401.5))
        j.on_record(later)
        held = json.loads(Path(self.held_path).read_text(encoding="utf-8"))
        self.assertEqual(held[0]["awaits"], sorted([ex.exec_id, later.exec_id]))
        # One whose hold cannot be rewritten is refused, never accepted.
        third = execution_report(contract(), execution(exec_id="0001f4e8.6512a1b4.01.01", order_ref="tw-1",
                                                       side="SLD", shares="1", cum_qty="3", price=401.5))
        with mock.patch("reports.os.replace", side_effect=OSError("disk full")):
            with self.assertRaises(UnreportableOrder) as raised:
                j.on_record(third)
        self.assertIn("tw-1", str(raised.exception))
        j.on_record(CommissionReport(third.exec_id, Decimal("1"), "USD"))
        self.assertEqual([r.event_type for r in inputs], [LIFECYCLE_TYPE], "the refused execution is no fill")
        self.assertEqual(json.loads(Path(self.held_path).read_text(encoding="utf-8"))[0]["awaits"],
                         sorted([ex.exec_id, later.exec_id]))

    def test_an_execution_of_an_order_already_ended_stops_for_a_person(self):
        # The engine reads no fill after an order's terminal report: a fill
        # IBKR reports after the cancellation is a contradiction to reconcile.
        j, inputs = self.journal()
        self.sent(j)
        j.on_record(self.status("submitted"))
        j.on_record(self.status("canceled"))
        late = execution_report(contract(), execution(order_ref="tw-1", side="SLD", shares="1", cum_qty="1",
                                                      price=401.5))
        with self.assertRaises(UnreportableOrder) as raised:
            j.on_record(late)
        self.assertIn("tw-1", str(raised.exception))
        restarted, more = self.journal(journalled=list(inputs))
        with self.assertRaises(UnreportableOrder):
            restarted.on_record(late)
        self.assertEqual(more, [])

    def test_an_acknowledged_trade_after_an_ended_order_is_passed_over_and_never_a_fill(self):
        j, inputs = self.journal()
        self.sent(j)
        j.on_record(self.status("submitted"))
        j.on_record(self.status("canceled"))
        late = execution_report(contract(), execution(order_ref="tw-1", side="SLD", shares="1", cum_qty="1",
                                                      price=401.5))
        restarted, more = self.journal(journalled=list(inputs), late_fills={late.exec_id: "tw-1"})
        restarted.on_record(late)
        restarted.on_record(CommissionReport(late.exec_id, Decimal("1"), "USD"))
        restarted.on_record(late)
        restarted.check_overdue()
        self.assertEqual(more, [])
        # A correction of the acknowledged trade is not acknowledged with it.
        correction = late._replace(exec_id=late.exec_id[:-2] + "02")
        with self.assertRaises(UnreportableOrder):
            restarted.on_record(correction)
        # Acknowledged as after another order: still a stop.
        other, _ = self.journal(journalled=list(inputs), late_fills={late.exec_id: "tw-2"})
        with self.assertRaises(UnreportableOrder) as raised:
            other.on_record(late)
        self.assertIn("tw-2", str(raised.exception))
        # Acknowledged, but the order is not recorded as ended: still a stop.
        working, _ = self.journal(journalled=list(inputs[:1]), late_fills={late.exec_id: "tw-1"})
        with self.assertRaises(UnreportableOrder) as raised:
            working.on_record(late)
        self.assertIn("not recorded as ended", str(raised.exception))
        for bad in ({late.exec_id: ""}, {late.exec_id: 7}, {"nodots": "tw-1"}, {"": "tw-1"}):
            with self.subTest(late_fills=bad), self.assertRaises(ValueError):
                self.journal(late_fills=bad)

    def test_a_status_other_than_the_held_report_while_it_is_held_stops_for_a_person(self):
        # The held cancellation is the order's last status: IBKR repeating it
        # changes nothing, but any other status contradicts it, even the one
        # journalled before it.
        j, inputs, _ = self.held_cancellation()
        j.on_record(self.status("canceled"))
        with self.assertRaises(UnreportableOrder) as raised:
            j.on_record(self.status("submitted"))
        self.assertIn("after reporting it canceled", str(raised.exception))
        self.assertEqual([r.payload.get("status") for r in inputs], ["submitted"])

    def test_a_status_after_an_orders_terminal_report_stops_for_a_person(self):
        self.held_cancellation()
        restarted, inputs = self.journal()
        with self.assertRaises(UnreportableOrder):
            restarted.on_record(self.status("invalid"))
        self.assertEqual(inputs, [], "a held cancellation is never replaced or overtaken")
        self.held_path = str(Path(self.dir.name) / "other.held")
        j, journalled = self.journal()
        self.sent(j)
        j.on_record(self.status("canceled"))
        with self.assertRaises(UnreportableOrder):
            j.on_record(self.status("submitted"))
        self.assertEqual([r.payload["status"] for r in journalled], ["canceled"])

    def test_the_held_reports_are_read_and_written_only_while_the_adapter_holds_its_lock(self):
        written = []

        def sink(record):
            if record.payload.get("status") == "canceled":
                raise OSError("the first adapter's write failed")
            written.append(record)

        j, _, ex = self.held_cancellation(sink=sink)
        with self.assertRaises(OSError):
            j.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        before = Path(self.held_path).read_bytes()
        inputs = []
        with self.assertRaises(UnreportableOrder):
            OrderReportJournal(ACCOUNT, 11, inputs.append, now=lambda: RECEIVED, commission_wait=COMMISSION_WAIT,
                               held_path=self.held_path, owns_lock=lambda: False, ready=lambda: True,
                               journalled=list(written))
        self.assertEqual(inputs, [], "nothing held is journalled without the lock")
        self.assertEqual(Path(self.held_path).read_bytes(), before)
        # A journal whose adapter lost the lock writes no hold either.
        self.held_path = str(Path(self.dir.name) / "other.held")
        owned = {"lock": True}
        j, inputs = self.journal(owns_lock=lambda: owned["lock"])
        self.sent(j)
        j.on_record(self.status("submitted"))
        j.on_record(ex)
        owned["lock"] = False
        with self.assertRaises(UnreportableOrder):
            j.on_record(self.status("canceled"))
        self.assertFalse(Path(self.held_path).exists())

    def test_a_held_reports_replay_wait_starts_when_the_link_is_up_and_reconciled(self):
        # IBKR replays executions only once the link is up and reconciled, so
        # a slow connect or reconnect does not count against the wait.
        clock = {"now": RECEIVED}
        link = {"ready": False}
        self.held_cancellation()
        j, inputs = self.journal(now=lambda: clock["now"], ready=lambda: link["ready"])
        clock["now"] += timedelta(seconds=10 * COMMISSION_WAIT)
        j.check_overdue()
        link["ready"] = True
        j.check_overdue()
        clock["now"] += timedelta(seconds=COMMISSION_WAIT - 1)
        j.check_overdue()
        # The link drops and comes back: IBKR replays again, so the wait
        # starts again.
        link["ready"] = False
        j.check_overdue()
        link["ready"] = True
        j.check_overdue()
        clock["now"] += timedelta(seconds=COMMISSION_WAIT - 1)
        j.check_overdue()
        clock["now"] += timedelta(seconds=1)
        with self.assertRaises(UnreportableOrder):
            j.check_overdue()
        self.assertEqual(inputs, [])

    def test_a_held_cancellation_whose_fill_is_never_replayed_stops_for_a_person(self):
        clock = {"now": RECEIVED}
        self.held_cancellation()
        j, inputs = self.journal(now=lambda: clock["now"])
        j.check_overdue()
        clock["now"] = RECEIVED + timedelta(seconds=COMMISSION_WAIT)
        with self.assertRaises(UnreportableOrder) as raised:
            j.check_overdue()
        self.assertIn("0001f4e8", str(raised.exception))
        self.assertIn("within {} s of the link being up and reconciled".format(COMMISSION_WAIT), str(raised.exception))
        self.assertEqual(inputs, [])

    def sent(self, j, order_id=7, client_order_id="tw-1"):
        request = release_to_request({"account_id": ACCOUNT, "client_order_id": client_order_id,
                                      "instrument_id": "SPY", "side": "sell", "quantity": 3, "limit_price": 401.5,
                                      "mode": "paper", "send": True}, "paper", CON_IDS, SESSION)
        j.sent(SentRecord(request, True, 11, order_id))

    def status(self, lifecycle, order_id=7, client_id=11, client_order_id="tw-1", reason=None):
        return OrderStatusReport(client_id=client_id, broker_order_id=order_id, perm_id=None,
                                 client_order_id=client_order_id, ibkr_status=None, lifecycle=lifecycle,
                                 filled=Decimal("0"), remaining=Decimal("3"), avg_fill_price=None,
                                 last_fill_price=None, ibkr_code=None, reason=reason)

    def test_another_clients_orders_and_executions_are_not_this_engines_reports(self):
        j, inputs = self.journal()
        j.on_record(self.status("submitted", client_id=0, client_order_id=None))
        j.on_record(self.status("submitted", client_id=12, client_order_id="tw-1"))
        j.on_record(execution_report(contract(), execution(client_id=12)))
        j.on_record(CommissionReport("0001f4e8.6512a1b2.01.01", Decimal("1"), "USD"))
        self.assertEqual(inputs, [])

    def test_a_status_with_no_lifecycle_change_is_not_journalled_again(self):
        j, inputs = self.journal()
        self.sent(j)
        for lifecycle in (None, "submitted", "submitted", None):
            j.on_record(self.status(lifecycle))
        self.assertEqual([r.payload["status"] for r in inputs], ["submitted"])
        self.assertEqual(inputs[0].payload["occurred_at"], "2026-10-17T13:30:00Z")
        self.assertEqual(inputs[0].payload["side"], "sell")

    def test_a_report_on_an_order_it_cannot_describe_fails_closed(self):
        j, _ = self.journal()
        with self.assertRaises(UnreportableOrder):
            j.on_record(self.status("invalid", client_order_id=None, reason="rejected"))

    def test_an_execution_the_engine_cannot_read_fails_closed(self):
        exec_id = "0001f4e8.6512a1b2.01.01"
        report = execution_report(contract(), execution())
        # IBKR marks an unknown double with its largest value (UNSET_DOUBLE).
        unset = Decimal(repr(1.7976931348623157e308))
        for name, ex, commission in (
                ("part shares", execution_report(contract(), execution(shares="2.5", cum_qty="2.5")),
                 CommissionReport(exec_id, Decimal("1"), "USD")),
                ("another currency", report, CommissionReport(exec_id, Decimal("1"), "EUR")),
                ("negative commission", report, CommissionReport(exec_id, Decimal("-1"), "USD")),
                ("zero price", report._replace(price=Decimal("0")), CommissionReport(exec_id, Decimal("1"), "USD")),
                ("negative price", report._replace(price=Decimal("-1")), CommissionReport(exec_id, Decimal("1"), "USD")),
                ("price not a number", report._replace(price=Decimal("NaN")),
                 CommissionReport(exec_id, Decimal("1"), "USD")),
                ("price unset", report._replace(price=unset), CommissionReport(exec_id, Decimal("1"), "USD")),
                ("commission not a number", report, CommissionReport(exec_id, Decimal("NaN"), "USD")),
                ("commission infinite", report, CommissionReport(exec_id, Decimal("Infinity"), "USD")),
                ("commission unset", report, CommissionReport(exec_id, unset, "USD")),
                # 4 shares at 101.25: a commission of the whole trade or more.
                ("commission past the notional", report, CommissionReport(exec_id, Decimal("405"), "USD"))):
            j, inputs = self.journal()
            with self.subTest(name), self.assertRaises(UnreportableOrder):
                j.on_record(ex)
                j.on_record(commission)
            self.assertEqual(inputs, [])

    def test_the_commission_is_bounded_by_the_orders_notional_when_the_order_is_known(self):
        # The order sent was 3 shares at 401.5, a notional of 1204.5.
        for commission, refused in ((Decimal("1204.49"), False), (Decimal("1204.5"), True)):
            j, inputs = self.journal()
            self.sent(j)
            ex = execution_report(contract(), execution(order_ref="tw-1", side="SLD", shares="3", cum_qty="3",
                                                        price=1.0))
            j.on_record(ex)
            with self.subTest(commission=commission):
                if refused:
                    with self.assertRaises(UnreportableOrder):
                        j.on_record(CommissionReport(ex.exec_id, commission, "USD"))
                else:
                    j.on_record(CommissionReport(ex.exec_id, commission, "USD"))
                    self.assertEqual(inputs[0].payload["commission"], float(commission))

    def test_a_correction_of_an_execution_is_never_read_as_a_new_one(self):
        j, inputs = self.journal()
        first = execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.01"))
        j.on_record(first)
        j.on_record(CommissionReport(first.exec_id, Decimal("1"), "USD"))
        # The same report again (a replay) is not a correction.
        j.on_record(first)
        j.on_record(CommissionReport(first.exec_id, Decimal("1"), "USD"))
        with self.assertRaises(UnreportableOrder):
            j.on_record(execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.02", price=101.2)))
        self.assertEqual([r.payload["fill_id"] for r in inputs], ["0001f4e8.6512a1b2.01.01"])

    def test_records_journalled_before_a_restart_are_not_journalled_again(self):
        before, written = self.journal()
        self.sent(before)
        before.on_record(self.status("submitted"))
        first = execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.01"))
        before.on_record(first)
        before.on_record(CommissionReport(first.exec_id, Decimal("1"), "USD"))
        self.assertEqual(len(written), 2)
        after, inputs = self.journal(journalled=written)
        # The restarted adapter learns the order from IBKR's replay of it.
        from orders import open_order_record
        after.on_record(open_order_record(7, contract(), order(order_ref="tw-1", action="SELL",
                                                               total_quantity=Decimal("3"), order_type="LMT",
                                                               lmt_price=401.5, client_id=11), None))
        after.on_record(self.status("submitted"))
        after.on_record(first)
        after.on_record(CommissionReport(first.exec_id, Decimal("1"), "USD"))
        self.assertEqual(inputs, [])
        after.on_record(self.status("canceled"))
        self.assertEqual([r.payload["status"] for r in inputs], ["canceled"])
        with self.assertRaises(UnreportableOrder):
            after.on_record(execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.02")))

    def test_the_commission_wait_and_execution_ids_are_read_strictly(self):
        for wait in (0, -1, float("nan"), float("inf"), True, "30"):
            with self.subTest(wait=wait), self.assertRaises(ValueError):
                OrderReportJournal(ACCOUNT, 11, lambda r: None, now=lambda: RECEIVED, commission_wait=wait,
                                   held_path=self.held_path, owns_lock=lambda: True, ready=lambda: True)
        j, inputs = self.journal()
        with self.assertRaises(UnreportableOrder):
            j.on_record(execution_report(contract(), execution(exec_id="0001f4e8")))
        self.assertEqual(inputs, [])

    def test_only_this_accounts_records_seed_a_restarted_journal(self):
        from snapshot import Record
        other = Record(LIFECYCLE_TYPE, LIFECYCLE_VERSION, {"account_id": "DU0000002", "order_id": "order:11:7",
                                                           "status": "submitted"})
        unnamed = Record(LIFECYCLE_TYPE, LIFECYCLE_VERSION, {"account_id": ACCOUNT, "order_id": "perm:9007",
                                                             "status": "submitted"})
        j, inputs = self.journal(journalled=[other, unnamed])
        self.sent(j)
        j.on_record(self.status("submitted"))
        self.assertEqual([r.payload["status"] for r in inputs], ["submitted"])

    def test_an_execution_waits_for_its_commission_no_longer_than_configured(self):
        clock = {"now": RECEIVED}
        j, inputs = self.journal(now=lambda: clock["now"])
        j.on_record(execution_report(contract(), execution()))
        clock["now"] = RECEIVED + timedelta(seconds=COMMISSION_WAIT - 0.5)
        j.check_overdue()
        clock["now"] = RECEIVED + timedelta(seconds=COMMISSION_WAIT)
        with self.assertRaises(UnreportableOrder):
            j.check_overdue()
        # Any later record finds it overdue too.
        with self.assertRaises(UnreportableOrder):
            j.on_record(self.status("submitted"))
        self.assertEqual(inputs, [])

    def test_a_commission_that_completes_a_waiting_execution_is_recorded_however_late_it_is_read(self):
        # The adapter was busy for commission_wait between reading the
        # execution and reading its commission: the commission is read first,
        # so the fill is recorded and nothing stops.
        clock = {"now": RECEIVED}
        j, inputs = self.journal(now=lambda: clock["now"])
        ex = execution_report(contract(), execution())
        j.on_record(ex)
        clock["now"] = RECEIVED + timedelta(seconds=COMMISSION_WAIT + 5)
        j.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        self.assertEqual([r.payload["fill_id"] for r in inputs], [ex.exec_id])
        j.check_overdue()

    def test_a_commission_for_one_execution_does_not_excuse_another_overdue(self):
        clock = {"now": RECEIVED}
        j, inputs = self.journal(now=lambda: clock["now"])
        late = execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.01"))
        prompt = execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.02.01", cum_qty="8"))
        j.on_record(late)
        j.on_record(prompt)
        clock["now"] = RECEIVED + timedelta(seconds=COMMISSION_WAIT)
        with self.assertRaises(UnreportableOrder):
            j.on_record(CommissionReport(prompt.exec_id, Decimal("1"), "USD"))

    def test_a_cancellation_is_held_until_the_fill_before_it_is_recorded(self):
        # IBKR may report the order cancelled before the commission of a
        # partial fill: the engine reads no fill after a terminal report, so
        # the cancellation waits for the fill.
        j, inputs = self.journal()
        self.sent(j)
        j.on_record(self.status("submitted"))
        ex = execution_report(contract(), execution(order_ref="tw-1", side="SLD", shares="1", cum_qty="1", price=401.5))
        j.on_record(ex)
        j.on_record(self.status("canceled"))
        j.on_record(self.status("canceled"))
        self.assertEqual([r.payload.get("status") for r in inputs], ["submitted"])
        j.on_record(CommissionReport(ex.exec_id, Decimal("1"), "USD"))
        self.assertEqual([(r.event_type, r.payload.get("status")) for r in inputs],
                         [(LIFECYCLE_TYPE, "submitted"), (FILL_TYPE, None), (LIFECYCLE_TYPE, "canceled")])
        j.on_record(self.status("canceled"))
        self.assertEqual(len(inputs), 3, "a held report is journalled once")

    def test_an_order_ibkr_changes_after_describing_it_stops_for_a_person(self):
        # A changed limit or quantity on an order already reported is not a
        # repeat of its status: the run stops rather than let the changed
        # order work unreported, before and after a restart.
        def held(**changed):
            fields = dict(order_ref="tw-1", action="SELL", total_quantity=Decimal("3"), order_type="LMT",
                          lmt_price=401.5, client_id=11)
            fields.update(changed)
            return open_order_record(7, contract(), order(**fields), None)

        j, inputs = self.journal()
        j.on_record(held())
        j.on_record(self.status("submitted"))
        j.on_record(held())
        for changed in (dict(lmt_price=401.0), dict(total_quantity=Decimal("2")), dict(action="BUY")):
            with self.subTest(changed=changed), self.assertRaises(UnreportableOrder):
                j.on_record(held(**changed))
        after, _ = self.journal(journalled=list(inputs))
        after.on_record(held())
        with self.assertRaises(UnreportableOrder):
            after.on_record(held(lmt_price=402.0))
        self.assertEqual(len(inputs), 1)

    def test_an_acknowledgement_names_a_correction_of_the_execution_it_corrects(self):
        first = execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.01"))
        for acknowledged in ({"0001f4e8.6512a1b2.01.01": "0001f4e8.6512a1b2.01.01"},
                             {"0001f4e8.6512a1b2.01.02": "0001f4e8.ffffffff.01.01"}, {"nodots": "0001f4e8.6512a1b2.01.01"}):
            with self.subTest(acknowledged=acknowledged), self.assertRaises(ValueError):
                self.journal_acknowledging(acknowledged)
        # Acknowledged as correcting another execution than the one IBKR
        # first reported under that base: still a stop.
        j, inputs = self.journal_acknowledging({"0001f4e8.6512a1b2.01.03": "0001f4e8.6512a1b2.01.02"})
        j.on_record(first)
        with self.assertRaises(UnreportableOrder):
            j.on_record(execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.03")))
        # Acknowledged, but IBKR never reported the original to this journal.
        j, inputs = self.journal_acknowledging({"0001f4e8.6512a1b2.01.02": "0001f4e8.6512a1b2.01.01"})
        with self.assertRaises(UnreportableOrder) as raised:
            j.on_record(execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.02")))
        self.assertIn("never recorded as a fill", str(raised.exception))
        j, inputs = self.journal_acknowledging({"0001f4e8.6512a1b2.01.02": "0001f4e8.6512a1b2.01.01"})
        j.on_record(first)
        j.on_record(execution_report(contract(), execution(exec_id="0001f4e8.6512a1b2.01.02", price=101.2)))
        j.on_record(CommissionReport("0001f4e8.6512a1b2.01.02", Decimal("1"), "USD"))
        j.on_record(CommissionReport(first.exec_id, Decimal("1"), "USD"))
        self.assertEqual([r.payload["fill_id"] for r in inputs], [first.exec_id])

    def journal_acknowledging(self, acknowledged):
        inputs = []
        j = OrderReportJournal(ACCOUNT, 11, inputs.append, now=lambda: RECEIVED, commission_wait=COMMISSION_WAIT,
                               held_path=self.held_path, owns_lock=lambda: True, ready=lambda: True,
                               acknowledged=acknowledged)
        return j, inputs

    def test_a_receipt_time_without_a_zone_is_refused(self):
        j, inputs = self.journal(now=lambda: datetime(2026, 10, 17, 13, 30))
        self.sent(j)
        with self.assertRaises(UnreportableOrder):
            j.on_record(self.status("submitted"))
        self.assertEqual(inputs, [])

    def test_open_order_records_describe_the_order_as_ibkr_holds_it(self):
        from orders import open_order_record
        j, inputs = self.journal()
        echo = order(order_ref="tw-1", action="SELL", total_quantity=Decimal("3"), order_type="LMT", lmt_price=401.49,
                     client_id=11)
        j.on_record(open_order_record(7, contract(), echo, None))
        j.on_record(self.status("submitted"))
        self.assertEqual((inputs[0].payload["quantity"], inputs[0].payload["limit_price"],
                          inputs[0].payload["order_id"]), (3, 401.49, "order:11:7"))


if __name__ == "__main__":
    unittest.main()
