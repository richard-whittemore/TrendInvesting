"""The Traditional IRA's adapter: what the momentum engine reads, from one IBKR reading.

ADR 0026 points 2 and 3; docs/manual/momentum.md, "One IRA month". The
momentum engine reads account.snapshot version 4 (whole shares) or 6
(D7-F), with the settled and total cash its buys are budgeted from and values every order it plans against a
mark for that instrument as of the close (the risk gate refuses one with
none, missing-mark). The compact IRA stream carries bars only for SPY, IEF,
SHY and the holdings; a stock newly picked has only its close history and
its universe row. So the adapter marks every ranked member the batch's last
session carries, at its universe row's close, and states the IBKR contract
it resolved for it (reqContractDetails), or marks none, saying why, when
the contract cannot be resolved to one US-dollar stock. Boost's
configuration leaves all of this off, and Boost's records are unchanged.

The IBKR doubles here are built from what ibapi 10.51.01's own decoder hands
EWrapper (testdata/ibapi_shapes.json, captured by capture_ibapi_shapes.py;
lessons L5): synthetic values in IBKR's real shapes.
"""
import json
import re
import sys
import tempfile
import unittest
from unittest import mock
from datetime import date, datetime, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from engine_double import LoggedEngine, run_approval
from fake_tws import ACCOUNT, FakeTWS, contract, contract_details
from test_account import connected
from test_connection import Clock, config
from test_delivery import _envelope, write_stream
from test_loop import Ids
from config import ConfigError
from fake_tws import order
from test_account import full_summary
from test_connection import Harness
from test_snapshot import WHY, state
from test_account_shape import captured_fractional
from test_sender import limit_request, paper_sender
from snapshot import broker_snapshot_record, reconciliation_record, close_valuation
from book_basis import Basis, _to_json, _from_json, holdings_after, cash_change, previous_close
from probe import snapshot_fault, reconciliation_fault
from orders import CommissionReport, OrderStatusReport, execution_report
from sender import SentRecord, UnsendableOrder, release_to_request
from reports import OrderReportJournal, UnreportableOrder
import relay
from delivery import StreamRefused, last_session, read_stream
from loop import AdapterLoop
from snapshot import (ACCOUNT_SNAPSHOT_TYPE, MARK_TYPE, account_snapshot_record, mark_record, read_account,
                      resolve_contracts)

TESTDATA = tests_dir / "testdata"
CLOSE = datetime(2027, 1, 14, 21, 0, tzinfo=timezone.utc)
FRIDAY = date(2027, 1, 14)
RUN = "engine"
# The legacy whole-share IRA configuration; the deployed D7-F example uses version 6.
IRA = dict(client_id=12, account_snapshot_version=4, resolve_contracts=True, con_ids={})
# Invented contract IDs for invented stocks.
CON_IDS = {"AAA": 900001, "BBB": 900002, "SPY": 756733, "IEF": 8314, "SHY": 8315}


def fixture(name):
    return json.loads((TESTDATA / name).read_text())


def ira_summary(settled="9000.00", total="10000.00"):
    """The account values as ibapi delivers each updateAccountValue: every value a string (ibapi_shapes.json)."""
    rows = [(ACCOUNT, "NetLiquidation", "10000.00", "USD"), (ACCOUNT, "TotalCashValue", total, "USD"),
            (ACCOUNT, "ExcessLiquidity", "10000.00", "USD")]
    if settled is not None:
        rows.append((ACCOUNT, "SettledCash", settled, "USD"))
    return rows


class CapturedShapeTests(unittest.TestCase):
    """The doubles carry what ibapi hands EWrapper: each attribute the adapter reads, of the type ibapi gives it."""

    # Both of ibapi's decoders: the protobuf messages a Gateway at server version 205 or later sends, and the older
    # text messages a lower version gets (the adapter logs which one its session negotiated).
    PATHS = ("protobuf", "text")

    def test_the_contract_details_double_has_ibapis_attributes_and_types(self):
        details = contract_details("AAA", 900001, primary_exchange="NASDAQ")
        for path in self.PATHS:
            captured = fixture("ibapi_shapes.json")[path]["contract_details"]
            for name, shape in captured["contract"].items():
                self.assertEqual(type(getattr(details.contract, name)).__name__, shape["type"], (path, name))
            for name, shape in captured["details"].items():
                self.assertEqual(type(getattr(details, name)).__name__, shape["type"], (path, name))
            self.assertEqual(captured["req_id"]["type"], "int")

    def test_an_account_value_is_a_string_as_ibapi_hands_it(self):
        for path in self.PATHS:
            captured = fixture("ibapi_shapes.json")[path]["account_value"]
            self.assertEqual({k: v["type"] for k, v in captured.items()},
                             {"key": "str", "val": "str", "currency": "str", "accountName": "str"}, path)
            self.assertEqual(captured["key"]["value"], "SettledCash")
        for row in ira_summary():
            self.assertTrue(all(isinstance(v, str) for v in row))


class ConfigurationTests(unittest.TestCase):
    def test_boosts_configuration_leaves_the_iras_inputs_off(self):
        c = config()
        self.assertEqual((c.account_snapshot_version, c.resolve_contracts), (3, False))

    def test_the_iras_configuration_names_version_4_and_contract_resolution(self):
        c = config(**IRA)
        self.assertEqual((c.client_id, c.account_snapshot_version, c.resolve_contracts), (12, 4, True))

    def test_another_snapshot_version_or_a_switch_that_is_not_a_bool_is_refused(self):
        for bad in ({"account_snapshot_version": 2}, {"account_snapshot_version": "4"},
                    {"account_snapshot_version": True}, {"resolve_contracts": 1}, {"resolve_contracts": "yes"}):
            with self.assertRaises(ConfigError, msg=bad):
                config(**bad)

    def test_the_shipped_examples_parse_and_differ_only_where_the_ira_needs(self):
        repo = tests_dir.parents[2]
        boost = json.loads((repo / "deploy/etc/adapters/boost.json.example").read_text())
        ira = json.loads((repo / "deploy/etc/adapters/trad-ira.json.example").read_text())
        for values in (boost, ira):
            config(**{k: v for k, v in values.items()})
        self.assertEqual((ira["client_id"], ira["mode"], ira["paper_sending_enabled"]), (12, "dry-run", False))
        self.assertEqual({k for k in set(boost) | set(ira) if boost.get(k) != ira.get(k)},
                         {"client_id", "account_snapshot_version", "resolve_contracts"})


class SnapshotRecordTests(unittest.TestCase):
    """account.snapshot version 4: version 3's positions and net liquidation, IBKR's settled and total cash."""

    def ira(self, summary):
        h = connected(**IRA)
        h.fake.summary = summary
        h.fake.positions = [(ACCOUNT, contract(con_id=8314, symbol="IEF"), Decimal("10"), 95.0)]
        return h

    def test_version_4_carries_ibkrs_settled_and_total_cash_from_the_same_reading(self):
        record = account_snapshot_record(read_account(self.ira(ira_summary()).conn), CLOSE, version=4)
        self.assertEqual((record.event_type, record.schema_version), (ACCOUNT_SNAPSHOT_TYPE, 4))
        self.assertEqual(record.payload, fixture("account_cash_snapshot.json"))

    def test_no_settled_cash_leaves_no_snapshot_never_a_zero(self):
        reading = read_account(self.ira(ira_summary(settled=None)).conn)
        self.assertIn("settled-cash", reading.unread)
        with self.assertRaises(ValueError):
            account_snapshot_record(reading, CLOSE, version=4)

    def test_no_total_cash_leaves_no_snapshot_either(self):
        summary = [r for r in ira_summary() if r[1] != "TotalCashValue"]
        with self.assertRaises(ValueError):
            account_snapshot_record(read_account(self.ira(summary).conn), CLOSE, version=4)

    def test_settled_cash_in_two_currencies_or_restated_is_unreadable(self):
        for extra in ((ACCOUNT, "SettledCash", "5.00", "EUR"), (ACCOUNT, "SettledCash", "8999.00", "USD")):
            reading = read_account(self.ira(ira_summary() + [extra]).conn)
            self.assertIn("settled-cash", reading.unread, extra)

    def test_settled_cash_that_is_not_a_number_is_unreadable(self):
        summary = [r for r in ira_summary() if r[1] != "SettledCash"] + [(ACCOUNT, "SettledCash", "n/a", "USD")]
        self.assertIn("settled-cash", read_account(self.ira(summary).conn).unread)

    def test_boosts_reading_is_unchanged_with_or_without_ibkrs_settled_cash(self):
        for summary in (ira_summary(), ira_summary(settled=None)):
            h = connected()
            h.fake.summary = summary
            reading = read_account(h.conn)
            self.assertNotIn("settled-cash", reading.unread)
            record = account_snapshot_record(reading, CLOSE)
            self.assertEqual(record.schema_version, 3)
            self.assertNotIn("settled_cash", record.payload)


class ContractTests(unittest.TestCase):
    """reqContractDetails resolves each name to one US-dollar stock on SMART, or says why it cannot."""

    def resolved(self, symbols, configured=None, **fake):
        h = connected(**IRA)
        for k, v in fake.items():
            setattr(h.fake, k, v)
        return h, resolve_contracts(h.conn, symbols, configured or {})

    def test_each_name_resolves_to_its_one_contract_with_its_exchange_and_currency(self):
        h, (contracts, refused) = self.resolved(["AAA", "BBB"], contract_details={
            "AAA": [contract_details("AAA", 900001, primary_exchange="NASDAQ")],
            "BBB": [contract_details("BBB", 900002, primary_exchange="NYSE")]})
        self.assertEqual(refused, {})
        self.assertEqual((contracts["AAA"].con_id, contracts["AAA"].exchange, contracts["AAA"].primary_exchange,
                          contracts["AAA"].currency), (900001, "SMART", "NASDAQ", "USD"))
        asked = h.fake.called("req_contract_details")
        self.assertEqual([(c[2].symbol, c[2].secType, c[2].currency, c[2].exchange) for c in asked],
                         [("AAA", "STK", "USD", "SMART"), ("BBB", "STK", "USD", "SMART")])

    def test_two_contracts_for_one_name_is_ambiguous_and_resolves_nothing(self):
        _, (contracts, refused) = self.resolved(["AAA"], contract_details={
            "AAA": [contract_details("AAA", 900001), contract_details("AAA", 900009, primary_exchange="ARCA")]})
        self.assertEqual(contracts, {})
        self.assertIn("2 contracts", refused["AAA"])

    def test_ibkrs_no_security_definition_resolves_nothing_and_says_so(self):
        _, (contracts, refused) = self.resolved(["ZZZ"], contract_detail_errors={
            "ZZZ": (200, "No security definition has been found for the request")})
        self.assertEqual(contracts, {})
        self.assertIn("200", refused["ZZZ"])

    def test_a_contract_that_is_not_a_us_dollar_stock_resolves_nothing(self):
        for details in (contract_details("AAA", 900001, currency="CAD"), contract_details("AAA", 900001, sec_type="ETF"),
                        contract_details("AAB", 900001), contract_details("AAA", 0)):
            _, (contracts, refused) = self.resolved(["AAA"], contract_details={"AAA": [details]})
            self.assertEqual(contracts, {}, details)
            self.assertIn("AAA", refused)

    def test_an_answer_that_never_ends_resolves_nothing(self):
        _, (contracts, refused) = self.resolved(["AAA"], end_contract_details=False, contract_details={
            "AAA": [contract_details("AAA", 900001)]})
        self.assertEqual(contracts, {})
        self.assertIn("did not end", refused["AAA"])

    def test_a_configured_contract_id_that_ibkr_contradicts_resolves_nothing(self):
        _, (contracts, refused) = self.resolved(["SPY"], configured={"SPY": 756733}, contract_details={
            "SPY": [contract_details("SPY", 756799, primary_exchange="ARCA")]})
        self.assertEqual(contracts, {})
        self.assertIn("756733", refused["SPY"])

    def test_the_link_lost_during_resolution_gives_none(self):
        h, result = self.resolved(["AAA"], drop_on_contract_details=True)
        self.assertIsNone(result)


class MarkRecordTests(unittest.TestCase):
    def test_a_mark_with_its_contract_is_version_3(self):
        h = connected(**IRA)
        h.fake.contract_details = {"AAA": [contract_details("AAA", 900001, primary_exchange="NASDAQ")]}
        contracts, _ = resolve_contracts(h.conn, ["AAA"], {})
        record = mark_record("AAA", CLOSE, 52.5, "marketdata", Decimal("52.41"), contract=contracts["AAA"])
        self.assertEqual((record.event_type, record.schema_version), (MARK_TYPE, 3))
        self.assertEqual(record.payload, fixture("mark_contract.json"))

    def test_a_mark_with_no_contract_stays_version_2_byte_for_byte(self):
        self.assertEqual(mark_record("SPY", CLOSE, 401.5, "marketdata", Decimal("401.17")).payload,
                         fixture("mark.json"))


def ira_session(day, sequence_start=1, held=()):
    """One compact IRA session as the data job writes it: SPY, IEF, SHY and holdings' bars, the universe, the
    ranked members' histories, the close. Invented prices only."""
    closed = "{}T21:00:00Z".format(day.isoformat())
    out = []
    bars = sorted(("SPY", "IEF", "SHY") + tuple(held))
    for symbol in bars:
        out.append(("market.bar.completed", 2, "marketdata:bar:{}:{}".format(symbol, day), closed,
                    {"instrument_id": symbol, "period_end": closed, "raw": {"close": 100.25}}))
    rows = [{"instrument_id": s, "type": "CS", "close": c, "volume": 1e6, "sector": "Tech",
             "sector_source": "ishares-iwv-gics"} for s, c in (("AAA", 52.5), ("BBB", 41.25), ("CCC", 3.0))]
    out.append(("market.universe.snapshot", 1, "marketdata:universe:{}".format(day), closed,
                {"session": day.isoformat(), "source": "massive", "rows": rows}))
    for symbol in ("AAA", "BBB"):
        out.append(("market.close.history", 1, "marketdata:history:{}:{}".format(symbol, day), closed,
                    {"instrument_id": symbol, "session": day.isoformat(), "adjusted_base": "2025-01-02",
                     "closes": [{"session": day.isoformat(), "close": 50.0}]}))
    out.append(("market.session.closed", 1, "marketdata:session-closed:{}".format(day), closed,
                {"period_end": closed, "instrument_ids": bars}))
    return [_envelope(*e, sequence=n) for n, e in enumerate(out, sequence_start)]


class RankedMarkTests(unittest.TestCase):
    """A ranked member with no bar is marked at its universe row's close (last_session)."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.folder = Path(self.dir.name)

    def stream(self, envelopes):
        path = write_stream(self.folder, FRIDAY, envelopes, prefix="trad-ira")
        from engine_double import CONFIGURATION_HASH, STRATEGY_VERSION
        return read_stream(str(path), RUN, CONFIGURATION_HASH, STRATEGY_VERSION)

    def test_each_ranked_member_is_marked_at_its_universe_close_beside_the_bars(self):
        close, closes = last_session([self.stream(ira_session(FRIDAY))])
        self.assertEqual(close, CLOSE)
        self.assertEqual(closes, {"SPY": 100.25, "IEF": 100.25, "SHY": 100.25, "AAA": 52.5, "BBB": 41.25})

    def test_a_history_with_no_universe_row_for_its_session_is_refused(self):
        envelopes = [e for e in ira_session(FRIDAY) if e["type"] != "market.universe.snapshot"]
        for n, e in enumerate(envelopes, 1):
            e["sequence"] = n
        with self.assertRaises(StreamRefused):
            last_session([self.stream(envelopes)])

    def test_a_held_member_with_a_bar_is_marked_at_its_bar(self):
        envelopes = ira_session(FRIDAY, held=("AAA",))
        close, closes = last_session([self.stream(envelopes)])
        self.assertEqual(closes["AAA"], 100.25)


class IraBatchHarness(unittest.TestCase):
    """One IRA batch delivered by the real loop to the engine stand-in, read the evening of its close."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        root = Path(self.dir.name)
        self.pending = str(root / "pending.json")
        self.engine_log = str(root / (RUN + ".log"))
        self.streams = root / "streams"
        self.folder = self.streams / RUN
        self.folder.mkdir(parents=True)
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(ACCOUNT, starting_book=({}, 10000.0)))
        self.fake = FakeTWS()
        # Read the evening of the close, on its own New York date.
        self.fake.server_time = int(datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc).timestamp())
        self.fake.summary = ira_summary(settled="10000.00")
        self.fake.positions = []
        self.fake.contract_details = {s: [contract_details(s, c)] for s, c in CON_IDS.items()}
        self.fake.marks = {900001: 52.41}

    def run_loop(self, held=(), **overrides):
        clock = Clock()

        def sleep(seconds):
            clock.now += seconds
            self.fake.waited()
        values = dict(IRA)
        values.update(overrides)
        loop = AdapterLoop(config(**values), self.fake, self.engine, self.pending, self.engine_log,
                           now=lambda: datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc), today=lambda: FRIDAY,
                           sleep=sleep, monotonic=clock, poll=0, new_id=Ids(), streams=str(self.streams))
        self.addCleanup(loop.close)
        write_stream(self.folder, FRIDAY, ira_session(FRIDAY, held=held), prefix="trad-ira")
        loop.start()
        for _ in range(40):
            loop.step()
        return self.engine.inputs()


class IraDeliveryTests(IraBatchHarness):
    """The IRA adapter's batch: the version 4 snapshot, and a mark with its contract for every name the engine can
    decide before the session closes."""

    def test_every_ranked_member_and_bar_has_a_mark_with_its_contract_before_the_close(self):
        sent = self.run_loop()
        kinds = [(e["type"], e["schema_version"]) for e in sent]
        self.assertIn((ACCOUNT_SNAPSHOT_TYPE, 4), kinds)
        marks = {e["payload"]["instrument_id"]: e for e in sent if e["type"] == MARK_TYPE}
        self.assertEqual(sorted(marks), ["AAA", "BBB", "IEF", "SHY", "SPY"])
        self.assertTrue(all(m["schema_version"] == 3 for m in marks.values()))
        self.assertEqual((marks["AAA"]["payload"]["price"], marks["AAA"]["payload"]["contract"]["con_id"],
                          marks["AAA"]["payload"]["broker_last"]), (52.5, 900001, 52.41))
        closed = [n for n, e in enumerate(sent) if e["type"] == "market.session.closed"][-1]
        self.assertTrue(all(sent.index(m) < closed for m in marks.values()), "every mark before the close")
        self.assertEqual(self.fake.placed, [], "a dry-run sends nothing")

    def test_a_name_whose_contract_cannot_be_resolved_gets_no_mark_and_the_rest_do(self):
        self.fake.contract_details.pop("BBB")
        self.fake.contract_detail_errors = {"BBB": (200, "No security definition has been found for the request")}
        with self.assertLogs("ibkr", "WARNING") as logged:
            sent = self.run_loop()
        marks = sorted(e["payload"]["instrument_id"] for e in sent if e["type"] == MARK_TYPE)
        self.assertEqual(marks, ["AAA", "IEF", "SHY", "SPY"])
        self.assertTrue(any("BBB" in line and "no mark" in line for line in logged.output), logged.output)

    def test_unresolved_names_are_reasked_on_each_later_account_reading_attempt(self):
        # ADR 0026, amendment of 2026-10-29: a later reading re-resolves
        # an unresolved pick before its weight is left in cash for a month.
        self.fake.contract_detail_errors = {"BBB": (200, "No security definition")}
        retries = []

        def incomplete_twice(conn):
            reading = read_account(conn)
            asks = [c for c in self.fake.called("req_contract_details") if c[2].symbol == "BBB"]
            if asks and len(retries) < 2:
                retries.append(len(asks))
                if len(retries) == 2:
                    self.fake.contract_detail_errors.clear()
                return reading._replace(unread=frozenset({"settled-cash"}))
            return reading

        with mock.patch("loop.read_account", side_effect=incomplete_twice):
            sent = self.run_loop()
        asks = [c[2].symbol for c in self.fake.called("req_contract_details")]
        self.assertEqual(asks.count("BBB"), 3)
        self.assertEqual(asks.count("AAA"), 1, "resolved names remain cached within the reading")
        self.assertIn("BBB", [e["payload"]["instrument_id"] for e in sent if e["type"] == MARK_TYPE])
        self.assertFalse(any(e["type"] == "market.mark.unavailable" for e in sent))

    def test_a_timed_out_name_resolves_on_the_next_reading_attempt(self):
        request = self.fake.req_contract_details
        asks = []
        retries = []

        def unended_twice(req_id, contract):
            asks.append(contract.symbol)
            self.fake.end_contract_details = contract.symbol != "BBB" or asks.count("BBB") > 2
            request(req_id, contract)
            self.fake.end_contract_details = True

        def incomplete_once(conn):
            reading = read_account(conn)
            if asks and not retries:
                retries.append(True)
                return reading._replace(unread=frozenset({"settled-cash"}))
            return reading

        with mock.patch.object(self.fake, "req_contract_details", side_effect=unended_twice), \
                mock.patch("loop.read_account", side_effect=incomplete_once):
            sent = self.run_loop()
        self.assertEqual(asks.count("BBB"), 3, "asked twice, then re-asked on the next reading")
        self.assertEqual(asks.count("AAA"), 1)
        self.assertIn("BBB", [e["payload"]["instrument_id"] for e in sent if e["type"] == MARK_TYPE])
        self.assertFalse(any(e["type"] == "market.mark.unavailable" for e in sent))

    def test_unresolved_name_records_final_adapter_detail_before_the_session_close(self):
        self.fake.contract_detail_errors = {"BBB": (200, "No security definition")}
        sent = self.run_loop()
        failures = [e for e in sent if e["type"] == "market.mark.unavailable"]
        self.assertEqual(len(failures), 1)
        failure = failures[0]
        self.assertEqual(failure["schema_version"], 1)
        self.assertEqual(failure["payload"], {
            "instrument_id": "BBB", "as_of": "2027-01-14T21:00:00Z", "source": "ibkr",
            "failure": "final", "detail": "IBKR answered its contract request with error 200: No security definition",
        })
        closed = [n for n, e in enumerate(sent) if e["type"] == "market.session.closed"][-1]
        self.assertLess(sent.index(failure), closed)

    def test_unended_lookup_records_transient_detail_and_bounded_retries(self):
        self.fake.end_contract_details = False
        sent = self.run_loop()
        failures = [e for e in sent if e["type"] == "market.mark.unavailable"]
        self.assertEqual(len(failures), 5)
        for failure in failures:
            self.assertEqual(failure["payload"]["failure"], "transient")
            self.assertIn("did not end within", failure["payload"]["detail"])
            self.assertIn("asked twice", failure["payload"]["detail"])
        self.assertEqual(len(self.fake.called("req_contract_details")), 10)

    def test_marks_are_asked_of_ibkr_in_batches_within_its_market_data_lines(self):
        self.run_loop()
        self.assertEqual(sorted(c[2].conId for c in self.fake.called("req_mkt_data")), sorted(CON_IDS.values()))



class IraFlexTests(unittest.TestCase):
    """ti-flex@trad-ira reads the IRA engine's log, whose account snapshots are version 4 (lessons L3)."""

    def test_the_flex_reader_takes_the_equity_before_a_transfer_from_a_version_4_snapshot(self):
        import io
        import flex
        from test_delivery import commit
        from test_flex import ACCOUNT as FLEX_ACCOUNT, QUERY, TOKEN, FakeFlex
        with tempfile.TemporaryDirectory() as d:
            log, ledger = Path(d) / "run-1.log", Path(d) / "cash-movements.jsonl"
            approval = run_approval(FLEX_ACCOUNT, starting_book=({}, 8000.0))
            approval["approved_at"] = "2027-01-12T14:00:00Z"
            engine = LoggedEngine(str(log), approval=approval)
            for ident, as_of, value in (("s1", "2027-01-13T21:00:00Z", 8530.0), ("s2", "2027-01-14T21:00:00Z", 11030.0)):
                commit(engine, ident, kind="account.snapshot", version=4, payload={
                    "account_id": FLEX_ACCOUNT, "as_of": as_of, "net_liquidation": value, "currency": "USD",
                    "positions": [], "settled_cash": value, "total_cash": value})
            out = io.StringIO()
            ok = flex.read_once(TOKEN, QUERY, FLEX_ACCOUNT, str(log), str(ledger), get=FakeFlex(),
                                sleep=lambda s: None, now=lambda: datetime(2027, 1, 16, 2, 30, tzinfo=timezone.utc),
                                err=out)
            self.assertTrue(ok, out.getvalue())
            equity = [json.loads(line)["payload"]["equity_before"] for line in ledger.read_text().splitlines()]
            self.assertEqual(equity, [8530.0, 11030.0])

    def test_a_snapshot_version_the_reader_does_not_know_is_still_refused(self):
        import flex
        self.assertEqual(sorted(flex.ACCOUNT_SNAPSHOT_VERSIONS), [3, 4, 6])


class DeliveredKindsTests(unittest.TestCase):
    """A fresh run's log holds no copied history; the IRA's adapter inputs count as history as Boost's do."""

    def test_the_iras_snapshot_and_contract_mark_are_delivered_kinds(self):
        from delivery import DELIVERED_KINDS
        self.assertIn(("account.snapshot", 4), DELIVERED_KINDS)
        self.assertIn(("market.mark.recorded", 3), DELIVERED_KINDS)
        self.assertIn(("market.mark.unavailable", 1), DELIVERED_KINDS)


REPO = tests_dir.parents[2]
TEMPLATE = REPO / "deploy" / "systemd" / "ti-adapter@.service"


def unit_lines(text):
    return [line for line in text.splitlines() if line and not line.startswith("#")]


class AdapterTemplateTests(unittest.TestCase):
    """ti-adapter@<instance>.service: one adapter per IRA, each with its own client ID, configuration, account file,
    engine socket, streams and Flex ledger (ADR 0026 point 8; lessons L6, L8); Boost's unit is left as it was."""

    def exec_start(self, instance, run="run-1"):
        import shlex
        from cli import _parser
        text = TEMPLATE.read_text()
        settings = (REPO / "deploy" / "etc" / "engines" / "{}.env".format(instance)).read_text()
        env = dict(line.split("=", 1) for line in settings.splitlines() if line and not line.startswith("#"))
        line = [l for l in unit_lines(text) if l.startswith("ExecStart=")]
        self.assertEqual(len(line), 1)
        words = shlex.split(line[0][len("ExecStart="):])
        expanded = []
        for w in words:
            w = w.replace("%i", instance).replace("%d", "/run/credentials/ti-adapter@{}.service".format(instance))
            w = w.replace("${TI_RUN}", run)
            for k, v in env.items():
                w = w.replace("${" + k + "}", v)
            self.assertNotIn("$", w, "a variable the unit's files do not set")
            expanded.append(w)
        self.assertEqual(expanded[:3], ["/opt/trend-investing/adapter-venv/bin/python3", "-m", "adapter.ibkr"])
        return _parser().parse_args(expanded[3:]), text

    def test_the_trad_ira_instance_runs_its_own_adapter_with_client_id_12(self):
        args, text = self.exec_start("trad-ira")
        self.assertEqual(args.config, "/etc/trend-investing/adapters/trad-ira.json")
        self.assertEqual((args.client_id, args.account_key), (12, "IBKR_ACCOUNT_TRAD_IRA"))
        self.assertEqual(args.secrets, "/run/credentials/ti-adapter@trad-ira.service/account")
        self.assertEqual(args.engine_log, "/var/lib/trend-investing/engines/trad-ira/run-1.log")
        self.assertEqual(args.socket, "/run/ti-engine-trad-ira/engine.sock")
        self.assertEqual(args.pending, "/var/lib/trend-investing/adapters/trad-ira/pending.json")
        self.assertEqual(args.streams, "/var/lib/trend-investing/data/streams/trad-ira")
        self.assertEqual(args.cash_ledger, "/var/lib/trend-investing/flex/trad-ira/cash-movements.jsonl")
        lines = unit_lines(text)
        for want in ("OnFailure=ti-unit-failed@%n.service", "Restart=no",
                     "LoadCredential=account:/run/trend-investing/secrets/accounts/%i.env",
                     "AssertPathExists=/run/trend-investing/secrets/accounts/%i.env",
                     "ConditionPathExists=/etc/trend-investing/adapters/%i.json",
                     "EnvironmentFile=/etc/trend-investing/engines/%i.env",
                     "EnvironmentFile=/var/lib/trend-investing/engines/%i/run.env",
                     "ReadWritePaths=/var/lib/trend-investing/adapters/%i"):
            self.assertIn(want, lines)
        self.assertEqual([l for l in lines if l.startswith("LoadCredential=")],
                         ["LoadCredential=account:/run/trend-investing/secrets/accounts/%i.env"],
                         "handed its own account file and nothing else")
        self.assertNotRegex(text, r"\bD?U[0-9]{5,}", "no account number in the unit")

    def test_the_data_job_writes_the_stream_folder_the_ira_adapter_reads(self):
        data = (REPO / "deploy" / "systemd" / "ti-data@.service").read_text()
        data_dir = re.search(r"^Environment=TI_DATA_DIR=(\S+)$", data, re.M).group(1)
        written = re.search(r"--streams (\S+)", data).group(1).replace("${TI_DATA_DIR}", data_dir)
        args, _ = self.exec_start("trad-ira")
        self.assertEqual(args.streams, written + "/trad-ira")

    def test_the_template_refuses_the_boost_instance(self):
        text = TEMPLATE.read_text()
        pre = [l for l in unit_lines(text) if l.startswith("ExecStartPre=")]
        self.assertEqual(len(pre), 1)
        self.assertIn('test "%i" != boost', pre[0])

    def test_client_ids_are_unique_and_the_iras_settings_name_its_account_line(self):
        ids = {}
        for instance in ("boost", "trad-ira", "roth-ira"):
            settings = (REPO / "deploy" / "etc" / "engines" / "{}.env".format(instance)).read_text()
            ids[instance] = re.search(r"^TI_ADAPTER_CLIENT_ID=(\d+)$", settings, re.M).group(1)
        self.assertEqual(ids, {"boost": "11", "trad-ira": "12", "roth-ira": "13"})
        ira = json.loads((REPO / "deploy/etc/adapters/trad-ira.json.example").read_text())
        self.assertEqual(str(ira["client_id"]), ids["trad-ira"])

    def test_boosts_unit_and_configuration_are_byte_identical(self):
        import hashlib
        pins = {"deploy/systemd/ti-adapter-boost.service":
                "5e59b37a552307d21c128d1afc7f823f248a07b71c4386e764761b83306e855a",
                "deploy/etc/adapters/boost.json.example":
                "934f66330c46920a018ea6828dc6cbab16daca091c287ab6d74459e636ea3253"}
        for path, digest in pins.items():
            self.assertEqual(hashlib.sha256((REPO / path).read_bytes()).hexdigest(), digest, path)

    def test_provisioning_creates_the_iras_configuration_once_and_never_enables_the_adapter(self):
        provision = (REPO / "deploy" / "provision.sh").read_text()
        # Every account's adapter configuration is created once from its example.
        self.assertIn('for f in "$src"/*.json.example; do', provision)
        self.assertIn("systemctl disable ti-adapter-boost.service ti-adapter@trad-ira.service", provision)

    def test_the_trad_ira_flex_reader_fires_at_its_own_staggered_times(self):
        dropin = REPO / "deploy" / "systemd" / "ti-flex@trad-ira.timer.d" / "stagger.conf"
        lines = unit_lines(dropin.read_text())
        self.assertEqual(lines, ["[Timer]", "OnCalendar=", "OnCalendar=Mon..Fri 05:45 America/New_York",
                                 "OnCalendar=Mon..Fri 06:45 America/New_York",
                                 "OnCalendar=Mon..Fri 07:45 America/New_York"])
        provision = (REPO / "deploy" / "provision.sh").read_text()
        self.assertIn("for instance in boost trad-ira roth-ira; do", provision)
        self.assertIn('"$unit_dir/ti-flex@$instance.timer.d"', provision)


class ClientIdTests(unittest.TestCase):
    """--client-id: the instance's settings and its configuration name the same client ID, or nothing starts."""

    def test_rehearsal_drains_journal_after_matching_client_ids(self):
        import subprocess
        text = (REPO / "deploy/rehearsal/first-day.sh").read_text()
        assertion = next(line for line in text.splitlines()
                         if line.startswith("journalctl ") and "two client IDs" in line)
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / "journal.txt"
            for matched in (True, False):
                with self.subTest(matched=matched):
                    message = ("refused: names client_id 11, but the instance's settings give 12\n"
                               if matched else "refused: another reason\n")
                    journal.write_text(message + "later journal entry\n" * 100000)
                    command = 'journalctl() { cat "$1"; }; fail() { exit 1; }; ' + assertion.replace(
                        "journalctl -u ti-adapter@trad-ira.service --no-pager -o cat", 'journalctl "$1"')
                    result = subprocess.run(["bash", "-euo", "pipefail", "-c", command, "test", str(journal)],
                                            capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, 0 if matched else 1, result.stderr)

    def test_a_configuration_whose_client_id_differs_from_the_settings_is_refused(self):
        import io
        from cli import main
        with tempfile.TemporaryDirectory() as d:
            values = json.loads((REPO / "deploy/etc/adapters/trad-ira.json.example").read_text())
            values["account_id"] = ACCOUNT
            path = Path(d) / "trad-ira.json"
            path.write_text(json.dumps(values))
            err = io.StringIO()
            status = main(["run", "--config", str(path), "--client-id", "11", "--pending", str(Path(d) / "p.json"),
                           "--engine-log", str(Path(d) / "run-1.log"), "--socket", str(Path(d) / "s"),
                           "--ibapi-record", str(Path(d) / "INSTALLED"), "--versions", str(Path(d) / "v.env")],
                          api_factory=lambda cfg: self.fail("connected"), err=err)
            self.assertEqual(status, 1)
            self.assertIn("client_id 12", err.getvalue())
            self.assertIn("11", err.getvalue())


class ProbeTests(unittest.TestCase):
    """The same-day probe (lessons L10) parses what the adapter really logged and the engine really journalled for
    one start: here, the adapter's own command run against the scripted Gateway and the engine stand-in."""

    def setUp(self):
        from unittest import mock
        import ibapi_pin
        from test_ibapi_pin import PINS, SHA, record
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.dir, True))
        values = json.loads((REPO / "deploy/etc/adapters/trad-ira.json.example").read_text())
        # These regression scenarios retain the legacy whole-share IRA contract.
        values.update(account_snapshot_version=4, connect_timeout=0.05, request_timeout=0.05, mark_timeout=0.05)
        self.config = self.dir / "trad-ira.json"
        self.config.write_text(json.dumps(values))
        (self.dir / "account.env").write_text("IBKR_ACCOUNT_TRAD_IRA={}\n".format(ACCOUNT))
        (self.dir / "versions.env").write_text("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA) + PINS)
        (self.dir / "INSTALLED").write_text(record())
        machine = mock.patch.object(ibapi_pin.platform, "machine", return_value="x86_64")
        machine.start()
        self.addCleanup(machine.stop)
        self.engine_log = self.dir / (RUN + ".log")
        self.engine = LoggedEngine(str(self.engine_log), approval=run_approval(ACCOUNT, starting_book=({}, 10000.0)))
        self.fake = FakeTWS()
        self.fake.server_time = int(datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc).timestamp())
        self.fake.summary = ira_summary(settled="10000.00")
        self.fake.contract_details = {s: [contract_details(s, c)] for s, c in CON_IDS.items()}
        write_stream(self.dir / "streams" / RUN, FRIDAY, ira_session(FRIDAY), prefix="trad-ira")

    def start(self, client_id="12", steps=30):
        """The adapter's journal for one start: what its command writes to standard error, as journald keeps it."""
        import io
        from cli import main
        from test_cli import Steps
        err = io.StringIO()
        main(["run", "--config", str(self.config), "--client-id", client_id, "--secrets", str(self.dir / "account.env"),
              "--account-key", "IBKR_ACCOUNT_TRAD_IRA", "--pending", str(self.dir / "pending.json"),
              "--engine-log", str(self.engine_log), "--socket", str(self.dir / "s"), "--streams",
              str(self.dir / "streams"), "--ibapi-record", str(self.dir / "INSTALLED"),
              "--versions", str(self.dir / "versions.env")],
             api_factory=lambda cfg: self.fake, engine_connect=lambda path, timeout: self.engine,
             should_stop=Steps(steps), err=err, poll=0.01, ibapi_version=lambda: (10, 51, 1),
             protobuf_version=lambda: "5.29.5", today=lambda: FRIDAY,
             now=lambda: datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc))
        journal = self.dir / "adapter.txt"
        journal.write_text(err.getvalue())
        return journal

    def probe(self, journal, *extra):
        import io
        from cli import main
        out = io.StringIO()
        status = main(["probe", "--journal", str(journal), "--engine-log", str(self.engine_log), "--client-id", "12",
                       *extra], out=out)
        return status, out.getvalue().splitlines()

    def test_a_good_start_passes_naming_each_check(self):
        status, lines = self.probe(self.start())
        self.assertEqual(status, 0, lines)
        self.assertEqual(lines[-1], "probe: passed")
        for want in ("probe: ok the adapter started: mode=dry-run client_id=12 sending=off",
                     "probe: ok connected to IB Gateway as client_id=12",
                     "probe: ok reconciled client_id=12", "probe: ok queued run=" + RUN,
                     "probe: ok account snapshot version 4", "probe: ok reconciliation journalled",
                     "probe: ok 5 mark(s) at version 3", "probe: ok no order released to be sent and none refused for a missing mark",
                     "probe: no decision yet"):
            self.assertTrue(any(line.startswith(want) for line in lines), (want, lines))
        self.assertNotIn(ACCOUNT, "\n".join(lines))

    def answered(self, journal, ident, kind):
        """journal with a line saying this start delivered input ident: the probe's evidence is this start's only."""
        line = "x ibkr input={} type={} answered with 1 decisions".format(ident, kind)
        lines = journal.read_text().splitlines()
        # Inside the batch: before the adapter's outcome line for it, so it is one of the batch's records.
        at = next((i for i, l in enumerate(lines) if "ibkr stream batch files=" in l), len(lines))
        journal.write_text("\n".join(lines[:at] + [line] + lines[at:]) + "\n")
        return journal

    def test_a_missing_or_empty_journal_fails_loudly(self):
        status, lines = self.probe(self.dir / "no-such-file.txt")
        self.assertEqual(status, 1)
        self.assertTrue(any("cannot read the adapter's journal" in line for line in lines), lines)
        empty = self.dir / "empty.txt"
        empty.write_text("")
        status, lines = self.probe(empty)
        self.assertEqual((status, lines[-1][:15]), (1, "probe: FAILED, "))

    def test_another_client_id_fails(self):
        status, lines = self.probe(self.start(), "--client-id", "11")
        self.assertEqual(status, 1)
        self.assertTrue(any("client_id=12" in line and line.startswith("probe: FAIL") for line in lines), lines)

    def test_a_refused_start_fails_with_its_reason(self):
        status, lines = self.probe(self.start(client_id="13"))
        self.assertEqual(status, 1)
        self.assertTrue(any("refused to start" in line and "client_id 12" in line for line in lines), lines)

    def test_a_snapshot_without_its_settled_cash_fails(self):
        from test_delivery import commit
        journal = self.start()
        commit(self.engine, "bad-snapshot", kind="account.snapshot", version=4, payload={
            "account_id": ACCOUNT, "as_of": "2027-01-15T21:00:00Z", "net_liquidation": 10000.0, "currency": "USD",
            "positions": [], "total_cash": 10000.0})
        status, lines = self.probe(self.answered(journal, "bad-snapshot", "account.snapshot"))
        self.assertEqual(status, 1)
        self.assertTrue(any("bad-snapshot" in line and "settled_cash" in line for line in lines), lines)

    def test_a_version_3_snapshot_for_an_ira_fails(self):
        from test_delivery import commit
        journal = self.start()
        commit(self.engine, "boost-shaped", kind="account.snapshot", version=3, payload={
            "account_id": ACCOUNT, "as_of": "2027-01-15T21:00:00Z", "net_liquidation": 10000.0, "currency": "USD",
            "positions": []})
        status, lines = self.probe(self.answered(journal, "boost-shaped", "account.snapshot"))
        self.assertEqual(status, 1)
        self.assertTrue(any("not at version 4" in line for line in lines), lines)

    def test_a_missing_mark_refusal_fails(self):
        from test_delivery import commit
        journal = self.start()
        self.engine.decisions["refusing"] = [{"type": "risk.order.refused", "schema_version": 1, "payload": {
            "reason": "missing-mark", "instrument_id": "BBB"}}]
        commit(self.engine, "refusing")
        status, lines = self.probe(self.answered(journal, "refusing", "test.input"))
        self.assertEqual(status, 1)
        self.assertTrue(any("missing mark: BBB" in line for line in lines), lines)

    def test_an_engine_log_that_cannot_be_read_fails(self):
        journal = self.start()
        self.engine_log.write_text("not a log\n")
        status, lines = self.probe(journal)
        self.assertEqual(status, 1)


class SettlesBetweenReads(FakeTWS):
    """IBKR's settled cash moves between the broker snapshot's two account reads (a sale settling), once."""

    def __init__(self):
        super().__init__()
        self.downloads = 0

    def req_account_updates(self, subscribe, account_id):
        if subscribe:
            self.downloads += 1
            settled = "9500.00" if self.downloads == 3 else "10000.00"
            self.summary = [r for r in self.summary if r[1] != "SettledCash"] + [
                (ACCOUNT, "SettledCash", settled, "USD")]
        super().req_account_updates(subscribe, account_id)


class SettledCashCutoffTests(IraBatchHarness):
    """The two reads of a broker snapshot must agree on the settled cash too, as on the holdings and total cash."""

    def test_settled_cash_that_moves_between_the_reads_is_read_again(self):
        fake = SettlesBetweenReads()
        fake.server_time, fake.summary = self.fake.server_time, self.fake.summary
        fake.contract_details = self.fake.contract_details
        self.fake = fake
        with self.assertLogs("ibkr", "INFO") as logged:
            sent = self.run_loop()
        self.assertTrue(any("changed between its reads" in line for line in logged.output), logged.output)
        snapshots = [e for e in sent if e["type"] == ACCOUNT_SNAPSHOT_TYPE]
        self.assertEqual([s["payload"]["settled_cash"] for s in snapshots], [10000.0])


class PacedHarness(unittest.TestCase):
    """The scripted Gateway with every outbound message's time on the connection's own clock."""

    def connected(self, **fake):
        h = connected(**IRA)
        self.times = []
        for name in ("req_contract_details", "req_mkt_data", "cancel_mkt_data"):
            original = getattr(h.fake, name)

            def timed(*args, _original=original):
                self.times.append(h.clock.now)
                return _original(*args)
            setattr(h.fake, name, timed)
        for k, v in fake.items():
            setattr(h.fake, k, v)
        return h

    def assert_paced(self):
        """No more than the connection's PACE_LIMIT messages in any one-second window (IBKR's limit is 50)."""
        from connection import PACE_LIMIT
        self.assertLess(PACE_LIMIT, 50)
        for i, start in enumerate(self.times):
            in_window = [t for t in self.times[i:] if t < start + 1.0]
            self.assertLessEqual(len(in_window), PACE_LIMIT, "messages at {}".format(in_window))


class PacingTests(PacedHarness):
    """IBKR accepts at most 50 API messages a second; the adapter's bulk requests, their cancels included, share one
    pacer so no burst passes it, however fast IBKR answers or refuses."""

    def test_marks_refused_at_once_for_500_names_never_exceed_the_pace(self):
        from snapshot import read_marks
        names = {"N{:03d}".format(i): 700000 + i for i in range(500)}
        h = self.connected(mark_errors={c: (354, "synthetic: not subscribed") for c in names.values()})
        self.assertEqual(read_marks(h.conn, names), {})
        self.assertEqual(len(self.times), 500)
        self.assert_paced()

    def test_marks_whose_snapshots_never_end_are_cancelled_within_the_pace(self):
        from snapshot import read_marks
        names = {"N{:03d}".format(i): 700000 + i for i in range(120)}
        h = self.connected(end_marks=False)
        read_marks(h.conn, names)
        self.assertEqual(len(self.times), 240, "each asked and each cancelled")
        self.assert_paced()

    def test_500_contract_lookups_answered_at_once_never_exceed_the_pace(self):
        names = ["N{:03d}".format(i) for i in range(500)]
        h = self.connected(contract_details={n: [contract_details(n, 700000 + i)] for i, n in enumerate(names)})
        contracts, refused = resolve_contracts(h.conn, names, {})
        self.assertEqual((len(contracts), refused), (500, {}))
        self.assert_paced()


class ContractRetryAndSpellingTests(unittest.TestCase):
    def resolved(self, symbols, **fake):
        h = connected(**IRA)
        for k, v in fake.items():
            setattr(h.fake, k, v)
        return h, resolve_contracts(h.conn, symbols, {})

    def test_an_answer_that_did_not_end_is_asked_once_more_and_then_resolves(self):
        h, (contracts, refused) = self.resolved(["AAA"], contract_details={"AAA": [contract_details("AAA", 900001)]},
                                                unended_contract_details_once={"AAA"})
        self.assertEqual((sorted(contracts), refused), (["AAA"], {}))
        self.assertEqual(len(h.fake.called("req_contract_details")), 2)

    def test_an_answer_that_never_ends_is_asked_twice_and_then_has_no_mark(self):
        h, (contracts, refused) = self.resolved(["AAA"], end_contract_details=False,
                                                contract_details={"AAA": [contract_details("AAA", 900001)]})
        self.assertEqual(contracts, {})
        self.assertIn("did not end", refused["AAA"])
        self.assertIn("twice", refused["AAA"])
        self.assertEqual(len(h.fake.called("req_contract_details")), 2)

    def test_a_definitive_refusal_is_not_asked_again(self):
        h, (contracts, refused) = self.resolved(["ZZZ"], contract_detail_errors={
            "ZZZ": (200, "No security definition has been found for the request")})
        self.assertIn("200", refused["ZZZ"])
        self.assertEqual(len(h.fake.called("req_contract_details")), 1)

    def test_a_class_share_ticker_is_asked_in_ibkrs_spelling_and_resolves_to_the_universe_name(self):
        h, (contracts, refused) = self.resolved(["BRK.B"], contract_details={
            "BRK B": [contract_details("BRK B", 900077, primary_exchange="NYSE")]})
        self.assertEqual(refused, {})
        self.assertEqual((contracts["BRK.B"].symbol, contracts["BRK.B"].con_id), ("BRK.B", 900077))
        self.assertEqual([c[2].symbol for c in h.fake.called("req_contract_details")], ["BRK B"])
        record = mark_record("BRK.B", CLOSE, 401.5, "marketdata", contract=contracts["BRK.B"])
        self.assertEqual((record.payload["instrument_id"], record.payload["contract"]["con_id"]), ("BRK.B", 900077))

    def test_an_answer_for_another_name_than_the_spelling_asked_is_refused(self):
        wrong = contract_details("BRK A", 900078, primary_exchange="NYSE")
        _, (contracts, refused) = self.resolved(["BRK.B"], contract_details={"BRK B": [wrong]})
        self.assertEqual(contracts, {})
        self.assertIn("BRK A", refused["BRK.B"])

    def test_a_local_symbol_that_is_not_the_name_is_refused(self):
        odd = contract_details("BRK B", 900079, primary_exchange="NYSE")
        odd.contract.localSymbol = "BRKB.OLD"
        _, (contracts, refused) = self.resolved(["BRK.B"], contract_details={"BRK B": [odd]})
        self.assertEqual(contracts, {})
        self.assertIn("BRKB.OLD", refused["BRK.B"])



class ProbeEvidenceTests(ProbeTests):
    """The probe's evidence is this start's: the inputs it delivered, never an earlier start's or another run's."""

    test_a_good_start_passes_naming_each_check = None

    def restart(self):
        """A second start of the adapter after the first delivered its batch: caught up, it queues nothing."""
        self.start()
        return self.start()

    def test_a_restart_with_no_new_file_passes_saying_so(self):
        status, lines = self.probe(self.restart())
        self.assertEqual(status, 0, lines)
        self.assertTrue(any(line.startswith("probe: ok no new batch since start; caught up") for line in lines), lines)

    def test_a_restart_with_no_new_file_fails_when_a_batch_is_expected(self):
        status, lines = self.probe(self.restart(), "--expect-batch")
        self.assertEqual(status, 1, lines)
        self.assertTrue(any(line.startswith("probe: FAIL no stream batch was queued") for line in lines), lines)

    def test_an_earlier_starts_records_never_pass_for_this_one(self):
        first = self.start()
        journal = self.dir / "second.txt"
        # This start's journal claims a batch, but none of its inputs reached the engine.
        lines = [line for line in first.read_text().splitlines() if " input=" not in line]
        journal.write_text("\n".join(lines) + "\n")
        status, out = self.probe(journal)
        self.assertEqual(status, 1, out)
        self.assertTrue(any("this start" in line and line.startswith("probe: FAIL") for line in out), out)

    def test_probe_prints_the_last_staleness_or_cross_check_verdict_as_an_observation(self):
        journal = self.start()
        self.assertIn("ibkr gateway clock recheck", journal.read_text())
        for observation in (
                "staleness at=2027-01-14T23:06:00Z verdict=unchecked reason=stale",
                "cross-check gateway_at=2027-01-14T22:06:00Z received_at=2027-01-14T23:06:00Z "
                "verdict=unchecked reason=Gateway clock is behind"):
            with self.subTest(observation=observation):
                with journal.open("a") as out:
                    out.write("ibkr gateway clock " + observation + "\n")
                status, lines = self.probe(journal)
                self.assertEqual(status, 0, lines)
                checks = [line for line in lines if line.startswith("probe: Gateway clock")]
                self.assertEqual(checks, ["probe: Gateway clock " + observation])

    def test_a_release_marked_to_be_sent_fails_a_dry_run_probe(self):
        from test_delivery import commit
        self.engine.decisions["sending"] = [{"type": "risk.order.released", "schema_version": 1, "payload": {
            "send": True, "instrument_id": "AAA", "side": "buy"}}]
        journal = self.start()
        commit(self.engine, "sending")
        text = journal.read_text() + "x ibkr input=sending type=test.input answered with 1 decisions\n"
        journal.write_text(text)
        status, lines = self.probe(journal)
        self.assertEqual(status, 1, lines)
        self.assertTrue(any("marked to be sent in a dry-run" in line for line in lines), lines)
        self.assertFalse(any("no order sent" in line for line in lines), lines)

    def tampered(self, kind, mutate):
        """This start's journal, after the delivered input of kind is rewritten in the engine's log by mutate."""
        journal = self.start()
        text = self.engine_log.read_text()
        out = []
        for line in text.splitlines():
            if '"type":"{}"'.format(kind) in line and '"record"' in line:
                record = json.loads(line)
                mutate(record["record"]["envelope"]["payload"])
                line = json.dumps(record, separators=(",", ":"))
            out.append(line)
        self.engine_log.write_text("\n".join(out) + "\n")
        return journal

    def probe_unchained(self, journal):
        """The probe on a log whose payloads were rewritten: its validation, not the hash chain, must name the field."""
        from unittest import mock
        with mock.patch("probe._committed_groups", side_effect=lambda path: _read_groups(path)):
            return self.probe(journal)

    def test_a_negative_net_liquidation_fails_naming_the_field(self):
        journal = self.tampered("account.snapshot", lambda p: p.update(net_liquidation=-5.0))
        status, lines = self.probe_unchained(journal)
        self.assertEqual(status, 1, lines)
        self.assertTrue(any("net_liquidation" in line and line.startswith("probe: FAIL") for line in lines), lines)

    def test_a_contract_with_only_a_boolean_id_fails_naming_the_field(self):
        journal = self.tampered("market.mark.recorded", lambda p: p.update(contract={"con_id": True}))
        status, lines = self.probe_unchained(journal)
        self.assertEqual(status, 1, lines)
        self.assertTrue(any("contract.con_id" in line and line.startswith("probe: FAIL") for line in lines), lines)

    def test_a_reconciliation_without_its_cash_fails_naming_the_field(self):
        journal = self.tampered("account.reconciliation", lambda p: p.pop("total_cash"))
        status, lines = self.probe_unchained(journal)
        self.assertEqual(status, 1, lines)
        self.assertTrue(any("total_cash" in line and line.startswith("probe: FAIL") for line in lines), lines)

    def test_the_gateways_negotiated_server_version_is_shown(self):
        status, lines = self.probe(self.start())
        self.assertTrue(any(line.startswith("probe: Gateway server version") for line in lines), lines)


def _read_groups(path):
    """The engine log's groups as engine_log reads them, without its record hash chain (for tampered payloads)."""
    groups, current = [], []
    with open(path, encoding="utf-8") as f:
        f.readline()
        for n, line in enumerate(f, 2):
            row = json.loads(line)
            if "record" in row:
                current.append((n, row["record"]["kind"], row["record"]["envelope"]))
            elif "commit" in row and current:
                groups.append(current)
                current = []
    return None, groups



class ClassShareHoldingTests(IraBatchHarness):
    """IBKR spells a held class share "BRK B"; the adapter names it as the data job and the engine do, BRK.B, at the
    boundary where it reads positions, open orders and executions, so the book, the closes and the marks all join."""

    def test_a_held_class_share_reconciles_against_the_book_and_gets_its_close_and_mark(self):
        from pathlib import Path as P
        P(self.engine_log).unlink()
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(ACCOUNT, starting_book=({"BRK.B": 10}, 10000.0)))
        self.fake.positions = [(ACCOUNT, contract(con_id=900077, symbol="BRK B"), Decimal("10"), 300.0)]
        self.fake.summary = [(ACCOUNT, "NetLiquidation", "11002.50", "USD"), (ACCOUNT, "TotalCashValue", "10000.00", "USD"),
                             (ACCOUNT, "ExcessLiquidity", "10000.00", "USD"), (ACCOUNT, "SettledCash", "10000.00", "USD")]
        self.fake.contract_details["BRK B"] = [contract_details("BRK B", 900077, primary_exchange="NYSE")]
        sent = self.run_loop(held=("BRK.B",))
        account = [e for e in sent if e["type"] == ACCOUNT_SNAPSHOT_TYPE][0]["payload"]
        self.assertEqual(account["positions"], [{"instrument_id": "BRK.B", "quantity": 10}])
        self.assertEqual(account["as_of"], "2027-01-14T21:00:00Z", "the reading stands for the close: valued at BRK.B's")
        broker = [e for e in sent if e["type"] == "account.broker.snapshot"][0]["payload"]
        self.assertEqual([(p["instrument_id"], p["mark_set"]) for p in broker["positions"]], [("BRK.B", True)])
        reconciliation = [e for e in sent if e["type"] == "account.reconciliation"][0]["payload"]
        self.assertEqual(reconciliation["positions"], [{"instrument_id": "BRK.B", "quantity": 10}])
        mark = [e for e in sent if e["type"] == MARK_TYPE and e["payload"]["instrument_id"] == "BRK.B"][0]
        self.assertEqual(mark["payload"]["contract"]["con_id"], 900077)

    def test_ibkrs_spelling_is_turned_into_the_data_jobs_and_back(self):
        from spelling import engine_symbol, ibkr_symbol
        self.assertEqual((engine_symbol("BRK B"), ibkr_symbol("BRK.B")), ("BRK.B", "BRK B"))
        self.assertEqual((engine_symbol("SPY"), ibkr_symbol("SPY")), ("SPY", "SPY"))
        self.assertEqual(engine_symbol(ibkr_symbol("BF.B")), "BF.B")

    def test_an_order_names_ibkrs_spelling_beside_its_contract_id(self):
        from sender import request_shape, OrderRequest
        req = OrderRequest(client_order_id="tw-1", account_id=ACCOUNT, con_id=900077, symbol="BRK.B", action="BUY",
                           quantity=Decimal(1), order_type="LMT", limit_price=Decimal("300"), tif="DAY")
        contract_shape, _ = request_shape(req)
        self.assertEqual((contract_shape.symbol, contract_shape.conId), ("BRK B", 900077))


class MarkValidationTests(unittest.TestCase):
    """The probe judges a mark by the engine's own rules (internal/event MarkRecordedPayload.ValidateSchema)."""

    CLOSE = "2027-01-14T21:00:00Z"

    def good(self):
        return {"instrument_id": "AAA", "as_of": self.CLOSE, "price": 52.5, "source": "marketdata",
                "broker_last": 52.41, "broker_last_set": True,
                "contract": {"con_id": 900001, "exchange": "SMART", "primary_exchange": "NASDAQ", "currency": "USD"}}

    def fault(self, payload, version=3):
        from probe import mark_fault
        return mark_fault(payload, version, self.CLOSE)

    def test_a_good_mark_passes(self):
        self.assertEqual(self.fault(self.good()), "")
        unset = dict(self.good(), broker_last=0, broker_last_set=False)
        self.assertEqual(self.fault(unset), "")

    def test_each_field_the_engine_refuses_is_named(self):
        cases = {
            "as_of": [dict(self.good(), as_of=""), dict(self.good(), as_of="2027-01-14T20:00:00Z"),
                      dict(self.good(), as_of="yesterday")],
            "source": [dict(self.good(), source=""), {k: v for k, v in self.good().items() if k != "source"}],
            "broker_last": [dict(self.good(), broker_last=-1.0), dict(self.good(), broker_last=0, broker_last_set=True),
                            dict(self.good(), broker_last=5.0, broker_last_set=False),
                            dict(self.good(), broker_last=float("nan"))],
            "broker_last_set": [dict(self.good(), broker_last_set="yes")],
            "contract.con_id": [dict(self.good(), contract=dict(self.good()["contract"], con_id=2 ** 63))],
        }
        for field, payloads in cases.items():
            for payload in payloads:
                self.assertEqual(self.fault(payload), field, payload)

    def test_a_version_2_mark_carries_no_contract_and_version_1_no_broker_price(self):
        self.assertEqual(self.fault(self.good(), version=2), "contract")
        v1 = {k: v for k, v in self.good().items() if k not in ("contract", "broker_last", "broker_last_set")}
        self.assertEqual(self.fault(v1, version=1), "")
        self.assertEqual(self.fault(dict(v1, broker_last=1.0, broker_last_set=True), version=1), "broker_last")


class ProbeCatchUpTests(ProbeTests):
    """No batch passes only on the adapter's own word that the stream is caught up; a start still waiting, or one with
    no word either way, fails; and every batch a start queued must reach the engine."""

    test_a_good_start_passes_naming_each_check = None

    def journal_of(self, lines):
        path = self.dir / "made.txt"
        path.write_text("\n".join(lines) + "\n")
        return path

    def head(self):
        return [line for line in self.start().read_text().splitlines()
                if "adapter starting" in line or "link state=connected" in line or "reconciled" in line]

    def test_a_caught_up_restart_passes_on_the_adapters_caught_up_line(self):
        self.start()
        second = self.start()
        self.assertIn("ibkr stream no new file since", second.read_text())
        status, lines = self.probe(second)
        self.assertEqual(status, 0, lines)
        self.assertTrue(any(line.startswith("probe: ok no new batch since start; caught up") for line in lines), lines)

    def test_a_start_still_waiting_for_its_reading_fails_saying_why(self):
        lines = self.head() + ["x ibkr broker snapshot waits for the Flex reader's executions proof until 09:00 New "
                               "York: no executions proof is written"]
        status, out = self.probe(self.journal_of(lines))
        self.assertEqual(status, 1, out)
        self.assertTrue(any(line.startswith("probe: FAIL") and "waits for the Flex reader" in line for line in out), out)

    def test_a_start_with_no_batch_and_no_word_either_way_fails(self):
        status, out = self.probe(self.journal_of(self.head()))
        self.assertEqual(status, 1, out)
        self.assertTrue(any(line.startswith("probe: FAIL") and "caught up" in line for line in out), out)

    def test_an_earlier_delivered_batch_never_covers_a_later_undelivered_one(self):
        journal = self.start().read_text().splitlines()
        later = "x ibkr stream queued run={} files=trad-ira-2027-01-15.jsonl inputs=9".format(RUN)
        status, out = self.probe(self.journal_of(journal[:-1] + [later, journal[-1]]))
        self.assertEqual(status, 1, out)
        self.assertTrue(any(line.startswith("probe: FAIL") and "trad-ira-2027-01-15.jsonl" in line for line in out), out)



OUTCOME = "ibkr stream batch files="


class mock_stderr:
    """Swallow what logging's handleError prints for a log call it cannot format (the test's own)."""

    def __enter__(self):
        import io
        import sys as _sys
        self._saved, _sys.stderr = _sys.stderr, io.StringIO()

    def __exit__(self, *exc):
        import sys as _sys
        _sys.stderr = self._saved


class BatchOutcomeTests(IraBatchHarness):
    """The adapter states one outcome per batch from its own accounting (delivery.Deliverer): every entry it queued,
    answered by the engine or struck off as already committed, and nothing else counted."""

    def loop_with(self, now=datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc)):
        clock = Clock()

        def sleep(seconds):
            clock.now += seconds
            self.fake.waited()
        loop = AdapterLoop(config(**IRA), self.fake, self.engine, self.pending, self.engine_log, now=lambda: now,
                           today=lambda: FRIDAY, sleep=sleep, monotonic=clock, poll=0, new_id=Ids(),
                           streams=str(self.streams))
        self.addCleanup(loop.close)
        return loop

    def steps(self, loop, n=40):
        for _ in range(n):
            loop.step()

    def outcomes(self, logged):
        return [line.split("ibkr ", 1)[1] for line in logged.output if OUTCOME in line]

    def test_a_delivered_batch_states_delivered_with_its_count(self):
        write_stream(self.folder, FRIDAY, ira_session(FRIDAY), prefix="trad-ira")
        loop = self.loop_with()
        with self.assertLogs("ibkr", "INFO") as logged:
            loop.start()
            self.steps(loop)
        out = self.outcomes(logged)
        self.assertEqual(len(out), 1, out)
        self.assertRegex(out[0], r"files=trad-ira-2027-01-14\.jsonl inputs=(\d+) answered=\1 struck=0 outcome=delivered")

    def test_a_later_file_whose_reading_waits_states_waiting_with_the_reason(self):
        write_stream(self.folder, FRIDAY, ira_session(FRIDAY), prefix="trad-ira")
        loop = self.loop_with()
        loop.start()
        self.steps(loop)
        # The next morning's file, read on a later date before 09:00 with no executions proof written yet.
        later = date(2027, 1, 15)
        write_stream(self.folder, later, ira_session(later), prefix="trad-ira")
        loop._now = lambda: datetime(2027, 1, 18, 12, 5, tzinfo=timezone.utc)
        loop._cash_ledger = str(Path(self.dir.name) / "engine" / "cash-movements.jsonl")
        with self.assertLogs("ibkr", "INFO") as logged:
            self.steps(loop)
        out = self.outcomes(logged)
        self.assertTrue(out, logged.output)
        self.assertIn("files=trad-ira-2027-01-15.jsonl", out[-1])
        self.assertIn("outcome=waiting", out[-1])
        self.assertIn("reason=", out[-1])

    def test_a_repeated_retraction_struck_off_as_committed_counts_and_the_batch_is_delivered(self):
        retraction = ("market.session.retracted", 1, "marketdata:retracted:2027-01-12", "2027-01-13T21:00:00Z",
                      {"source": "nyse", "session": "2027-01-12", "reason": "synthetic closure"})
        first = [_envelope(*retraction, sequence=1)] + [dict(e, sequence=n) for n, e in enumerate(ira_session(FRIDAY), 2)]
        write_stream(self.folder, FRIDAY, first, prefix="trad-ira")
        loop = self.loop_with()
        loop.start()
        self.steps(loop)
        later = date(2027, 1, 15)
        again = [_envelope(*retraction, sequence=1)] + [dict(e, sequence=n) for n, e in enumerate(ira_session(later), 2)]
        write_stream(self.folder, later, again, prefix="trad-ira")
        self.fake.server_time = int(datetime(2027, 1, 15, 22, 5, tzinfo=timezone.utc).timestamp())
        loop._now = lambda: datetime(2027, 1, 15, 22, 5, tzinfo=timezone.utc)
        with self.assertLogs("ibkr", "INFO") as logged:
            self.steps(loop)
        out = self.outcomes(logged)
        self.assertEqual(len(out), 1, logged.output)
        self.assertRegex(out[0], r"inputs=(\d+) answered=(\d+) struck=1 outcome=delivered")
        inputs, answered = map(int, re.search(r"inputs=(\d+) answered=(\d+)", out[0]).groups())
        self.assertEqual(answered + 1, inputs)

    def test_a_batch_a_stop_left_queuing_and_this_start_finished_states_its_outcome(self):
        from unittest import mock
        from loop import Outbox
        write_stream(self.folder, FRIDAY, ira_session(FRIDAY), prefix="trad-ira")
        loop = self.loop_with()
        loop.start()
        with mock.patch.object(Outbox, "extend", side_effect=OSError("synthetic: the machine stopped")):
            with self.assertRaises(OSError):
                loop.step()
        loop.close()
        restarted = self.loop_with()
        with self.assertLogs("ibkr", "INFO") as logged:
            restarted.start()
            self.steps(restarted)
        out = self.outcomes(logged)
        self.assertEqual(len(out), 1, logged.output)
        self.assertRegex(out[0], r"files=trad-ira-2027-01-14\.jsonl inputs=(\d+) answered=\1 struck=0 outcome=delivered")
        self.assertTrue(any("ibkr stream queued" in line for line in logged.output), "the finished batch is queued too")

    def test_a_second_waiting_step_keeps_the_specific_reason(self):
        write_stream(self.folder, FRIDAY, ira_session(FRIDAY), prefix="trad-ira")
        loop = self.loop_with()
        loop.start()
        self.steps(loop)
        later = date(2027, 1, 15)
        write_stream(self.folder, later, ira_session(later), prefix="trad-ira")
        loop._now = lambda: datetime(2027, 1, 18, 12, 5, tzinfo=timezone.utc)
        loop._cash_ledger = str(Path(self.dir.name) / "engine" / "cash-movements.jsonl")
        with self.assertLogs("ibkr", "INFO") as logged:
            self.steps(loop, 6)
        out = [o for o in self.outcomes(logged) if "outcome=waiting" in o]
        self.assertEqual(len(out), 1, out)
        self.assertIn("executions proof", out[0])

    def test_a_log_call_that_cannot_be_formatted_never_raises_into_the_delivery(self):
        import logging
        loop = self.loop_with()

        def reading(close, closes):
            logging.getLogger("ibkr").info("ibkr %d broken", "not a number")
            return None
        loop._post_close_reading = reading
        logger = logging.getLogger("ibkr")
        level = logger.level
        logger.setLevel(logging.INFO)
        self.addCleanup(logger.setLevel, level)
        with mock_stderr():
            self.assertIsNone(loop._batch_reading(CLOSE, {}))

    def test_an_input_answered_on_a_retry_after_a_stop_settles_its_batch(self):
        from unittest import mock
        import loop as loop_module
        loop = self.loop_with()
        loop.start()
        loop.delivery._tracking = {"files": "trad-ira-2027-01-14.jsonl", "ids": {"x"}, "inputs": 1, "answered": 0,
                                   "struck": 0, "resumed": ""}
        with mock.patch.object(loop.pending, "load", return_value={"id": "x", "type": "market.session.closed"}), \
                mock.patch.object(loop_module, "earlier_run", return_value=None), \
                mock.patch.object(loop.relay, "exchange", return_value={"payload": {"decisions": []}}), \
                self.assertLogs("ibkr", "INFO") as logged:
            loop._settle_waiting()
        self.assertTrue(any("answered=1 struck=0 outcome=delivered" in o for o in self.outcomes(logged)), logged.output)

    def test_a_batch_a_stop_left_queued_with_its_entries_unsent_states_its_outcome_on_the_next_start(self):
        from unittest import mock
        write_stream(self.folder, FRIDAY, ira_session(FRIDAY), prefix="trad-ira")
        loop = self.loop_with()
        loop.start()
        with mock.patch.object(loop, "_exchange", side_effect=OSError("synthetic: the machine stopped")):
            with self.assertRaises(OSError):
                self.steps(loop, 5)
        self.assertTrue(loop.outbox.load(), "the batch is queued and unsent")
        loop.close()
        restarted = self.loop_with()
        with self.assertLogs("ibkr", "INFO") as logged:
            restarted.start()
            self.steps(restarted)
        out = self.outcomes(logged)
        self.assertEqual(len(out), 1, logged.output)
        self.assertRegex(out[0], r"files=trad-ira-2027-01-14\.jsonl inputs=(\d+) answered=\1 struck=0 resumed=1 outcome=delivered")
        self.assertTrue(any("ibkr stream queued" in line for line in logged.output))

    def stopped_awaiting(self, ident):
        """A start whose engine never answered input ident (the stop came while it awaited its reply)."""
        write_stream(self.folder, FRIDAY, ira_session(FRIDAY), prefix="trad-ira")
        loop = self.loop_with()
        loop.start()
        decide = self.engine.decide

        def stops_at(envelope):
            if envelope["id"] == ident:
                raise OSError("synthetic: the machine stopped while the engine decided")
            return decide(envelope)
        self.engine.decide = stops_at
        with self.assertRaises(OSError):
            self.steps(loop)
        self.engine.decide = decide
        loop.close()
        return self.loop_with()

    def test_a_batch_whose_last_input_awaited_its_reply_at_a_stop_is_counted_on_the_restart(self):
        restarted = self.stopped_awaiting("marketdata:session-closed:2027-01-14")
        with self.assertLogs("ibkr", "INFO") as logged:
            restarted.start()
            self.steps(restarted)
        out = self.outcomes(logged)
        self.assertEqual(out, ["stream batch files=trad-ira-2027-01-14.jsonl inputs=1 answered=1 struck=0 resumed=1 "
                               "outcome=delivered reason=every input answered or already committed"], logged.output)
        self.assertTrue(any("ibkr stream queued" in l and "inputs=1 resumed=1" in l for l in logged.output))

    def test_a_mark_retried_on_the_restart_is_counted_with_the_two_entries_after_it(self):
        mark = [e.id for e in self.stream_marks()][-2]
        restarted = self.stopped_awaiting(mark)
        with self.assertLogs("ibkr", "INFO") as logged:
            restarted.start()
            self.steps(restarted)
        out = self.outcomes(logged)
        self.assertEqual(len(out), 1, logged.output)
        self.assertIn("inputs=3 answered=3 struck=0 resumed=1 outcome=delivered", out[0])

    def stream_marks(self):
        """The marks a batch of ira_session delivers, in delivery order (SHY, SPY last, then the close)."""
        from types import SimpleNamespace
        return [SimpleNamespace(id="ibkr:account.broker.snapshot:1:market.mark.recorded:" + s)
                for s in ("AAA", "BBB", "IEF", "SHY", "SPY")]


class ProbeOutcomeTests(ProbeTests):
    """The probe judges the latest batch by the adapter's own outcome line, never by counting answered lines."""

    test_a_good_start_passes_naming_each_check = None

    def made(self, lines):
        path = self.dir / "made.txt"
        path.write_text("\n".join(lines) + "\n")
        return path

    def test_a_later_waiting_batch_fails_though_an_earlier_one_was_delivered(self):
        lines = self.start().read_text().splitlines()
        lines.append("x ibkr stream batch files=trad-ira-2027-01-15.jsonl inputs=0 answered=0 struck=0 "
                     "outcome=waiting reason=ibkr broker snapshot waits for the Flex reader's executions proof")
        status, out = self.probe(self.made(lines))
        self.assertEqual(status, 1, out)
        self.assertTrue(any(l.startswith("probe: FAIL") and "waits for the Flex reader" in l for l in out), out)

    def test_interleaved_answers_do_not_pad_a_batch_the_adapter_did_not_finish(self):
        lines = [l for l in self.start().read_text().splitlines() if OUTCOME not in l]
        lines.append("x ibkr input=reconciliation:after-reconnect type=account.reconciliation answered with 0 decisions")
        status, out = self.probe(self.made(lines))
        self.assertEqual(status, 1, out)
        self.assertTrue(any(l.startswith("probe: FAIL") and "no outcome" in l for l in out), out)

    def test_a_delivered_outcome_whose_count_does_not_add_up_fails(self):
        lines = [l.replace("struck=0", "struck=0").replace(" answered=", " answered=1") if OUTCOME in l else l
                 for l in self.start().read_text().splitlines()]
        lines = [re.sub(r"answered=\d+", "answered=3", l) if OUTCOME in l else l for l in lines]
        status, out = self.probe(self.made(lines))
        self.assertEqual(status, 1, out)

    def test_a_resumed_batch_is_judged_by_its_remainder(self):
        lines = [l for l in self.start().read_text().splitlines()]
        made = [l for l in lines if "ibkr stream queued" not in l and OUTCOME not in l and " input=" not in l]
        made += ["x ibkr stream queued run={} files=trad-ira-2027-01-14.jsonl inputs=1 resumed=1".format(RUN),
                 "x ibkr input=marketdata:session-closed:2027-01-14 type=market.session.closed answered with 0 decisions",
                 "x ibkr stream batch files=trad-ira-2027-01-14.jsonl inputs=1 answered=1 struck=0 resumed=1 "
                 "outcome=delivered reason=every input answered or already committed"]
        status, out = self.probe(self.made(made))
        self.assertEqual(status, 0, out)
        self.assertTrue(any("resumed" in l and l.startswith("probe: ok queued") for l in out), out)

    def test_an_impossible_date_or_the_zero_time_is_no_close(self):
        from probe import mark_fault
        good = MarkValidationTests().good()
        for stamp in ("2027-02-30T21:00:00Z", "0001-01-01T00:00:00Z"):
            self.assertEqual(mark_fault(dict(good, as_of=stamp), 3, stamp), "as_of", stamp)


def fractional_reading(quantity="12.3456", version=6):
    h = connected(account_snapshot_version=version)
    row = captured_fractional("fractional_position")
    h.fake.positions = [(ACCOUNT, row.contract, Decimal(quantity), row.avgCost)]
    h.fake.summary = full_summary() + [(ACCOUNT, "SettledCash", "500.00", "USD")]
    return h


class FractionalAccountTests(unittest.TestCase):
    def test_config_selects_fractional_policy_and_shipped_ira_uses_it(self):
        self.assertEqual(config(account_snapshot_version=6).account_snapshot_version, 6)
        repo = Path(__file__).resolve().parents[3]
        self.assertEqual(json.loads((repo / "deploy/etc/adapters/trad-ira.json.example").read_text())[
            "account_snapshot_version"], 6)

    def test_broker_decimal_is_written_in_exact_engine_units_on_all_records(self):
        reading = read_account(fractional_reading().conn)
        self.assertEqual(reading.positions, {"AAA": Decimal("12.3456")})
        self.assertFalse(reading.unread)
        records = [account_snapshot_record(reading, CLOSE, version=6),
                   broker_snapshot_record(reading, False, why=WHY),
                   reconciliation_record(reading, "fractional-reading")]
        self.assertEqual([r.schema_version for r in records], [6, 3, 2])
        for record in records:
            with self.subTest(record=record.event_type):
                self.assertEqual(record.payload["quantity_scale"], 10000)
                self.assertEqual(record.payload["positions"][0]["quantity"], 123456)
                json.dumps(record.payload, allow_nan=False)
        self.assertEqual(close_valuation(reading, {"AAA": "10"}), Decimal("635.7560"))

    def test_precision_and_whole_account_refusals_keep_the_reason(self):
        for quantity, version, reason in [("12.34567", 6, "four decimal places"),
                                          ("12.3456", 4, "whole-share"),
                                          ("-0.1", 6, "nonnegative")]:
            with self.subTest(quantity=quantity, version=version):
                # Before v6 support exists, use a validated legacy config with
                # only its version replaced so this seam reaches the callback.
                h = fractional_reading(quantity, version=4)
                h.conn.config = h.conn.config._replace(account_snapshot_version=version)
                with self.assertLogs("ibkr", level="WARNING") as logged:
                    reading = read_account(h.conn)
                self.assertIn("positions", reading.unread)
                self.assertEqual(reading.positions, {})
                self.assertIn(reason, " ".join(logged.output))
                self.assertIn(reason, reading.summary)

    def test_open_order_total_and_snapshot_policy_cannot_be_mis_scaled(self):
        h = fractional_reading()
        h.fake.open_orders = [(7, contract(symbol="AAA", con_id=900001),
                               order(action="SELL", total_quantity=Decimal("7.25")), state("Submitted"))]
        reading = read_account(h.conn)
        self.assertNotIn("open-orders", reading.unread)
        record = broker_snapshot_record(reading, False, why=WHY)
        self.assertEqual(record.payload["open_orders"][0]["remaining_quantity"], 72500)
        with self.assertRaisesRegex(ValueError, "scale|policy"):
            account_snapshot_record(reading, CLOSE, version=4)

    def test_quantity_conversion_refuses_rounding_nonfinite_and_overflow(self):
        from quantities import units, shares
        self.assertEqual(units(Decimal("12.3456"), 10000), 123456)
        self.assertEqual(shares(123456, 10000), Decimal("12.3456"))
        for bad in ("12.34567", "1.00000", "NaN", "Infinity", True, "922337203685477.5808"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                units(bad, 10000)
        for scale in (1, True, "10000", 10000.0):
            with self.subTest(scale=scale), self.assertRaises(ValueError):
                shares(1, scale)


class FractionalReconciliationTests(unittest.TestCase):
    def test_book_and_fill_compare_exact_decimal_shares_and_cash(self):
        from test_delivery import commit
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "synthetic.log"
            engine = LoggedEngine(str(log), approval=run_approval("synthetic", starting_book=({}, 1000)))
            payload = FractionalProbeTests().payload()
            payload["total_cash"] = 1000
            commit(engine, "reading:account.reconciliation", kind="account.reconciliation", version=2,
                   payload=dict(payload, unreadable=[]))
            commit(engine, "reading:account.snapshot", kind="account.snapshot", version=6, payload=payload)
            daily, _ = previous_close(str(log))
        positions, cash = daily.positions, daily.cash
        self.assertEqual(positions, {"AAA": Decimal("12.3456")})
        self.assertEqual(daily.refused, "")
        fill = {"instrument_id": "AAA", "quantity": 35000, "quantity_scale": 10000,
                "side": "sell", "price": 10, "commission": 1}
        self.assertEqual(holdings_after(positions, [fill]), {"AAA": Decimal("8.8456")})
        self.assertEqual(cash + cash_change([fill]), Decimal("1034.00"))
        basis = Basis(positions, cash, [], "synthetic", False, "", datetime(2027, 1, 14, tzinfo=timezone.utc))
        restored = _from_json(json.loads(json.dumps(_to_json(basis))))
        self.assertEqual(restored.positions, positions)


class FractionalProbeTests(unittest.TestCase):
    def payload(self):
        return {"account_id": "synthetic", "as_of": "2027-01-14T21:00:00Z", "currency": "USD",
                "net_liquidation": 1000, "settled_cash": 100, "total_cash": 100,
                "quantity_scale": 10000, "positions": [{"instrument_id": "AAA", "quantity": 123456}]}

    def test_snapshot_scale_must_agree_with_configured_version(self):
        p = self.payload()
        self.assertEqual(snapshot_fault(p, 6), "")
        self.assertIn("quantity_scale", snapshot_fault(p, 4))
        for value in (None, 1, True, 10000.0):
            p["quantity_scale"] = value
            self.assertIn("quantity_scale", snapshot_fault(p, 6))
        p = self.payload()
        p["positions"][0]["quantity"] = 12.3456
        self.assertTrue(snapshot_fault(p, 6))
        p.pop("quantity_scale")
        self.assertTrue(snapshot_fault(p, 4))
        p = self.payload()
        del p["settled_cash"]
        self.assertEqual(snapshot_fault(p, 6), "settled_cash")

    def test_probe_displays_decimal_holdings_for_a_complete_fractional_batch(self):
        from probe import Report, _check_batch
        from snapshot import mark_record, Contract
        p = self.payload()
        mark = mark_record("AAA", CLOSE, "10", "synthetic",
                           contract=Contract("AAA", 900001, "SMART", "TEST", "USD"))
        records = [("account.snapshot", 6, p), ("account.reconciliation", 2, dict(p, unreadable=[])),
                   (mark.event_type, mark.schema_version, mark.payload),
                   ("market.session.closed", 1, {"period_end": p["as_of"]})]
        report = Report()
        _check_batch([dict(id=str(n), type=kind, schema_version=version, payload=payload)
                      for n, (kind, version, payload) in enumerate(records)], 6, report)
        self.assertEqual(report.failures, 0, report.lines)
        self.assertTrue(any("AAA 12.3456" in line for line in report.lines), report.lines)

    def test_reconciliation_and_fill_use_account_policy(self):
        import probe
        p = self.payload()
        p["unreadable"] = []
        self.assertEqual(reconciliation_fault(p, snapshot_version=6, version=2), "")
        self.assertIn("quantity_scale", reconciliation_fault(p, snapshot_version=4, version=2))
        fill = {"account_id": "synthetic", "client_order_id": "sell-aaa", "fill_id": "synthetic.01",
                "instrument_id": "AAA", "side": "sell", "quantity": 35000, "quantity_scale": 10000,
                "price": 10, "commission": 1, "filled_at": "2027-01-14T21:00:00Z"}
        self.assertEqual(probe.fill_fault(fill, 6, 6), "")
        self.assertIn("quantity_scale", probe.fill_fault(fill, 6, 4))
        fill["quantity"] = 3.5
        self.assertTrue(probe.fill_fault(fill, 6, 6))


class FractionalLoopTests(IraBatchHarness):
    def test_fractional_position_explained_by_fill_reaches_delivery_and_reconciliation(self):
        from test_delivery import commit
        row = captured_fractional("fractional_position")
        self.fake.positions = [(ACCOUNT, row.contract, row.position, row.avgCost)]
        self.fake.summary = ira_summary(settled="9876.54", total="9876.54")
        commit(self.engine, "synthetic-fill", kind="execution.fill", version=6, payload={
            "account_id": ACCOUNT, "client_order_id": "synthetic-order", "fill_id": "synthetic.01",
            "instrument_id": "AAA", "side": "buy", "quantity": 123456, "quantity_scale": 10000,
            "price": 10, "commission": 0, "filled_at": "2027-01-14T15:00:00Z"})
        sent = self.run_loop(held=("AAA",), account_snapshot_version=6)
        for kind, version in (("account.snapshot", 6), ("account.broker.snapshot", 3),
                              ("account.reconciliation", 2)):
            matching = [e for e in sent if e["type"] == kind]
            self.assertTrue(matching, (kind, sent))
            for envelope in matching:
                self.assertEqual(envelope["schema_version"], version)
                self.assertEqual(envelope["payload"]["quantity_scale"], 10000)
                self.assertEqual(envelope["payload"]["positions"][0]["quantity"], 123456)
        self.assertEqual(self.fake.placed, [])


class FractionalDeliveryTests(unittest.TestCase):
    def test_delivery_keeps_scale_and_classifies_fractional_inputs_as_history(self):
        from types import SimpleNamespace
        from delivery import _entries, PostCloseReading, DELIVERED_KINDS
        reading = read_account(fractional_reading().conn)
        account = account_snapshot_record(reading, CLOSE, version=6)
        broker = broker_snapshot_record(reading, False, why=WHY)
        stream = SimpleNamespace(envelopes=[dict(id="close", type="market.session.closed", schema_version=1,
                                                 event_time="2027-01-14T21:00:00Z", payload={})])
        batch = PostCloseReading(account, broker, [], reading)
        entries = _entries("synthetic", "snapshot", batch, [stream], book=True)
        self.assertEqual([(e["event_type"], e["schema_version"]) for e in entries[:3]],
                         [("account.reconciliation", 2), ("account.snapshot", 6), ("account.broker.snapshot", 3)])
        for entry in entries[:3]:
            self.assertEqual(entry["payload"]["quantity_scale"], 10000)
            self.assertEqual(entry["payload"]["positions"][0]["quantity"], 123456)
        self.assertIn(("account.snapshot", 6), DELIVERED_KINDS)
        self.assertIn(("account.broker.snapshot", 3), DELIVERED_KINDS)

    def test_flex_equity_reader_accepts_fractional_cash_snapshot(self):
        import tempfile
        import flex
        from engine_double import LoggedEngine, run_approval
        from test_delivery import commit
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "synthetic.log"
            approval = run_approval("synthetic", starting_book=({}, 1000))
            engine = LoggedEngine(str(path), approval=approval)
            payload = FractionalProbeTests().payload()
            commit(engine, "snapshot", kind="account.snapshot", version=6, payload=payload)
            _, snapshots = flex.read_chain(str(path))
            self.assertEqual(snapshots[0][1:], (Decimal("1000.0"), "USD"))


NOW = datetime(2026, 10, 29, 19, 1, tzinfo=timezone.utc)
SESSION = date(2026, 10, 16)


def release():
    return dict(account_id="synthetic", client_order_id="tw-fractional", instrument_id="AAA", side="sell",
                quantity=72500, quantity_scale=10000, limit_price=10, mode="paper", send=True)


def status(lifecycle="submitted"):
    return OrderStatusReport(12, 17, None, "synthetic-order", None, lifecycle, Decimal("3.5"), Decimal("3.75"),
                             None, None, None, None)


class FractionalOrdersTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.held = str(Path(self.directory.name) / "held")

    def journal(self, written, journalled=(), scale=10000):
        return OrderReportJournal("synthetic", 12, written.append, lambda: NOW, 30, self.held,
                                  lambda: True, lambda: True, journalled=journalled, quantity_scale=scale)

    def test_release_maps_ticks_to_exact_decimal_and_rejects_bad_scales(self):
        request = release_to_request(release(), "paper", {"AAA": 900001}, SESSION)
        self.assertEqual(request.quantity, Decimal("7.25"))
        for bad in (True, 1, 1000, 10000.0, "10000"):
            with self.subTest(scale=bad), self.assertRaises(ValueError):
                release_to_request(dict(release(), quantity_scale=bad), "paper", {"AAA": 900001}, SESSION)

    def test_whole_valued_release_must_match_adapter_scale_before_handover(self):
        for version, scale in ((4, 10000), (6, 0)):
            for method in ("check", "send"):
                with self.subTest(version=version, scale=scale, method=method):
                    h = Harness(mode="paper")
                    h.conn.connect()
                    h.conn.mark_reconciled()
                    h.conn.config = h.conn.config._replace(account_snapshot_version=version)
                    order = dict(release(), account_id=h.conn.config.account_id,
                                 quantity=scale or 1, quantity_scale=scale)
                    request = release_to_request(order, "paper", {"AAA": 900001}, SESSION)
                    written = []
                    sender = paper_sender(h, written)
                    next_id = h.conn.next_order_id
                    with self.assertRaisesRegex(UnsendableOrder, "quantity scale"):
                        getattr(sender, method)(request)
                    self.assertEqual(h.fake.placed, [])
                    self.assertEqual(written, [])
                    self.assertEqual(h.conn.next_order_id, next_id)

    def test_account_policy_controls_fractional_request_before_sending(self):
        h = Harness(mode="paper")
        h.conn.connect()
        h.conn.mark_reconciled()
        request = limit_request(quantity=Decimal("7.25"), quantity_scale=10000)
        with self.assertRaisesRegex(UnsendableOrder, "whole"):
            paper_sender(h, []).check(request)
        h.conn.config = h.conn.config._replace(account_snapshot_version=6)
        paper_sender(h, []).send(request)
        self.assertEqual(h.fake.placed[0][2].totalQuantity, Decimal("7.25"))
        with self.assertRaisesRegex(UnsendableOrder, "four decimal"):
            paper_sender(h, []).check(limit_request(quantity=Decimal("7.25001"), quantity_scale=10000))

    def test_resolve_errors_name_both_schema_scale_contracts(self):
        cases = ((relay._fill_id, relay.FILL_TYPE, 5, 6),
                 (relay._lifecycle, relay.LIFECYCLE_TYPE, 2, 4),
                 (relay._retired, relay.RETIRED_TYPE, 1, 2))
        for reader, event_type, legacy, fractional in cases:
            for scale in (0, 10000.0):
                with self.subTest(event_type=event_type, scale=scale):
                    envelope = dict(schema_version=fractional, payload={"quantity_scale": scale})
                    args = ("synthetic.log", 1, envelope) if reader == relay._retired else (
                        "synthetic.log", 1, relay.LOG_INPUT, envelope)
                    with self.assertRaises(relay.ResolveRefused) as caught:
                        reader(*args)
                    message = str(caught.exception)
                    self.assertIn(event_type, message)
                    self.assertIn("version {} (whole shares)".format(legacy), message)
                    self.assertIn("version {} (quantity_scale integer 10000)".format(fractional), message)

    def test_captured_partial_fill_and_held_cancel_survive_restart_at_exact_scale(self):
        written = []
        j = self.journal(written)
        request = release_to_request(dict(release(), client_order_id="synthetic-order"), "paper", {"AAA": 900001}, SESSION)
        j.sent(SentRecord(request, True, 12, 17))
        j.on_record(status())
        row = captured_fractional("fractional_fill")
        report = execution_report(row.contract, row.execution)
        j.on_record(report)
        j.on_record(status("canceled"))
        self.assertEqual([(r.schema_version, r.payload["quantity"]) for r in written], [(4, 72500)])
        restarted = self.journal(written, list(written))
        restarted.on_record(report)
        restarted.on_record(CommissionReport(report.exec_id, Decimal("0.25"), "USD"))
        self.assertEqual([(r.schema_version, r.payload["quantity"]) for r in written],
                         [(4, 72500), (6, 35000), (4, 72500)])
        self.assertTrue(all(r.payload["quantity_scale"] == 10000 for r in written))
        self.assertEqual(Decimal(written[0].payload["quantity"] - written[1].payload["quantity"]) / 10000,
                         Decimal("3.75"))
        self.assertEqual(relay._fill_id("test", 1, relay.LOG_INPUT,
                                       dict(schema_version=6, payload=written[1].payload)), report.exec_id)
        self.assertEqual(relay._lifecycle("test", 1, relay.LOG_INPUT,
                                         dict(schema_version=4, payload=written[2].payload)),
                         ("synthetic-order", "canceled"))
        # The Go report tests read this same adapter-produced shape.
        expected = Path(__file__).parent / "testdata" / "fractional_reports.json"
        self.assertEqual([dict(event_type=r.event_type, schema_version=r.schema_version, payload=r.payload)
                          for r in written], json.loads(expected.read_text()))

    def test_held_fractional_record_refuses_float_scale_and_overflow(self):
        written = []
        j = self.journal(written)
        request = release_to_request(dict(release(), client_order_id="synthetic-order"),
                                     "paper", {"AAA": 900001}, SESSION)
        j.sent(SentRecord(request, True, 12, 17))
        row = captured_fractional("fractional_fill")
        j.on_record(execution_report(row.contract, row.execution))
        j.on_record(status("canceled"))
        original = json.loads(Path(self.held).read_text())
        for field, value in (("quantity_scale", 10000.0), ("quantity", 2 ** 63)):
            with self.subTest(field=field):
                changed = json.loads(json.dumps(original))
                changed[0]["record"]["payload"][field] = value
                Path(self.held).write_text(json.dumps(changed))
                with self.assertRaises(UnreportableOrder):
                    self.journal([])

    def test_whole_share_report_refuses_fractional_fill_with_reason(self):
        j = self.journal([], scale=0)
        row = captured_fractional("fractional_fill")
        with self.assertRaisesRegex(UnreportableOrder, "whole"):
            j.on_record(execution_report(row.contract, row.execution))

    def test_relay_compares_mixed_version_quantities_in_shares(self):
        fill = dict(account_id="synthetic", client_order_id="tw-fractional", instrument_id="AAA", side="sell",
                    fill_id="synthetic.01", quantity=35000, quantity_scale=10000, price=10, commission=0.25,
                    filled_at="2026-10-16T14:00:00Z")
        groups = [[(1, relay.LOG_INPUT, dict(type=relay.FILL_TYPE, schema_version=6, payload=fill)),
                   (2, relay.LOG_DECISION, dict(type=relay.RELEASED, schema_version=4, payload=release()))]]
        ended, fills = relay._order_ends("test", groups, (), {})
        self.assertEqual(fills["synthetic.01"], ("tw-fractional", Decimal("3.5")))
        self.assertEqual(relay._open_orders(groups, ended, fills), {"tw-fractional"})
        fills["synthetic.02"] = ("tw-fractional", Decimal("3.75"))
        self.assertEqual(relay._open_orders(groups, ended, fills), set())
        for version, scale in ((5, 10000), (6, 0), (6, True), (6, 1000)):
            with self.subTest(version=version, scale=scale), self.assertRaises(relay.ResolveRefused):
                relay._fill_id("test", 1, relay.LOG_INPUT,
                               dict(schema_version=version, payload=dict(fill, quantity_scale=scale)))

    def test_synthetic_fractional_decision_is_not_a_broker_cancellation(self):
        groups = [[(1, relay.LOG_DECISION, dict(type=relay.LIFECYCLE_TYPE, schema_version=5,
                   payload=dict(release(), status="canceled", synthetic=True)))]]
        self.assertEqual(relay._order_ends("test", groups, (), {}), (set(), {}))

    def test_journal_seed_refuses_schema_scale_disagreement(self):
        from snapshot import Record
        payload = dict(account_id="synthetic", client_order_id="synthetic-order", instrument_id="AAA",
                       order_id="order:12:17", status="submitted", side="sell", quantity=72500,
                       quantity_scale=10000, limit_price=10, occurred_at="2026-10-29T19:01:00Z", message="")
        for version, scale in ((2, 10000), (4, 0), (4, 10000.0)):
            with self.subTest(version=version, scale=scale), self.assertRaises(UnreportableOrder):
                self.journal([], [Record("execution.order.lifecycle", version,
                                        dict(payload, quantity_scale=scale))])

    def test_journal_seed_refuses_invalid_fill_quantity(self):
        from snapshot import Record
        for quantity in (0, -1, 2 ** 63, True, 3.5):
            with self.subTest(quantity=quantity), self.assertRaises(UnreportableOrder):
                self.journal([], [Record("execution.fill", 6,
                    dict(account_id="synthetic", fill_id="synthetic.01.01", quantity=quantity,
                         quantity_scale=10000))])

    def test_retirement_keeps_scale_and_version_in_durable_outbox(self):
        pending = relay.PendingInput(str(Path(self.directory.name) / "pending"))
        relay.retire_order(pending, release(), relay.RETIRED_SESSION_OVER, "session expired",
                           relay.RETIRED_BY_ADAPTER, NOW, SESSION, "input:1")
        entries = relay.Outbox(pending.path + relay.OUTBOX_SUFFIX).load()
        self.assertEqual(entries[0]["schema_version"], 2)
        self.assertEqual(entries[0]["payload"]["quantity_scale"], 10000)
        self.assertTrue(relay._is_queued_retirement(entries[0]))


if __name__ == "__main__":
    unittest.main()
