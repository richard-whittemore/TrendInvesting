"""The Flex cash-movement reader (flex.py): IBKR's Cash Transactions read into the instance's own ledger.

ADR 0007 (a deposit or withdrawal is an account.cash-movement), ADR 0015
(version 2 carries IBKR's transaction ID) and ADR 0019 (the cash-drift
check stays the cross-check). Every statement here is synthetic: a made-up
account, amounts and IDs, never a real statement. No network: the Flex Web
Service is a fake that answers from the files in testdata/flex.
"""
import fcntl
import http.server
import io
import json
import re
import sys
import tempfile
import threading
import unittest
import unittest.mock
from datetime import date, datetime, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
adapter_dir = tests_dir.parent
repo = adapter_dir.parents[1]
for p in (str(tests_dir), str(adapter_dir)):
    if p not in sys.path:
        sys.path.insert(0, p)

import flex
from cli import main
from engine_double import LoggedEngine, final_hash, run_approval
from fake_tws import ACCOUNT
from test_delivery import commit

FLEX = tests_dir / "testdata" / "flex"
TESTDATA = tests_dir / "testdata"
TOKEN = "synthetic-token-000111"
QUERY = "123456"


def fixture(name):
    return (FLEX / name).read_bytes()


class FakeFlex:
    """IBKR's Flex Web Service as a fake: SendRequest answers send, each GetStatement the next of statements.

    Each answer is a file name in testdata/flex, or the bytes themselves."""

    def __init__(self, send="send_ok.xml", statements=("statement.xml",)):
        self.send, self.statements, self.calls = send, list(statements), []

    def __call__(self, url, params):
        self.calls.append((url, dict(params)))
        if url == flex.SEND_REQUEST_URL:
            return fixture(self.send)
        answer = self.statements.pop(0)
        return answer if isinstance(answer, bytes) else fixture(answer)


def snapshot(engine, ident, as_of, net_liquidation):
    """A Boost account snapshot (account.snapshot version 3) committed to engine's durable log."""
    commit(engine, ident, kind="account.snapshot", version=3, payload={
        "account_id": ACCOUNT, "as_of": as_of, "net_liquidation": net_liquidation, "currency": "USD",
        "positions": [{"instrument_id": "SPY", "quantity": 20}]})


class ParseTests(unittest.TestCase):
    def test_every_detail_posting_is_read_and_typed(self):
        statement = flex.parse_statement(fixture("statement.xml"), ACCOUNT)
        self.assertEqual(statement.account_id, ACCOUNT)
        got = [(p.transaction_id, p.kind, p.ibkr_type, p.amount, p.currency) for p in statement.postings]
        self.assertEqual(got, [
            ("900000101", flex.TRANSFER, "Deposits/Withdrawals", Decimal("2500"), "USD"),
            ("900000102", flex.TRANSFER, "Deposits/Withdrawals", Decimal("-400.50"), "USD"),
            ("900000103", flex.DIVIDEND, "Dividends", Decimal("12.34"), "USD"),
            ("900000104", flex.INTEREST_PAID, "Broker Interest Paid", Decimal("-3.21"), "USD"),
            ("900000105", flex.INTEREST_RECEIVED, "Broker Interest Received", Decimal("0.42"), "USD"),
            ("900000106", flex.COMMISSION, "Commission Adjustments", Decimal("-1.00"), "USD"),
            ("900000107", flex.FEE, "Other Fees", Decimal("-10"), "USD"),
        ], "the SUMMARY row repeats the detail rows and is not read")

    def test_a_posting_is_dated_in_new_york(self):
        deposit, withdrawal = flex.parse_statement(fixture("statement.xml"), ACCOUNT).postings[:2]
        self.assertEqual(deposit.as_of, datetime(2027, 1, 14, 5, 0, tzinfo=timezone.utc),
                         "a date alone is the start of that New York day")
        self.assertEqual(withdrawal.as_of, datetime(2027, 1, 15, 15, 15, tzinfo=timezone.utc))

    def test_a_statement_for_another_account_is_refused_without_naming_either(self):
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.parse_statement(fixture("other_account.xml"), ACCOUNT)
        self.assertIn("not this instance's account", str(caught.exception))
        self.assertNotIn("DU0000999", str(caught.exception))
        self.assertNotIn(ACCOUNT, str(caught.exception))

    def test_a_row_naming_another_account_is_refused(self):
        xml = fixture("statement.xml").replace(b'accountId="DU0000001" currency="USD" type="Dividends"',
                                               b'accountId="DU0000998" currency="USD" type="Dividends"')
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.parse_statement(xml, ACCOUNT)
        self.assertIn("another account", str(caught.exception))

    def test_a_statement_with_no_cash_section_is_refused(self):
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.parse_statement(fixture("no_cash_section.xml"), ACCOUNT)
        self.assertIn("Cash Transactions", str(caught.exception))

    def test_an_empty_cash_section_is_a_period_with_nothing_posted(self):
        self.assertEqual(flex.parse_statement(fixture("empty_cash_section.xml"), ACCOUNT).postings, [])

    def test_a_malformed_statement_is_refused(self):
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.parse_statement(fixture("truncated.xml"), ACCOUNT)
        self.assertIn("not XML", str(caught.exception))
        for bad, why in ((b'amount="2500"', b'amount="lots"'), (b'transactionID="900000101" ', b""),
                         (b'dateTime="20270114"', b'dateTime="2027-01-14"'),
                         (b'currency="USD" type="Deposits/Withdrawals" dateTime="20270114"',
                          b'currency="" type="Deposits/Withdrawals" dateTime="20270114"')):
            with self.subTest(bad=bad, why=why):
                with self.assertRaises(flex.StatementRefused):
                    flex.parse_statement(fixture("statement.xml").replace(bad, why, 1), ACCOUNT)

    def test_a_row_without_its_date_and_time_is_refused(self):
        # dateTime is the only field the reader dates a posting by; a report date or settle date is not when it moved.
        xml = fixture("statement.xml").replace(b'dateTime="20270114" amount="2500"', b'date="20270114" amount="2500"')
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.parse_statement(xml, ACCOUNT)
        self.assertIn("900000101", str(caught.exception))

    def test_a_transaction_id_twice_is_refused(self):
        xml = fixture("statement.xml").replace(b'transactionID="900000102"', b'transactionID="900000101"')
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.parse_statement(xml, ACCOUNT)
        self.assertIn("900000101", str(caught.exception))

    def test_a_service_answer_in_place_of_a_statement_is_refused(self):
        with self.assertRaises(flex.StatementRefused):
            flex.parse_statement(fixture("send_ok.xml"), ACCOUNT)


class ServiceTests(unittest.TestCase):
    def test_send_request_then_get_statement_at_version_3(self):
        service = FakeFlex()
        self.assertEqual(flex.fetch_statement(TOKEN, QUERY, get=service, sleep=lambda s: None),
                         fixture("statement.xml"))
        (send, send_params), (get, get_params) = service.calls
        self.assertEqual((send, send_params), (flex.SEND_REQUEST_URL, {"t": TOKEN, "q": QUERY, "v": "3"}))
        self.assertEqual((get, get_params), (
            "https://ndcdyn.interactivebrokers.com/AccountManagement/FlexWebService/GetStatement",
            {"t": TOKEN, "q": "1111111111", "v": "3"}))

    def test_a_rejected_token_is_named_by_its_code_and_never_quoted(self):
        service = FakeFlex(send="send_expired.xml")
        with self.assertRaises(flex.FlexRejected) as caught:
            flex.fetch_statement(TOKEN, QUERY, get=service, sleep=lambda s: None)
        self.assertEqual(caught.exception.code, "1012")
        self.assertIn("Token has expired", str(caught.exception))
        self.assertNotIn(TOKEN, str(caught.exception))
        self.assertEqual(len(service.calls), 1, "no statement is asked for on a rejected token")

    def test_a_busy_service_is_unavailable_not_rejected(self):
        with self.assertRaises(flex.FlexUnavailable) as caught:
            flex.fetch_statement(TOKEN, QUERY, get=FakeFlex(send="send_busy.xml"), sleep=lambda s: None)
        self.assertEqual(caught.exception.code, "1009")

    def test_a_statement_still_generating_is_asked_for_again(self):
        slept = []
        service = FakeFlex(statements=("get_in_progress.xml", "get_in_progress.xml", "statement.xml"))
        self.assertEqual(flex.fetch_statement(TOKEN, QUERY, get=service, sleep=slept.append),
                         fixture("statement.xml"))
        self.assertEqual(slept, [flex.GENERATION_WAIT] * 2)

    def test_a_statement_that_never_finishes_is_unavailable(self):
        service = FakeFlex(statements=["get_in_progress.xml"] * flex.GENERATION_ATTEMPTS)
        with self.assertRaises(flex.FlexUnavailable) as caught:
            flex.fetch_statement(TOKEN, QUERY, get=service, sleep=lambda s: None)
        self.assertEqual(caught.exception.code, "1019")

    def test_the_token_goes_to_ibkr_only(self):
        def get(url, params):
            calls.append(url)
            return fixture("send_ok.xml").replace(b"ndcdyn.interactivebrokers.com", b"example.org")
        calls = []
        with self.assertRaises(flex.FlexUnavailable) as caught:
            flex.fetch_statement(TOKEN, QUERY, get=get, sleep=lambda s: None)
        self.assertEqual(calls, [flex.SEND_REQUEST_URL])
        self.assertNotIn(TOKEN, str(caught.exception))

    def test_a_redirect_is_never_followed(self):
        # The token rides in the query string, so a redirect would re-send it to wherever the Location names.
        hits = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                hits.append(self.path)
                if self.path.startswith("/elsewhere"):
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(b"followed")
                    return
                self.send_response(302)
                # A fixed target carrying the token, as a redirect re-sending the query string would.
                self.send_header("Location", "/elsewhere?t=" + TOKEN)
                self.end_headers()

            def log_message(self, *args):
                pass

        server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        self.addCleanup(server.server_close)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        with self.assertRaises(OSError):
            flex._get("http://127.0.0.1:{}/statement".format(server.server_port), {"t": TOKEN})
        self.assertEqual([h.split("?")[0] for h in hits], ["/statement"], "the redirect's target was never asked")

    def test_a_network_failure_is_unavailable_and_never_quotes_the_token(self):
        def get(url, params):
            raise OSError("connection refused for t={}".format(params["t"]))
        with self.assertRaises(flex.FlexUnavailable) as caught:
            flex.fetch_statement(TOKEN, QUERY, get=get, sleep=lambda s: None)
        self.assertNotIn(TOKEN, str(caught.exception))


class ReadOnceHarness(unittest.TestCase):
    """A first run's durable log, the ledger's folder, and a reading of a fake Flex Web Service."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.ledger = self.dir / "cash-movements.jsonl"
        self.out = io.StringIO()
        self.chain("2027-01-12T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 8530.0),
                                            ("s2", "2027-01-14T21:00:00Z", 11030.0)))

    def chain(self, approved_at, snapshots, approved=True, approval_version=3):
        """A first run approved at approved_at (its starting book stated), holding snapshots (ID, as of, value)."""
        self.log = self.dir / "run-{}.log".format(len(list(self.dir.glob("run-*.log"))) + 1)
        approval = run_approval(ACCOUNT, starting_book=({}, 8000.0))
        approval["approved_at"] = approved_at
        self.engine = LoggedEngine(str(self.log), approval=approval if approved else None,
                                   approval_version=approval_version)
        for ident, as_of, value in snapshots:
            snapshot(self.engine, ident, as_of, value)

    def statement(self, *rows, period=None):
        """statement.xml with its two transfers replaced by rows, each (ID, dateTime, amount[, currency][, type]), and
        its period (fromDate, toDate) replaced by period when given."""
        xml = fixture("statement.xml")
        if period is not None:
            xml = xml.replace(b'fromDate="20270112" toDate="20270115"',
                              'fromDate="{}" toDate="{}"'.format(*period).encode())
        start = xml.index(b'        <CashTransaction accountId="DU0000001" currency="USD" type="Deposits/Withdrawals" '
                          b'dateTime="20270114"')
        end = xml.index(b'        <CashTransaction accountId="DU0000001" currency="USD" type="Dividends"')
        lines = b"".join('        <CashTransaction accountId="DU0000001" currency="{}" type="{}" dateTime="{}" '
                         'amount="{}" transactionID="{}" levelOfDetail="DETAIL"/>\n'.format(
                             row[3] if len(row) > 3 else "USD", row[4] if len(row) > 4 else "Deposits/Withdrawals",
                             row[1], row[2], row[0]).encode() for row in rows)
        return xml[:start] + lines + xml[end:]

    def payloads(self):
        return {r["payload"]["transaction_id"]: r["payload"] for r in self.records()}

    def assert_set_aside(self, statement, *said):
        before = self.ledger.read_bytes() if self.ledger.exists() else None
        self.assertFalse(self.read(FakeFlex(statements=(statement,))))
        self.assertEqual(self.ledger.read_bytes() if self.ledger.exists() else None, before, "the ledger is untouched")
        status = self.status()
        self.assertEqual(status["state"], "set-aside")
        for text in said:
            self.assertIn(text, status["reason"])
        return status

    def read(self, service, now=datetime(2027, 1, 16, 2, 30, tzinfo=timezone.utc)):
        return flex.read_once(TOKEN, QUERY, ACCOUNT, str(self.log), str(self.ledger), get=service,
                              sleep=lambda s: None, now=lambda: now, err=self.out)

    def read_on(self, day, to_date):
        """A reading at 21:30 New York on day (a date), of a statement ending on to_date."""
        at = datetime(day.year, day.month, day.day, 21, 30, tzinfo=flex.NEW_YORK).astimezone(timezone.utc)
        return self.read(FakeFlex(statements=(self.statement(period=("20270112", to_date)),)), now=at)

    def records(self):
        return [json.loads(line) for line in self.ledger.read_text().splitlines()]

    def status(self):
        return json.loads(Path(str(self.ledger) + flex.STATUS_SUFFIX).read_text())

    def coverage(self):
        return json.loads(Path(str(self.ledger) + flex.COVERAGE_SUFFIX).read_text())


class ReadOnceTests(ReadOnceHarness):
    def test_each_transfer_is_one_cash_movement_with_the_equity_before_it(self):
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual(self.records(), [
            {"id": "ibkr-flex:900000101", "type": "account.cash-movement", "schema_version": 2,
             "event_time": "2027-01-14T05:00:00Z", "source": "ibkr-flex",
             "payload": {"as_of": "2027-01-14T05:00:00Z", "amount": 2500.0, "equity_before": 8530.0,
                         "currency": "USD", "transaction_id": "900000101"}},
            {"id": "ibkr-flex:900000102", "type": "account.cash-movement", "schema_version": 2,
             "event_time": "2027-01-15T15:15:00Z", "source": "ibkr-flex",
             "payload": {"as_of": "2027-01-15T15:15:00Z", "amount": -400.5, "equity_before": 11030.0,
                         "currency": "USD", "transaction_id": "900000102"}},
        ], "dividends and interest are costs-ledger business, not cash movements")
        status = self.status()
        self.assertEqual((status["state"], status["written"], status["checked_at"]),
                         ("read", 2, "2027-01-16T02:30:00Z"))

    def test_the_payload_is_the_one_the_engine_decodes(self):
        # internal/event's TestTheIBKRAdapterPayloadsDecodeAndValidate reads this file at version 2.
        self.read(FakeFlex())
        self.assertEqual(self.records()[0]["payload"], json.loads((TESTDATA / "cash_movement.json").read_text()))

    def test_an_approximated_payload_is_one_the_engine_decodes(self):
        # internal/event's TestTheIBKRAdapterPayloadsDecodeAndValidate reads this file at version 2 too.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9030.0),))
        self.read(FakeFlex(statements=(self.statement(("900000201", "20270114;140000", "2500")),)))
        self.assertEqual(self.records()[0]["payload"],
                         json.loads((TESTDATA / "cash_movement_approximated.json").read_text()))

    def test_a_second_run_writes_nothing_already_in_the_ledger(self):
        self.read(FakeFlex())
        first = self.ledger.read_bytes()
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual(self.ledger.read_bytes(), first)
        self.assertEqual(self.status()["written"], 0)

    def test_a_later_statement_adds_only_the_new_transfer(self):
        self.read(FakeFlex())
        later = fixture("statement.xml").replace(
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Dividends"',
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Deposits/Withdrawals" '
            b'dateTime="20270115;120000" amount="100" transactionID="900000110" levelOfDetail="DETAIL"/>\n'
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Dividends"')
        self.read(FakeFlex(statements=(later,)))
        self.assertEqual([r["id"] for r in self.records()],
                         ["ibkr-flex:900000101", "ibkr-flex:900000102", "ibkr-flex:900000110"])

    def test_a_known_transfer_restated_differently_sets_the_reader_aside(self):
        self.read(FakeFlex())
        before = self.ledger.read_bytes()
        changed = fixture("statement.xml").replace(b'amount="2500" transactionID', b'amount="2600" transactionID')
        self.assertFalse(self.read(FakeFlex(statements=(changed,))))
        self.assertEqual(self.ledger.read_bytes(), before)
        self.assertEqual(self.status()["state"], "set-aside")
        self.assertIn("900000101", self.status()["reason"])

    def test_a_transfer_before_the_approval_is_in_the_first_book(self):
        # The first book is the starting book the owner states at the first run's approval: its time is the cutoff.
        # The deposit is dated by day alone, the day before the approval's.
        self.chain("2027-01-15T14:00:00Z", (("s1", "2027-01-15T21:00:00Z", 11030.0),))
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual([r["id"] for r in self.records()], ["ibkr-flex:900000102"])
        self.assertIn("900000101", self.out.getvalue())
        self.assertEqual(self.status()["in_first_book"], 1)

    def test_a_first_run_approved_at_each_version_stating_the_book_has_its_time_read(self):
        # Versions 3 to 5 of operator.run.approved state the starting book;
        # cmd/engine records the current one (internal/event).
        for version in (3, 4, 5):
            with self.subTest(version=version):
                self.chain("2027-01-15T14:00:00Z", (("s1", "2027-01-15T21:00:00Z", 11030.0),),
                           approval_version=version)
                self.assertTrue(self.read(FakeFlex()))
                self.assertEqual(self.status()["in_first_book"], 1)

    def test_a_transfer_on_the_first_snapshots_day_after_the_approval_is_written(self):
        # Approved 08:00 New York, a deposit at 14:00, the first close snapshot at 16:00, which already holds it.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9030.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000201", "20270114;140000", "2500")),))))
        self.assertEqual(self.payloads()["900000201"], {
            "as_of": "2027-01-14T19:00:00Z", "amount": 2500.0, "equity_before": 6530.0, "currency": "USD",
            "transaction_id": "900000201", "equity_before_basis": "next-snapshot-less-amount"})
        self.assertEqual(self.status()["in_first_book"], 0)

    def test_a_date_only_transfer_on_the_approval_day_is_written(self):
        # The owner's book was read before the approval, so a transfer IBKR dates by that day alone cannot be in it:
        # it is taken as after the approval, its equity before from the first snapshot after it.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9030.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000202", "20270114", "2500")),))))
        self.assertEqual(self.payloads()["900000202"], {
            "as_of": "2027-01-14T05:00:00Z", "amount": 2500.0, "equity_before": 6530.0, "currency": "USD",
            "transaction_id": "900000202", "equity_before_basis": "next-snapshot-less-amount"})
        status = self.status()
        self.assertEqual((status["in_first_book"], status["date_only_on_approval_day"]), (0, 1))

    def test_a_date_only_transfer_on_the_day_before_the_approval_is_in_the_first_book(self):
        self.chain("2027-01-15T13:00:00Z", (("s1", "2027-01-15T21:00:00Z", 9030.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000214", "20270114", "2500")),))))
        self.assertFalse(self.ledger.exists())
        status = self.status()
        self.assertEqual((status["in_first_book"], status["date_only_on_approval_day"]), (1, 0))

    def evening_approval(self):
        """A first run approved at 17:00 New York on the 14th, after that session's close, and a deposit IBKR dates
        by the 14th alone: the starting book may or may not hold it."""
        self.chain("2027-01-14T22:00:00Z", (("s1", "2027-01-15T21:00:00Z", 9030.0),))
        return self.statement(("900000230", "20270114", "2500"), period=("20270114", "20270115"))

    def test_a_date_only_transfer_on_an_evening_approvals_day_is_set_aside_as_ambiguous(self):
        self.assertFalse(self.read(FakeFlex(statements=(self.evening_approval(),))))
        status = self.status()
        self.assertEqual(status["state"], "ambiguous-transfer")
        for text in ("900000230", "2500", "--resolve 900000230=in-first-book", "--resolve 900000230=after-approval"):
            self.assertIn(text, status["reason"])
        self.assertNotIn(ACCOUNT, json.dumps(status) + self.out.getvalue())
        self.assertFalse(self.ledger.exists())
        self.assertFalse(Path(str(self.ledger) + flex.COVERAGE_SUFFIX).exists())

    def set_aside_as_ambiguous(self):
        """The evening approval's statement, read once and set aside as ambiguous: the state a decision answers."""
        statement = self.evening_approval()
        self.assertFalse(self.read(FakeFlex(statements=(statement,))))
        self.assertEqual(self.status()["ambiguous"], ["900000230"])
        return statement

    def test_an_ambiguous_transfer_resolved_after_the_approval_is_written(self):
        statement = self.set_aside_as_ambiguous()
        flex.record_resolutions(str(self.ledger), {"900000230": flex.AFTER_APPROVAL})
        self.assertTrue(self.read(FakeFlex(statements=(statement,))))
        self.assertEqual(self.payloads()["900000230"], {
            "as_of": "2027-01-14T05:00:00Z", "amount": 2500.0, "equity_before": 6530.0, "currency": "USD",
            "transaction_id": "900000230", "equity_before_basis": "next-snapshot-less-amount"})
        status = self.status()
        self.assertEqual((status["resolved"], status["in_first_book"]), (1, 0))
        self.assertTrue(self.read(FakeFlex(statements=(statement,))), "the decision is kept for later readings")

    def test_an_ambiguous_transfer_resolved_in_the_first_book_is_not_written(self):
        statement = self.set_aside_as_ambiguous()
        flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        self.assertTrue(self.read(FakeFlex(statements=(statement,))))
        self.assertFalse(self.ledger.exists())
        status = self.status()
        self.assertEqual((status["resolved"], status["in_first_book"]), (1, 1))
        self.assertIn("900000230", self.out.getvalue())
        self.assertTrue(self.read(FakeFlex(statements=(statement,))), "the decision is kept for later readings")
        self.assertFalse(self.ledger.exists())

    def test_a_resolution_is_never_changed(self):
        self.set_aside_as_ambiguous()
        flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.record_resolutions(str(self.ledger), {"900000230": flex.AFTER_APPROVAL})
        self.assertIn("900000230", str(caught.exception))
        self.assertEqual(flex.read_resolutions(str(self.ledger) + flex.RESOLUTIONS_SUFFIX),
                         {"900000230": flex.IN_FIRST_BOOK})

    def test_an_identical_decision_is_recorded_again_once_the_reader_is_no_longer_waiting(self):
        # After a good reading the status no longer lists the transfer as ambiguous; repeating the same decision is
        # allowed (it changes nothing), while a different one is refused, naming the decision on record.
        statement = self.set_aside_as_ambiguous()
        flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        self.assertTrue(self.read(FakeFlex(statements=(statement,))))
        self.assertNotIn("ambiguous", self.status())
        flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.record_resolutions(str(self.ledger), {"900000230": flex.AFTER_APPROVAL})
        self.assertEqual(str(caught.exception), "transfer 900000230 is already recorded as in-first-book; a recorded "
                                                "decision is never changed (docs/manual/reconciliation.md)")
        self.assertEqual(flex.read_resolutions(str(self.ledger) + flex.RESOLUTIONS_SUFFIX),
                         {"900000230": flex.IN_FIRST_BOOK})

    def test_a_decision_for_a_transfer_already_in_the_ledger_is_refused(self):
        self.assertTrue(self.read(FakeFlex()))
        self.assertIn("900000101", self.payloads())
        Path(str(self.ledger) + flex.STATUS_SUFFIX).write_text(json.dumps(
            {"state": "ambiguous-transfer", "ambiguous": ["900000101"]}))
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.record_resolutions(str(self.ledger), {"900000101": flex.IN_FIRST_BOOK})
        self.assertIn("already journalled as ibkr-flex:900000101", str(caught.exception))
        self.assertFalse(Path(str(self.ledger) + flex.RESOLUTIONS_SUFFIX).exists())

    def test_a_decision_is_refused_unless_the_last_reading_set_that_transfer_aside_as_ambiguous(self):
        self.set_aside_as_ambiguous()
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.record_resolutions(str(self.ledger), {"900000999": flex.IN_FIRST_BOOK})
        self.assertIn("waiting on", str(caught.exception))
        self.assertIn("900000230", str(caught.exception))
        Path(str(self.ledger) + flex.STATUS_SUFFIX).write_text(json.dumps({"state": "read"}))
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        self.assertIn("not waiting on a decision", str(caught.exception))
        Path(str(self.ledger) + flex.STATUS_SUFFIX).unlink()
        with self.assertRaises(flex.StatementRefused):
            flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        self.assertFalse(Path(str(self.ledger) + flex.RESOLUTIONS_SUFFIX).exists())

    def test_a_decision_waits_a_short_while_for_a_reading_then_says_so(self):
        self.set_aside_as_ambiguous()
        waits = []
        with open(str(self.ledger) + flex.LOCK_SUFFIX, "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            with self.assertRaises(flex.StatementRefused) as caught:
                flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK}, wait=3,
                                        sleep=waits.append)
        self.assertIn("a reading is running, try again in a few minutes", str(caught.exception))
        self.assertTrue(waits and sum(waits) >= 3)
        self.assertFalse(Path(str(self.ledger) + flex.RESOLUTIONS_SUFFIX).exists())

    def test_a_recorded_decision_the_statement_now_contradicts_sets_the_reader_aside(self):
        # Recorded while the transfer was dated by day alone; IBKR later gives it a time after the approval.
        self.set_aside_as_ambiguous()
        flex.record_resolutions(str(self.ledger), {"900000230": flex.IN_FIRST_BOOK})
        restated = self.statement(("900000230", "20270114;180000", "2500"), period=("20270114", "20270115"))
        self.assertFalse(self.read(FakeFlex(statements=(restated,))))
        status = self.status()
        self.assertEqual(status["state"], "set-aside")
        self.assertIn("900000230", status["reason"])
        self.assertIn("in-first-book", status["reason"])
        self.assertFalse(self.ledger.exists())

    def test_a_resolutions_file_not_as_the_reader_writes_it_sets_the_reader_aside(self):
        Path(str(self.ledger) + flex.RESOLUTIONS_SUFFIX).write_text('{"900000230": "maybe"}\n')
        self.assert_set_aside(fixture("statement.xml"), "resolutions")

    def test_two_transfers_before_the_first_snapshot_each_take_it_less_those_up_to_it(self):
        # Approved 08:00 New York; +2500 at 10:00 and -300 at 14:00; the first close snapshot, 9030, holds both.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9030.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000215", "20270114;100000", "2500"), ("900000216", "20270114;140000", "-300")),))))
        self.assertEqual({k: (v["equity_before"], v.get("equity_before_basis")) for k, v in self.payloads().items()},
                         {"900000215": (6830.0, "next-snapshot-less-amount"),
                          "900000216": (9330.0, "next-snapshot-less-amount")})

    def test_two_transfers_after_the_close_each_take_that_days_snapshot_less_those_from_it_on(self):
        # The close snapshot, 9600, is read after both: +500 at 16:30 and -200 at 18:00.
        self.chain("2027-01-12T14:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9600.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000217", "20270114;163000", "500"), ("900000218", "20270114;180000", "-200")),))))
        self.assertEqual({k: (v["equity_before"], v.get("equity_before_basis")) for k, v in self.payloads().items()},
                         {"900000217": (9300.0, "same-day-snapshot-less-amount"),
                          "900000218": (9800.0, "same-day-snapshot-less-amount")})

    def test_the_first_runs_approval_is_the_cutoff_in_a_chain_of_two_runs(self):
        # The first run approved on the 13th holds the snapshots; a fresh run approved on the 15th restates a book.
        # A transfer on the 14th is after the first run's approval, so it is written, never taken as in either book.
        self.chain("2027-01-13T13:00:00Z", (("s1", "2027-01-13T21:00:00Z", 8530.0),
                                            ("s2", "2027-01-14T21:00:00Z", 11030.0)))
        fresh = run_approval(ACCOUNT, previous_log=self.log.name, previous_log_final_hash=final_hash(str(self.log)),
                             starting_book=({}, 8000.0))
        fresh["approved_at"] = "2027-01-15T13:00:00Z"
        self.log = self.dir / "run-fresh.log"
        LoggedEngine(str(self.log), approval=fresh)
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000219", "20270114;100000", "100")),))))
        self.assertEqual(self.payloads()["900000219"]["equity_before"], 8530.0)
        self.assertEqual(self.status()["in_first_book"], 0)

    def test_a_transfer_after_an_early_close_takes_that_days_snapshot_less_itself(self):
        # 26 November 2027 closes at 1 p.m. New York (adapter/marketdata/nyse_calendar.py): the close snapshot is
        # stamped 13:00 and read after it, so it holds a deposit at 14:00.
        self.chain("2027-01-12T14:00:00Z", (("s1", "2027-11-24T21:00:00Z", 4900.0),
                                            ("s2", "2027-11-26T18:00:00Z", 6000.0)))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000220", "20271126;140000", "1000"),
                                                                      period=("20270112", "20271126")),))))
        self.assertEqual((self.payloads()["900000220"]["equity_before"],
                          self.payloads()["900000220"].get("equity_before_basis")),
                         (5000.0, "same-day-snapshot-less-amount"))

    def test_a_close_is_the_sessions_official_close(self):
        for moment, close in ((datetime(2027, 1, 14, 21, 0, tzinfo=timezone.utc), True),
                              (datetime(2027, 11, 26, 18, 0, tzinfo=timezone.utc), True),
                              (datetime(2027, 11, 26, 21, 0, tzinfo=timezone.utc), False),
                              (datetime(2027, 1, 16, 21, 0, tzinfo=timezone.utc), False),
                              (datetime(2027, 1, 14, 21, 0, 1, tzinfo=timezone.utc), False)):
            with self.subTest(moment=moment):
                self.assertEqual(flex._is_close_stamp(moment), close)

    def test_a_snapshot_outside_the_nyse_calendar_is_refused(self):
        moment = datetime(flex.nyse_calendar.LAST_YEAR + 1, 1, 2, 21, 0, tzinfo=timezone.utc)
        with self.assertRaises(flex.StatementRefused) as caught:
            flex._is_close_stamp(moment)
        self.assertIn("NYSE calendar", str(caught.exception))

    def test_a_transfer_after_a_snapshot_outside_the_nyse_calendar_sets_the_reader_aside(self):
        year = flex.nyse_calendar.LAST_YEAR + 1
        self.chain("2027-01-12T14:00:00Z", (("s1", "{}-01-02T21:00:00Z".format(year), 9000.0),))
        status = self.assert_set_aside(self.statement(("900000227", "{}0103;100000".format(year), "100"),
                                                      period=("20270112", "{}0103".format(year))),
                                       "NYSE calendar", "{}-01-02T21:00:00Z".format(year))
        self.assertIsNone(status["code"])

    def test_a_transfer_after_the_approval_with_no_snapshot_yet_sets_the_reader_aside(self):
        self.chain("2027-01-14T13:00:00Z", ())
        self.assert_set_aside(self.statement(("900000203", "20270114;140000", "2500")), "900000203",
                              "first close snapshot")

    def test_a_chain_with_no_first_run_approval_sets_the_reader_aside(self):
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9030.0),), approved=False)
        self.assert_set_aside(fixture("statement.xml"), "starting book")

    def test_only_the_first_runs_approval_is_the_first_books_time(self):
        # A fresh run's approval may restate a book; a chain whose first run has no approval has no first book.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-13T21:00:00Z", 8530.0),), approved=False)
        fresh = run_approval(ACCOUNT, previous_log=self.log.name, previous_log_final_hash=final_hash(str(self.log)),
                             starting_book=({}, 8000.0))
        self.log = self.dir / "run-fresh.log"
        LoggedEngine(str(self.log), approval=fresh)
        self.assert_set_aside(fixture("statement.xml"), "starting book")

    def test_a_snapshot_the_reader_does_not_know_sets_it_aside(self):
        commit(self.engine, "s9", kind="account.snapshot", version=4, payload={"as_of": "2027-01-15T21:00:00Z"})
        self.assert_set_aside(fixture("statement.xml"), "version 4")

    def test_a_late_added_earlier_transfer_is_written_and_says_a_later_one_was_written_first(self):
        # Evening of the 14th: X at 10:00, +1000, written from the 13th's close (8000). Evening of the 15th IBKR also
        # shows W at 09:00 the same day, +500: W is written from the same close, and says X was written first, whose
        # equity_before (8000, without W) is not revised.
        self.chain("2027-01-12T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 8000.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000221", "20270114;100000", "1000")),))))
        first = self.ledger.read_text()
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000222", "20270114;090000", "500"), ("900000221", "20270114;100000", "1000")),))))
        self.assertTrue(self.ledger.read_text().startswith(first), "the record written first is not changed")
        records = {r["payload"]["transaction_id"]: r for r in self.records()}
        self.assertEqual(records["900000222"]["payload"]["equity_before"], 8000.0)
        self.assertEqual(records["900000222"]["note"], flex.LATER_WRITTEN_FIRST)
        self.assertNotIn("note", records["900000221"])
        self.assertEqual(self.status()["out_of_order"], 1)
        self.assertIn("900000222", self.out.getvalue())

    def test_a_late_added_later_evening_transfer_says_an_earlier_one_was_written_first(self):
        # After the close the earlier transfer's figure takes the later ones off the snapshot, so one added later in
        # the same evening leaves the earlier record's figure without it: that is said on the new record too.
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000223", "20270114;163000", "100")),))))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000223", "20270114;163000", "100"), ("900000224", "20270114;180000", "200")),))))
        records = {r["payload"]["transaction_id"]: r for r in self.records()}
        self.assertEqual(records["900000224"]["payload"]["equity_before"], 10830.0)
        self.assertEqual(records["900000224"]["note"], flex.EARLIER_WRITTEN_FIRST)
        self.assertEqual(self.status()["out_of_order"], 1)

    def test_a_late_added_evening_transfer_says_an_earlier_one_from_before_the_first_snapshot_was_written_first(self):
        # Approved 08:00 New York on the 14th; +1000 at 10:00 is written from that day's close snapshot less the
        # transfers up to its reading. A +200 at 18:00 IBKR adds a reading late is in that figure too (the snapshot,
        # read later, holds it), so the new record says the earlier one was written first.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9200.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000228", "20270114;100000", "1000")),))))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000228", "20270114;100000", "1000"), ("900000229", "20270114;180000", "200")),))))
        records = {r["payload"]["transaction_id"]: r for r in self.records()}
        self.assertEqual(records["900000228"]["payload"]["equity_before"], 8200.0)
        self.assertEqual(records["900000229"]["payload"]["equity_before"], 9000.0)
        self.assertEqual(records["900000229"]["note"], flex.EARLIER_WRITTEN_FIRST)
        self.assertEqual(self.status()["out_of_order"], 1)

    def test_a_late_added_transfer_before_the_first_snapshot_after_an_evening_one_carries_no_note(self):
        # The evening transfer's figure takes only that evening's transfers off the snapshot, so a morning one added
        # later is in no figure already written.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9200.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000229", "20270114;180000", "200")),))))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000228", "20270114;100000", "1000"), ("900000229", "20270114;180000", "200")),))))
        self.assertNotIn("note", {r["payload"]["transaction_id"]: r for r in self.records()}["900000228"])
        self.assertEqual(self.status()["out_of_order"], 0)

    def test_a_ledger_note_not_one_the_reader_writes_sets_the_reader_aside(self):
        self.read(FakeFlex())
        good = self.ledger.read_text().splitlines()
        record = json.loads(good[1])
        record["note"] = "anything"
        self.ledger.write_text(good[0] + "\n" + json.dumps(record) + "\n")
        self.assert_set_aside(fixture("statement.xml"), "line 2")

    def test_transfers_in_one_reading_in_time_order_carry_no_note(self):
        self.read(FakeFlex())
        self.assertEqual([("note" in r) for r in self.records()], [False, False])
        self.assertEqual(self.status()["out_of_order"], 0)

    def test_each_reading_keeps_the_period_it_covered_less_its_last_day(self):
        # The statement's last day is not complete when it is read: a transfer that day can reach IBKR's report
        # after the evening's reading, so it is covered only up to the day before.
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual(self.coverage(), {"from_date": "20270112", "covered_to": "20270114"})
        self.assertEqual(self.status()["covered_to"], "20270114")

    def test_a_statement_starting_on_the_last_readings_last_day_is_read(self):
        self.assertTrue(self.read(FakeFlex()))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(period=("20270115", "20270118")),))))
        self.assertEqual(self.coverage(), {"from_date": "20270115", "covered_to": "20270117"})

    def test_a_statement_starting_the_day_after_the_last_readings_last_day_is_a_coverage_gap(self):
        # The last reading ended on the 15th, read that evening: a transfer IBKR posts for the 15th afterwards is in
        # no statement that starts on the 16th.
        self.assertTrue(self.read(FakeFlex()))
        self.assertFalse(self.read(FakeFlex(statements=(self.statement(period=("20270116", "20270118")),))))
        self.assertEqual(self.status()["state"], "coverage-gap")
        self.assertIn("20270115", self.status()["reason"])
        self.assertEqual(self.coverage(), {"from_date": "20270112", "covered_to": "20270114"}, "coverage not moved on")

    def test_a_statement_that_leaves_days_unread_since_the_last_reading_is_a_coverage_gap(self):
        # The last reading covered up to the 15th; one from the 17th leaves the 16th unread, and a transfer that day
        # would be lost, so nothing is written and the reading is set aside naming both days.
        self.assertTrue(self.read(FakeFlex()))
        before = self.ledger.read_bytes()
        self.assertFalse(self.read(FakeFlex(statements=(self.statement(
            ("900000225", "20270117;100000", "100"), period=("20270117", "20270120")),))))
        self.assertEqual(self.ledger.read_bytes(), before)
        status = self.status()
        self.assertEqual(status["state"], "coverage-gap")
        self.assertIn("20270115", status["reason"])
        self.assertIn("20270117", status["reason"])
        self.assertEqual(self.coverage(), {"from_date": "20270112", "covered_to": "20270114"}, "coverage not moved on")
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000225", "20270117;100000", "100"), period=("20270110", "20270120")),))))
        self.assertIn("900000225", self.payloads())

    def test_a_statement_ending_days_before_the_reading_is_a_coverage_gap(self):
        # The reading is on the 15th, so the last session before it is the 14th. A custom period left in place ends
        # on the 5th: every later evening returns the same window, so transfers after its end would be in no
        # statement. Nothing is written, naming the 5th and the 14th.
        self.assertFalse(self.read(FakeFlex(statements=(self.statement(period=("20270101", "20270105")),))))
        status = self.status()
        self.assertEqual(status["state"], "coverage-gap")
        self.assertIn("20270105", status["reason"])
        self.assertIn("20270114", status["reason"])
        self.assertFalse(self.ledger.exists())
        self.assertFalse(Path(str(self.ledger) + flex.COVERAGE_SUFFIX).exists())

    def test_a_monday_reading_of_a_statement_ending_on_the_friday_is_read(self):
        self.assertTrue(self.read_on(date(2027, 1, 25), "20270122"))

    def test_a_reading_after_a_monday_holiday_of_a_statement_ending_on_the_friday_is_read(self):
        # The 18th is Martin Luther King Day: the last session before the 19th is Friday the 15th.
        self.assertTrue(self.read_on(date(2027, 1, 19), "20270115"))

    def test_a_statement_ending_the_session_before_the_last_one_is_read(self):
        # Whether IBKR's last day is the last business day or one earlier, a nightly reading is not refused.
        self.assertTrue(self.read_on(date(2027, 1, 19), "20270114"))

    def test_a_statement_ending_two_sessions_before_the_last_one_is_a_coverage_gap(self):
        self.assertFalse(self.read_on(date(2027, 1, 19), "20270113"))
        status = self.status()
        self.assertEqual(status["state"], "coverage-gap")
        self.assertIn("20270113", status["reason"])
        self.assertIn("20270115", status["reason"], "the latest day it should end on")
        self.assertIn("period", status["reason"])

    def test_a_reading_on_a_day_the_nyse_calendar_does_not_hold_is_set_aside(self):
        # The calendar holds 2022 to 2030: from 2031 no reading can say whether its statement is current.
        at = datetime(2031, 1, 6, 21, 30, tzinfo=flex.NEW_YORK).astimezone(timezone.utc)
        self.assertFalse(self.read(FakeFlex(statements=(self.statement(period=("20270112", "20270115")),)), now=at))
        status = self.status()
        self.assertEqual(status["state"], "set-aside")
        self.assertIn("20310106", status["reason"], "the day it could not check")
        self.assertIn("NYSE calendar", status["reason"])
        self.assertIn("nyse_calendar.py", status["reason"], "the remedy: extend the calendar")
        self.assertFalse(self.ledger.exists())

    def test_a_saturday_reading_of_a_statement_ending_on_yesterdays_date_is_read(self):
        self.assertTrue(self.read_on(date(2027, 1, 16), "20270115"))

    def test_a_sunday_reading_of_a_statement_ending_on_yesterdays_date_is_read(self):
        self.assertTrue(self.read_on(date(2027, 1, 17), "20270116"))

    def test_a_thanksgiving_week_reading_of_a_statement_ending_on_the_wednesday_is_read(self):
        self.assertTrue(self.read_on(date(2027, 11, 26), "20271124"))

    def test_the_first_reading_reaching_back_to_the_approval_day_is_read(self):
        # The chain's first run was approved on 12 January (New York): a first statement from that day leaves none
        # unread.
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(period=("20270112", "20270115")),))))
        self.assertEqual(self.status()["state"], "read")

    def test_the_first_reading_starting_after_the_approval_day_is_a_coverage_gap(self):
        # Approved 12 January; the reader first reads on 1 March with a 30-day window: a transfer between the two
        # would be in no statement, so nothing is written until one reaches back to the approval day.
        self.assertFalse(self.read(FakeFlex(statements=(self.statement(
            ("900000226", "20270305;100000", "100"), period=("20270301", "20270330")),))))
        status = self.status()
        self.assertEqual(status["state"], "coverage-gap")
        self.assertIn("20270112", status["reason"])
        self.assertIn("20270301", status["reason"])
        self.assertFalse(self.ledger.exists())
        self.assertFalse(Path(str(self.ledger) + flex.COVERAGE_SUFFIX).exists(), "no coverage is kept from it")
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000226", "20270305;100000", "100"), period=("20270112", "20270330")),))))
        self.assertIn("900000226", self.payloads())

    def test_a_lost_coverage_file_is_a_first_reading(self):
        self.assertTrue(self.read(FakeFlex()))
        Path(str(self.ledger) + flex.COVERAGE_SUFFIX).unlink()
        self.assertFalse(self.read(FakeFlex(statements=(self.statement(period=("20270115", "20270118")),))))
        self.assertEqual(self.status()["state"], "coverage-gap")
        self.assertIn("20270112", self.status()["reason"])

    def test_a_statement_without_its_period_is_refused(self):
        for bad in (b'fromDate="20270112" ', b'toDate="20270115" '):
            with self.subTest(bad=bad):
                self.assert_set_aside(fixture("statement.xml").replace(bad, b""), "period")
        self.assert_set_aside(fixture("statement.xml").replace(b'toDate="20270115"', b'toDate="2027-01-15"'),
                              "period")

    def test_a_coverage_file_not_as_the_reader_writes_it_sets_the_reader_aside(self):
        self.read(FakeFlex())
        Path(str(self.ledger) + flex.COVERAGE_SUFFIX).write_text("{not json\n")
        self.assert_set_aside(fixture("statement.xml"), "coverage")

    def test_transfers_between_two_snapshots_count_toward_the_equity_before_the_next(self):
        self.chain("2027-01-12T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 1000.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000204", "20270114;100000", "5000"), ("900000205", "20270114;110000", "-2000")),))))
        self.assertEqual({k: (v["equity_before"], v.get("equity_before_basis")) for k, v in self.payloads().items()},
                         {"900000204": (1000.0, None), "900000205": (6000.0, None)})

    def test_a_transfer_already_in_the_ledger_counts_toward_a_later_one(self):
        self.chain("2027-01-12T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 1000.0),))
        self.read(FakeFlex(statements=(self.statement(("900000204", "20270114;100000", "5000")),)))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000204", "20270114;100000", "5000"), ("900000205", "20270114;110000", "-2000")),))))
        self.assertEqual(self.payloads()["900000205"]["equity_before"], 6000.0)

    def test_a_transfer_before_the_close_takes_the_previous_close(self):
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000206", "20270114;153000", "100")),))))
        self.assertEqual((self.payloads()["900000206"]["equity_before"],
                          self.payloads()["900000206"].get("equity_before_basis")), (8530.0, None))

    def test_a_transfer_after_the_close_takes_that_days_snapshot_less_itself(self):
        # The post-close reading is stamped 16:00 but read later: that day's snapshot already holds the transfer.
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000207", "20270114;163000", "100")),))))
        self.assertEqual(self.payloads()["900000207"], {
            "as_of": "2027-01-14T21:30:00Z", "amount": 100.0, "equity_before": 10930.0, "currency": "USD",
            "transaction_id": "900000207", "equity_before_basis": "same-day-snapshot-less-amount"})

    def test_a_transfer_at_the_close_takes_that_days_snapshot_less_itself(self):
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000213", "20270114;160000", "100")),))))
        self.assertEqual((self.payloads()["900000213"]["equity_before"],
                          self.payloads()["900000213"].get("equity_before_basis")),
                         (10930.0, "same-day-snapshot-less-amount"))

    def test_a_transfer_in_another_currency_sets_the_reader_aside(self):
        self.assert_set_aside(self.statement(("900000208", "20270115;101500", "100", "EUR")), "900000208", "EUR")

    def test_a_transfer_of_nothing_sets_the_reader_aside(self):
        self.assert_set_aside(self.statement(("900000209", "20270115;101500", "0")), "900000209", "of nothing")

    def test_a_withdrawal_to_zero_sets_the_reader_aside(self):
        self.assert_set_aside(self.statement(("900000210", "20270115;101500", "-11030")), "900000210", "zero")

    def test_a_type_the_reader_does_not_know_is_counted_and_said(self):
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000211", "20270115;101500", "5", "USD", "Bond Interest Received"),
            ("900000212", "20270115;101600", "6", "USD", "Bond Interest Received")),))))
        self.assertEqual(self.status()["unknown_types"], {"Bond Interest Received": 2})
        self.assertIn("Bond Interest Received", self.out.getvalue())
        self.assertFalse(self.ledger.exists())

    def test_a_reading_says_running_before_it_asks_ibkr(self):
        # A reading killed part way (its unit's timeout) leaves "running", never the previous evening's "read".
        seen = []

        def get(url, params):
            seen.append(self.status())
            return FakeFlex()(url, params)
        self.read(get)
        self.assertEqual((seen[0]["state"], seen[0]["checked_at"]), ("running", "2027-01-16T02:30:00Z"))
        self.assertEqual(self.status()["state"], "read")

    def test_a_rejected_token_sets_the_reader_aside_and_writes_nothing(self):
        self.assertFalse(self.read(FakeFlex(send="send_expired.xml")))
        self.assertFalse(self.ledger.exists())
        status = self.status()
        self.assertEqual((status["state"], status["code"]), ("set-aside", "1012"))
        self.assertIn("token", status["reason"])
        self.assertNotIn(TOKEN, json.dumps(status) + self.out.getvalue())

    def test_a_service_error_sets_the_reader_aside(self):
        self.assertFalse(self.read(FakeFlex(send="send_busy.xml")))
        self.assertEqual((self.status()["state"], self.status()["code"]), ("set-aside", "1009"))

    def test_a_statement_for_another_account_writes_nothing(self):
        self.assertFalse(self.read(FakeFlex(statements=("other_account.xml",))))
        self.assertFalse(self.ledger.exists())
        self.assertNotIn("DU0000999", json.dumps(self.status()) + self.out.getvalue())

    def test_a_statement_with_no_cash_section_writes_nothing(self):
        self.assertFalse(self.read(FakeFlex(statements=("no_cash_section.xml",))))
        self.assertIn("Cash Transactions", self.status()["reason"])

    def test_a_malformed_statement_writes_nothing(self):
        self.assertFalse(self.read(FakeFlex(statements=("truncated.xml",))))
        self.assertFalse(self.ledger.exists())

    def test_a_torn_ledger_is_never_appended_to(self):
        self.read(FakeFlex())
        whole = self.ledger.read_bytes()
        self.ledger.write_bytes(whole[:-5])
        self.assert_set_aside(fixture("statement.xml"), "whole line")

    def test_a_ledger_value_not_as_the_reader_writes_it_sets_the_reader_aside_naming_its_line(self):
        self.read(FakeFlex())
        good = self.ledger.read_text().splitlines()
        for field, value in (("amount", "2500"), ("amount", True), ("amount", 0), ("equity_before", None),
                             ("equity_before", "8530"), ("equity_before", -1), ("currency", "usd"),
                             ("currency", 840), ("equity_before_basis", "a guess")):
            with self.subTest(field=field, value=value):
                record = json.loads(good[1])
                record["payload"][field] = value
                self.ledger.write_text(good[0] + "\n" + json.dumps(record) + "\n")
                self.assert_set_aside(fixture("statement.xml"), "line 2")

    def test_a_ledger_value_that_is_not_finite_sets_the_reader_aside(self):
        self.read(FakeFlex())
        good = self.ledger.read_text().splitlines()
        self.ledger.write_text(good[0].replace('"amount":2500.0', '"amount":NaN') + "\n")
        self.assert_set_aside(fixture("statement.xml"), "line 1")

    def test_a_ledger_not_as_the_reader_writes_it_is_never_appended_to(self):
        self.ledger.write_text('{"id": "something else"}\n')
        self.assertFalse(self.read(FakeFlex()))
        self.assertEqual(self.ledger.read_text(), '{"id": "something else"}\n')
        self.assertIn("ledger", self.status()["reason"])

    def costs(self):
        return [json.loads(line) for line in self.costs_ledger.read_text().splitlines()]

    @property
    def costs_ledger(self):
        return self.dir / "costs.jsonl"

    def test_each_commission_fee_and_interest_posting_is_one_cost_record_beside_the_cash_ledger(self):
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual(self.costs(), [
            {"id": "ibkr-flex:900000104", "type": "account.cost", "schema_version": 1,
             "event_time": "2027-01-13T05:00:00Z", "source": "ibkr-flex",
             "payload": {"kind": "interest-paid", "amount": -3.21, "currency": "USD",
                         "as_of": "2027-01-13T05:00:00Z", "transaction_id": "900000104"}},
            {"id": "ibkr-flex:900000105", "type": "account.cost", "schema_version": 1,
             "event_time": "2027-01-13T05:00:00Z", "source": "ibkr-flex",
             "payload": {"kind": "interest-received", "amount": 0.42, "currency": "USD",
                         "as_of": "2027-01-13T05:00:00Z", "transaction_id": "900000105"}},
            {"id": "ibkr-flex:900000106", "type": "account.cost", "schema_version": 1,
             "event_time": "2027-01-13T05:00:00Z", "source": "ibkr-flex",
             "payload": {"kind": "commission", "amount": -1.0, "currency": "USD",
                         "as_of": "2027-01-13T05:00:00Z", "transaction_id": "900000106"}},
            {"id": "ibkr-flex:900000107", "type": "account.cost", "schema_version": 1,
             "event_time": "2027-01-13T05:00:00Z", "source": "ibkr-flex",
             "payload": {"kind": "fee", "amount": -10.0, "currency": "USD",
                         "as_of": "2027-01-13T05:00:00Z", "transaction_id": "900000107"}},
        ], "dividends are not costs; the transfers stay in the cash ledger")
        self.assertEqual(self.costs_status()["costs_written"], 4)

    def test_the_cost_payload_is_the_one_the_engine_decodes(self):
        # internal/event's TestTheIBKRAdapterPayloadsDecodeAndValidate reads this file at version 1.
        self.read(FakeFlex())
        self.assertEqual(self.costs()[2]["payload"], json.loads((TESTDATA / "cost.json").read_text()))

    def test_a_second_reading_writes_no_cost_twice(self):
        self.read(FakeFlex())
        first = self.costs_ledger.read_bytes()
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual(self.costs_ledger.read_bytes(), first)
        self.assertEqual(self.costs_status()["costs_written"], 0)

    def test_a_later_statement_adds_only_the_new_cost(self):
        self.read(FakeFlex())
        later = fixture("statement.xml").replace(
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Other Fees"',
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Other Fees" dateTime="20270115" '
            b'amount="-2.50" transactionID="900000120" levelOfDetail="DETAIL"/>\n'
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Other Fees"')
        self.read(FakeFlex(statements=(later,)))
        self.assertEqual([r["id"] for r in self.costs()][-1], "ibkr-flex:900000120")
        self.assertEqual(len(self.costs()), 5)

    def costs_status(self):
        return json.loads(Path(str(self.costs_ledger) + flex.STATUS_SUFFIX).read_text())

    def assert_costs_set_aside(self, statement, *said, state="set-aside"):
        """A reading of statement sets aside the costs alone: the cash reading is read and its transfers written."""
        before = self.costs_ledger.read_bytes() if self.costs_ledger.exists() else None
        self.assertFalse(self.read(FakeFlex(statements=(statement,))), "a costs reading set aside fails the unit")
        self.assertEqual(self.costs_ledger.read_bytes() if self.costs_ledger.exists() else None, before,
                         "the costs ledger is untouched")
        self.assertEqual(self.status()["state"], "read", "the costs never stop the cash reading")
        status = self.costs_status()
        self.assertEqual(status["state"], state)
        for text in said:
            self.assertIn(text, status["reason"])
        return status

    def restated_statement(self, amount="-11"):
        """The fixture statement with cost 900000107 stated at amount, and a new deposit beside it."""
        changed = fixture("statement.xml").replace(b'amount="-10" transactionID',
                                                   'amount="{}" transactionID'.format(amount).encode())
        return changed.replace(
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Dividends"',
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Deposits/Withdrawals" '
            b'dateTime="20270115;120000" amount="100" transactionID="900000130" levelOfDetail="DETAIL"/>\n'
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Dividends"')

    def test_a_known_cost_restated_is_recorded_and_the_same_reading_reads_on(self):
        self.read(FakeFlex())
        before = self.costs()
        changed = self.restated_statement()
        later = changed.replace(
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Other Fees"',
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Other Fees" dateTime="20270115" '
            b'amount="-2.50" transactionID="900000120" levelOfDetail="DETAIL"/>\n'
            b'<CashTransaction accountId="DU0000001" currency="USD" type="Other Fees"')
        self.assertTrue(self.read(FakeFlex(statements=(later,))), "a restatement does not fail the reading")
        self.assertEqual(self.status()["state"], "read")
        self.assertIn("ibkr-flex:900000130", [r["id"] for r in self.records()], "the new deposit is still recorded")
        status = self.costs_status()
        self.assertEqual(status["state"], "read", "the same reading reads on")
        self.assertEqual(status["covered_to"], "20270114", "and advances the period")
        self.assertEqual(status["costs_written"], 1, "the other new cost is written in the same reading")
        self.assertEqual(status["restated"], 1)
        self.assertIn("900000107", self.out.getvalue(), "the restatement is said once as a notice")
        self.assertEqual(self.costs()[:len(before)], before, "a record once written is never changed")
        self.assertEqual(self.costs()[len(before):], [
            {"id": "ibkr-flex:900000107:restated:1", "type": "account.cost", "schema_version": 1,
             "event_time": "2027-01-13T05:00:00Z", "source": "ibkr-flex",
             "payload": {"kind": "fee", "amount": -11.0, "currency": "USD",
                         "as_of": "2027-01-13T05:00:00Z", "transaction_id": "900000107"},
             "note": "restated: was fee -10.0 USD on 2027-01-13T05:00:00Z, now fee -11.0 USD on 2027-01-13T05:00:00Z"},
            self.costs()[-1],
        ], "the restatement names the old and the new value; the latest counts")
        self.assertEqual(self.costs()[-1]["id"], "ibkr-flex:900000120")
        self.assertTrue(self.read(FakeFlex(statements=(later,))), "the next reading reads on")
        self.assertEqual(self.costs_status().get("restated", 0), 0, "and says nothing more of it")
        self.assertEqual(len(self.costs()), len(before) + 2, "the restated value is now the ledger's")
        again = later.replace(b'amount="-11" transactionID', b'amount="-12" transactionID')
        self.assertTrue(self.read(FakeFlex(statements=(again,))))
        self.assertIn("ibkr-flex:900000107:restated:2", [r["id"] for r in self.costs()])

    def test_a_cost_restated_to_zero_is_a_reversal_both_readers_accept(self):
        self.read(FakeFlex())
        before = self.costs()
        self.assertTrue(self.read(FakeFlex(statements=(self.restated_statement("0"),))), "a reversal does not fail")
        status = self.costs_status()
        self.assertEqual((status["state"], status["restated"]), ("read", 1))
        reversal = self.costs()[len(before)]
        self.assertEqual((reversal["id"], reversal["payload"]["amount"]), ("ibkr-flex:900000107:restated:1", 0.0))
        self.assertTrue(self.read(FakeFlex(statements=(self.restated_statement("0"),))), "the next reading reads it")
        self.assertEqual(self.costs_status().get("restated", 0), 0, "and does not record it again")
        self.assertEqual(len(self.costs()), len(before) + 1)
        self.assertTrue(self.read(FakeFlex(statements=(self.restated_statement("-12"),))), "and can be restated again")
        self.assertIn("ibkr-flex:900000107:restated:2", [r["id"] for r in self.costs()])

    def test_a_new_cost_of_zero_is_never_read_as_written(self):
        self.read(FakeFlex())
        lines = self.costs_ledger.read_text().splitlines()
        zero = json.loads(lines[0])
        zero["payload"]["amount"] = 0.0
        self.costs_ledger.write_text("".join(json.dumps(r) + "\n" for r in [zero] + [json.loads(x) for x in lines[1:]]))
        self.assert_costs_set_aside(fixture("statement.xml"), "not a cost as this reader writes it")

    def test_a_cost_restated_past_a_float_sets_the_costs_aside_writing_nothing(self):
        self.read(FakeFlex())
        status = self.assert_costs_set_aside(self.restated_statement("1e999"), "900000107")
        self.assertNotIn("not a cost as this reader writes it", status["reason"])
        self.assertTrue(self.read(FakeFlex()), "the ledger is still one both readers accept")

    def test_a_cost_that_does_not_round_trip_through_a_float_is_not_restated_every_reading(self):
        long = "-1234567890.12345678901"
        statement = fixture("statement.xml").replace(b'amount="-10" transactionID', 'amount="{}" transactionID'.format(long).encode())
        self.assertTrue(self.read(FakeFlex(statements=(statement,))))
        first = self.costs_ledger.read_bytes()
        for _ in range(2):
            self.assertTrue(self.read(FakeFlex(statements=(statement,))))
            self.assertEqual(self.costs_ledger.read_bytes(), first, "the same statement adds nothing")
            self.assertEqual(self.costs_status().get("restated", 0), 0)

    def test_a_restatement_line_not_as_the_reader_writes_it_sets_the_costs_aside(self):
        self.read(FakeFlex())
        good = self.costs_ledger.read_text()
        restated = json.loads(good.splitlines()[3])
        restated["id"] += ":restated:1"
        restated["note"] = "restated: was fee -10.0 USD on 2027-01-13T05:00:00Z, now fee -10.0 USD on 2027-01-13T05:00:00Z"
        for name, change in (("a second restatement numbered 1", lambda r: r),
                             ("a note that is not a restatement", lambda r: dict(r, note="edited")),
                             ("a restatement of a cost never recorded",
                              lambda r: dict(r, id="ibkr-flex:900000199:restated:1",
                                             payload=dict(r["payload"], transaction_id="900000199")))):
            with self.subTest(name):
                lines = [restated, change(dict(restated))] if name.startswith("a second") else [change(dict(restated))]
                self.costs_ledger.write_text(good + "".join(json.dumps(r) + "\n" for r in lines))
                self.assert_costs_set_aside(fixture("statement.xml"), "not a cost as this reader writes it")

    def test_an_unwritable_costs_status_never_stops_the_transfers_being_recorded(self):
        Path(str(self.costs_ledger) + flex.STATUS_SUFFIX).mkdir()
        self.assertFalse(self.read(FakeFlex()), "the costs not recorded fail the unit")
        self.assertEqual(self.status()["state"], "read")
        self.assertEqual([r["id"] for r in self.records()], ["ibkr-flex:900000101", "ibkr-flex:900000102"])
        self.assertIn("costs", self.out.getvalue())

    def test_a_torn_costs_line_sets_aside_only_the_costs_and_the_transfers_are_still_written(self):
        self.costs_ledger.write_text('{"id": "something else"}\n')
        self.assert_costs_set_aside(fixture("statement.xml"), "costs")
        self.assertEqual([r["id"] for r in self.records()], ["ibkr-flex:900000101", "ibkr-flex:900000102"])

    def test_a_costs_ledger_not_utf8_sets_aside_only_the_costs_with_its_reason(self):
        self.costs_ledger.write_bytes(b"\xff\xfe\n")
        self.assert_costs_set_aside(fixture("statement.xml"), "UTF-8")

    def test_the_costs_status_says_running_once_the_cash_reading_is_final(self):
        # A costs step stopped part way leaves "running" in the costs status, never the previous "read"; it is
        # written only after the cash ledger, its period and its status, so it can never stop them.
        self.read(FakeFlex())
        seen = []
        read_costs = flex.read_costs

        def spy(path):
            seen.append((self.costs_status(), self.status()))
            return read_costs(path)
        with unittest.mock.patch.object(flex, "read_costs", spy):
            self.read(FakeFlex(), now=datetime(2027, 1, 17, 2, 30, tzinfo=timezone.utc))
        costs, cash = seen[0]
        self.assertEqual((costs["state"], costs["checked_at"]), ("running", "2027-01-17T02:30:00Z"))
        self.assertEqual((cash["state"], cash["checked_at"]), ("read", "2027-01-17T02:30:00Z"))
        self.assertEqual(self.costs_status()["state"], "read")

    def test_a_reading_set_aside_before_its_statement_sets_aside_the_costs_too(self):
        self.assertFalse(self.read(FakeFlex(send="send_expired.xml")))
        self.assertEqual(self.costs_status()["state"], "set-aside")
        self.assertIn("no statement", self.costs_status()["reason"])

    def test_costs_first_read_after_cash_coverage_must_reach_back_to_the_approval_day(self):
        # An installation whose cash ledger was read before the costs were: the costs keep their own period, so
        # their first reading must reach back to the first run's approval day as the cash ledger's first did.
        Path(str(self.ledger) + flex.COVERAGE_SUFFIX).write_text(
            json.dumps({"from_date": "20270112", "covered_to": "20270114"}) + "\n")
        status = self.assert_costs_set_aside(self.statement(period=("20270115", "20270118")), "20270112",
                                             state="coverage-gap")
        self.assertNotIn("covered_to", status)
        self.assertFalse(self.costs_ledger.exists())

    def test_a_cost_dated_before_the_first_runs_approval_day_is_not_written(self):
        xml = fixture("statement.xml").replace(b'dateTime="20270113" amount="-3.21"',
                                               b'dateTime="20270111" amount="-3.21"')
        self.assertTrue(self.read(FakeFlex(statements=(xml,))))
        self.assertNotIn("ibkr-flex:900000104", [r["id"] for r in self.costs()])
        self.assertEqual(len(self.costs()), 3)

    def test_a_cost_of_nothing_is_not_written(self):
        xml = fixture("statement.xml").replace(b'amount="-10" transactionID', b'amount="0" transactionID')
        self.assertTrue(self.read(FakeFlex(statements=(xml,))))
        self.assertEqual(len(self.costs()), 3)

    def test_a_costs_ledger_not_as_the_reader_writes_it_is_never_appended_to(self):
        self.costs_ledger.write_text('{"id": "something else"}\n')
        self.assert_costs_set_aside(fixture("statement.xml"), "costs")
        self.assertEqual(self.costs_ledger.read_text(), '{"id": "something else"}\n')

    def test_a_torn_costs_ledger_is_never_appended_to(self):
        self.read(FakeFlex())
        self.costs_ledger.write_bytes(self.costs_ledger.read_bytes()[:-5])
        self.assert_costs_set_aside(fixture("statement.xml"), "whole line")

    def test_a_cost_ledger_value_not_as_the_reader_writes_it_sets_the_costs_aside(self):
        self.read(FakeFlex())
        good = self.costs_ledger.read_text().splitlines()
        for field, value in (("amount", "1"), ("amount", 0), ("kind", "dividend"), ("currency", "usd")):
            with self.subTest(field=field, value=value):
                record = json.loads(good[1])
                record["payload"][field] = value
                self.costs_ledger.write_text(good[0] + "\n" + json.dumps(record) + "\n")
                self.assert_costs_set_aside(fixture("statement.xml"), "line 2")

    def test_the_cost_status_file_says_how_far_the_costs_are_recorded(self):
        self.read(FakeFlex())
        status = json.loads(Path(str(self.costs_ledger) + flex.STATUS_SUFFIX).read_text())
        self.assertEqual((status["state"], status["covered_to"]), ("read", "20270114"))

    def test_a_reading_after_a_set_aside_clears_it(self):
        self.read(FakeFlex(send="send_expired.xml"))
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual(self.status()["state"], "read")


def with_trades(xml, *rows):
    """An Activity statement with a Trades section of rows, each (ID, dateTime[, levelOfDetail[, account]])."""
    lines = "".join('        <Trade accountId="{}" symbol="SPY" quantity="4" tradePrice="400.5" dateTime="{}" '
                    'tradeID="{}" levelOfDetail="{}"/>\n'.format(row[3] if len(row) > 3 else ACCOUNT, row[1], row[0],
                                                                row[2] if len(row) > 2 else "EXECUTION")
                    for row in rows)
    return xml.replace(b"      </CashTransactions>\n",
                       b"      </CashTransactions>\n      <Trades>\n" + lines.encode() + b"      </Trades>\n")


def confirms(*rows, extra=b""):
    """trade_confirms.xml with its rows replaced by rows, each (ID, dateTime[, levelOfDetail[, account]]), and extra
    (raw XML) added inside the TradeConfirms section."""
    xml = fixture("trade_confirms.xml")
    start, end = xml.index(b"        <TradeConfirm "), xml.index(b"      </TradeConfirms>")
    lines = "".join('        <TradeConfirm accountId="{}" symbol="SPY" quantity="4" tradePrice="400.5" dateTime="{}" '
                    'tradeID="{}" levelOfDetail="{}"/>\n'.format(row[3] if len(row) > 3 else ACCOUNT, row[1], row[0],
                                                                row[2] if len(row) > 2 else "EXECUTION")
                    for row in rows)
    return xml[:start] + lines.encode() + extra + xml[end:]


class ExecutionsTests(ReadOnceHarness):
    """The executions proof (ADR 0019, amended 2026-10-27): every execution IBKR's Trade Confirmation statement lists
    for the account, and the moment up to which it lists them all, kept beside the ledger for the adapter's morning
    reading."""

    # 05:30 New York on the statement's last day, before the data job's first firing.
    MORNING = datetime(2027, 1, 15, 10, 30, tzinfo=timezone.utc)

    def proof(self):
        return json.loads(Path(flex.executions_path(str(self.ledger))).read_text())

    def read_trades(self, *trades, now=MORNING, trades_query="654321", service=None):
        """A reading of the cash query, then the trades query answered by trades (fixture names or bytes)."""
        service = service or FakeFlex(statements=("statement.xml",) + (trades or ("trade_confirms.xml",)))
        return flex.read_once(TOKEN, QUERY, ACCOUNT, str(self.log), str(self.ledger), get=service,
                              sleep=lambda s: None, now=lambda: now, err=self.out, trades_query_id=trades_query)

    def assert_proof_set_aside(self, *said):
        proof = self.proof()
        self.assertEqual(proof["state"], "set-aside")
        for text in said:
            self.assertIn(text, proof["reason"])
        self.assertNotIn(TOKEN, json.dumps(proof))
        return proof

    def test_a_trades_query_is_read_beside_the_cash_query(self):
        service = FakeFlex(statements=("statement.xml", "trade_confirms.xml"))
        self.assertTrue(self.read_trades(service=service), self.out.getvalue())
        asked = [params.get("q") for url, params in service.calls if url == flex.SEND_REQUEST_URL]
        self.assertEqual(asked, [QUERY, "654321"])
        proof = self.proof()
        self.assertEqual((proof["state"], proof["query"], proof["section"], proof["from_date"], proof["to_date"]),
                         ("read", "trades", "TradeConfirms", "20270108", "20270115"))
        self.assertEqual(proof["account_id"], ACCOUNT, "the account the statement was checked against")
        self.assertEqual(proof["fetched_at"], "2027-01-15T10:30:00Z")
        # A statement lists every execution only up to the start of its last day (an execution late in an
        # evening can carry the next day's trade date), and no later than an hour before it was asked for.
        self.assertEqual(proof["covered_until"], "2027-01-15T05:00:00Z")
        self.assertEqual(proof["executions"], [{"id": "0001f4e8.0001.01.01", "at": "2027-01-14T15:15:00Z"},
                                               {"id": "700000201", "at": "2027-01-14T15:15:00Z"}],
                         "every row counts, an order's row too: a proof can only be stricter for it")
        self.assertEqual(len(self.records()), 2, "the cash ledger is written as before")

    def test_the_trades_query_is_asked_for_the_last_seven_days_and_the_cash_query_for_its_own_period(self):
        # The portal fixes a Trade Confirmation query's period at Today, so the reader asks for the 7 New York
        # calendar days before its own day through that day (Flex Web Service v3 SendRequest fd and td).
        service = FakeFlex(statements=("statement.xml", "trade_confirms.xml"))
        self.assertTrue(self.read_trades(service=service), self.out.getvalue())
        sent = [params for url, params in service.calls if url == flex.SEND_REQUEST_URL]
        self.assertEqual([(p.get("q"), p.get("fd"), p.get("td")) for p in sent],
                         [(QUERY, None, None), ("654321", "20270108", "20270115")])

    def test_a_statement_that_ignored_the_requested_dates_is_set_aside(self):
        # IBKR answering with the query's saved period (Today) instead of the dates asked for.
        today = fixture("trade_confirms.xml").replace(b'fromDate="20270108"', b'fromDate="20270115"')
        self.assertFalse(self.read_trades(today))
        self.assert_proof_set_aside("IBKR ignored the requested dates", "20270108", "20270115")

    def test_a_statement_ending_before_the_requested_day_is_set_aside(self):
        early = fixture("trade_confirms.xml").replace(b'toDate="20270115"', b'toDate="20270114"')
        self.assertFalse(self.read_trades(early))
        self.assert_proof_set_aside("IBKR ignored the requested dates")

    def test_a_requested_start_on_a_holiday_may_begin_at_the_next_session(self):
        # Monday 2027-01-25: seven days before is Martin Luther King Jr. Day; a statement from the session after it
        # still covers every day the request does that held a session.
        monday = datetime(2027, 1, 25, 10, 30, tzinfo=timezone.utc)
        xml = fixture("trade_confirms.xml").replace(b'fromDate="20270108" toDate="20270115"',
                                                    b'fromDate="20270119" toDate="20270125"')
        cash = self.statement(("900000101", "20270114", "2500"), period=("20261226", "20270124"))
        self.assertTrue(self.read_trades(now=monday, service=FakeFlex(statements=(cash, xml))), self.out.getvalue())
        self.assertEqual(self.proof()["from_date"], "20270119")

    def test_a_holiday_reading_ending_on_the_session_before_covers_only_to_that_sessions_start(self):
        # Monday 2027-01-18 (Martin Luther King Jr. Day): td is the holiday and IBKR's statement ends on Friday, the
        # session before. That is the period asked for (no session in between), so the proof is read, but it lists
        # every execution only up to the start of Friday: Friday's evening is not proven.
        holiday = datetime(2027, 1, 18, 10, 30, tzinfo=timezone.utc)
        xml = fixture("trade_confirms.xml").replace(b'fromDate="20270108" toDate="20270115"',
                                                    b'fromDate="20270111" toDate="20270115"')
        cash = self.statement(("900000101", "20270114", "2500"), period=("20261219", "20270117"))
        self.assertTrue(self.read_trades(now=holiday, service=FakeFlex(statements=(cash, xml))), self.out.getvalue())
        proof = self.proof()
        self.assertEqual((proof["state"], proof["to_date"], proof["covered_until"]),
                         ("read", "20270115", "2027-01-15T05:00:00Z"))

    def test_a_proof_never_reaches_past_an_hour_before_it_was_asked_for(self):
        # IBKR: a new execution is typically in a newly generated Trade Confirmation report within 5 to 10 minutes.
        early = datetime(2027, 1, 15, 5, 30, tzinfo=timezone.utc)
        self.assertTrue(self.read_trades(now=early))
        self.assertEqual(self.proof()["covered_until"], "2027-01-15T04:30:00Z")

    def test_the_cash_querys_trades_section_is_no_proof(self):
        # An Activity statement is refreshed once a day and its securities day ends at IBKR's 8:20 p.m. cutoff, so
        # only a Trade Confirmation statement is a proof (ADR 0019, amended 2026-10-27).
        statement = with_trades(fixture("statement.xml"), ("700000302", "20270114;101500"))
        self.assertTrue(self.read(FakeFlex(statements=(statement,)), now=self.MORNING), self.out.getvalue())
        proof = self.proof()
        self.assertEqual(proof["state"], "none")
        self.assertIn("TI_IBKR_FLEX_TRADES_QUERY_ID", proof["reason"])

    def test_no_trades_query_is_no_proof_and_no_failure(self):
        self.assertTrue(self.read(FakeFlex(), now=self.MORNING))
        self.assertEqual(self.proof()["state"], "none")

    def test_an_activity_statement_named_as_the_trades_query_is_set_aside(self):
        statement = with_trades(fixture("statement.xml"), ("700000302", "20270114;101500"))
        self.assertFalse(self.read_trades(statement))
        self.assert_proof_set_aside("Trade Confirmations")

    def test_a_trades_query_without_its_section_is_set_aside(self):
        self.assertFalse(self.read_trades("statement.xml"))
        self.assert_proof_set_aside("Trade Confirmations")

    def test_a_trades_query_set_aside_leaves_no_proof_and_the_cash_written(self):
        class Expired(FakeFlex):
            def __call__(self, url, params):
                if url == flex.SEND_REQUEST_URL and params.get("q") == "654321":
                    self.calls.append((url, dict(params)))
                    return fixture("send_expired.xml")
                return super().__call__(url, params)
        self.assertFalse(self.read_trades(service=Expired()))
        self.assert_proof_set_aside("1012")
        self.assertEqual(len(self.records()), 2)
        self.assertEqual(self.status()["state"], "read")

    def test_an_execution_dated_by_day_alone_is_set_aside(self):
        self.assertFalse(self.read_trades(confirms(("700000303", "20270114"))))
        self.assert_proof_set_aside("700000303")

    def test_an_execution_of_another_account_is_set_aside(self):
        self.assertFalse(self.read_trades(confirms(("700000304", "20270114;163000", "EXECUTION", "DU0000998"))))
        self.assert_proof_set_aside("another account")

    def test_a_summary_row_sets_the_proof_aside(self):
        # A summary totals executions it does not list: what it stands for cannot be checked.
        for level in ("SUMMARY", "SYMBOL_SUMMARY", "ASSET_SUMMARY"):
            with self.subTest(level=level):
                self.assertFalse(self.read_trades(confirms(("700000305", "20270114;101500", level))))
                self.assert_proof_set_aside(level)

    def test_a_summary_only_section_sets_the_proof_aside(self):
        self.assertFalse(self.read_trades(confirms(("", "", "SYMBOL_SUMMARY"))))
        self.assert_proof_set_aside("SYMBOL_SUMMARY")

    def test_an_unknown_element_in_the_section_sets_the_proof_aside(self):
        extra = b'        <Execution accountId="DU0000001" dateTime="20270114;210000" tradeID="700000306"/>\n'
        self.assertFalse(self.read_trades(confirms(extra=extra)))
        self.assert_proof_set_aside("Execution")

    def test_an_element_inside_a_row_sets_the_proof_aside(self):
        xml = confirms(("700000307", "20270114;101500")).replace(
            b'levelOfDetail="EXECUTION"/>', b'levelOfDetail="EXECUTION"><Leg tradeID="9"/></TradeConfirm>')
        self.assertFalse(self.read_trades(xml))
        self.assert_proof_set_aside("700000307")

    def test_an_empty_section_is_a_proof_of_none(self):
        self.assertTrue(self.read_trades(confirms()), self.out.getvalue())
        self.assertEqual((self.proof()["state"], self.proof()["executions"]), ("read", []))

    def test_a_cash_reading_set_aside_still_writes_the_proof_state(self):
        self.assertFalse(self.read_trades(service=FakeFlex(send="send_expired.xml")))
        self.assertEqual(self.proof()["state"], "set-aside")

    def test_a_reading_sets_the_proof_aside_before_it_asks_ibkr(self):
        # A reading stopped part way (its unit's timeout, a kill) leaves a proof set aside, never the last one.
        seen = []
        self.read_trades(confirms())
        self.assertEqual(self.proof()["state"], "read", "setup: a proof read by the reading before")
        service = FakeFlex(statements=("statement.xml", "trade_confirms.xml"))

        def watched(url, params):
            seen.append(self.proof())
            return service(url, params)
        self.read_trades(service=watched)
        self.assertEqual(seen[0]["state"], "set-aside")
        self.assertIn("not finished", seen[0]["reason"])

    def test_a_proof_that_cannot_be_set_aside_never_stops_the_cash_and_is_removed(self):
        # The proof's own file failing (here a directory where its temporary copy goes) never stops a transfer being
        # recorded, and the earlier proof is removed, so the morning delivery finds none (step 5 of the module).
        self.read_trades(confirms())
        self.assertEqual(self.proof()["state"], "read", "setup: a proof read by the reading before")
        Path(flex.executions_path(str(self.ledger)) + ".tmp").mkdir()
        self.ledger.unlink()
        self.assertFalse(self.read_trades())
        self.assertEqual(len(self.records()), 2, "the cash ledger is written")
        self.assertEqual(self.status()["state"], "read")
        self.assertFalse(Path(flex.executions_path(str(self.ledger))).exists(), "no earlier proof is left standing")
        self.assertIn("executions proof", self.out.getvalue())

    def test_the_earlier_proofs_removal_is_made_durable(self):
        self.read_trades(confirms())
        Path(flex.executions_path(str(self.ledger)) + ".tmp").mkdir()
        events = []
        real_unlink, real_sync = flex.os.unlink, flex._sync_directory

        def unlink(path):
            events.append(("unlink", Path(path).name))
            real_unlink(path)

        def sync(path):
            events.append(("sync", path))
            real_sync(path)
        with unittest.mock.patch.object(flex.os, "unlink", unlink), \
                unittest.mock.patch.object(flex, "_sync_directory", sync):
            self.read_trades()
        first = events.index(("unlink", flex.EXECUTIONS_NAME))
        self.assertIn(("sync", str(self.dir)), events[first + 1:first + 2],
                      "the folder is synced right after the earlier proof is removed")

    def test_the_proof_is_set_aside_before_the_status_says_running(self):
        order = []
        real = flex._write_whole

        def watched(path, text):
            order.append(Path(path).name)
            real(path, text)
        with unittest.mock.patch.object(flex, "_write_whole", watched):
            self.read_trades()
        self.assertEqual(order[:2], [flex.EXECUTIONS_NAME, self.ledger.name + flex.STATUS_SUFFIX])

    def test_the_proof_reader_refuses_times_at_the_edge_of_the_calendar(self):
        self.read_trades(confirms())
        path = Path(flex.executions_path(str(self.ledger)))
        good = json.loads(path.read_text())
        for edge in ("0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z"):
            with self.subTest(edge=edge):
                path.write_text(json.dumps(dict(good, checked_at=edge, fetched_at=edge, covered_until=edge)))
                with self.assertRaises(flex.StatementRefused):
                    flex.read_executions(str(path), ACCOUNT)

    def test_the_proof_reader_reads_what_the_reader_wrote(self):
        self.read_trades(confirms(("700000301", "20270114;163000")))
        proof = flex.read_executions(flex.executions_path(str(self.ledger)), ACCOUNT)
        self.assertEqual(proof.covered_until, datetime(2027, 1, 15, 5, 0, tzinfo=timezone.utc))
        self.assertEqual(proof.from_day, date(2027, 1, 8))
        self.assertEqual(proof.executions, [("700000301", datetime(2027, 1, 14, 21, 30, tzinfo=timezone.utc))])

    def test_the_proof_reader_refuses_a_file_not_as_written(self):
        self.read_trades(confirms())
        path = Path(flex.executions_path(str(self.ledger)))
        good = json.loads(path.read_text())
        bad = ["", "{}", '{"state": "read"}', "not json", '{"state": []}', '{"state": {"a": 1}}', "[]", "3",
               json.dumps(dict(good, covered_until="2027-01-16T05:00:00Z")),
               json.dumps(dict(good, fetched_at="2027-01-14T10:30:00Z")),
               json.dumps(dict(good, from_date="20270116")),
               json.dumps(dict(good, executions=[{"id": "x", "at": 3}])),
               json.dumps(dict(good, executions=[["x", "2027-01-14T10:30:00Z"]])),
               json.dumps(dict(good, account_id=None))]
        for text in bad:
            with self.subTest(text=text):
                path.write_text(text)
                with self.assertRaises(flex.StatementRefused):
                    flex.read_executions(str(path), ACCOUNT)

    def test_the_proof_reader_refuses_another_accounts_proof_without_naming_either(self):
        self.read_trades(confirms())
        with self.assertRaises(flex.StatementRefused) as caught:
            flex.read_executions(flex.executions_path(str(self.ledger)), "DU0000998")
        self.assertIn("another account", str(caught.exception))
        self.assertNotIn(ACCOUNT, str(caught.exception))
        self.assertNotIn("DU0000998", str(caught.exception))


class CommandTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.log = self.dir / "run-1.log"
        approval = run_approval(ACCOUNT, starting_book=({}, 8000.0))
        approval["approved_at"] = "2027-01-12T14:00:00Z"  # on the statement's first day, before that session's close
        engine = LoggedEngine(str(self.log), approval=approval)
        snapshot(engine, "s1", "2027-01-13T21:00:00Z", 8530.0)
        snapshot(engine, "s2", "2027-01-14T21:00:00Z", 11030.0)
        self.secrets = self.dir / "secrets"
        self.secrets.write_text("MASSIVE_API_KEY=x\nTI_IBKR_FLEX_TOKEN={}\nTI_IBKR_FLEX_QUERY_ID={}\n".format(
            TOKEN, QUERY))
        self.account = self.dir / "account"
        self.account.write_text("IBKR_ACCOUNT_BOOST={}\nIBKR_ACCOUNT_TRAD_IRA=DU0000777\n".format(ACCOUNT))
        self.ledger = self.dir / "cash-movements.jsonl"

    def run_command(self, service, secrets=None, instance="boost", now=datetime(2027, 1, 16, 2, 30, tzinfo=timezone.utc)):
        err = io.StringIO()
        code = main(["flex", "--secrets", str(secrets or self.secrets), "--account-secrets", str(self.account),
                     "--instance", instance, "--engine-log", str(self.log),
                     "--ledger", str(self.ledger)], err=err, flex_get=service, flex_sleep=lambda s: None,
                    now=lambda: now)
        return code, err.getvalue()

    def test_a_reading_exits_0(self):
        code, said = self.run_command(FakeFlex())
        self.assertEqual(code, 0, said)
        self.assertEqual(len(self.ledger.read_text().splitlines()), 2)
        self.assertNotIn(TOKEN, said)
        self.assertNotIn(ACCOUNT, said)

    def test_the_reading_stamps_the_clock_its_caller_gives(self):
        # The CLI passes its clock to the reader, so a reading on a named day does not depend on the real date.
        code, said = self.run_command(FakeFlex(), now=datetime(2027, 1, 16, 2, 30, tzinfo=timezone.utc))
        self.assertEqual(code, 0, said)
        status = json.loads(Path(str(self.ledger) + flex.STATUS_SUFFIX).read_text())
        self.assertEqual(status["checked_at"], "2027-01-16T02:30:00Z")

    def test_the_instance_names_its_account_line(self):
        # The IRA instance reads its own line, so a statement for the Boost account is refused as another's.
        code, said = self.run_command(FakeFlex(), instance="trad-ira")
        self.assertEqual(code, 1)
        self.assertIn("not this instance's account", said)

    def test_a_set_aside_exits_1_naming_why_and_never_the_token(self):
        code, said = self.run_command(FakeFlex(send="send_expired.xml"))
        self.assertEqual(code, 1)
        self.assertIn("1012", said)
        self.assertNotIn(TOKEN, said)

    def test_resolve_records_the_owners_decision_and_reads_nothing(self):
        err = io.StringIO()
        service = FakeFlex()
        Path(str(self.ledger) + flex.STATUS_SUFFIX).write_text(json.dumps(
            {"state": "ambiguous-transfer", "ambiguous": ["900000230", "900000231"]}))
        code = main(["flex", "--ledger", str(self.ledger), "--resolve", "900000230=after-approval",
                     "--resolve", "900000231=in-first-book"], err=err, flex_get=service)
        self.assertEqual((code, service.calls), (0, []), err.getvalue())
        self.assertEqual(flex.read_resolutions(str(self.ledger) + flex.RESOLUTIONS_SUFFIX),
                         {"900000230": "after-approval", "900000231": "in-first-book"})

    def test_resolve_refuses_a_decision_it_does_not_know(self):
        for bad in ("900000230", "900000230=maybe", "=after-approval"):
            with self.subTest(bad=bad):
                err = io.StringIO()
                self.assertEqual(main(["flex", "--ledger", str(self.ledger), "--resolve", bad], err=err), 1)
                self.assertIn("in-first-book", err.getvalue())
        self.assertFalse(Path(str(self.ledger) + flex.RESOLUTIONS_SUFFIX).exists())

    def test_a_reading_needs_its_secrets_engine_log_and_instance(self):
        err = io.StringIO()
        self.assertEqual(main(["flex", "--ledger", str(self.ledger)], err=err, flex_get=FakeFlex()), 1)
        self.assertIn("--secrets", err.getvalue())

    def test_the_trades_query_id_is_read_when_its_line_is_filled_in(self):
        self.secrets.write_text(self.secrets.read_text() + "TI_IBKR_FLEX_TRADES_QUERY_ID=654321\n")
        service = FakeFlex(statements=("statement.xml", "trade_confirms.xml"))
        code, said = self.run_command(service)
        self.assertEqual(code, 0, said)
        self.assertEqual([p.get("q") for u, p in service.calls if u == flex.SEND_REQUEST_URL], [QUERY, "654321"])

    def test_an_empty_trades_query_line_asks_only_the_cash_query(self):
        self.secrets.write_text(self.secrets.read_text() + "TI_IBKR_FLEX_TRADES_QUERY_ID=\n")
        service = FakeFlex()
        code, said = self.run_command(service)
        self.assertEqual(code, 0, said)
        self.assertEqual([p.get("q") for u, p in service.calls if u == flex.SEND_REQUEST_URL], [QUERY])

    def test_a_missing_token_is_refused_before_anything_is_asked(self):
        bare = self.dir / "bare"
        bare.write_text("TI_IBKR_FLEX_TOKEN=\nTI_IBKR_FLEX_QUERY_ID={}\n".format(QUERY))
        service = FakeFlex()
        code, said = self.run_command(service, secrets=bare)
        self.assertEqual((code, service.calls), (1, []))
        self.assertIn("TI_IBKR_FLEX_TOKEN", said)


class DeploymentTests(unittest.TestCase):
    """The daily job (deploy/systemd/ti-flex@.*): after the close, one instance per account, its own files only."""

    def setUp(self):
        self.service = (repo / "deploy/systemd/ti-flex@.service").read_text()
        self.timer = (repo / "deploy/systemd/ti-flex@.timer").read_text()

    def test_one_template_serves_every_instance(self):
        self.assertFalse((repo / "deploy/systemd/ti-flex-boost.service").exists())
        self.assertFalse((repo / "deploy/systemd/ti-flex-boost.timer").exists())
        self.assertRegex(self.timer, r"(?m)^Unit=ti-flex@%i\.service$")

    def test_the_timer_fires_each_weekday_morning_before_the_data_job(self):
        # The executions proof the morning delivery needs (ADR 0019, amended 2026-10-27) is read before the data
        # job's first firing at 06:05 New York, and again before each later one.
        times = re.findall(r"(?m)^OnCalendar=Mon\.\.Fri ([0-9]{2}:[0-9]{2}) America/New_York$", self.timer)
        self.assertEqual(times, ["05:30", "06:30", "07:30"])
        self.assertRegex(self.timer, r"(?m)^Persistent=true$")

    def test_the_service_reads_the_flex_secrets_and_the_instances_account_file(self):
        # Both from the copies ti-unlock decrypts into a root-only tmpfs after
        # each boot, each asserted, so a locked server refuses the reading
        # and names the file (ADR 0028).
        for source in ("/run/trend-investing/secrets/secrets.env", "/run/trend-investing/secrets/accounts/%i.env"):
            self.assertRegex(self.service, r"(?m)^AssertPathExists={}$".format(re.escape(source)))
        self.assertIn("LoadCredential=secrets:/run/trend-investing/secrets/secrets.env", self.service)
        self.assertIn("LoadCredential=account:/run/trend-investing/secrets/accounts/%i.env", self.service)
        self.assertRegex(self.service, r"(?m)^ExecStart=.* -m adapter\.ibkr flex --secrets %d/secrets "
                                       r"--account-secrets %d/account --instance %i ")
        self.assertIn("--engine-log /var/lib/trend-investing/engines/%i/${TI_RUN}.log", self.service)
        self.assertIn("--ledger /var/lib/trend-investing/flex/%i/cash-movements.jsonl", self.service)
        self.assertIn("costs ledger", self.service, "the costs ledger sits beside the cash ledger, in the same folder")
        self.assertRegex(self.service, r"(?m)^ReadWritePaths=/var/lib/trend-investing/flex/%i$")
        self.assertRegex(self.service, r"(?m)^ReadOnlyPaths=/var/lib/trend-investing/engines/%i$")

    def test_the_unit_outlasts_the_readers_worst_case(self):
        # For each of the two queries, SendRequest, then every GetStatement attempt, each to its request timeout,
        # and the waits between them.
        worst = 2 * (flex.REQUEST_TIMEOUT * (1 + flex.GENERATION_ATTEMPTS)
                     + flex.GENERATION_WAIT * (flex.GENERATION_ATTEMPTS - 1))
        minutes = int(re.search(r"(?m)^TimeoutStartSec=([0-9]+)min$", self.service).group(1))
        self.assertGreater(minutes * 60, worst)

    def test_every_instance_has_its_account_file_stub(self):
        # The unit's LoadCredential of accounts/%i.env needs the file, so provision.sh writes each, named only, and
        # the reader then refuses it with the line it needs filled in.
        provision = (repo / "deploy/provision.sh").read_text()
        for instance in flex.INSTANCES:
            with self.subTest(instance=instance):
                self.assertIn('write_stub "$etc/accounts/{0}.env" "$here/etc/accounts/{0}.env.example"'.format(
                    instance), provision)
                example = (repo / "deploy/etc/accounts/{}.env.example".format(instance)).read_text()
                self.assertRegex(example, r"(?m)^{}=$".format(flex.account_key(instance)))
                with self.assertRaises(flex.StatementRefused) as caught:
                    flex.read_key(str(repo / "deploy/etc/accounts/{}.env.example".format(instance)),
                                  flex.account_key(instance))
                self.assertIn(flex.account_key(instance), str(caught.exception))

    def test_the_secrets_example_names_the_flex_lines(self):
        example = (repo / "deploy/etc/secrets.env.example").read_text()
        self.assertRegex(example, r"(?m)^TI_IBKR_FLEX_TOKEN=$")
        self.assertRegex(example, r"(?m)^TI_IBKR_FLEX_QUERY_ID=$")
        self.assertRegex(example, r"(?m)^TI_IBKR_FLEX_TRADES_QUERY_ID=$")


if __name__ == "__main__":
    unittest.main()
