"""The Flex cash-movement reader (flex.py): IBKR's Cash Transactions read into the instance's own ledger.

ADR 0007 (a deposit or withdrawal is an account.cash-movement), ADR 0015
(version 2 carries IBKR's transaction ID) and ADR 0019 (the cash-drift
check stays the cross-check). Every statement here is synthetic: a made-up
account, amounts and IDs, never a real statement. No network: the Flex Web
Service is a fake that answers from the files in testdata/flex.
"""
import http.server
import io
import json
import re
import sys
import tempfile
import threading
import unittest
from datetime import datetime, timezone
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
                self.send_header("Location", "/elsewhere" + self.path[self.path.index("?"):])
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
        self.chain("2026-10-17T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 8530.0),
                                            ("s2", "2027-01-14T21:00:00Z", 11030.0)))

    def chain(self, approved_at, snapshots, approved=True):
        """A first run approved at approved_at (its starting book stated), holding snapshots (ID, as of, value)."""
        self.log = self.dir / "run-{}.log".format(len(list(self.dir.glob("run-*.log"))) + 1)
        approval = run_approval(ACCOUNT, starting_book=({}, 8000.0))
        approval["approved_at"] = approved_at
        self.engine = LoggedEngine(str(self.log), approval=approval if approved else None)
        for ident, as_of, value in snapshots:
            snapshot(self.engine, ident, as_of, value)

    def statement(self, *rows):
        """statement.xml with its two transfers replaced by rows, each (ID, dateTime, amount[, currency][, type])."""
        xml = fixture("statement.xml")
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

    def read(self, service):
        return flex.read_once(TOKEN, QUERY, ACCOUNT, str(self.log), str(self.ledger), get=service,
                              sleep=lambda s: None, now=lambda: datetime(2027, 1, 16, 2, 30, tzinfo=timezone.utc),
                              err=self.out)

    def records(self):
        return [json.loads(line) for line in self.ledger.read_text().splitlines()]

    def status(self):
        return json.loads(Path(str(self.ledger) + flex.STATUS_SUFFIX).read_text())

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
        self.chain("2027-01-14T14:00:00Z", (("s1", "2027-01-14T21:00:00Z", 11030.0),))
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
        # A date alone is 00:00 New York, before an approval later that day: in the first book.
        self.chain("2027-01-14T13:00:00Z", (("s1", "2027-01-14T21:00:00Z", 9030.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(("900000202", "20270114", "2500")),))))
        self.assertFalse(self.ledger.exists())
        self.assertEqual(self.status()["in_first_book"], 1)

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

    def test_transfers_between_two_snapshots_count_toward_the_equity_before_the_next(self):
        self.chain("2026-10-17T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 1000.0),))
        self.assertTrue(self.read(FakeFlex(statements=(self.statement(
            ("900000204", "20270114;100000", "5000"), ("900000205", "20270114;110000", "-2000")),))))
        self.assertEqual({k: (v["equity_before"], v.get("equity_before_basis")) for k, v in self.payloads().items()},
                         {"900000204": (1000.0, None), "900000205": (6000.0, None)})

    def test_a_transfer_already_in_the_ledger_counts_toward_a_later_one(self):
        self.chain("2026-10-17T14:00:00Z", (("s1", "2027-01-13T21:00:00Z", 1000.0),))
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
        engine = LoggedEngine(str(self.log), approval=run_approval(ACCOUNT, starting_book=({}, 8000.0)))
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

    def test_the_secrets_example_names_the_flex_lines(self):
        example = (repo / "deploy/etc/secrets.env.example").read_text()
        self.assertRegex(example, r"(?m)^TI_IBKR_FLEX_TOKEN=$")
        self.assertRegex(example, r"(?m)^TI_IBKR_FLEX_QUERY_ID=$")


if __name__ == "__main__":
    unittest.main()
