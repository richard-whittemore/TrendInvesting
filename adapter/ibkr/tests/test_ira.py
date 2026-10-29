"""The Traditional IRA's adapter: what the momentum engine reads, from one IBKR reading.

ADR 0026 points 2 and 3; docs/manual/momentum.md, "One IRA month". The
momentum engine reads account.snapshot version 4 (the settled and total
cash its buys are budgeted from) and values every order it plans against a
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
from delivery import StreamRefused, last_session, read_stream
from loop import AdapterLoop
from snapshot import (ACCOUNT_SNAPSHOT_TYPE, MARK_TYPE, account_snapshot_record, mark_record, read_account,
                      resolve_contracts)

TESTDATA = tests_dir / "testdata"
CLOSE = datetime(2027, 1, 14, 21, 0, tzinfo=timezone.utc)
FRIDAY = date(2027, 1, 14)
RUN = "engine"
# The IRA's adapter configuration as deploy/etc/adapters/trad-ira.json.example sets it.
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
        self.assertEqual(sorted(flex.ACCOUNT_SNAPSHOT_VERSIONS), [3, 4])


class DeliveredKindsTests(unittest.TestCase):
    """A fresh run's log holds no copied history; the IRA's adapter inputs count as history as Boost's do."""

    def test_the_iras_snapshot_and_contract_mark_are_delivered_kinds(self):
        from delivery import DELIVERED_KINDS
        self.assertIn(("account.snapshot", 4), DELIVERED_KINDS)
        self.assertIn(("market.mark.recorded", 3), DELIVERED_KINDS)


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
        self.assertIn('"$here/etc/adapters/trad-ira.json.example"', provision)
        self.assertIn("systemctl disable ti-adapter-boost.service ti-adapter@trad-ira.service", provision)

    def test_the_trad_ira_flex_reader_fires_at_its_own_staggered_times(self):
        dropin = REPO / "deploy" / "systemd" / "ti-flex@trad-ira.timer.d" / "stagger.conf"
        lines = unit_lines(dropin.read_text())
        self.assertEqual(lines, ["[Timer]", "OnCalendar=", "OnCalendar=Mon..Fri 05:45 America/New_York",
                                 "OnCalendar=Mon..Fri 06:45 America/New_York",
                                 "OnCalendar=Mon..Fri 07:45 America/New_York"])
        provision = (REPO / "deploy" / "provision.sh").read_text()
        self.assertIn("ti-flex@trad-ira.timer.d", provision)


class ClientIdTests(unittest.TestCase):
    """--client-id: the instance's settings and its configuration name the same client ID, or nothing starts."""

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
        values.update(connect_timeout=0.05, request_timeout=0.05, mark_timeout=0.05)
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
        journal.write_text(journal.read_text() + "x ibkr input={} type={} answered with 1 decisions\n".format(ident, kind))
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


if __name__ == "__main__":
    unittest.main()
