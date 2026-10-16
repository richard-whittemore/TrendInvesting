"""Tests for the envelope encoder: recordings in, engine inputs out.
Synthetic data only; no network."""
import datetime as dt
import hashlib
import json
import os
import socket
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from marketdata import __main__ as cli
from marketdata import encode, record
from marketdata.tests.test_sectors import iwv_csv, screener
from marketdata import sectors

D = dt.date
REPO = Path(__file__).resolve().parents[3]
FIRST, LAST = D(2026, 11, 23), D(2026, 11, 30)   # Thanksgiving week: 11-26 closed, 11-27 closes early
GROUPED_DAYS = [D(2026, 11, 23), D(2026, 11, 24), D(2026, 11, 25), D(2026, 11, 27)]
IDENTITY = encode.Identity(configuration_hash="hash-1", strategy_version="momentum/test", run_id="run-1")
REQUIRED, OPTIONAL = ["SPY", "IEF"], ["AAA", "BBB", "CCC", "DDD"]


def row(t, c, v=1000, spread=1.0):
    return {"T": t, "o": c, "h": c + spread, "l": c - spread / 2, "c": c, "v": v, "t": 0}


def bars_for(day):
    """BBB splits 2-for-1 on 11-25; CCC goes ex a 1.00 dividend on 11-27;
    DDD does not trade on 11-24; ETFX is not common stock."""
    bbb = 100.0 if day < D(2026, 11, 25) else 50.0
    ccc = 49.0 if day >= D(2026, 11, 27) else 50.0
    rows = [row("SPY", 600.0), row("IEF", 95.0, v=5000), row("AAA", 20.0 + day.day / 100), row("BBB", bbb, v=400),
            row("CCC", ccc), row("ETFX", 30.0)]
    if day != D(2026, 11, 24):
        rows.append(row("DDD", 7.5))
    return rows


def stamp(day):
    return "{}T23:30:00Z".format(day.isoformat())


def build(d, *, splits_windows=(("2026-11-01", "2026-11-27"), ("2026-11-20", "2026-11-30")),
          sector_session="2026-11-30"):
    for day in GROUPED_DAYS:
        record.write(d, {"kind": "grouped", "source": "massive", "session": day.isoformat(), "fetched_at": stamp(day),
                         "adjusted": False, "bars": bars_for(day)})
    tickers = [{"ticker": t, "type": "CS", "market": "stocks"} for t in ("AAA", "BBB", "CCC", "DDD", "ZZZ")]
    record.write(d, {"kind": "universe", "source": "massive", "session": LAST.isoformat(), "fetched_at": stamp(LAST),
                     "adjusted": False, "ticker_type": "CS", "bars": bars_for(LAST), "tickers": tickers})
    split = {"id": "S1", "ticker": "BBB", "execution_date": "2026-11-25", "split_from": 1, "split_to": 2}
    for a, b in splits_windows:
        record.write(d, {"kind": "splits", "source": "massive", "session": b, "from": a, "fetched_at": stamp(LAST),
                         "rows": [split]})
    record.write(d, {"kind": "dividends", "source": "massive", "session": "2026-11-30", "from": "2026-11-01",
                     "fetched_at": stamp(LAST),
                     "rows": [{"id": "V1", "ticker": "CCC", "ex_dividend_date": "2026-11-27", "cash_amount": 1.0}]})
    if sector_session:
        record.write(d, {"kind": "sectors-iwv", "source": sectors.IWV_SOURCE, "session": sector_session,
                         "fetched_at": stamp(LAST), "url": sectors.IWV_URL, "as_of": "Nov 27, 2026",
                         "body": iwv_csv(("AAA", "Information Technology", "Equity"), ("BBB", "Financials", "Equity"))})
        record.write(d, {"kind": "sectors-nasdaq", "source": sectors.NASDAQ_SOURCE, "session": sector_session,
                         "fetched_at": stamp(LAST), "url": sectors.NASDAQ_URL,
                         "rows": json.loads(screener(("CCC", "Energy"), ("AAA", "Finance")))["data"]["rows"]})


def run(d, **kw):
    args = dict(first=FIRST, last=LAST, identity=IDENTITY, required=REQUIRED, optional=OPTIONAL)
    args.update(kw)
    return encode.encode(d, **args)


def of(stream, type_, session=None):
    out = [e for e in stream if e["type"] == type_]
    if session:
        out = [e for e in out if e["event_time"].startswith(session)]
    return out


def bar_of(stream, ticker, session):
    return next(e["payload"] for e in of(stream, "market.bar.completed", session)
                if e["payload"]["instrument_id"] == ticker)


class StreamShapeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name)
        self.stream = run(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_calendar_first_then_each_sessions_bars_universe_and_close(self):
        kinds = [(e["type"], e["event_time"][:10]) for e in self.stream]
        self.assertEqual(kinds[0][0], "market.session.calendar")
        sessions = ["2026-11-23", "2026-11-24", "2026-11-25", "2026-11-27", "2026-11-30"]
        closes = [e for e in self.stream if e["type"] == "market.session.closed"]
        self.assertEqual([e["event_time"][:10] for e in closes], sessions)
        for session in sessions:
            block = [t for t, s in kinds[1:] if s == session]
            self.assertEqual(block[-1], "market.session.closed")
            self.assertEqual(set(block[:-1]) - {"market.bar.completed", "market.universe.snapshot"}, set())
        self.assertEqual([e["payload"]["session"] for e in of(self.stream, "market.universe.snapshot")], ["2026-11-30"])

    def test_the_calendar_lists_every_session_and_one_past_the_last(self):
        cal = of(self.stream, "market.session.calendar")[0]["payload"]
        self.assertEqual(cal, {"source": "nyse-calendar", "sessions": [
            "2026-11-23", "2026-11-24", "2026-11-25", "2026-11-27", "2026-11-30", "2026-12-01"]})

    def test_each_session_closes_the_bars_it_carried(self):
        for close in of(self.stream, "market.session.closed"):
            day = close["event_time"][:10]
            ids = [e["payload"]["instrument_id"] for e in of(self.stream, "market.bar.completed", day)]
            self.assertEqual(close["payload"]["instrument_ids"], ids)
            self.assertEqual(ids, sorted(ids))
        self.assertEqual(of(self.stream, "market.session.closed", "2026-11-24")[0]["payload"]["instrument_ids"],
                         ["AAA", "BBB", "CCC", "IEF", "SPY"])   # DDD did not trade; ETFX not asked for

    def test_envelopes_carry_the_run_identity_contiguous_sequences_and_their_payload_hash(self):
        self.assertEqual([e["sequence"] for e in self.stream], list(range(2, 2 + len(self.stream))))
        self.assertEqual(len({e["id"] for e in self.stream}), len(self.stream))
        for e in self.stream:
            self.assertEqual((e["envelope_version"], e["source"], e["configuration_hash"], e["strategy_version"],
                              e["correlation_id"]), (1, "marketdata", "hash-1", "momentum/test", "run-1"))
            raw = json.dumps(e["payload"], separators=(",", ":"), allow_nan=False)
            self.assertEqual(e["payload_hash"], hashlib.sha256(raw.encode()).hexdigest())

    def test_bars_close_at_the_sessions_close_and_record_when_they_were_fetched(self):
        thu = of(self.stream, "market.bar.completed", "2026-11-23")[0]
        self.assertEqual((thu["event_time"], thu["payload"]["period_end"], thu["recorded_at"]),
                         ("2026-11-23T21:00:00Z", "2026-11-23T21:00:00Z", "2026-11-23T23:30:00Z"))
        early = bar_of(self.stream, "SPY", "2026-11-27")
        self.assertEqual(early["period_end"], "2026-11-27T18:00:00Z")   # 1 p.m. New York


class ViewsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name)
        self.stream = run(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_raw_is_the_recorded_bar(self):
        b = bar_of(self.stream, "BBB", "2026-11-25")
        self.assertEqual(b["raw"], {"view": "raw", "open": 50.0, "high": 51.0, "low": 49.5, "close": 50.0, "volume": 400.0})

    def test_a_split_is_neutralised_in_both_adjusted_views_from_the_base(self):
        before, after = bar_of(self.stream, "BBB", "2026-11-24"), bar_of(self.stream, "BBB", "2026-11-25")
        for view in ("split_adjusted", "adjusted"):
            self.assertEqual(before[view]["close"], 100.0)
            self.assertEqual(after[view]["close"], 100.0)
            self.assertEqual(after[view]["volume"], 200.0)   # in the base session's shares
        self.assertEqual((after["split_adjusted"]["view"], after["adjusted"]["view"]),
                         ("split-adjusted", "split-and-dividend-adjusted"))
        self.assertEqual(after["adjusted_base"], "2026-11-23")

    def test_a_dividend_moves_only_the_adjusted_view(self):
        before, after = bar_of(self.stream, "CCC", "2026-11-25"), bar_of(self.stream, "CCC", "2026-11-27")
        self.assertEqual(after["split_adjusted"]["close"], 49.0)
        self.assertAlmostEqual(after["adjusted"]["close"], 50.0, places=12)   # 49 x 50/49: no false return
        self.assertEqual(before["adjusted"]["close"], 50.0)
        self.assertEqual(after["adjusted"]["volume"], 1000.0)

    def test_an_overlapping_action_window_applies_a_split_once(self):
        self.assertEqual(bar_of(self.stream, "BBB", "2026-11-30")["adjusted"]["close"], 100.0)   # not 200

    def test_a_later_run_never_restates_an_earlier_bar(self):
        early = run(self.tmp.name, last=D(2026, 11, 24))
        for e in of(early, "market.bar.completed"):
            later = next(x for x in of(self.stream, "market.bar.completed") if x["id"] == e["id"])
            self.assertEqual(e["payload"], later["payload"])


class UniverseTest(unittest.TestCase):
    def test_rows_are_the_common_stocks_with_a_bar_unadjusted_with_their_sector_and_source(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            snap = of(run(d), "market.universe.snapshot")[0]
        self.assertEqual((snap["payload"]["session"], snap["payload"]["source"], snap["recorded_at"]),
                         ("2026-11-30", "massive", "2026-11-30T23:30:00Z"))
        self.assertEqual(snap["payload"]["rows"], [
            {"instrument_id": "AAA", "type": "CS", "close": 20.3, "volume": 1000.0, "sector": "Technology",
             "sector_source": sectors.IWV_SOURCE},
            {"instrument_id": "BBB", "type": "CS", "close": 50.0, "volume": 400.0, "sector": "Financial Services",
             "sector_source": sectors.IWV_SOURCE},
            {"instrument_id": "CCC", "type": "CS", "close": 49.0, "volume": 1000.0, "sector": "Energy",
             "sector_source": sectors.NASDAQ_SOURCE},
            {"instrument_id": "DDD", "type": "CS", "close": 7.5, "volume": 1000.0, "sector": "", "sector_source": ""},
        ])

    def test_universe_members_adds_every_common_stock_with_a_bar_to_the_bars_written(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            stream = run(d, optional=[], universe_members=True)
        ids = of(stream, "market.session.closed", "2026-11-30")[0]["payload"]["instrument_ids"]
        self.assertEqual(ids, ["AAA", "BBB", "CCC", "DDD", "IEF", "SPY"])


class FailClosedTest(unittest.TestCase):
    def assertRefused(self, error, **build_kw):
        with tempfile.TemporaryDirectory() as d:
            build(d, **build_kw)
            with self.assertRaises(error):
                run(d)

    def test_a_trading_day_with_no_recording_is_a_gap(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            os.remove(os.path.join(d, "grouped_2026-11-24.json"))
            with self.assertRaises(encode.Gap):
                run(d)

    def test_a_required_instrument_without_a_bar_writes_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            with self.assertRaisesRegex(encode.MissingData, "NOPE"):
                run(d, required=["SPY", "NOPE"])

    def test_actions_that_do_not_cover_the_sessions_are_refused(self):
        self.assertRefused(encode.MissingData, splits_windows=(("2026-11-01", "2026-11-25"),))

    def test_conflicting_copies_of_one_action_are_refused(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            record.write(d, {"kind": "splits", "source": "massive", "session": "2026-11-30", "from": "2026-11-25",
                             "fetched_at": stamp(LAST), "rows": [{"id": "S1", "ticker": "BBB",
                                                                  "execution_date": "2026-11-25",
                                                                  "split_from": 1, "split_to": 3}]})
            with self.assertRaisesRegex(encode.Conflict, "S1"):
                run(d)

    def test_a_universe_without_recent_sector_recordings_is_refused(self):
        self.assertRefused(encode.MissingData, sector_session=None)
        self.assertRefused(encode.MissingData, sector_session="2026-11-02")

    def test_a_malformed_bar_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            path = os.path.join(d, "grouped_2026-11-25.json")
            rec = record.read(path)
            os.remove(path)
            rec["bars"][0]["h"] = 1.0   # SPY's high below its close
            record.write(d, rec)
            with self.assertRaisesRegex(encode.Malformed, "SPY"):
                run(d)

    def test_two_recordings_of_one_session_that_disagree_are_refused(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            record.write(d, {"kind": "grouped", "source": "massive", "session": "2026-11-30",
                             "fetched_at": stamp(LAST), "adjusted": False, "bars": bars_for(D(2026, 11, 27))})
            with self.assertRaises(encode.Conflict):
                run(d)

    def test_a_base_after_the_first_session_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            with self.assertRaises(ValueError):
                run(d, base=D(2026, 11, 24))


class ReplayTest(unittest.TestCase):
    def test_two_runs_write_byte_identical_streams_without_touching_the_network(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            with mock.patch.object(socket, "socket", side_effect=AssertionError("network used")), \
                    mock.patch("urllib.request.urlopen", side_effect=AssertionError("network used")):
                one, two = os.path.join(d, "one.jsonl"), os.path.join(d, "two.jsonl")
                encode.write_stream(one, run(d))
                encode.write_stream(two, run(d))
            self.assertEqual(Path(one).read_bytes(), Path(two).read_bytes())
            with self.assertRaises(FileExistsError):
                encode.write_stream(one, run(d))

    def test_the_cli_encodes_without_a_key(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            out = os.path.join(d, "stream.jsonl")
            with mock.patch.dict(os.environ, {}, clear=True), \
                    mock.patch("marketdata.massive.load_key", side_effect=AssertionError("key loaded")), \
                    mock.patch("urllib.request.urlopen", side_effect=AssertionError("network used")):
                status = cli.main(["encode", "--out", d, "--first", "2026-11-23", "--asof", "2026-11-30",
                                   "--instruments", "SPY,IEF", "--optional", "AAA,BBB,CCC,DDD",
                                   "--configuration-hash", "hash-1", "--strategy-version", "momentum/test",
                                   "--run-id", "run-1", "--stream", out])
            self.assertEqual(status, 0)
            lines = Path(out).read_text().splitlines()
            self.assertEqual([json.loads(line) for line in lines], run(d))


    def test_the_sectors_command_needs_no_massive_key(self):
        with tempfile.TemporaryDirectory() as d, \
                mock.patch("marketdata.massive.load_key", side_effect=AssertionError("key loaded")), \
                mock.patch("marketdata.sectors.record_sectors", return_value=["p"]) as rec:
            self.assertEqual(cli.main(["sectors", "--out", d, "--asof", "2026-11-30"]), 0)
            rec.assert_called_once_with(d, D(2026, 11, 30))


class GoContractTest(unittest.TestCase):
    """The stream decodes and validates against internal/event's contracts."""

    def check(self, path):
        with open(path) as f:
            return subprocess.run(["go", "run", "./adapter/marketdata/tests/testdata/envelope_contract.go"],
                                  cwd=REPO, stdin=f, text=True, capture_output=True)

    def test_the_encoded_stream_satisfies_the_go_validators(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            path = os.path.join(d, "stream.jsonl")
            encode.write_stream(path, run(d, universe_members=True))
            result = self.check(path)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("envelopes valid", result.stdout)

    def test_the_checker_refuses_a_stream_go_would_refuse(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            stream = run(d)
            bad = next(e for e in stream if e["type"] == "market.bar.completed")
            bad["payload"]["adjusted_base"] = "2027-01-01"   # after the bar's session
            raw = json.dumps(bad["payload"], separators=(",", ":"))
            bad["payload_hash"] = hashlib.sha256(raw.encode()).hexdigest()
            path = os.path.join(d, "stream.jsonl")
            encode.write_stream(path, stream)
            result = self.check(path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("adjusted base", result.stderr)


if __name__ == "__main__":
    unittest.main()
