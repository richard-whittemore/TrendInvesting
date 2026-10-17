"""Tests for the envelope encoder: recordings in, engine inputs out.
Synthetic data only; no network."""
import datetime as dt
import hashlib
import io
import json
import os
import socket
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from marketdata import __main__ as cli
from marketdata import encode, history, nyse_calendar, record
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
        fetched = stamp(D.fromisoformat(sector_session))   # the evening of the labels' own session
        record.write(d, {"kind": "sectors-iwv", "source": sectors.IWV_SOURCE, "session": sector_session,
                         "fetched_at": fetched, "url": sectors.IWV_URL, "as_of": "Nov 27, 2026",
                         "body": iwv_csv(("AAA", "Information Technology", "Equity"), ("BBB", "Financials", "Equity"))})
        record.write(d, {"kind": "sectors-nasdaq", "source": sectors.NASDAQ_SOURCE, "session": sector_session,
                         "fetched_at": fetched, "url": sectors.NASDAQ_URL,
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


def unadjustable_ddd(d):
    """A DDD dividend ex 11-25, the session after the one DDD did not trade:
    no prior close, so DDD cannot be adjusted exactly from 11-25 on."""
    record.write(d, {"kind": "dividends", "source": "massive", "session": "2026-11-25", "from": "2026-11-20",
                     "fetched_at": stamp(LAST),
                     "rows": [{"id": "V2", "ticker": "DDD", "ex_dividend_date": "2026-11-25", "cash_amount": 0.1}]})


def rewrite(d, kind, day, ticker, **fields):
    """Rewrites ``ticker``'s bar in the ``kind`` recording of ``day``."""
    path = os.path.join(d, "{}_{}.json".format(kind, day))
    rec = record.read(path)
    os.remove(path)
    next(b for b in rec["bars"] if b["T"] == ticker).update(fields)
    record.write(d, rec)


def break_bar(d, day, ticker):
    """Rewrites ``ticker``'s bar on ``day`` with its high below its close."""
    rewrite(d, "grouped", day, ticker, h=1.0)


def sessions_with(stream, ticker):
    return [e["event_time"][:10] for e in of(stream, "market.session.closed")
            if ticker in e["payload"]["instrument_ids"]]


ALL_SESSIONS = ["2026-11-23", "2026-11-24", "2026-11-25", "2026-11-27", "2026-11-30"]


class UniverseMemberExclusionTest(unittest.TestCase):
    """A universe member with bad data is left out of the sessions it reaches
    (a bad bar: that session; an unadjustable action: from its ex-date): no
    bar is written for those sessions, its other bars are, and the sessions
    are named in ``excluded``; the other inputs are still written."""

    def encoded(self, spoil, **kw):
        excluded = {}
        with tempfile.TemporaryDirectory() as d:
            build(d)
            spoil(d)
            stream = run(d, **dict(dict(optional=[], universe_members=True, excluded=excluded), **kw))
        return stream, excluded

    def assertLeftOutFrom(self, stream, ticker, day):
        bars = [e["payload"]["period_end"][:10] for e in of(stream, "market.bar.completed")
                if e["payload"]["instrument_id"] == ticker]
        self.assertEqual(bars, [s for s in ALL_SESSIONS if s < day and (ticker, s) != ("DDD", "2026-11-24")])
        self.assertEqual(sessions_with(stream, ticker), bars)
        self.assertEqual(of(stream, "market.session.closed", "2026-11-30")[0]["payload"]["instrument_ids"],
                         sorted({"AAA", "BBB", "CCC", "DDD", "IEF", "SPY"} - {ticker}))

    def test_a_member_that_cannot_be_adjusted_is_left_out_from_the_ex_date_and_named(self):
        stream, excluded = self.encoded(unadjustable_ddd)
        self.assertLeftOutFrom(stream, "DDD", "2026-11-25")
        self.assertEqual(list(excluded), ["DDD"])
        [gone] = excluded["DDD"]
        self.assertEqual((gone.first, gone.last), (D(2026, 11, 25), LAST))
        self.assertIn("no close on the session before ex-date 2026-11-25", gone.reason)

    def test_a_member_with_a_malformed_bar_is_left_out_of_that_session_only_and_named(self):
        stream, excluded = self.encoded(lambda d: break_bar(d, "2026-11-25", "AAA"))
        written = [s for s in ALL_SESSIONS if s != "2026-11-25"]
        self.assertEqual([e["payload"]["period_end"][:10] for e in of(stream, "market.bar.completed")
                          if e["payload"]["instrument_id"] == "AAA"], written)
        self.assertEqual(sessions_with(stream, "AAA"), written)
        [gone] = excluded["AAA"]
        self.assertEqual((gone.first, gone.last), (D(2026, 11, 25), D(2026, 11, 25)))
        self.assertIn("inconsistent", gone.reason)

    def test_later_bad_data_never_changes_an_earlier_sessions_bars(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            clean = run(d, optional=[], universe_members=True)
            break_bar(d, "2026-11-25", "AAA")
            spoiled = run(d, optional=[], universe_members=True)
        for day in ("2026-11-23", "2026-11-24"):
            self.assertEqual(bar_of(spoiled, "AAA", day), bar_of(clean, "AAA", day))

    def test_a_member_action_with_no_readable_date_leaves_it_out_of_every_session(self):
        def undated(d):
            record.write(d, {"kind": "dividends", "source": "massive", "session": "2026-11-25", "from": "2026-11-20",
                             "fetched_at": stamp(LAST),
                             "rows": [{"id": "V3", "ticker": "DDD", "ex_dividend_date": "soon", "cash_amount": 0.1}]})
        stream, excluded = self.encoded(undated)
        self.assertEqual(sessions_with(stream, "DDD"), [])
        self.assertEqual([(g.first, g.last, g.sessions) for g in excluded["DDD"]], [(FIRST, LAST, 5)])
        self.assertIn("soon", excluded["DDD"][0].reason)

    def test_a_member_with_a_missing_or_negative_close_or_volume_in_the_universe_is_left_out_of_that_session(self):
        for field, value in (("c", None), ("c", -1.0), ("v", None), ("v", -5)):
            with self.subTest(field=field, value=value):
                stream, excluded = self.encoded(
                    lambda d: rewrite(d, "universe", LAST.isoformat(), "AAA", **{field: value}))
                self.assertLeftOutFrom(stream, "AAA", "2026-11-30")
                rows = of(stream, "market.universe.snapshot")[0]["payload"]["rows"]
                self.assertEqual([r["instrument_id"] for r in rows], ["BBB", "CCC", "DDD"])
                [gone] = excluded["AAA"]
                self.assertEqual((gone.first, gone.last), (LAST, LAST))

    def test_a_universe_row_with_a_bad_close_is_left_out_without_universe_members(self):
        stream, excluded = self.encoded(lambda d: rewrite(d, "universe", LAST.isoformat(), "AAA", c=None),
                                        universe_members=False)
        rows = of(stream, "market.universe.snapshot")[0]["payload"]["rows"]
        self.assertEqual([r["instrument_id"] for r in rows], ["BBB", "CCC", "DDD"])
        self.assertEqual([(g.first, g.last) for g in excluded["AAA"]], [(LAST, LAST)])

    def test_the_universe_snapshot_still_lists_an_excluded_member_whose_row_is_good(self):
        stream, _ = self.encoded(unadjustable_ddd)
        rows = of(stream, "market.universe.snapshot")[0]["payload"]["rows"]
        self.assertIn("DDD", [r["instrument_id"] for r in rows])

    def test_a_required_or_named_optional_instrument_is_still_refused(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            break_bar(d, "2026-11-25", "SPY")
            with self.assertRaisesRegex(encode.Malformed, "SPY"):
                run(d, optional=[], universe_members=True)
        with tempfile.TemporaryDirectory() as d:
            build(d)
            unadjustable_ddd(d)
            with self.assertRaisesRegex(encode.Malformed, "DDD"):
                run(d, optional=["DDD"], universe_members=True)
        for field, value in (("c", None), ("v", -5)):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as d:
                build(d)
                rewrite(d, "universe", LAST.isoformat(), "AAA", **{field: value})
                with self.assertRaisesRegex(encode.Malformed, "AAA"):
                    run(d, optional=["AAA"], universe_members=True)
                with self.assertRaisesRegex(encode.Malformed, "AAA"):
                    run(d, optional=["AAA"], universe_members=False)

    def test_the_cli_names_each_excluded_member_and_its_sessions(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            unadjustable_ddd(d)
            out = os.path.join(d, "stream.jsonl")
            with mock.patch("sys.stderr", new_callable=io.StringIO) as err:
                status = cli.main(["encode", "--out", d, "--first", "2026-11-23", "--asof", "2026-11-30",
                                   "--instruments", "SPY,IEF", "--universe-members",
                                   "--configuration-hash", "hash-1", "--strategy-version", "momentum/test",
                                   "--run-id", "run-1", "--stream", out])
            self.assertEqual(status, 0)
            self.assertIn("excluded DDD from 2026-11-25 to 2026-11-30 (3 sessions): ", err.getvalue())
            self.assertEqual([json.loads(line) for line in Path(out).read_text().splitlines()],
                             run(d, optional=[], universe_members=True))

    def test_the_stream_with_an_excluded_member_satisfies_the_go_validators(self):
        for spoil in (unadjustable_ddd, lambda d: break_bar(d, "2026-11-25", "AAA"),
                      lambda d: rewrite(d, "universe", LAST.isoformat(), "AAA", c=None)):
            stream, _ = self.encoded(spoil)
            with tempfile.TemporaryDirectory() as d:
                path = os.path.join(d, "stream.jsonl")
                encode.write_stream(path, stream)
                with open(path) as f:
                    check = subprocess.run(["go", "run", "./adapter/marketdata/tests/testdata/envelope_contract.go"],
                                           cwd=REPO, stdin=f, text=True, capture_output=True)
            self.assertEqual(check.returncode, 0, check.stderr)


def record_span(d, days, aaa, splits=(), dividends=()):
    """Records ``days`` with SPY, IEF and AAA closing at ``aaa(i)`` (a bar,
    or a close) on session i, the actions given, and the universe and
    sector recordings of the last session."""
    last = days[-1]
    for i, day in enumerate(days):
        a = aaa(i)
        bars = [row("SPY", 600.0), row("IEF", 95.0), a if isinstance(a, dict) else row("AAA", a, spread=0.01)]
        rec = {"source": "massive", "session": day.isoformat(), "fetched_at": stamp(day),
               "adjusted": False, "bars": bars}
        if day == last:
            rec.update(kind="universe", ticker_type="CS",
                       tickers=[{"ticker": "AAA", "type": "CS", "market": "stocks"}])
        else:
            rec.update(kind="grouped")
        record.write(d, rec)
    for kind, rows in (("splits", list(splits)), ("dividends", list(dividends))):
        record.write(d, {"kind": kind, "source": "massive", "session": last.isoformat(),
                         "from": days[0].isoformat(), "fetched_at": stamp(last), "rows": rows})
    for kind, source, url, body in (
            ("sectors-iwv", sectors.IWV_SOURCE, sectors.IWV_URL,
             {"as_of": "x", "body": iwv_csv(("AAA", "Information Technology", "Equity"))}),
            ("sectors-nasdaq", sectors.NASDAQ_SOURCE, sectors.NASDAQ_URL,
             {"rows": json.loads(screener(("AAA", "Technology")))["data"]["rows"]})):
        record.write(d, dict({"kind": kind, "source": source, "session": last.isoformat(),
                              "fetched_at": stamp(last), "url": url}, **body))


def span_sessions(n):
    return nyse_calendar.sessions(D(2025, 1, 2), D(2026, 12, 31))[:n]


class ExclusionWindowTest(unittest.TestCase):
    """Over spans longer than the 253-close window. A member with one bad bar
    is left out of that session only; the momentum rule's own window then
    keeps it out of every ranking that reaches the session (ADR 0026 point
    5). A member with a split or dividend that cannot be adjusted exactly
    has a price series that jumps at the ex-date, so it is left out from the
    ex-date for 253 sessions and written again at ex-date + 253: no written
    window spans the jump."""

    def encoded(self, n, aaa, **actions):
        days = span_sessions(n)
        excluded = {}
        with tempfile.TemporaryDirectory() as d:
            record_span(d, days, aaa, **actions)
            stream = run(d, first=days[0], last=days[-1], optional=[], universe_members=True, excluded=excluded)
        return days, stream, excluded

    def test_a_member_with_one_bad_bar_is_left_out_of_that_session_only(self):
        def aaa(i):
            bar = row("AAA", 20.0)
            if i == 1:
                bar["h"] = 1.0   # high below close
            return bar
        days, stream, excluded = self.encoded(256, aaa)
        self.assertEqual([(g.first, g.last) for g in excluded["AAA"]], [(days[1], days[1])])
        iso = [day.isoformat() for day in days]
        self.assertEqual(sessions_with(stream, "AAA"), iso[:1] + iso[2:])

    def test_a_refused_dividend_keeps_that_sessions_close_for_the_next_sessions_dividend(self):
        days = span_sessions(256)
        dividends = [{"id": "V1", "ticker": "AAA", "ex_dividend_date": days[1].isoformat(), "cash_amount": 100.0},
                     {"id": "V2", "ticker": "AAA", "ex_dividend_date": days[2].isoformat(), "cash_amount": 0.5}]
        days, stream, excluded = self.encoded(256, lambda i: 20.0, dividends=dividends)
        [gone] = excluded["AAA"]
        self.assertEqual((gone.first, gone.last, gone.sessions), (days[1], days[253], 253))
        self.assertIn("not below the prior close", gone.reason)
        iso = [day.isoformat() for day in days]
        self.assertEqual(sessions_with(stream, "AAA"), [iso[0], iso[254], iso[255]])

    def test_a_dropped_split_leaves_exact_returns_on_each_side_and_no_window_spans_the_gap(self):
        n, x = 300, 3
        days = span_sessions(n)
        raw = [(20.0 + i / 100) / (2 if i >= x else 1) for i in range(n)]
        split = {"ticker": "AAA", "execution_date": days[x].isoformat(), "split_from": 1, "split_to": 2}
        days, stream, excluded = self.encoded(n, lambda i: raw[i],
                                              splits=[dict(split, id="S1"), dict(split, id="S2")])
        [gone] = excluded["AAA"]
        self.assertEqual((gone.first, gone.last, gone.sessions), (days[x], days[x + 252], 253))
        self.assertIn("two splits", gone.reason)
        iso = [day.isoformat() for day in days]
        before, after = list(range(x)), list(range(x + 253, n))
        self.assertEqual(sessions_with(stream, "AAA"), [iso[i] for i in before + after])
        closes = {i: bar_of(stream, "AAA", iso[i])["adjusted"]["close"] for i in before + after}
        for side in (before, after):
            for i in side[1:]:
                self.assertAlmostEqual(closes[i] / closes[side[0]], raw[i] / raw[side[0]], places=12)
        self.assertGreater(after[0] - before[-1], encode.WINDOW)   # no 253-close window holds both sides


class BoostStreamTest(unittest.TestCase):
    """Boost reads one instrument's bars and the session closes, and refuses
    any other input type (targetweight.Handler.Apply), so its stream carries
    no calendar and no universe snapshot."""

    def test_a_boost_stream_carries_only_the_one_instruments_bars_and_the_closes(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)   # a universe is recorded for 11-30: it must not appear
            stream = run(d, required=["SPY"], optional=[], engine="boost")
        self.assertEqual([e["type"] for e in stream], ["market.bar.completed", "market.session.closed"] * 5)
        self.assertEqual({e["payload"]["instrument_id"] for e in of(stream, "market.bar.completed")}, {"SPY"})
        self.assertEqual([e["sequence"] for e in stream], list(range(2, 12)))

    def test_a_boost_stream_names_exactly_one_required_instrument(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            for kw in ({"required": ["SPY", "IEF"], "optional": []}, {"required": ["SPY"], "optional": ["AAA"]},
                       {"required": ["SPY"], "optional": [], "universe_members": True}):
                with self.subTest(kw), self.assertRaises(ValueError):
                    run(d, engine="boost", **kw)
            with self.assertRaises(ValueError):
                run(d, engine="turtle")

    def test_the_cli_writes_a_boost_stream(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            out = os.path.join(d, "boost.jsonl")
            status = cli.main(["encode", "--out", d, "--first", "2026-11-23", "--asof", "2026-11-30",
                               "--instruments", "SPY", "--engine", "boost", "--configuration-hash", "hash-1",
                               "--strategy-version", "momentum/test", "--run-id", "run-1", "--stream", out])
            self.assertEqual(status, 0)
            got = [json.loads(line) for line in Path(out).read_text().splitlines()]
            self.assertEqual(got, run(d, required=["SPY"], optional=[], engine="boost"))


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

    def test_actions_are_read_through_the_history_reader(self):
        # One reader of the action recordings (history.load_actions), so its
        # per-id dedup and coverage rules apply to the encoder too.
        with tempfile.TemporaryDirectory() as d:
            build(d)
            with mock.patch.object(history, "load_actions", wraps=history.load_actions) as reader:
                run(d)
        self.assertEqual(sorted(c.args for c in reader.call_args_list),
                         [(d, "dividends", FIRST + dt.timedelta(days=1), LAST),
                          (d, "splits", FIRST + dt.timedelta(days=1), LAST)])

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

    def test_a_failed_write_leaves_no_file_and_a_retry_succeeds(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            stream, path = run(d), os.path.join(d, "stream.jsonl")
            with self.assertRaises(ValueError):   # NaN is refused after the earlier lines were written
                encode.write_stream(path, stream + [{"bad": float("nan")}])
            self.assertEqual(sorted(f for f in os.listdir(d) if f.endswith((".jsonl", ".part"))), [])
            encode.write_stream(path, stream)
            self.assertEqual([json.loads(line) for line in Path(path).read_text().splitlines()], stream)

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
            rec.assert_called_once_with(d, D(2026, 11, 30), settle=dt.timedelta(minutes=60))

    def test_the_sectors_command_records_the_last_trading_day_on_or_before_asof(self):
        with tempfile.TemporaryDirectory() as d, \
                mock.patch("marketdata.sectors.record_sectors", return_value=["p"]) as rec:
            self.assertEqual(cli.main(["sectors", "--out", d, "--asof", "2026-11-29", "--settle-minutes", "5"]), 0)
            rec.assert_called_once_with(d, D(2026, 11, 27), settle=dt.timedelta(minutes=5))   # Sunday -> Friday


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

    def test_a_boost_stream_satisfies_the_go_validators(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            path = os.path.join(d, "boost.jsonl")
            encode.write_stream(path, run(d, required=["SPY"], optional=[], engine="boost"))
            result = self.check(path)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

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
