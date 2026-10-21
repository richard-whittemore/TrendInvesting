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


class ReadOnceTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.ledger = self.dir / "cash-movements.jsonl"
        self.out = io.StringIO()
        self.chain("2027-01-12T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 8530.0),
                                            ("s2", "2027-01-14T21:00:00Z", 11030.0)))

    def chain(self, approved_at, snapshots, approved=True):
        """A first run approved at approved_at (its starting book stated), holding snapshots (ID, as of, value)."""
        self.log = self.dir / "run-{}.log".format(len(list(self.dir.glob("run-*.log"))) + 1)
        approval = run_approval(ACCOUNT, starting_book=({}, 8000.0))
        approval["approved_at"] = approved_at
        self.engine = LoggedEngine(str(self.log), approval=approval if approved else None)
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
        ], "dividends, interest, commissions and fees are not journalled yet")
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

    def test_a_reading_after_a_set_aside_clears_it(self):
        self.read(FakeFlex(send="send_expired.xml"))
        self.assertTrue(self.read(FakeFlex()))
        self.assertEqual(self.status()["state"], "read")


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

    def run_command(self, service, secrets=None, instance="boost"):
        err = io.StringIO()
        code = main(["flex", "--secrets", str(secrets or self.secrets), "--account-secrets", str(self.account),
                     "--instance", instance, "--engine-log", str(self.log),
                     "--ledger", str(self.ledger)], err=err, flex_get=service, flex_sleep=lambda s: None)
        return code, err.getvalue()

    def test_a_reading_exits_0(self):
        code, said = self.run_command(FakeFlex())
        self.assertEqual(code, 0, said)
        self.assertEqual(len(self.ledger.read_text().splitlines()), 2)
        self.assertNotIn(TOKEN, said)
        self.assertNotIn(ACCOUNT, said)

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

    def test_the_timer_fires_after_the_close(self):
        self.assertRegex(self.timer, r"(?m)^OnCalendar=Mon\.\.Fri 2[0-3]:[0-5][0-9] America/New_York$")
        self.assertRegex(self.timer, r"(?m)^Persistent=true$")

    def test_the_service_reads_the_flex_secrets_and_the_instances_account_file(self):
        self.assertIn("LoadCredential=secrets:/etc/trend-investing/secrets.env", self.service)
        self.assertIn("LoadCredential=account:/etc/trend-investing/accounts/%i.env", self.service)
        self.assertRegex(self.service, r"(?m)^ExecStart=.* -m adapter\.ibkr flex --secrets %d/secrets "
                                       r"--account-secrets %d/account --instance %i ")
        self.assertIn("--engine-log /var/lib/trend-investing/engines/%i/${TI_RUN}.log", self.service)
        self.assertIn("--ledger /var/lib/trend-investing/flex/%i/cash-movements.jsonl", self.service)
        self.assertRegex(self.service, r"(?m)^ReadWritePaths=/var/lib/trend-investing/flex/%i$")
        self.assertRegex(self.service, r"(?m)^ReadOnlyPaths=/var/lib/trend-investing/engines/%i$")

    def test_the_unit_outlasts_the_readers_worst_case(self):
        # SendRequest, then every GetStatement attempt, each to its request timeout, and the waits between them.
        worst = (flex.REQUEST_TIMEOUT * (1 + flex.GENERATION_ATTEMPTS)
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


if __name__ == "__main__":
    unittest.main()
