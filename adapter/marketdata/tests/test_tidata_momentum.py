"""Tests for ti-data's momentum run (--engine momentum): the daily data job
writing an IRA's compact stream, <instance>-<session>.jsonl, from the
recordings, the Go ranker and the engine's own durable log. Synthetic
data only; no network."""
import datetime as dt
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from marketdata import encode, nyse_calendar, record, tidata
from marketdata.tests.test_compact import (CHURN_DAILY_SHA256, CHURN_FIRST, CHURN_LAST, CHURN_NAMES, CHURN_RETRACTED,
                                           FakeRanker,
                                           build_churn, churn_ranker)
from marketdata.tests.test_encode import IDENTITY, bars_for, build
from marketdata.tests.test_sectors import iwv_csv, screener
from marketdata.tests.test_tidata import CHAIN, SYNTHETIC_VALUE, after_settle

D = dt.date
UTC = dt.timezone.utc
REPO = Path(__file__).resolve().parents[3]
INSTANCE = "trad-ira"
RUN = IDENTITY.run_id
SNAPSHOT = ("account.snapshot", 3)
UNUSED_RANKER = "/nonexistent/universe-rank"   # the ranker path when a test hands the job its own ranker


def momentum_config(**momentum):
    """The Go ranker's synthetic momentum configuration, with one bond fund (IEF) so its required instruments
    are IEF and SPY, the instruments the encoder's fixtures record."""
    cfg = json.loads((REPO / "cmd/universe-rank/testdata/momentum.json").read_text())
    cfg["glide"]["bond_funds"] = [{"instrument_id": "IEF", "fraction": 1}]
    cfg["momentum"].update(momentum)
    return cfg


def write_momentum_log(path, config, inputs=(), configuration_hash=IDENTITY.configuration_hash,
                       strategy_version=IDENTITY.strategy_version, broken=None):
    """A durable log in the engine's shape: the header, the configuration input, then each of ``inputs``
    ((type, schema version, payload)) as one committed group, each record's hash chained as
    LOG_CHAIN_ALGORITHM states; with ``broken`` (a record's sequence), that record's stated hash is not its
    chain's, as in a log changed after the engine wrote it."""
    header = {"chain_algorithm": CHAIN, "configuration_hash": configuration_hash, "log_version": 1,
              "strategy_version": strategy_version}
    lines = [json.dumps(header, sort_keys=True, separators=(",", ":"))]
    chain = tidata.engine_log.chain_seed(header)
    groups = [("configuration:" + configuration_hash, tidata.CONFIGURATION_TYPE, 1, config)]
    groups += [("input-{}".format(n), kind, version, payload) for n, (kind, version, payload) in enumerate(inputs, 2)]
    for n, (ident, kind, version, payload) in enumerate(groups, 1):
        raw = json.dumps(payload, separators=(",", ":"))
        envelope = {"id": ident, "type": kind, "envelope_version": 1, "schema_version": version,
                    "event_time": "2025-01-02T21:00:00Z", "recorded_at": "2025-01-02T21:00:00Z", "sequence": n,
                    "source": "engine", "strategy_version": strategy_version, "configuration_hash": configuration_hash,
                    "payload_hash": hashlib.sha256(raw.encode()).hexdigest(), "payload": payload}
        chain = tidata.engine_log.chain_next(chain, "input", envelope, raw.encode("utf-8"))
        digest = "sha256:" + (hashlib.sha256(b"changed").hexdigest() if n == broken else chain.hex())
        lines.append(json.dumps({"record": {"sequence": n, "kind": "input", "envelope": envelope,
                                            "record_hash": digest}}, separators=(",", ":")))
        lines.append(json.dumps({"commit": {"sequence": n, "record_hash": digest}}, separators=(",", ":")))
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_text("\n".join(lines) + "\n")


def snapshot(*held, version=SNAPSHOT[1]):
    return (SNAPSHOT[0], version) + ({"account_id": "trad-ira", "as_of": "2025-01-02T21:00:00Z", "net_liquidation": 1000.0,
                        "currency": "USD", "positions": [{"instrument_id": t, "quantity": 1} for t in sorted(held)]},)


def fill(ticker):
    return ("execution.fill", 5, {"instrument_id": ticker, "quantity": 1, "price": 10.0})


TICKERS = [{"ticker": t, "type": "CS", "market": "stocks"} for t in ("AAA", "BBB", "CCC", "DDD", "ZZZ")]


class FakeMassive:
    """Massive's endpoints the job uses. ``grouped`` answers only the days in ``bars`` (a dict of day ->
    rows); any other day is a fetch the test did not expect."""

    def __init__(self, bars=None):
        self.bars, self.calls = dict(bars or {}), []

    def grouped(self, day):
        self.calls.append(("grouped", day))
        if day not in self.bars:
            raise AssertionError("the job fetched the grouped bars of {}, which are recorded".format(day))
        return {"status": "OK", "results": self.bars[day]}

    def tickers(self, day, kind):
        self.calls.append(("tickers", day, kind))
        return [{"results": list(TICKERS)}]

    def splits(self, first, last):
        self.calls.append(("splits", first, last))
        return [{"results": []}]

    def all_dividends(self, first, last):
        self.calls.append(("dividends", first, last))
        return [{"results": []}]


def sector_pages(url):
    """The two sector sources' files, as sectors.http_get returns them."""
    from marketdata import sectors
    if url == sectors.IWV_URL:
        return iwv_csv(("AAA", "Information Technology", "Equity"), ("BBB", "Financials", "Equity")), url
    return screener(("CCC", "Energy"), ("AAA", "Finance")), url


def no_sector_pages(url):
    raise AssertionError("the job fetched the sector file {} in a test that records none".format(url))


class MomentumJob:
    """A temporary server layout for one IRA instance: data folder, configuration, the engine's log, secrets."""

    def __init__(self, root, data=None, config=None, inputs=(), client=None, ranker=None, sectors_get=sector_pages,
                 instance=INSTANCE):
        self.instance = instance
        self.root = Path(root)
        self.data = Path(data) if data else self.root / "data"
        self.data.mkdir(parents=True, exist_ok=True)
        self.streams = self.root / "streams"
        self.cfg = config or momentum_config()
        self.config = self.root / (instance + ".config")
        self.config.write_text(json.dumps(self.cfg, indent=2))
        self.log = self.root / "engines" / instance / (RUN + ".log")
        self.write_log(inputs)
        self.env_file = self.root / "env"
        self.env_file.write_text("MASSIVE_API_KEY=" + SYNTHETIC_VALUE + "\n")
        self.client = client or FakeMassive()
        self.ranker = ranker
        self.sectors_get = sectors_get

    def write_log(self, inputs=()):
        write_momentum_log(self.log, self.cfg, inputs)

    def argv(self, *extra, ranker=UNUSED_RANKER):
        return ["--engine", "momentum", "--instance", self.instance, "--ranker", ranker, "--data", str(self.data),
                "--streams", str(self.streams), "--config", str(self.config), "--engine-log", str(self.log),
                "--secrets-file", str(self.env_file)] + list(extra)

    def main(self, now, *extra, ranker_path=UNUSED_RANKER):
        out, err = io.StringIO(), io.StringIO()
        with mock.patch("sys.stdout", out), mock.patch("sys.stderr", err):
            status = tidata.main(self.argv(*extra, ranker=ranker_path), now=lambda: now,
                                 client_factory=lambda key: self.client, ranker=self.ranker,
                                 sectors_get=self.sectors_get)
        return status, out.getvalue(), err.getvalue()

    def stream_dir(self):
        return self.streams / self.instance / RUN

    def path(self, day):
        return self.stream_dir() / "{}-{}.jsonl".format(self.instance, day)

    def envelopes(self, day):
        return [json.loads(line) for line in self.path(day).read_text().splitlines()]


def encoded_bytes(data, first, last, base, optional, ranker, required=("IEF", "SPY")):
    """What `encode --ranker` writes for the span (iter_encode, the CLI's own call), numbered from 1 as the
    data job's file contract numbers a file."""
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "encoded.jsonl")
        encode.write_stream(path, encode.iter_encode(str(data), first, last, IDENTITY, required=list(required),
                                                     optional=list(optional), base=base, first_sequence=1,
                                                     ranker=ranker))
        return Path(path).read_bytes()


def renumbered_from_two(raw):
    """A file's envelopes numbered from 2, as the encoder's own default numbers them."""
    out = []
    for n, line in enumerate(raw.decode().splitlines(), 2):
        e = json.loads(line)
        e["sequence"] = n
        out.append(json.dumps(e, separators=(",", ":"), allow_nan=False) + "\n")
    return "".join(out).encode()


class ChurnMassive(FakeMassive):
    """The churn fixture's listing: its first twenty sessions are recorded with their bars only, and those
    with sector labels within a week get their universe from this listing."""

    def tickers(self, day, kind):
        self.calls.append(("tickers", day, kind))
        return [{"results": [{"ticker": t, "type": "CS", "market": "stocks"} for t in CHURN_NAMES]}]


class MomentumOutputContractTest(unittest.TestCase):
    """The encoder's output contract: the job's momentum run writes exactly what `encode --ranker` writes for
    the IRA's span, on the compact stream's recorded fixture (churn, actions, bad data and a retraction):
    the warm-up from the base, then one session a morning."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name) / "data"
        self.data.mkdir()
        build_churn(str(self.data))
        self.job = MomentumJob(self.tmp.name, data=self.data, ranker=churn_ranker, sectors_get=no_sector_pages,
                               client=ChurnMassive())
        self.previous = nyse_calendar.sessions(D(2026, 4, 29), D(2026, 4, 29))[0]

    def tearDown(self):
        self.tmp.cleanup()

    def warm_up(self):
        """The warm-up, run once the engine has started and before the adapter delivers anything, then the
        engine's log as the adapter's first delivery leaves it: an account snapshot holding C01."""
        status, out, err = self.job.main(after_settle(self.previous), "--backfill-from", CHURN_FIRST.isoformat())
        self.assertEqual(status, 0, err)
        self.assertEqual(out.strip(), str(self.job.path(self.previous)))
        self.job.write_log([snapshot("C01")])
        return err

    def test_the_warm_up_is_the_encoders_compact_stream_from_the_base(self):
        self.warm_up()
        self.assertEqual(self.job.path(self.previous).read_bytes(),
                         encoded_bytes(self.data, CHURN_FIRST, self.previous, CHURN_FIRST, [], churn_ranker))
        # Every session is recorded, so nothing is fetched, and the retracted day is never asked for.
        self.assertEqual([c for c in self.job.client.calls if c[0] == "grouped"], [])
        retracted = [e for e in self.job.envelopes(self.previous) if e["type"] == "market.session.retracted"]
        self.assertEqual([e["payload"]["session"] for e in retracted], [CHURN_RETRACTED.isoformat()])

    def test_the_next_morning_writes_one_session_the_recorded_daily_bytes(self):
        self.warm_up()
        status, out, err = self.job.main(after_settle(CHURN_LAST))
        self.assertEqual(status, 0, err)
        self.assertEqual(out.strip(), str(self.job.path(CHURN_LAST)))
        daily = self.job.path(CHURN_LAST).read_bytes()
        self.assertEqual(daily, encoded_bytes(self.data, CHURN_LAST, CHURN_LAST, CHURN_FIRST, ["C01"], churn_ranker))
        # The compact stream's recorded daily bytes (numbered from 2 there, from 1 in a file).
        self.assertEqual(hashlib.sha256(renumbered_from_two(daily)).hexdigest(), CHURN_DAILY_SHA256)
        closes = [e["payload"]["period_end"][:10] for e in self.job.envelopes(CHURN_LAST)
                  if e["type"] == "market.session.closed"]
        self.assertEqual(closes, [CHURN_LAST.isoformat()])
        self.assertEqual({e["correlation_id"] for e in self.job.envelopes(CHURN_LAST)}, {RUN})

    def test_a_second_firing_finds_the_file_and_fetches_nothing(self):
        self.warm_up()
        self.assertEqual(self.job.main(after_settle(CHURN_LAST))[0], 0)
        before = self.job.path(CHURN_LAST).read_bytes()
        self.job.client = mock.Mock(side_effect=AssertionError("fetched"))
        status, out, err = self.job.main(after_settle(CHURN_LAST, minutes=600))
        self.assertEqual(status, 0, err)
        self.assertEqual(out.strip(), str(self.job.path(CHURN_LAST)))
        self.assertEqual(self.job.path(CHURN_LAST).read_bytes(), before)

    def test_a_daily_firing_before_the_warm_up_is_refused_naming_it(self):
        status, _, err = self.job.main(after_settle(CHURN_LAST))
        self.assertEqual(status, 1)
        self.assertIn("--backfill-from", err)
        self.assertFalse(self.job.stream_dir().exists())

    def test_a_file_already_written_that_no_longer_encodes_stops_the_job(self):
        self.warm_up()
        path = self.job.path(self.previous)
        path.write_text(path.read_text().replace('"source":"marketdata"', '"source":"other"', 1))
        status, _, err = self.job.main(after_settle(CHURN_LAST))
        self.assertEqual(status, 1)
        self.assertIn(str(path), err)
        self.assertIn("a person", err)
        self.assertFalse(self.job.path(CHURN_LAST).exists())


class MomentumHoldingsTest(unittest.TestCase):
    """The holdings source: the instruments that get their own daily bars (--optional) are the ones the
    engine's own durable log says the account holds, read from it, never from a flag."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name) / "data"
        self.data.mkdir()
        build(str(self.data))

    def tearDown(self):
        self.tmp.cleanup()

    def bars_on_the_decision(self, inputs):
        job = MomentumJob(self.tmp.name, data=self.data, ranker=FakeRanker(members=("BBB", "AAA")))
        status, _, err = job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")
        self.assertEqual(status, 0, err)
        job.write_log(inputs)   # what the engine has committed since, the adapter's deliveries among it
        status, _, err = job.main(after_settle(D(2026, 11, 30)))
        self.assertEqual(status, 0, err)
        stream = job.envelopes(D(2026, 11, 30))
        bars = sorted(e["payload"]["instrument_id"] for e in stream if e["type"] == "market.bar.completed")
        histories = sorted(e["payload"]["instrument_id"] for e in stream if e["type"] == "market.close.history")
        return bars, histories

    def test_with_no_holding_in_the_log_only_the_required_instruments_get_bars(self):
        self.assertEqual(self.bars_on_the_decision([]), (["IEF", "SPY"], ["AAA", "BBB"]))

    def test_the_names_held_in_the_last_account_snapshot_get_bars_and_others_do_not(self):
        # CCC is held and not ranked: its own bars. AAA and BBB are ranked, not held: histories only. DDD is
        # neither: nothing.
        bars, histories = self.bars_on_the_decision([snapshot("AAA", "CCC"), snapshot("CCC")])
        self.assertEqual(bars, ["CCC", "IEF", "SPY"])
        self.assertEqual(histories, ["AAA", "BBB"])

    def test_fractional_snapshot_scale_is_checked_before_replacing_holdings(self):
        for scale in (None, 0, 1, 10000.0, "10000", True):
            with self.subTest(scale=scale):
                held = tidata._Holdings("synthetic.log")
                held.groups = 1
                held.current = {"AAA"}
                payload = {"positions": []}
                if scale is not None:
                    payload["quantity_scale"] = scale
                with self.assertRaisesRegex(tidata.Refused, "quantity_scale"):
                    held.keep([(2, "input", {"type": "account.snapshot", "schema_version": 6,
                                            "payload": payload})])
                self.assertEqual(held.held(), ("AAA",))

    def test_positions_contract_documents_fractional_snapshots(self):
        self.assertIn("6", tidata._positions.__doc__)
        self.assertIn("ADR 0026", tidata._positions.__doc__)
        self.assertIn("never read as holding nothing", tidata._positions.__doc__)

    def test_fractional_snapshot_and_fill_keep_holdings_coverage(self):
        inputs = [("account.snapshot", 6, {"positions": [
            {"instrument_id": "CCC", "quantity": 123456}], "quantity_scale": 10000}),
            ("execution.fill", 6, {"instrument_id": "DDD", "quantity": 35000, "quantity_scale": 10000})]
        bars, histories = self.bars_on_the_decision(inputs)
        self.assertEqual(bars, ["CCC", "DDD", "IEF", "SPY"])
        self.assertEqual(histories, ["AAA", "BBB"])

    def test_a_fill_after_the_last_snapshot_is_held_too(self):
        bars, _ = self.bars_on_the_decision([snapshot("CCC"), fill("DDD")])
        self.assertEqual(bars, ["CCC", "DDD", "IEF", "SPY"])

    def test_with_no_snapshot_the_run_approvals_starting_book_is_the_holding(self):
        approval = ("operator.run.approved", 3, {"approved_by": "owner", "starting_book": {
            "positions": [{"instrument_id": "CCC", "quantity": 3}], "total_cash": 10.0, "currency": "USD"}})
        bars, _ = self.bars_on_the_decision([approval])
        self.assertEqual(bars, ["CCC", "IEF", "SPY"])

    def test_the_holdings_are_read_from_the_log(self):
        log = Path(self.tmp.name) / "run-1.log"
        write_momentum_log(log, momentum_config(), [snapshot("BBB"), fill("AAA"), snapshot("CCC", "SPY"), fill("DDD")])
        self.assertEqual(tidata.read_engine_log(str(log), engine="momentum").holdings, ("CCC", "DDD", "SPY"))
        write_momentum_log(log, momentum_config(), [])
        self.assertEqual(tidata.read_engine_log(str(log), engine="momentum").holdings, ())

    def test_a_snapshot_whose_positions_cannot_be_read_is_refused(self):
        log = Path(self.tmp.name) / "run-1.log"
        bad = SNAPSHOT + ({"account_id": "trad-ira", "positions": [{"quantity": 1}]},)
        write_momentum_log(log, momentum_config(), [bad])
        with self.assertRaises(tidata.Refused) as caught:
            tidata.read_engine_log(str(log), engine="momentum")
        self.assertIn("positions", str(caught.exception))

    def test_there_is_no_flag_for_the_holdings(self):
        job = MomentumJob(self.tmp.name, data=self.data)
        with mock.patch("sys.stderr", io.StringIO()), self.assertRaises(SystemExit):
            tidata.parse(job.argv("--optional", "CCC"))


class MomentumJobTest(unittest.TestCase):
    """The morning's recordings for an IRA: the session's grouped bars once (shared with Boost's job), its
    universe (the listing) from those bars, and its sector labels; then the stream."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name) / "data"
        self.data.mkdir()
        build(str(self.data))
        os.remove(self.data / "grouped_2026-11-27.json")

    def tearDown(self):
        self.tmp.cleanup()

    def test_the_latest_session_is_recorded_with_its_universe_and_sectors(self):
        client = FakeMassive({D(2026, 11, 27): bars_for(D(2026, 11, 27))})
        job = MomentumJob(self.tmp.name, data=self.data, client=client, ranker=FakeRanker())
        status, _, err = job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")
        self.assertEqual(status, 0, err)
        self.assertEqual([c for c in client.calls if c[0] in ("grouped", "tickers")],
                         [("grouped", D(2026, 11, 27)), ("tickers", D(2026, 11, 27), "CS")])
        grouped = record.read(str(self.data / "grouped_2026-11-27.json"))
        universe = record.read(str(self.data / "universe_2026-11-27.json"))
        self.assertEqual(universe["bars"], grouped["bars"])   # one fetch, so the two recordings agree
        self.assertTrue((self.data / "sectors-iwv_2026-11-27.json").exists())
        self.assertTrue((self.data / "sectors-nasdaq_2026-11-27.json").exists())
        snapshots = [e for e in job.envelopes(D(2026, 11, 27)) if e["type"] == "market.universe.snapshot"]
        self.assertEqual([e["payload"]["session"] for e in snapshots], ["2026-11-27"])

    def test_a_session_with_no_labels_within_a_week_gets_no_universe_and_the_next_morning_carries_on(self):
        # The sector sources are down at 11-27 and no labels are recorded within the week before it: the
        # universe of 11-27 is not recorded (a universe the encoder can never label would refuse every later
        # stream), the file is written without its snapshot, and the next morning writes its own.
        def down(url):
            raise tidata.massive.MarketDataError("HTTP 503 from a sector source")
        client = FakeMassive({D(2026, 11, 27): bars_for(D(2026, 11, 27))})
        job = MomentumJob(self.tmp.name, data=self.data, client=client, ranker=FakeRanker(), sectors_get=down)
        last_firing = dt.datetime(2026, 11, 30, 8, 15, tzinfo=nyse_calendar.NEW_YORK)
        status, _, err = job.main(last_firing, "--backfill-from", "2026-11-23")
        self.assertEqual(status, 0, err)
        self.assertIn("no universe is recorded for", err)
        self.assertIn("2026-11-27", err)
        self.assertFalse((self.data / "universe_2026-11-27.json").exists())
        self.assertNotIn("market.universe.snapshot", {e["type"] for e in job.envelopes(D(2026, 11, 27))})
        self.assertEqual([c for c in client.calls if c[0] == "tickers"], [])
        job.sectors_get = sector_pages
        status, _, err = job.main(after_settle(D(2026, 11, 30)))
        self.assertEqual(status, 0, err)
        self.assertEqual([e["payload"]["session"] for e in job.envelopes(D(2026, 11, 30))
                          if e["type"] == "market.universe.snapshot"], ["2026-11-30"])
        manifest = json.loads((job.stream_dir() / "manifest.json").read_text())
        self.assertEqual([f["without_universe"] for f in manifest["files"]],
                         [["2026-11-23", "2026-11-24", "2026-11-25", "2026-11-27"], []])

    def test_a_missing_run_env_is_a_quiet_wait_naming_the_instance(self):
        job = MomentumJob(self.tmp.name, data=self.data)
        os.remove(job.log)
        status, out, err = job.main(after_settle(D(2026, 11, 27)), "--run-env",
                                    str(job.log.parent / "run.env"))
        self.assertEqual((status, out), (0, ""))
        self.assertIn("start the trad-ira engine once", err)

    def test_a_configuration_the_engine_did_not_log_is_refused(self):
        job = MomentumJob(self.tmp.name, data=self.data, ranker=FakeRanker())
        cfg = dict(job.cfg, momentum=dict(job.cfg["momentum"], universe_size=3))
        job.config.write_text(json.dumps(cfg))
        status, _, err = job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")
        self.assertEqual(status, 1)
        self.assertIn("not the one the engine's durable log", err)

    def test_a_bars_configuration_is_refused(self):
        cfg = momentum_config(universe_input="bars")
        job = MomentumJob(self.tmp.name, data=self.data, config=cfg, ranker=FakeRanker())
        status, _, err = job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")
        self.assertEqual(status, 1)
        self.assertIn("compact", err)

    def test_a_boost_log_is_refused_for_a_momentum_instance(self):
        job = MomentumJob(self.tmp.name, data=self.data, ranker=FakeRanker())
        boost = json.loads((REPO / "deploy/etc/engines/boost.config").read_text())
        write_momentum_log(job.log, boost)
        status, _, err = job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")
        self.assertEqual(status, 1)
        self.assertIn("momentum configuration", err)

    def test_the_boost_flags_and_the_momentum_flags_do_not_mix(self):
        job = MomentumJob(self.tmp.name, data=self.data)
        for argv in (job.argv()[2:],                                  # --instance without --engine momentum
                     [a for a in job.argv() if a != INSTANCE and a != "--instance"],   # momentum without --instance
                     ["--engine", "momentum", "--instance", "Boost!"] + job.argv()[4:]):
            with mock.patch("sys.stderr", io.StringIO()), self.assertRaises(SystemExit, msg=argv):
                tidata.parse(argv)


# A month whose last day is a holiday: Memorial Day 2027 falls on Monday 31
# May, so Friday 28 May is May's last session and the morning of Tuesday
# 1 June reads it (docs/live-trading/lessons-2026-10.md L2).
HOLIDAY_DAYS = nyse_calendar.sessions(D(2027, 5, 17), D(2027, 5, 28))


def holiday_bars(day):
    i = HOLIDAY_DAYS.index(day)

    def bar(t, c, v):
        return {"T": t, "o": c, "h": c + 1, "l": c - 1, "c": c, "v": v, "t": 0}
    return [bar("SPY", 600.0 + i, 1000), bar("IEF", 95.0, 5000), bar("AAA", 20.0 + i / 10, 3000),
            bar("BBB", 50.0 - i / 10, 2000), bar("CCC", 30.0, 10)]


class HolidayMonthEndTest(unittest.TestCase):
    """The Go ranker decides from the calendar: May 2027's last session is Friday the 28th (Memorial Day is
    the 31st), so the job's firing on the morning of 1 June ranks it as the decision session and writes its
    histories before the open."""

    @classmethod
    def setUpClass(cls):
        cls.bin = tempfile.TemporaryDirectory()
        cls.ranker = Path(cls.bin.name) / "universe-rank"
        subprocess.run(["go", "build", "-o", str(cls.ranker), "./cmd/universe-rank"], cwd=REPO, check=True)

    @classmethod
    def tearDownClass(cls):
        cls.bin.cleanup()

    def test_the_morning_after_a_holiday_month_end_writes_its_histories(self):
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            for day in HOLIDAY_DAYS[:-1]:
                record.write(str(data), {"kind": "grouped", "source": "massive", "session": day.isoformat(),
                                         "fetched_at": "{}T23:30:00Z".format(day), "adjusted": False,
                                         "bars": holiday_bars(day)})
            for kind in ("splits", "dividends"):
                record.write(str(data), {"kind": kind, "source": "massive", "session": "2027-05-27",
                                         "from": "2027-05-01", "fetched_at": "2027-05-27T23:30:00Z", "rows": []})
            client = FakeMassive({HOLIDAY_DAYS[-1]: holiday_bars(HOLIDAY_DAYS[-1])})
            job = MomentumJob(root, data=data, client=client)
            warm = HOLIDAY_DAYS[-2]
            status, _, err = job.main(after_settle(warm), "--backfill-from", HOLIDAY_DAYS[0].isoformat(),
                                      ranker_path=str(self.ranker))
            self.assertEqual(status, 0, err)
            self.assertEqual([e for e in job.envelopes(warm) if e["type"] == "market.close.history"], [])
            tuesday = dt.datetime(2027, 6, 1, 6, 15, tzinfo=nyse_calendar.NEW_YORK)
            status, out, err = job.main(tuesday, ranker_path=str(self.ranker))
            self.assertEqual(status, 0, err)
            self.assertEqual(out.strip(), str(job.path(D(2027, 5, 28))))
            stream = job.envelopes(D(2027, 5, 28))
            calendar = stream[0]["payload"]["sessions"]
            self.assertEqual(calendar[-2:], ["2027-05-28", "2027-06-01"])
            histories = [e["payload"] for e in stream if e["type"] == "market.close.history"]
            # The two largest common stocks by dollar volume; each history reaches back to the base.
            self.assertEqual([h["instrument_id"] for h in histories], ["AAA", "BBB"])
            self.assertEqual({h["session"] for h in histories}, {"2027-05-28"})
            self.assertEqual([c["session"] for c in histories[0]["closes"]], [d.isoformat() for d in HOLIDAY_DAYS])


class TemplateUnitTest(unittest.TestCase):
    """ti-data@<instance>.service runs one momentum engine's job, failing loudly and paging as Boost's does;
    ti-data@.timer fires it on Boost's mornings, a few minutes after Boost's job."""

    def setUp(self):
        self.service = (REPO / "deploy/systemd/ti-data@.service").read_text()
        self.timer = (REPO / "deploy/systemd/ti-data@.timer").read_text()

    def test_the_instance_runs_the_momentum_job_for_its_own_engine(self):
        start = re.search(r"(?m)^ExecStart=(.*)$", self.service).group(1)
        for part in ("/opt/trend-investing/bin/ti-data", "--engine momentum", "--instance %i",
                     "--ranker /opt/trend-investing/bin/universe-rank",
                     "--run-env /var/lib/trend-investing/engines/%i/run.env",
                     "--config /etc/trend-investing/engines/%i.config",
                     "--engine-log /var/lib/trend-investing/engines/%i/${TI_RUN}.log",
                     "--secrets-file %d/secrets", "--settle-minutes 60"):
            self.assertIn(part, start)
        self.assertRegex(self.service, r"(?m)^EnvironmentFile=-/var/lib/trend-investing/engines/%i/run\.env$")
        self.assertRegex(self.service, r"(?m)^ReadOnlyPaths=/var/lib/trend-investing/engines/%i$")

    def test_it_fails_loudly_and_pages_as_boosts_job_does(self):
        boost = (REPO / "deploy/systemd/ti-data.service").read_text()
        for rule in (r"(?m)^OnFailure=ti-unit-failed@%n\.service$", r"(?m)^TimeoutStartSec=1h$",
                     r"(?m)^AssertPathExists=/run/trend-investing/secrets/secrets\.env$",
                     r"(?m)^LoadCredential=secrets:/run/trend-investing/secrets/secrets\.env$",
                     r"(?m)^User=ti-trader$", r"(?m)^ProtectSystem=strict$",
                     r"(?m)^ReadWritePaths=/var/lib/trend-investing/data$"):
            self.assertRegex(boost, rule)
            self.assertRegex(self.service, rule)
        self.assertNotRegex(self.service, r"(?m)^Condition")
        self.assertNotRegex(self.service, r"(?m)^ExecStartPre=")
        self.assertNotIn("TI_ADAPTER_CLIENT_ID", self.service)
        inaccessible = re.search(r"(?m)^InaccessiblePaths=.*$", boost).group(0)
        self.assertIn(inaccessible, self.service)

    def firings(self, text):
        when = re.search(r"^OnCalendar=Mon\.\.Fri ([0-9,]+):(\d\d) America/New_York$", text, re.M)
        self.assertIsNotNone(when, "the timer must fire Mon..Fri at New York times")
        return [(int(h), int(when.group(2))) for h in when.group(1).split(",")]

    def test_the_timer_fires_a_few_minutes_after_each_of_boosts_firings(self):
        self.assertRegex(self.timer, r"(?m)^OnCalendar=Mon\.\.Fri 6,7,8:15 America/New_York$")
        self.assertRegex(self.timer, r"(?m)^Persistent=true$")
        self.assertRegex(self.timer, r"(?m)^Unit=ti-data@%i\.service$")
        boost = self.firings((REPO / "deploy/systemd/ti-data.timer").read_text())
        mine = self.firings(self.timer)
        self.assertEqual(len(mine), len(boost))
        for (bh, bm), (h, m) in zip(boost, mine):
            self.assertTrue(0 < (h * 60 + m) - (bh * 60 + bm) <= 15, (bh, bm, h, m))
        quiet = (tidata.QUIET_BEFORE.hour, tidata.QUIET_BEFORE.minute)
        self.assertEqual([t < quiet for t in mine], [True] * (len(mine) - 1) + [False])
        self.assertLess(mine[-1], (8, 30), "the last firing leaves the job half an hour before the 09:00 cutoff")

    def test_each_morning_firing_reads_the_previous_session(self):
        settle = dt.timedelta(minutes=int(re.search(r"--settle-minutes (\d+)", self.service).group(1)))
        for hour, minute in self.firings(self.timer):
            day = D(2026, 1, 1)
            while day <= D(2027, 12, 31):
                if day.weekday() < 5:
                    fire = dt.datetime(day.year, day.month, day.day, hour, minute, tzinfo=nyse_calendar.NEW_YORK)
                    previous = day - dt.timedelta(days=1)
                    while not nyse_calendar.is_trading_day(previous):
                        previous -= dt.timedelta(days=1)
                    self.assertEqual(tidata.settled_session(fire, settle), previous, (day, hour, minute))
                day += dt.timedelta(days=1)

    def test_the_ranker_is_built_for_the_server_and_installed_where_the_unit_runs_it(self):
        build = (REPO / "deploy/build-engine.sh").read_text()
        self.assertIn("universe-rank:universe-rank", build)
        provision = (REPO / "deploy/provision.sh").read_text()
        self.assertRegex(provision, r"for b in ti-reporter ti-dashboard operator journal-query [a-z -]*universe-rank; do")
        self.assertRegex(provision, r"for b in ti-engine ti-reporter ti-dashboard operator journal-query [a-z -]*universe-rank; do")

    def test_provisioning_enables_the_iras_timer(self):
        provision = (REPO / "deploy/provision.sh").read_text()
        self.assertIn("systemctl enable --now ti-data.timer ti-data@trad-ira.timer ti-reporter@boost.timer ti-reporter@trad-ira.timer", provision)

    def test_every_reporter_resolves_the_iras_data_job(self):
        reporter = (REPO / "deploy/systemd/ti-reporter@.service").read_text()
        self.assertIn("-resolve-units ti-data.service,ti-data@trad-ira.service,", reporter)


class LogHoldingsRefusalTest(unittest.TestCase):
    """The holdings are read only from inputs whose versions the job reads, from a log whose record hash
    chain holds; anything else refuses the firing (AGENTS.md rule 5: fail closed)."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.log = Path(self.tmp.name) / "run-1.log"

    def tearDown(self):
        self.tmp.cleanup()

    def held(self, inputs, **kw):
        write_momentum_log(self.log, momentum_config(), inputs, **kw)
        return tidata.read_engine_log(str(self.log), engine="momentum").holdings

    def refused(self, inputs, **kw):
        write_momentum_log(self.log, momentum_config(), inputs, **kw)
        with self.assertRaises(tidata.Refused) as caught:
            tidata.read_engine_log(str(self.log), engine="momentum")
        return str(caught.exception)

    def test_the_versions_with_positions_are_read(self):
        # Version 3 is what the adapter writes today (Boost's book); version 4 adds the cash an IRA's buys are
        # budgeted from, which the momentum engine reads.
        self.assertEqual(self.held([snapshot("AAA"), snapshot("CCC", version=4)]), ("CCC",))
        self.assertEqual(self.held([snapshot("CCC", version=4), snapshot("AAA", version=3)]), ("AAA",))

    def test_a_snapshot_of_another_version_is_refused(self):
        for version in (2, 5):
            why = self.refused([snapshot("AAA"), snapshot("CCC", version=version)])
            self.assertIn("cannot read holdings: account.snapshot version {}".format(version), why)

    def test_fractional_fill_after_a_legacy_snapshot_is_held(self):
        self.assertEqual(self.held([snapshot("CCC"), ("execution.fill", 6, {
            "instrument_id": "DDD", "quantity": 35000, "quantity_scale": 10000})]), ("CCC", "DDD"))

    def test_a_fill_or_an_approval_of_a_version_the_job_does_not_read_is_refused(self):
        self.assertIn("execution.fill version 4", self.refused([("execution.fill", 4, {"instrument_id": "AAA"})]))
        approval = ("operator.run.approved", 7, {"starting_book": {"positions": []}})
        self.assertIn("operator.run.approved version 7", self.refused([approval]))

    def test_a_log_whose_record_hash_chain_breaks_is_refused_naming_it(self):
        why = self.refused([snapshot("AAA"), snapshot("CCC")], broken=3)
        self.assertIn(str(self.log), why)
        self.assertIn("hash chain", why)

    def test_a_firing_on_a_log_whose_chain_breaks_writes_nothing(self):
        data = Path(self.tmp.name) / "data"
        data.mkdir()
        build(str(data))
        job = MomentumJob(self.tmp.name, data=data, ranker=FakeRanker())
        self.assertEqual(job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")[0], 0)
        write_momentum_log(job.log, job.cfg, [snapshot("CCC")], broken=2)
        status, _, err = job.main(after_settle(D(2026, 11, 30)))
        self.assertEqual(status, 1)
        self.assertIn(str(job.log), err)
        self.assertFalse(job.path(D(2026, 11, 30)).exists())


class StreamChainTest(unittest.TestCase):
    """The run's files must form one unbroken chain from the warm-up to the newest; a missing file refuses
    the firing, whichever files the morning re-encodes."""

    def test_a_missing_file_between_two_others_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data))
            job = MomentumJob(root, data=data, ranker=FakeRanker())
            self.assertEqual(job.main(after_settle(D(2026, 11, 25)), "--backfill-from", "2026-11-23")[0], 0)
            for day in (D(2026, 11, 27), D(2026, 11, 30)):
                status, _, err = job.main(after_settle(day))
                self.assertEqual(status, 0, err)
            os.remove(job.path(D(2026, 11, 27)))
            # The 11-30 file is outside no action window, but the firing that finds it still sees the gap.
            status, _, err = job.main(after_settle(D(2026, 11, 30)))
            self.assertEqual(status, 1)
            self.assertIn("trad-ira-2026-11-27.jsonl is missing", err)


class RankerConfigurationTest(unittest.TestCase):
    """The ranker reads the configuration the job checked against the engine's log, not the file's path,
    which may change while the job runs."""

    def test_the_ranker_reads_the_checked_bytes_even_when_the_file_changes(self):
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data))
            os.remove(data / "grouped_2026-11-27.json")
            seen = []

            def go_ranker(argv, timeout=None):
                def rank(requests):
                    path = argv[argv.index("-config") + 1]
                    seen.append((path, Path(path).read_bytes()))
                    return FakeRanker()(requests)
                return rank

            class Changing(FakeMassive):
                def grouped(self, day):
                    job.config.write_text(json.dumps(momentum_config(universe_size=99)))
                    return super().grouped(day)
            job = MomentumJob(root, data=data, client=Changing({D(2026, 11, 27): bars_for(D(2026, 11, 27))}))
            checked = job.config.read_bytes()
            with mock.patch.object(tidata.encode, "go_ranker", go_ranker):
                status, _, err = job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23",
                                          ranker_path="/opt/trend-investing/bin/universe-rank")
            self.assertEqual(status, 0, err)
            self.assertTrue(seen)
            for path, raw in seen:
                self.assertNotEqual(path, str(job.config))
                self.assertEqual(raw, checked)


class ExclusionReportTest(unittest.TestCase):
    def test_a_member_left_out_for_bad_data_is_named_on_stderr(self):
        from marketdata.tests.test_encode import break_bar
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data))
            break_bar(str(data), "2026-11-27", "DDD")
            job = MomentumJob(root, data=data, ranker=FakeRanker())
            self.assertEqual(job.main(after_settle(D(2026, 11, 25)), "--backfill-from", "2026-11-23")[0], 0)
            status, _, err = job.main(after_settle(D(2026, 11, 27)))
            self.assertEqual(status, 0, err)
            self.assertIn("ti-data: excluded DDD from 2026-11-27 to 2026-11-27", err)


class SharedRecordingTest(unittest.TestCase):
    """Boost's job and the IRAs' may run at once (a Persistent catch-up after a boot, a start by hand, a long
    catch-up still running at the next timer): a recording another job writes between this job's check and
    its write is taken as recorded, and the firing carries on (record.py: a recording is written once)."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name) / "data"
        self.data.mkdir()
        build(str(self.data))
        os.remove(self.data / "grouped_2026-11-27.json")
        self.bars = bars_for(D(2026, 11, 27))

    def tearDown(self):
        self.tmp.cleanup()

    def other(self, kind, **extra):
        record.write(str(self.data), dict({"kind": kind, "source": "massive", "session": "2026-11-27",
                                           "fetched_at": "2026-11-27T22:10:00Z", "adjusted": False,
                                           "bars": self.bars}, **extra))

    def run_with(self, client, sectors_get=sector_pages):
        job = MomentumJob(self.tmp.name, data=self.data, client=client, ranker=FakeRanker(), sectors_get=sectors_get)
        status, _, err = job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")
        self.assertEqual(status, 0, err)
        return job

    def test_grouped_bars_another_job_records_first_are_used(self):
        test = self

        class Racing(FakeMassive):
            def grouped(self, day):
                test.other("grouped")
                return super().grouped(day)
        self.run_with(Racing({D(2026, 11, 27): self.bars}))
        self.assertEqual(record.read(str(self.data / "grouped_2026-11-27.json"))["fetched_at"], "2026-11-27T22:10:00Z")

    def test_a_universe_another_job_records_first_is_used(self):
        test = self

        class Racing(FakeMassive):
            def tickers(self, day, kind):
                test.other("universe", ticker_type="CS", tickers=list(TICKERS))
                return super().tickers(day, kind)
        job = self.run_with(Racing({D(2026, 11, 27): self.bars}))
        self.assertTrue(job.path(D(2026, 11, 27)).exists())

    def test_sector_labels_another_job_records_first_are_used(self):
        from marketdata import sectors as sector_module

        def racing(url):
            if url == sector_module.IWV_URL and not (self.data / "sectors-iwv_2026-11-27.json").exists():
                text, read = sector_pages(url)
                record.write(str(self.data), {"kind": "sectors-iwv", "source": sector_module.IWV_SOURCE,
                                              "session": "2026-11-27", "fetched_at": "2026-11-27T22:10:00Z",
                                              "url": read, "as_of": "Mar 26, 2026", "body": text})
            return sector_pages(url)
        job = self.run_with(FakeMassive({D(2026, 11, 27): self.bars}), sectors_get=racing)
        self.assertTrue((self.data / "sectors-nasdaq_2026-11-27.json").exists())
        self.assertIn("market.universe.snapshot", {e["type"] for e in job.envelopes(D(2026, 11, 27))})


class MissedMonthStartTest(unittest.TestCase):
    """A month's first session whose morning firings all failed (a 403 that lasted past 08:00): the labels of
    the month's last session were recorded that morning before any request to Massive, so the next morning's
    catch-up records that session's universe from its date and the file carries the month's decision."""

    def test_the_catch_up_after_a_missed_month_start_writes_the_month_end_universe_and_histories(self):
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data), sector_session="2026-11-27")
            os.remove(data / "universe_2026-11-30.json")

            class Refusing(FakeMassive):
                def grouped(self, day):
                    if self.refuse:
                        self.calls.append(("grouped", day))
                        raise tidata.massive.MarketDataError("HTTP 403 from massive", status=403)
                    return super().grouped(day)
            client = Refusing({D(2026, 11, 30): bars_for(D(2026, 11, 30)), D(2026, 12, 1): bars_for(D(2026, 12, 1))})
            client.refuse = False
            job = MomentumJob(root, data=data, client=client, ranker=FakeRanker(members=("BBB", "AAA")))
            self.assertEqual(job.main(after_settle(D(2026, 11, 27)), "--backfill-from", "2026-11-23")[0], 0)
            client.refuse = True
            status, _, err = job.main(dt.datetime(2026, 12, 1, 8, 15, tzinfo=nyse_calendar.NEW_YORK))
            self.assertEqual(status, 1, err)
            self.assertTrue((data / "sectors-iwv_2026-11-30.json").exists())
            client.refuse = False
            status, out, err = job.main(dt.datetime(2026, 12, 2, 6, 15, tzinfo=nyse_calendar.NEW_YORK))
            self.assertEqual(status, 0, err)
            stream = job.envelopes(D(2026, 12, 1))
            self.assertEqual([e["payload"]["session"] for e in stream if e["type"] == "market.universe.snapshot"],
                             ["2026-11-30", "2026-12-01"])
            self.assertEqual(sorted({e["payload"]["session"] for e in stream if e["type"] == "market.close.history"}),
                             ["2026-11-30"])
            self.assertIn(("tickers", D(2026, 11, 30), "CS"), client.calls)


def warmed(root, data=None, **kw):
    """A MomentumJob on the encoder's small fixture, warmed up through 11-25 and then 11-27 and 11-30."""
    data = Path(data or Path(root) / "data")
    data.mkdir(exist_ok=True)
    build(str(data))
    job = MomentumJob(root, data=data, ranker=FakeRanker(), **kw)
    status, _, err = job.main(after_settle(D(2026, 11, 25)), "--backfill-from", "2026-11-23")
    assert status == 0, err
    for day in (D(2026, 11, 27), D(2026, 11, 30)):
        status, _, err = job.main(after_settle(day))
        assert status == 0, err
    return job


def sha256_of(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


class ManifestTest(unittest.TestCase):
    """The job keeps a manifest of every file it published for the run (its sessions, length and digest),
    written whole beside them, and every firing checks the folder against it: a file truncated, changed or
    gone, the newest included, stops the job; it is never written again from today's holdings."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.job = warmed(self.tmp.name)
        self.manifest = self.job.stream_dir() / "manifest.json"

    def tearDown(self):
        self.tmp.cleanup()

    def test_each_published_file_is_in_the_manifest(self):
        manifest = json.loads(self.manifest.read_text())
        self.assertEqual(manifest["base"], "2026-11-23")
        self.assertEqual([(f["name"], f["first"], f["last"]) for f in manifest["files"]],
                         [("trad-ira-2026-11-25.jsonl", "2026-11-23", "2026-11-25"),
                          ("trad-ira-2026-11-27.jsonl", "2026-11-27", "2026-11-27"),
                          ("trad-ira-2026-11-30.jsonl", "2026-11-30", "2026-11-30")])
        for f in manifest["files"]:
            path = self.job.stream_dir() / f["name"]
            self.assertEqual((f["bytes"], f["sha256"]), (path.stat().st_size, sha256_of(path)))
        self.assertTrue(manifest["actions"])

    def refused(self, now, *words):
        status, _, err = self.job.main(now)
        self.assertEqual(status, 1, err)
        for w in words:
            self.assertIn(w, err)
        return err

    def test_an_older_file_cut_after_its_calendar_is_refused(self):
        path = self.job.path(D(2026, 11, 25))
        path.write_text(path.read_text().splitlines()[0] + "\n")
        self.refused(after_settle(D(2026, 11, 30)), str(path), "changed since it was published")

    def test_a_missing_newest_file_is_refused_never_written_again(self):
        os.remove(self.job.path(D(2026, 11, 30)))
        self.refused(after_settle(D(2026, 11, 30)), "trad-ira-2026-11-30.jsonl", "missing")
        self.assertFalse(self.job.path(D(2026, 11, 30)).exists())

    def test_a_file_published_but_not_yet_in_the_manifest_is_taken_in_when_it_reencodes(self):
        # A stop between the file's publication and the manifest's: the file is checked like any written one.
        manifest = json.loads(self.manifest.read_text())
        manifest["files"].pop()
        self.manifest.write_text(json.dumps(manifest))
        status, out, err = self.job.main(after_settle(D(2026, 11, 30)))
        self.assertEqual(status, 0, err)
        self.assertEqual(out.strip(), str(self.job.path(D(2026, 11, 30))))
        self.assertEqual(len(json.loads(self.manifest.read_text())["files"]), 3)

    def test_files_without_a_manifest_are_refused(self):
        os.remove(self.manifest)
        self.refused(after_settle(D(2026, 11, 30)), "manifest")


class ActionRecordingChangeTest(unittest.TestCase):
    """A split or dividend recording that appears after a file was published (another account's backfill
    records a window the run had not seen), dated inside an older file, makes that file and every later one
    be encoded again, however old: the files are checked against the action recordings their manifest says
    they were checked with, not only against the morning's own window."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.job = warmed(self.tmp.name)
        self.job.client = FakeMassive({D(2026, 12, 1): bars_for(D(2026, 12, 1))})

    def tearDown(self):
        self.tmp.cleanup()

    def add_dividends(self, rows):
        record.write(str(self.job.data), {"kind": "dividends", "source": "massive", "session": "2026-11-24",
                                          "from": "2026-11-20", "fetched_at": "2026-11-28T12:00:00Z", "rows": rows})

    def test_a_new_recording_reaching_an_older_file_has_it_encoded_again(self):
        self.add_dividends([])
        checked = []
        original = tidata._verify_momentum

        def spy(files, *args, **kw):
            checked.extend(os.path.basename(f[2]) for f in files)
            return original(files, *args, **kw)
        with mock.patch.object(tidata, "ACTION_OVERLAP", dt.timedelta(days=1)), \
                mock.patch.object(tidata, "_verify_momentum", spy):
            status, _, err = self.job.main(after_settle(D(2026, 12, 1)))
        self.assertEqual(status, 0, err)
        self.assertIn("trad-ira-2026-11-25.jsonl", checked)

    def test_a_new_dividend_inside_an_older_file_stops_the_job_naming_it(self):
        self.add_dividends([{"id": "V-late", "ticker": "IEF", "ex_dividend_date": "2026-11-24", "cash_amount": 0.5}])
        with mock.patch.object(tidata, "ACTION_OVERLAP", dt.timedelta(days=1)):
            status, _, err = self.job.main(after_settle(D(2026, 12, 1)))
        self.assertEqual(status, 1)
        self.assertIn("trad-ira-2026-11-25.jsonl differs", err)

    def test_more_files_than_one_firing_checks_refuse_until_checked_by_hand(self):
        self.add_dividends([])
        with mock.patch.object(tidata, "ACTION_OVERLAP", dt.timedelta(days=1)), \
                mock.patch.object(tidata, "MAX_REENCODED", 1):
            status, _, err = self.job.main(after_settle(D(2026, 12, 1)))
            self.assertEqual(status, 1)
            self.assertIn("--reencode-all", err)
            status, _, err = self.job.main(after_settle(D(2026, 12, 1)), "--reencode-all")
        self.assertEqual(status, 0, err)


class RunLockTest(unittest.TestCase):
    def test_a_firing_while_another_holds_the_instances_lock_writes_nothing_and_waits_for_it(self):
        import fcntl
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data))
            job = MomentumJob(root, data=data, ranker=FakeRanker())
            (job.streams / INSTANCE).mkdir(parents=True)
            with open(job.streams / INSTANCE / ".ti-data.lock", "a") as held:
                fcntl.flock(held, fcntl.LOCK_EX)
                status, out, err = job.main(after_settle(D(2026, 11, 25)), "--backfill-from", "2026-11-23")
            self.assertEqual((status, out), (0, ""), err)
            self.assertIn("another run of the trad-ira data job holds its lock", err)
            self.assertFalse(job.stream_dir().exists())


class LabelsNotYetTest(unittest.TestCase):
    """The latest session's labels can still be fetched until its next close, so before the last morning
    firing a file that would lack that session's universe is not written: the firing waits and the next asks
    again. At the last firing it is written without it, a decision on it is withheld, and the manifest and
    every later firing that finds the file name the session."""

    def test_before_the_last_firing_the_job_waits_for_the_labels(self):
        def down(url):
            raise tidata.massive.MarketDataError("HTTP 503 from a sector source")
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data), sector_session=None)
            os.remove(data / "universe_2026-11-30.json")
            client = FakeMassive({D(2026, 11, 30): bars_for(D(2026, 11, 30))})
            job = MomentumJob(root, data=data, client=client, ranker=FakeRanker(), sectors_get=down)
            warm = dt.datetime(2026, 11, 30, 8, 15, tzinfo=nyse_calendar.NEW_YORK)   # the last firing for 11-27
            status, _, err = job.main(warm, "--backfill-from", "2026-11-23")
            self.assertEqual(status, 0, err)
            status, out, err = job.main(dt.datetime(2026, 12, 1, 7, 15, tzinfo=nyse_calendar.NEW_YORK))
            self.assertEqual((status, out), (0, ""), err)
            self.assertIn("the sector labels of 2026-11-30 are not recorded yet", err)
            self.assertFalse(job.path(D(2026, 11, 30)).exists())
            status, _, err = job.main(dt.datetime(2026, 12, 1, 8, 15, tzinfo=nyse_calendar.NEW_YORK))
            self.assertEqual(status, 0, err)
            self.assertNotIn("market.universe.snapshot", {e["type"] for e in job.envelopes(D(2026, 11, 30))})
            status, _, err = job.main(dt.datetime(2026, 12, 1, 8, 30, tzinfo=nyse_calendar.NEW_YORK))
            self.assertEqual(status, 0, err)
            self.assertIn("written without the universe of 2026-11-30", err)


class PublishedWithoutUniverseTest(unittest.TestCase):
    def test_another_instances_late_labels_and_universe_do_not_change_the_published_file(self):
        def down(url):
            raise tidata.massive.MarketDataError("HTTP 503 from a sector source")
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data), sector_session=None)
            os.remove(data / "universe_2026-11-30.json")
            first = MomentumJob(root, data=data, ranker=FakeRanker(), sectors_get=down,
                                client=FakeMassive({day: bars_for(day) for day in (D(2026, 11, 30), D(2026, 12, 1))}))
            firing = dt.datetime(2026, 12, 1, 8, 15, tzinfo=nyse_calendar.NEW_YORK)
            status, _, err = first.main(firing, "--backfill-from", "2026-11-23")
            self.assertEqual(status, 0, err)
            path = first.path(D(2026, 11, 30))
            before = path.read_bytes()
            self.assertNotIn("market.universe.snapshot", {e["type"] for e in first.envelopes(D(2026, 11, 30))})
            second_root = Path(root) / "second"
            second_root.mkdir()
            second = MomentumJob(second_root, data=data, ranker=FakeRanker(), instance="roth-ira")
            status, _, err = second.main(firing + dt.timedelta(seconds=5), "--backfill-from", "2026-11-23")
            self.assertEqual(status, 0, err)
            self.assertTrue((data / "sectors-iwv_2026-11-30.json").exists())
            self.assertTrue((data / "sectors-nasdaq_2026-11-30.json").exists())
            self.assertTrue((data / "universe_2026-11-30.json").exists())
            self.assertIn("market.universe.snapshot", {e["type"] for e in second.envelopes(D(2026, 11, 30))})
            status, _, err = first.main(firing + dt.timedelta(minutes=15))
            self.assertEqual(status, 0, err)
            self.assertIn("missing-universe", err)
            self.assertEqual(path.read_bytes(), before)
            # ADR 0027: action-triggered verification must preserve the same published inputs.
            record.write(str(data), {"kind": "dividends", "source": "massive", "session": "2026-11-30",
                                     "from": "2026-11-20", "fetched_at": "2026-12-01T14:00:00Z", "rows": []})
            status, _, err = first.main(after_settle(D(2026, 12, 1)))
            self.assertEqual(status, 0, err)
            self.assertEqual(path.read_bytes(), before)

    def test_a_published_universe_is_still_verified(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            path = job.path(D(2026, 11, 30))
            before = path.read_bytes()
            manifest = json.loads((job.stream_dir() / "manifest.json").read_text())
            self.assertEqual(manifest["files"][-1]["without_universe"], [])
            status, _, err = job.main(after_settle(D(2026, 11, 30)))
            self.assertEqual(status, 0, err)
            self.assertEqual(path.read_bytes(), before)
            record.write(str(job.data), {"kind": "grouped", "source": "massive", "session": "2026-11-30",
                                         "fetched_at": "2026-11-30T23:30:00Z", "adjusted": False,
                                         "bars": bars_for(D(2026, 11, 30))})
            os.remove(job.data / "universe_2026-11-30.json")
            status, _, err = job.main(after_settle(D(2026, 11, 30)))
            self.assertEqual(status, 1, err)
            self.assertIn("differs", err)

    def test_without_universe_must_match_the_sha256_checked_stream(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            path = job.stream_dir() / "manifest.json"
            original = json.loads(path.read_text())
            for index, without in ((-1, ["2026-11-30"]), (0, ["2026-11-23"]), (0, [])):
                with self.subTest(index=index, without=without):
                    manifest = json.loads(json.dumps(original))
                    entry = manifest["files"][index]
                    entry["without_universe"] = without
                    path.write_text(json.dumps(manifest))
                    status, _, err = job.main(after_settle(D(2026, 11, 30)))
                    self.assertEqual(status, 1, err)
                    self.assertIn("without_universe", err)
                    self.assertIn("sha256", err)


class InheritedBookTest(unittest.TestCase):
    """A fresh run approved without a starting book takes the previous run's book (ADR 0027): its holdings
    are the previous run's, read from the log its approval names, through the same reader, never nothing."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def approval(self, previous, final_hash):
        return ("operator.run.approved", 6, {"approved_by": "owner", "previous_log": previous,
                                             "previous_log_final_hash": final_hash})

    def test_the_previous_runs_last_snapshot_is_the_book(self):
        write_momentum_log(self.dir / "run-0.log", momentum_config(), [snapshot("CCC"), fill("DDD")])
        final = tidata.engine_log.read(str(self.dir / "run-0.log")).final_hash
        write_momentum_log(self.dir / "run-1.log", momentum_config(), [self.approval("run-0.log", final), fill("AAA")])
        self.assertEqual(tidata.read_engine_log(str(self.dir / "run-1.log"), engine="momentum").holdings,
                         ("AAA", "CCC", "DDD"))

    def test_a_previous_log_that_is_not_the_one_approved_is_refused(self):
        write_momentum_log(self.dir / "run-0.log", momentum_config(), [snapshot("CCC")])
        write_momentum_log(self.dir / "run-1.log", momentum_config(), [self.approval("run-0.log", "sha256:" + "0" * 64)])
        with self.assertRaises(tidata.Refused) as caught:
            tidata.read_engine_log(str(self.dir / "run-1.log"), engine="momentum")
        self.assertIn("run-0.log", str(caught.exception))
        write_momentum_log(self.dir / "run-1.log", momentum_config(), [self.approval("run-9.log", "sha256:" + "0" * 64)])
        with self.assertRaises(tidata.Refused):
            tidata.read_engine_log(str(self.dir / "run-1.log"), engine="momentum")


class SharedRecordingCheckTest(unittest.TestCase):
    def test_a_grouped_recording_another_job_wrote_first_is_checked_for_bars(self):
        from marketdata import history
        with tempfile.TemporaryDirectory() as d:
            class Racing:
                def grouped(self, day):
                    record.write(d, {"kind": "grouped", "source": "massive", "session": day.isoformat(),
                                     "fetched_at": "2026-11-27T22:10:00Z", "adjusted": False, "bars": []})
                    return {"results": bars_for(day)}
            with self.assertRaises(history.Gap):
                history.backfill(Racing(), d, D(2026, 11, 27), 1, now=lambda: after_settle(D(2026, 11, 27)))

    def test_a_splits_recording_another_job_wrote_first_does_not_skip_the_dividends(self):
        from marketdata import history
        with tempfile.TemporaryDirectory() as d:
            window = {"source": "massive", "session": "2026-11-27", "from": "2026-11-13",
                      "fetched_at": "2026-11-27T22:10:00Z", "rows": []}

            class Racing:
                def splits(self, first, last):
                    record.write(d, dict(window, kind="splits"))
                    return [{"results": []}]

                def all_dividends(self, first, last):
                    return [{"results": []}]
            paths = history.record_actions(Racing(), D(2026, 11, 13), D(2026, 11, 27), d,
                                           now=lambda: after_settle(D(2026, 11, 27)))
            self.assertTrue(all(os.path.exists(p) for p in paths))
            self.assertTrue(os.path.exists(os.path.join(d, "dividends_2026-11-13_2026-11-27.json")))



class RepeatFiringTest(unittest.TestCase):
    def test_a_retraction_recorded_inside_a_published_file_stops_the_firing_that_finds_it(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            encode.record_retraction(str(job.data), D(2026, 11, 24), "NYSE closed (rehearsed)",
                                     recorded_at="2026-11-28T12:00:00Z")
            status, _, err = job.main(after_settle(D(2026, 11, 25), minutes=600))
            self.assertEqual(status, 1, err)
            self.assertIn(str(job.path(D(2026, 11, 25))), err)
            self.assertIn("a person", err)


class ManifestEntryTest(unittest.TestCase):
    """A manifest entry is checked against the file it names: its name's session, and the first and last
    sessions the file itself holds."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.job = warmed(self.tmp.name)
        self.manifest = self.job.stream_dir() / "manifest.json"

    def tearDown(self):
        self.tmp.cleanup()

    def edit(self, change):
        manifest = json.loads(self.manifest.read_text())
        change(manifest)
        self.manifest.write_text(json.dumps(manifest))

    def test_a_last_session_the_file_does_not_end_at_is_refused(self):
        self.edit(lambda m: m["files"][-1].update(last="2026-12-01"))
        status, _, err = self.job.main(after_settle(D(2026, 12, 1)))
        self.assertEqual(status, 1, err)
        self.assertIn("trad-ira-2026-11-30.jsonl", err)
        self.assertFalse(self.job.path(D(2026, 12, 2)).exists())

    def test_a_manifest_missing_a_field_is_refused_plainly(self):
        self.edit(lambda m: m["files"][0].pop("without_universe"))
        status, _, err = self.job.main(after_settle(D(2026, 11, 30)))
        self.assertEqual(status, 1, err)
        self.assertIn("cannot be read", err)
        self.assertIn("without_universe", err)
        self.assertNotIn("KeyError", err)


class TakeInRefusalTest(unittest.TestCase):
    """The take-in rule lists only one newest unlisted file, and only one that encodes again byte for byte."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.job = warmed(self.tmp.name)
        self.manifest = self.job.stream_dir() / "manifest.json"

    def tearDown(self):
        self.tmp.cleanup()

    def refused(self, *words):
        status, _, err = self.job.main(after_settle(D(2026, 11, 30), minutes=600))
        self.assertEqual(status, 1, err)
        for w in words:
            self.assertIn(w, err)

    def test_an_unlisted_newest_file_that_differs_is_refused_and_stays_unlisted(self):
        manifest = json.loads(self.manifest.read_text())
        manifest["files"].pop()
        self.manifest.write_text(json.dumps(manifest))
        with open(self.job.path(D(2026, 11, 30)), "a") as f:
            f.write("\n")
        self.refused("differs")
        self.assertEqual(len(json.loads(self.manifest.read_text())["files"]), 2)

    def test_two_unlisted_files_are_refused(self):
        shutil.copy(self.job.path(D(2026, 11, 30)), self.job.stream_dir() / "trad-ira-2026-12-01.jsonl")
        shutil.copy(self.job.path(D(2026, 11, 30)), self.job.stream_dir() / "trad-ira-2026-12-02.jsonl")
        self.refused("does not list")

    def test_an_unlisted_file_older_than_the_newest_listed_is_refused(self):
        shutil.copy(self.job.path(D(2026, 11, 27)), self.job.stream_dir() / "trad-ira-2026-11-26.jsonl")
        self.refused("does not list", "trad-ira-2026-11-26.jsonl")


class ManifestRecoveryTest(unittest.TestCase):
    """The manifest is written before the run's first file, so a stop between the two leaves a file the next
    firing takes in by the same rule; a manifest lost or damaged is rebuilt only by hand
    (--rebuild-manifest), every file taken in by that rule in chain order."""

    def test_an_unlisted_file_keeps_its_omissions_after_another_instance_records_a_universe(self):
        self.recover_without_universe(rebuild=False)

    def test_a_rebuilt_manifest_keeps_omissions_after_another_instance_records_a_universe(self):
        self.recover_without_universe(rebuild=True)

    def recover_without_universe(self, rebuild):
        def down(url):
            raise tidata.massive.MarketDataError("HTTP 503 from a sector source")

        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data), sector_session=None)
            os.remove(data / "universe_2026-11-30.json")
            job = MomentumJob(root, data=data, ranker=FakeRanker(), sectors_get=down,
                              client=FakeMassive({D(2026, 11, 30): bars_for(D(2026, 11, 30))}))
            firing = dt.datetime(2026, 12, 1, 8, 15, tzinfo=nyse_calendar.NEW_YORK)
            status, _, err = job.main(firing, "--backfill-from", "2026-11-23")
            self.assertEqual(status, 0, err)
            path = job.path(D(2026, 11, 30))
            before = path.read_bytes()
            manifest_path = job.stream_dir() / "manifest.json"
            published = json.loads(manifest_path.read_text())
            self.assertIn("2026-11-30", published["files"][-1]["without_universe"])
            if rebuild:
                manifest_path.unlink()
            else:
                unlisted = json.loads(json.dumps(published))
                unlisted["files"].pop()
                manifest_path.write_text(json.dumps(unlisted))
            second_root = Path(root) / "second"
            second_root.mkdir()
            second = MomentumJob(second_root, data=data, ranker=FakeRanker(), instance="roth-ira")
            status, _, err = second.main(firing + dt.timedelta(seconds=5), "--backfill-from", "2026-11-23")
            self.assertEqual(status, 0, err)
            self.assertTrue((data / "universe_2026-11-30.json").exists())

            status, _, err = job.main(firing + dt.timedelta(minutes=15),
                                      *(("--rebuild-manifest",) if rebuild else ()))
            self.assertEqual(status, 0, err)
            recovered = json.loads(manifest_path.read_text())
            self.assertEqual(recovered["files"], published["files"])
            self.assertEqual(path.read_bytes(), before)
            # ADR 0027: a present manifest remains authoritative and must agree with the stream.
            recovered["files"][-1]["without_universe"] = []
            manifest_path.write_text(json.dumps(recovered))
            status, _, err = job.main(firing + dt.timedelta(minutes=20))
            self.assertEqual(status, 1, err)
            self.assertIn("manifest without_universe does not match its sha256-checked stream", err)
            self.assertEqual(path.read_bytes(), before)

    def test_a_stop_after_the_first_file_is_linked_is_taken_in_by_the_next_firing(self):
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data))
            job = MomentumJob(root, data=data, ranker=FakeRanker())
            original = tidata.encode.write_stream

            def stopped(path, envelopes):
                original(path, envelopes)
                raise RuntimeError("the job stopped here")
            with mock.patch.object(tidata.encode, "write_stream", stopped):
                status, _, _ = job.main(after_settle(D(2026, 11, 25)), "--backfill-from", "2026-11-23")
            self.assertEqual(status, 1)
            self.assertTrue(job.path(D(2026, 11, 25)).exists())
            status, out, err = job.main(after_settle(D(2026, 11, 25), minutes=600), "--backfill-from", "2026-11-23")
            self.assertEqual(status, 0, err)
            self.assertEqual(out.strip(), str(job.path(D(2026, 11, 25))))
            manifest = json.loads((job.stream_dir() / "manifest.json").read_text())
            self.assertEqual([f["name"] for f in manifest["files"]], ["trad-ira-2026-11-25.jsonl"])

    def test_a_lost_manifest_is_refused_naming_the_remedy_and_rebuilt_by_hand(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            manifest = job.stream_dir() / "manifest.json"
            before = json.loads(manifest.read_text())
            os.remove(manifest)
            status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600))
            self.assertEqual(status, 1)
            self.assertIn("--rebuild-manifest", err)
            manifest.write_text("{not json")
            status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600))
            self.assertEqual(status, 1)
            self.assertIn("--rebuild-manifest", err)
            status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600), "--rebuild-manifest")
            self.assertEqual(status, 0, err)
            self.assertEqual([f["sha256"] for f in json.loads(manifest.read_text())["files"]],
                             [f["sha256"] for f in before["files"]])

    def test_a_rebuild_refuses_a_file_that_differs(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            os.remove(job.stream_dir() / "manifest.json")
            with open(job.path(D(2026, 11, 27)), "a") as f:
                f.write("\n")
            status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600), "--rebuild-manifest")
            self.assertEqual(status, 1)
            self.assertIn("trad-ira-2026-11-27.jsonl differs", err)
            self.assertFalse((job.stream_dir() / "manifest.json").exists())


class LastFiringLockTest(unittest.TestCase):
    def test_at_the_last_morning_firing_a_held_lock_fails_the_firing(self):
        import fcntl
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            with open(job.streams / INSTANCE / ".ti-data.lock", "a") as held:
                fcntl.flock(held, fcntl.LOCK_EX)
                status, _, err = job.main(dt.datetime(2026, 12, 1, 8, 15, tzinfo=nyse_calendar.NEW_YORK))
            self.assertEqual(status, 1, err)
            self.assertIn("holds its lock", err)
            self.assertIn("last morning firing", err)



class FileEndsEarlyTest(unittest.TestCase):
    def test_a_listed_file_that_ends_before_its_last_session_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            path = job.path(D(2026, 11, 25))
            lines = path.read_text().splitlines(keepends=True)
            cut = max(i for i, line in enumerate(lines)
                      if '"type":"market.session.closed"' in line and '"2026-11-24T' in line)
            path.write_text("".join(lines[:cut + 1]))
            manifest_path = job.stream_dir() / "manifest.json"
            manifest = json.loads(manifest_path.read_text())
            manifest["files"][0].update(bytes=path.stat().st_size, sha256=sha256_of(path))
            manifest_path.write_text(json.dumps(manifest))
            status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600))
            self.assertEqual(status, 1, err)
            self.assertIn("ends at 2026-11-24, not at the 2026-11-25 its manifest lists", err)


class RebuildGuardTest(unittest.TestCase):
    """--rebuild-manifest is for a lost or unreadable manifest only, and acknowledges only the split and
    dividend recordings it encoded the files against."""

    def test_a_rebuild_with_a_readable_manifest_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            manifest = job.stream_dir() / "manifest.json"
            before = manifest.read_bytes()
            os.remove(job.path(D(2026, 11, 30)))
            status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600), "--rebuild-manifest")
            self.assertEqual(status, 1, err)
            self.assertIn("can be read", err)
            self.assertEqual(manifest.read_bytes(), before)

    def test_a_recording_written_during_a_rebuild_is_never_acknowledged(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            os.remove(job.stream_dir() / "manifest.json")
            original = tidata._verify_momentum

            def racing(files, *args, **kw):
                name = "dividends_2026-11-20_{}.json".format(os.path.basename(files[0][2])[9:19])
                if not (job.data / name).exists():
                    record.write(str(job.data), {"kind": "dividends", "source": "massive",
                                                 "session": os.path.basename(files[0][2])[9:19], "from": "2026-11-20",
                                                 "fetched_at": "2026-12-01T06:05:00Z", "rows": []})
                return original(files, *args, **kw)
            with mock.patch.object(tidata, "_verify_momentum", racing):
                status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600), "--rebuild-manifest")
            self.assertEqual(status, 1, err)
            self.assertIn("changed during the rebuild", err)
            self.assertFalse((job.stream_dir() / "manifest.json").exists())


class RetractedLatestSessionTest(unittest.TestCase):
    """A file is never named after a session the exchange cancelled: when the latest settled session is
    retracted, the job's session is the last real close before it, and the next file begins after it."""

    def test_a_retracted_latest_session_writes_nothing_new_and_the_next_session_follows(self):
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data))
            os.remove(data / "universe_2026-11-30.json")
            client = FakeMassive({D(2026, 12, 1): bars_for(D(2026, 12, 1))})
            job = MomentumJob(root, data=data, client=client, ranker=FakeRanker())
            self.assertEqual(job.main(after_settle(D(2026, 11, 25)), "--backfill-from", "2026-11-23")[0], 0)
            self.assertEqual(job.main(after_settle(D(2026, 11, 27)))[0], 0)
            encode.record_retraction(str(data), D(2026, 11, 30), "NYSE closed (rehearsed)",
                                     recorded_at="2026-11-30T15:00:00Z")
            for _ in range(2):
                status, out, err = job.main(after_settle(D(2026, 11, 30)))
                self.assertEqual(status, 0, err)
                self.assertEqual(out.strip(), str(job.path(D(2026, 11, 27))))
            self.assertNotIn(("grouped", D(2026, 11, 30)), client.calls)
            status, out, err = job.main(after_settle(D(2026, 12, 1)))
            self.assertEqual(status, 0, err)
            self.assertEqual(out.strip(), str(job.path(D(2026, 12, 1))))
            status, _, err = job.main(after_settle(D(2026, 12, 1), minutes=600))
            self.assertEqual(status, 0, err)


class RepeatFiringOnceTest(unittest.TestCase):
    def test_the_requested_file_is_encoded_once_when_changed_recordings_also_reach_it(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            record.write(str(job.data), {"kind": "dividends", "source": "massive", "session": "2026-11-30",
                                         "from": "2026-11-20", "fetched_at": "2026-12-01T06:05:00Z", "rows": []})
            checked = []
            original = tidata._verify_momentum

            def spy(files, *args, **kw):
                checked.extend(os.path.basename(f[2]) for f in files)
                return original(files, *args, **kw)
            with mock.patch.object(tidata, "_verify_momentum", spy):
                status, _, err = job.main(after_settle(D(2026, 11, 30), minutes=600))
            self.assertEqual(status, 0, err)
            self.assertEqual(checked.count("trad-ira-2026-11-30.jsonl"), 1)



class RetractionRuleTest(unittest.TestCase):
    """The one retraction rule: a file covers first..last and is encoded, when published and every time it
    is checked, under the retractions of sessions on or before last; a later session's retraction travels
    with the next file."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name) / "data"
        self.data.mkdir()
        build(str(self.data))
        os.remove(self.data / "universe_2026-11-30.json")
        self.client = FakeMassive({D(2026, 12, 1): bars_for(D(2026, 12, 1))})
        self.job = MomentumJob(self.tmp.name, data=self.data, client=self.client, ranker=FakeRanker())
        self.assertEqual(self.job.main(after_settle(D(2026, 11, 25)), "--backfill-from", "2026-11-23")[0], 0)

    def tearDown(self):
        self.tmp.cleanup()

    def retract(self, day, at):
        encode.record_retraction(str(self.data), day, "NYSE closed (rehearsed)", recorded_at=at)

    def fire(self, now):
        status, out, err = self.job.main(now)
        self.assertEqual(status, 0, err)
        return out.strip()

    def test_a_closure_recorded_before_the_preceding_sessions_file_is_written(self):
        # The delayed file: the 11-27 file is not yet written when the 11-30 closure is recorded (the same
        # holds for a closure announced days ahead). The 11-27 file is written with the rules' calendar, the
        # repeat firings return it unchanged, and the 12-01 file brings the retraction.
        self.retract(D(2026, 11, 30), "2026-11-30T15:00:00Z")
        self.assertEqual(self.fire(after_settle(D(2026, 11, 30))), str(self.job.path(D(2026, 11, 27))))
        written = self.job.path(D(2026, 11, 27)).read_bytes()
        self.assertNotIn(b"market.session.retracted", written)
        self.assertEqual(self.fire(after_settle(D(2026, 11, 30), minutes=600)), str(self.job.path(D(2026, 11, 27))))
        self.assertEqual(self.job.path(D(2026, 11, 27)).read_bytes(), written)
        self.assertEqual(self.fire(after_settle(D(2026, 12, 1))), str(self.job.path(D(2026, 12, 1))))
        self.assertEqual(self.job.path(D(2026, 11, 27)).read_bytes(), written)
        self.assertEqual([e["payload"]["session"] for e in self.job.envelopes(D(2026, 12, 1))
                          if e["type"] == "market.session.retracted"], ["2026-11-30"])
        self.assertEqual(self.fire(after_settle(D(2026, 12, 1), minutes=600)), str(self.job.path(D(2026, 12, 1))))

    def test_a_closure_announced_before_the_warm_up(self):
        with tempfile.TemporaryDirectory() as root:
            data = Path(root) / "data"
            data.mkdir()
            build(str(data))
            os.remove(data / "universe_2026-11-30.json")
            encode.record_retraction(str(data), D(2026, 11, 30), "NYSE closed (announced)",
                                     recorded_at="2026-11-20T15:00:00Z")
            job = MomentumJob(root, data=data, client=FakeMassive({D(2026, 12, 1): bars_for(D(2026, 12, 1))}),
                              ranker=FakeRanker())
            written = {}
            for now, extra in ((after_settle(D(2026, 11, 25)), ("--backfill-from", "2026-11-23")),
                               (after_settle(D(2026, 11, 27)), ()), (after_settle(D(2026, 11, 30)), ()),
                               (after_settle(D(2026, 12, 1)), ()), (after_settle(D(2026, 12, 1), minutes=600), ())):
                status, _, err = job.main(now, *extra)
                self.assertEqual(status, 0, err)
                for day in (D(2026, 11, 25), D(2026, 11, 27)):
                    if job.path(day).exists():
                        raw = job.path(day).read_bytes()
                        self.assertNotIn(b"market.session.retracted", raw, day)
                        self.assertEqual(written.setdefault(day, raw), raw, day)
            self.assertEqual(sorted(written), [D(2026, 11, 25), D(2026, 11, 27)])
            self.assertEqual([e["payload"]["session"] for e in job.envelopes(D(2026, 12, 1))
                              if e["type"] == "market.session.retracted"], ["2026-11-30"])

    def test_bars_recorded_on_a_cancelled_session_refuse_the_firing_that_steps_over_it(self):
        self.assertEqual(self.fire(after_settle(D(2026, 11, 27))), str(self.job.path(D(2026, 11, 27))))
        record.write(str(self.data), {"kind": "grouped", "source": "massive", "session": "2026-11-30",
                                      "fetched_at": "2026-12-01T11:05:00Z", "adjusted": False,
                                      "bars": bars_for(D(2026, 11, 30))})
        self.retract(D(2026, 11, 30), "2026-12-01T12:00:00Z")
        status, _, err = self.job.main(after_settle(D(2026, 11, 30), minutes=900))
        self.assertEqual(status, 1, err)
        self.assertIn("2026-11-30 is retracted, but its grouped bars are recorded", err)

    def test_an_action_recording_made_while_the_requested_file_is_checked_is_not_acknowledged(self):
        self.assertEqual(self.fire(after_settle(D(2026, 11, 27))), str(self.job.path(D(2026, 11, 27))))
        manifest_path = self.job.stream_dir() / "manifest.json"
        original = tidata._verify_momentum
        late = "dividends_2026-11-20_2026-11-27.json"

        def racing(files, *args, **kw):
            if not (self.data / late).exists():
                record.write(str(self.data), {"kind": "dividends", "source": "massive", "session": "2026-11-27",
                                              "from": "2026-11-20", "fetched_at": "2026-11-28T12:00:00Z", "rows": []})
            return original(files, *args, **kw)
        with mock.patch.object(tidata, "_verify_momentum", racing):
            self.fire(after_settle(D(2026, 11, 27), minutes=600))
        self.assertNotIn(late, json.loads(manifest_path.read_text())["actions"])



class FirstSessionRetractedLaterTest(unittest.TestCase):
    def test_a_retraction_of_a_published_files_first_session_is_refused_as_a_file_that_differs(self):
        with tempfile.TemporaryDirectory() as root:
            job = warmed(root)
            job.client = FakeMassive({D(2026, 12, 1): bars_for(D(2026, 12, 1))})
            encode.record_retraction(str(job.data), D(2026, 11, 30), "NYSE closed (recorded late)",
                                     recorded_at="2026-12-01T23:00:00Z")
            status, _, err = job.main(after_settle(D(2026, 12, 1)))
            self.assertEqual(status, 1, err)
            self.assertIn("retraction of 2026-11-30 was recorded after", err)
            self.assertIn("A file already written differs", err)
            self.assertNotIn("is missing", err)


if __name__ == "__main__":
    unittest.main()
