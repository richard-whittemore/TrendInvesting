"""Paper trading against the fake gateway: one released order goes out once, and IBKR's reports come back as inputs.

The sending interface (sender.PaperSender behind relay.Relay) is exercised
here against tests/fake_tws.py, never a real IB Gateway. The released order
is testdata/paper_released.json, and the records journalled from IBKR's
answers are testdata/paper_reports.json and paper_rejected.json, which the
engine's own test reads by the rule it reads a venue's reports by
(internal/event, TestTheIBKRAdaptersPaperReportsAreReadByOrderReports).
"""
import functools
import json
import sys
import tempfile
import unittest
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from fake_tws import ACCOUNT, contract, execution, order
from test_connection import Harness
from test_relay import Restarts
from connection import AccountNotManaged
from orders import CommissionReport, OrderStatusReport, OrderTracker, collect_broker_orders, execution_report
from relay import RELEASED, HandoverUnconfirmed, PendingInput, Relay
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
        self.reports = OrderReportJournal(self.conn.config.account_id, self.conn.config.client_id,
                                          self.inputs.append, now=lambda: self.received,
                                          commission_wait=self.conn.config.commission_wait,
                                          journalled=list(self.inputs))
        self.tracker = OrderTracker(self.conn.config.account_id, self.conn.config.client_id,
                                    self.reports.on_record)
        self.conn.subscribe(self.tracker.on_event)
        self.die_after_place = False
        self.sender = PaperSender(self.conn, self._journal_sent, today=lambda: self.today)
        self.engine = engine or Restarts({}, {"session:1": [{"type": RELEASED, "payload": fixture("paper_released.json")}]})
        self.pending = PendingInput(str(Path(directory) / "pending.json"))
        # The request is checked against the link before the relay marks it
        # about to be sent, so an order the sender would refuse leaves no mark.
        self.relay = Relay(self.engine, self.sender.send, self.pending, lambda: collect_broker_orders(self.conn),
                           lambda o, session: self.sender.check(release_to_request(o, "paper", CON_IDS, session)))

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

    def test_a_release_left_waiting_past_its_session_is_never_sent(self):
        # Refused yesterday for the link, the input is asked again today: its
        # order was for yesterday's session, so it is refused before the mark.
        a = self.adapter()
        a.h.fake.on_place = a.h.fake.accept
        a.conn.connect()
        with self.assertRaises(NotReconciled):
            a.exchange()
        a.today = date(2026, 10, 17)
        a.start()
        with self.assertRaises(SendRefused):
            a.exchange()
        self.assertEqual(a.placed(), 0)
        self.assertEqual(a.pending.handovers(), {})
        self.assertEqual(a.conn.next_order_id, 7)

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
    def journal(self, now=lambda: RECEIVED, journalled=()):
        inputs = []
        j = OrderReportJournal(ACCOUNT, 11, inputs.append, now=now, commission_wait=COMMISSION_WAIT,
                               journalled=journalled)
        return j, inputs

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
                OrderReportJournal(ACCOUNT, 11, lambda r: None, now=lambda: RECEIVED, commission_wait=wait)
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
