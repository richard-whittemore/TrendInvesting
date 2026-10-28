"""Tests for the compact universe stream (encode.encode with a ranker): one
universe snapshot per session, bars only for the named instruments, and at
a decision session one close history per member the Go ranker names.
Synthetic data only; no network."""
import datetime as dt
import hashlib
import json
import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from marketdata import __main__ as cli
from marketdata import encode, nyse_calendar, record, sectors
from marketdata.tests.test_encode import (ALL_SESSIONS, FIRST, IDENTITY, REPO, bar_of, bars_for, break_bar, build, of,
                                          rewrite, row, run, stamp, unadjustable_ddd)
from marketdata.tests.test_sectors import iwv_csv, screener

D = dt.date
HISTORY = "market.close.history"


class FakeRanker:
    """Stands in for cmd/universe-rank: a decision session is one whose next
    session is in another month, and its members are ``members``."""

    def __init__(self, members=("BBB", "AAA"), window=4):
        self.members, self.window, self.requests = list(members), window, []

    def __call__(self, requests):
        self.requests.extend(requests)
        return [{"session": r["universe"]["session"],
                 "decision_session": r["next_session"][:7] != r["universe"]["session"][:7],
                 "window": self.window,
                 "members": self.members if r["next_session"][:7] != r["universe"]["session"][:7] else []}
                for r in requests]


def compact(d, ranker=None, **kw):
    return run(d, optional=[], ranker=ranker or FakeRanker(), **kw)


class CompactShapeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_each_session_carries_bars_for_the_named_instruments_only(self):
        stream = compact(self.tmp.name)
        for session in ALL_SESSIONS:
            ids = [e["payload"]["instrument_id"] for e in of(stream, "market.bar.completed", session)]
            self.assertEqual(ids, ["IEF", "SPY"], session)
            self.assertEqual(of(stream, "market.session.closed", session)[0]["payload"]["instrument_ids"], ["IEF", "SPY"])

    def test_a_decision_session_carries_its_universe_then_one_history_per_ranked_member_then_its_close(self):
        stream = compact(self.tmp.name)
        block = [(e["type"], e["payload"].get("instrument_id")) for e in stream if e["event_time"].startswith("2026-11-30")]
        self.assertEqual(block, [("market.bar.completed", "IEF"), ("market.bar.completed", "SPY"),
                                 ("market.universe.snapshot", None),
                                 (HISTORY, "AAA"), (HISTORY, "BBB"),
                                 ("market.session.closed", None)])
        self.assertEqual(len(of(stream, "market.universe.snapshot")), 1)   # one recorded universe, one envelope
        self.assertEqual([e["event_time"][:10] for e in of(stream, HISTORY)], ["2026-11-30", "2026-11-30"])

    def test_counted_per_session_no_input_is_written_per_listed_stock(self):
        """The engine never ingests a bar per universe member: per session the
        compact stream holds the named instruments' bars, one universe
        snapshot where one was recorded, and close histories only at a
        decision session, one per ranked member (never one per listed stock)."""
        stream, full = compact(self.tmp.name), run(self.tmp.name, optional=[], universe_members=True)
        listed = len(of(stream, "market.universe.snapshot")[0]["payload"]["rows"])
        for session in ALL_SESSIONS:
            counts = {t: len(of(stream, t, session)) for t in ("market.bar.completed", HISTORY, "market.universe.snapshot")}
            full_bars = len(of(full, "market.bar.completed", session))
            self.assertEqual(counts["market.bar.completed"], 2, session)           # SPY and IEF
            self.assertLess(counts["market.bar.completed"], full_bars, session)
            self.assertEqual(counts[HISTORY], 2 if session == "2026-11-30" else 0, session)
            self.assertLess(counts[HISTORY], listed, session)
            self.assertLessEqual(counts["market.universe.snapshot"], 1, session)


class HistoryContentTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def history(self, stream, ticker):
        return next(e["payload"] for e in of(stream, HISTORY) if e["payload"]["instrument_id"] == ticker)

    def test_a_history_carries_the_closes_the_full_streams_bars_would_over_the_window(self):
        ranker = FakeRanker(members=("BBB", "CCC", "DDD"), window=4)
        stream = compact(self.tmp.name, ranker=ranker)
        full = run(self.tmp.name, optional=[], universe_members=True)
        window = ["2026-11-24", "2026-11-25", "2026-11-27", "2026-11-30"]
        for ticker in ("BBB", "CCC", "DDD"):   # BBB splits on 11-25, CCC goes ex a dividend on 11-27
            traded = [s for s in window if ticker in of(full, "market.session.closed", s)[0]["payload"]["instrument_ids"]]
            self.assertEqual(self.history(stream, ticker), {
                "instrument_id": ticker, "session": "2026-11-30", "adjusted_base": "2026-11-23",
                "closes": [{"session": s, "close": bar_of(full, ticker, s)["adjusted"]["close"]} for s in traded]})
        self.assertEqual([c["session"] for c in self.history(stream, "DDD")["closes"]], window[1:])   # no 11-24 bar

    def test_the_ranker_is_asked_with_each_snapshot_and_the_session_after_it(self):
        ranker = FakeRanker()
        stream = compact(self.tmp.name, ranker=ranker)
        snap = of(stream, "market.universe.snapshot")[0]["payload"]
        self.assertEqual(ranker.requests, [{"universe": snap, "next_session": "2026-12-01"}])


    def test_the_calendar_reaches_back_to_the_base_that_histories_reach(self):
        stream = compact(self.tmp.name, first=D(2026, 11, 25), base=FIRST)
        sessions = of(stream, "market.session.calendar")[0]["payload"]["sessions"]
        self.assertEqual(sessions[0], "2026-11-23")
        self.assertEqual(of(stream, "market.bar.completed")[0]["event_time"][:10], "2026-11-25")
        self.assertEqual([c["session"] for c in self.history(stream, "AAA")["closes"]],
                         ["2026-11-24", "2026-11-25", "2026-11-27", "2026-11-30"])

    def test_a_member_with_bad_data_has_those_sessions_left_out_of_its_history_and_is_named(self):
        break_bar(self.tmp.name, "2026-11-25", "AAA")
        unadjustable_ddd(self.tmp.name)
        excluded = {}
        stream = compact(self.tmp.name, ranker=FakeRanker(members=("AAA", "DDD")), excluded=excluded)
        self.assertEqual([c["session"] for c in self.history(stream, "AAA")["closes"]],
                         ["2026-11-24", "2026-11-27", "2026-11-30"])
        self.assertEqual([c["session"] for c in self.history(stream, "DDD")["closes"]],
                         ["2026-11-27", "2026-11-30"])   # left out of 11-25, the ex-date it cannot be adjusted on
        self.assertEqual({t: [(r.first, r.last) for r in runs] for t, runs in excluded.items()},
                         {"AAA": [(D(2026, 11, 25), D(2026, 11, 25))], "DDD": [(D(2026, 11, 25), D(2026, 11, 25))]})

    def test_a_member_left_out_of_the_decision_session_is_left_out_of_the_snapshot_ranked(self):
        """The compact stream's snapshot is the full stream's: a member left
        out of the session for bad data is not in the snapshot the ranker
        ranks, so no history is asked for it."""
        rewrite(self.tmp.name, "universe", "2026-11-30", "AAA", h=1.0)   # a high below the close
        ranker, excluded = FakeRanker(members=("BBB",)), {}
        stream = compact(self.tmp.name, ranker=ranker, excluded=excluded)
        full = run(self.tmp.name, optional=[], universe_members=True)
        snap = of(stream, "market.universe.snapshot")[0]["payload"]
        self.assertEqual([r["instrument_id"] for r in snap["rows"]], ["BBB", "CCC", "DDD"])
        self.assertEqual(snap, of(full, "market.universe.snapshot")[0]["payload"])
        self.assertEqual(ranker.requests, [{"universe": snap, "next_session": "2026-12-01"}])
        self.assertEqual([(r.first, r.last) for r in excluded["AAA"]], [(D(2026, 11, 30), D(2026, 11, 30))])

    def test_a_listed_stock_of_any_type_left_out_of_the_session_is_left_out_of_the_snapshot(self):
        """The run ranks its configured listing type, which the stream does
        not know, so a listed stock of any type with bad data on a session is
        left out of that session's snapshot, and no snapshot holds a stock
        left out of its session."""
        path = os.path.join(self.tmp.name, "universe_2026-11-30.json")
        rec = record.read(path)
        os.remove(path)
        next(t for t in rec["tickers"] if t["ticker"] == "CCC")["type"] = "ETF"
        record.write(self.tmp.name, rec)
        rewrite(self.tmp.name, "universe", "2026-11-30", "CCC", h=1.0)   # a high below the close
        excluded = {}
        stream = compact(self.tmp.name, ranker=FakeRanker(members=("BBB",)), excluded=excluded)
        snap = of(stream, "market.universe.snapshot")[0]["payload"]
        self.assertEqual([r["instrument_id"] for r in snap["rows"]], ["AAA", "BBB", "DDD"])
        self.assertEqual([(r.first, r.last) for r in excluded["CCC"]], [(D(2026, 11, 30), D(2026, 11, 30))])
        for e in of(stream, "market.universe.snapshot"):
            day = D.fromisoformat(e["payload"]["session"])
            left_out = {t for t, runs in excluded.items() if any(r.first <= day <= r.last for r in runs)}
            self.assertFalse(left_out & {r["instrument_id"] for r in e["payload"]["rows"]}, day)


class CompactRefusalTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_a_compact_stream_is_a_momentum_stream_without_member_bars(self):
        with self.assertRaises(ValueError):
            compact(self.tmp.name, universe_members=True)
        with self.assertRaises(ValueError):
            compact(self.tmp.name, required=["SPY"], engine="boost")

    def test_an_unusable_ranker_answer_writes_nothing(self):
        def answering(*answers):
            return lambda requests: list(answers)
        good = {"session": "2026-11-30", "decision_session": True, "window": 4, "members": ["AAA"]}
        for name, ranker in {
            "too few answers": answering(),
            "another session": answering(dict(good, session="2026-11-27")),
            "a member not in the snapshot": answering(dict(good, members=["ZZZ"])),
            "a repeated member": answering(dict(good, members=["AAA", "AAA"])),
            "members off a decision session": answering(dict(good, decision_session=False)),
            "no window": answering(dict(good, window=0)),
            "not an answer": answering(["AAA"]),
        }.items():
            with self.subTest(name), self.assertRaises(encode.RankerFailed):
                compact(self.tmp.name, ranker=ranker)

    def test_a_ranker_that_fails_or_answers_garbage_writes_nothing(self):
        for argv in (["false"], ["echo", "{"], ["/nonexistent/universe-rank"]):
            with self.subTest(argv), self.assertRaises(encode.RankerFailed):
                compact(self.tmp.name, ranker=encode.go_ranker(argv))

    def test_the_cli_writes_a_compact_stream_through_the_ranker_named(self):
        out = os.path.join(self.tmp.name, "compact.jsonl")
        ranker = FakeRanker()
        with mock.patch("marketdata.encode.go_ranker", return_value=ranker) as named:
            status = cli.main(["encode", "--out", self.tmp.name, "--first", "2026-11-23", "--asof", "2026-11-30",
                               "--instruments", "SPY,IEF", "--ranker", "bin/universe-rank -config 'my config.json'",
                               "--configuration-hash", "hash-1", "--strategy-version", "momentum/test",
                               "--run-id", "run-1", "--stream", out])
        self.assertEqual(status, 0)
        named.assert_called_once_with(["bin/universe-rank", "-config", "my config.json"])
        self.assertEqual([json.loads(line) for line in Path(out).read_text().splitlines()], compact(self.tmp.name))


class OneSessionAtATimeTest(unittest.TestCase):
    """The compact stream is read and ranked one session at a time, so the
    data job never holds every instrument of every session
    (docs/live-trading/lessons-2026-10.md L1), and the stream it writes is
    the one encode returns."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name, sector_session="2026-11-27")
        # A second universe recording, beside 11-27's grouped one (they agree).
        tickers = [{"ticker": t, "type": "CS", "market": "stocks"} for t in ("AAA", "BBB", "CCC", "DDD", "ZZZ")]
        record.write(self.tmp.name, {"kind": "universe", "source": "massive", "session": "2026-11-27",
                                     "fetched_at": "2026-11-27T23:30:00Z", "adjusted": False, "ticker_type": "CS",
                                     "bars": bars_for(D(2026, 11, 27)), "tickers": tickers})

    def tearDown(self):
        self.tmp.cleanup()

    def test_the_ranker_is_asked_one_session_at_a_time(self):
        calls, ranker = [], FakeRanker()

        def asked(requests):
            calls.append([r["universe"]["session"] for r in requests])
            return ranker(requests)
        compact(self.tmp.name, ranker=asked)
        self.assertEqual(calls, [["2026-11-27"], ["2026-11-30"]])

    def test_the_streamed_envelopes_are_the_bytes_encode_returns(self):
        break_bar(self.tmp.name, "2026-11-25", "AAA")
        unadjustable_ddd(self.tmp.name)
        ranker = FakeRanker(members=("AAA", "CCC", "DDD"))
        listed, streamed = {}, {}
        whole, lazy = os.path.join(self.tmp.name, "whole.jsonl"), os.path.join(self.tmp.name, "lazy.jsonl")
        encode.write_stream(whole, compact(self.tmp.name, ranker=ranker, excluded=listed))
        envelopes = encode.iter_encode(self.tmp.name, FIRST, D(2026, 11, 30), IDENTITY, required=["SPY", "IEF"],
                                       optional=[], ranker=ranker, excluded=streamed)
        self.assertEqual(streamed, listed)   # filled before the first envelope is written
        encode.write_stream(lazy, envelopes)
        self.assertEqual(Path(lazy).read_bytes(), Path(whole).read_bytes())

    def test_a_refusal_is_raised_before_any_envelope_is_written(self):
        rewrite(self.tmp.name, "universe", "2026-11-30", "SPY", c=None)   # a named instrument's bad row
        with self.assertRaises(encode.Malformed):
            encode.iter_encode(self.tmp.name, FIRST, D(2026, 11, 30), IDENTITY, required=["SPY", "IEF"],
                               optional=[], ranker=FakeRanker())


# A recorded fixture for the compact stream's bytes: sixteen months of
# universe sessions (the first twenty grouped only), so the last four
# decisions have full 260-session windows; a retracted session; a split on
# a Saturday and one on a weekday; a dividend; a dividend the encoder cannot
# adjust; a malformed bar inside a window; a stock priced near 1e30; and a
# ranked set that churns: C05 is ranked in January 2026, not February, and
# again in March. Synthetic prices only.
CHURN_FIRST, CHURN_LAST = D(2025, 1, 2), D(2026, 4, 30)
CHURN_RETRACTED = D(2025, 6, 11)
CHURN_NAMES = ["C{:02d}".format(n) for n in range(12)] + ["BIG"]
CHURN_DEFAULT = ["BIG", "C00", "C01", "C02", "C03", "C04"]
CHURN_MEMBERS = {"2026-01": CHURN_DEFAULT + ["C05"], "2026-02": CHURN_DEFAULT + ["C06"],
                 "2026-03": CHURN_DEFAULT + ["C05", "C06"]}
CHURN_WINDOW = 260
# The sha256 of the streams the encoder wrote, for this fixture, before it
# read the recordings one session at a time (git merge-base of that change,
# adapter/marketdata/encode.py), and the exclusions it reported.
CHURN_WARM_UP_SHA256 = "f32373ec7de1492956e65017534ace9860c28f71fec0c163bf4c69696719e33f"
CHURN_DAILY_SHA256 = "d9bfb434befd87d7c89daeae869f9f39ea6fa661850e83caff881945dbee00af"
CHURN_EXCLUDED = {"C03": [("2026-02-18", "2026-02-18", 1)], "C06": [("2026-02-10", "2026-02-10", 1)]}


def churn_days():
    return [d for d in nyse_calendar.sessions(CHURN_FIRST, CHURN_LAST) if d != CHURN_RETRACTED]


def churn_close(n, i):
    return 10.0 + n + 0.05 * ((i * (n + 3)) % 17)


def build_churn(d):
    days = churn_days()
    tickers = [{"ticker": t, "type": "CS", "market": "stocks"} for t in CHURN_NAMES]
    split_c04, split_big = D(2025, 9, 15), D(2025, 8, 1)   # C04's split executes Saturday 09-13
    last_sectors = None
    for i, day in enumerate(days):
        bars = [row("SPY", 600.0 + i * 0.1), row("IEF", 95.0, v=5000)]
        for n, t in enumerate(CHURN_NAMES[:-1]):
            c = churn_close(n, i) / (2 if t == "C04" and day >= split_c04 else 1)
            bars.append(row(t, c, v=1000 * (n + 1)))
        big = 1e30 * (1 + (i % 7) * 0.001) / (2 if day >= split_big else 1)
        bars.append(row("BIG", big, v=1, spread=big * 0.01))
        if i == 280:
            next(b for b in bars if b["T"] == "C03")["h"] = 1.0   # a high below the close
        if i < 20:
            record.write(d, {"kind": "grouped", "source": "massive", "session": day.isoformat(),
                             "fetched_at": stamp(day), "adjusted": False, "bars": bars})
        else:
            record.write(d, {"kind": "universe", "source": "massive", "session": day.isoformat(),
                             "fetched_at": stamp(day), "adjusted": False, "ticker_type": "CS", "bars": bars,
                             "tickers": tickers})
        if last_sectors is None or (day - last_sectors).days >= 5:
            last_sectors = day
            record.write(d, {"kind": "sectors-iwv", "source": sectors.IWV_SOURCE, "session": day.isoformat(),
                             "fetched_at": stamp(day), "url": sectors.IWV_URL, "as_of": "Jan 2, 2025",
                             "body": iwv_csv(("C00", "Information Technology", "Equity"), ("C01", "Financials", "Equity"))})
            record.write(d, {"kind": "sectors-nasdaq", "source": sectors.NASDAQ_SOURCE, "session": day.isoformat(),
                             "fetched_at": stamp(day), "url": sectors.NASDAQ_URL,
                             "rows": json.loads(screener(("C02", "Energy"), ("C00", "Finance")))["data"]["rows"]})
    record.write(d, {"kind": "splits", "source": "massive", "session": CHURN_LAST.isoformat(),
                     "from": CHURN_FIRST.isoformat(), "fetched_at": stamp(CHURN_LAST),
                     "rows": [{"id": "S1", "ticker": "C04", "execution_date": "2025-09-13", "split_from": 1, "split_to": 2},
                              {"id": "S2", "ticker": "BIG", "execution_date": "2025-08-01", "split_from": 1, "split_to": 2}]})
    record.write(d, {"kind": "dividends", "source": "massive", "session": CHURN_LAST.isoformat(),
                     "from": CHURN_FIRST.isoformat(), "fetched_at": stamp(CHURN_LAST),
                     "rows": [{"id": "V1", "ticker": "C02", "ex_dividend_date": "2025-10-15", "cash_amount": 0.5},
                              {"id": "V2", "ticker": "C06", "ex_dividend_date": "2026-02-10", "cash_amount": 99.0}]})
    encode.record_retraction(d, CHURN_RETRACTED, "NYSE closed for a national day of mourning",
                             recorded_at="2025-06-10T23:00:00Z")


def churn_ranker(requests):
    out = []
    for r in requests:
        session, nxt = r["universe"]["session"], r["next_session"]
        decision = session[:7] != nxt[:7]
        out.append({"session": session, "decision_session": decision, "window": CHURN_WINDOW,
                    "members": CHURN_MEMBERS.get(session[:7], CHURN_DEFAULT) if decision else []})
    return out


def churn_stream(d, first, path, encoder=encode, excluded=None):
    envelopes = encoder.encode(d, first, CHURN_LAST, IDENTITY, required=["SPY", "IEF"], optional=["C01"],
                               base=CHURN_FIRST, ranker=churn_ranker, excluded=excluded)
    encoder.write_stream(path, envelopes)
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def exclusions(excluded):
    return {t: [(r.first.isoformat(), r.last.isoformat(), r.sessions) for r in runs] for t, runs in sorted(excluded.items())}


class RecordedStreamTest(unittest.TestCase):
    """The compact stream is byte for byte the one the encoder wrote before
    it read the recordings one session at a time, on a fixture with churn,
    actions, bad data and a retraction."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        build_churn(cls.tmp.name)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def test_the_warm_up_and_the_daily_stream_are_the_recorded_bytes(self):
        excluded = {}
        self.assertEqual(churn_stream(self.tmp.name, CHURN_FIRST, os.path.join(self.tmp.name, "warm.jsonl"),
                                      excluded=excluded), CHURN_WARM_UP_SHA256)
        self.assertEqual(exclusions(excluded), CHURN_EXCLUDED)
        self.assertEqual(churn_stream(self.tmp.name, CHURN_LAST, os.path.join(self.tmp.name, "daily.jsonl")),
                         CHURN_DAILY_SHA256)

    def test_a_name_ranked_again_after_a_month_out_keeps_its_whole_window(self):
        path = os.path.join(self.tmp.name, "churn.jsonl")
        churn_stream(self.tmp.name, CHURN_FIRST, path)
        stream = [json.loads(line) for line in Path(path).read_text().splitlines()]
        histories = {(e["payload"]["instrument_id"], e["payload"]["session"]): e["payload"] for e in of(stream, HISTORY)}
        march = "2026-03-31"
        self.assertNotIn(("C05", "2026-02-27"), histories)
        window = [d.isoformat() for d in churn_days() if d.isoformat() <= march][-CHURN_WINDOW:]
        self.assertEqual([c["session"] for c in histories[("C05", march)]["closes"]], window)
        self.assertEqual(len(window), CHURN_WINDOW)
        # C03's malformed bar is inside this window, so that one session is missing.
        c03 = [c["session"] for c in histories[("C03", march)]["closes"]]
        self.assertEqual(len(c03), CHURN_WINDOW - 1)


class OverflowTest(unittest.TestCase):
    def test_an_adjusted_close_beyond_a_float_refuses_the_stream_and_writes_nothing(self):
        """A finite close that its split multiplier pushes past the largest
        float cannot be written as JSON, so the stream is refused, as the
        encoder refused it before it read one session at a time."""
        with tempfile.TemporaryDirectory() as d:
            build(d)
            rewrite(d, "grouped", "2026-11-27", "BBB", o=1e308, h=1e308, l=1e307, c=1e308)   # BBB splits 2-for-1 on 11-25
            with self.assertRaises(ValueError):
                compact(d)
            path = os.path.join(d, "overflow.jsonl")
            with self.assertRaises(ValueError):
                encode.write_stream(path, encode.iter_encode(d, FIRST, D(2026, 11, 30), IDENTITY, required=["SPY", "IEF"],
                                                             ranker=FakeRanker()))
            self.assertFalse(os.path.exists(path))


class FrozenRecordingsTest(unittest.TestCase):
    """The recordings are listed once, when the encode starts: a recording
    written meanwhile is not read, and one that disappears or changes refuses
    the stream (docs/live-trading/lessons-2026-10.md L1 keeps the passes
    separate, so they must read the same files)."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build_churn(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def during_ranking(self, change):
        done = []

        def rank(requests):
            if not done:
                done.append(change())
            return churn_ranker(requests)
        return rank

    def encode(self, ranker, path):
        envelopes = encode.iter_encode(self.tmp.name, CHURN_FIRST, CHURN_LAST, IDENTITY, required=["SPY", "IEF"],
                                       optional=["C01"], base=CHURN_FIRST, ranker=ranker)
        encode.write_stream(path, envelopes)

    def test_a_universe_recording_written_during_the_encode_is_not_read(self):
        clean = os.path.join(self.tmp.name, "clean.jsonl")
        churn_stream(self.tmp.name, CHURN_FIRST, clean)
        day = churn_days()[5]   # grouped only

        def add():
            rec = record.read(os.path.join(self.tmp.name, "grouped_{}.json".format(day)))
            record.write(self.tmp.name, dict(rec, kind="universe", ticker_type="CS",
                                             tickers=[{"ticker": t, "type": "CS", "market": "stocks"} for t in CHURN_NAMES]))
        path = os.path.join(self.tmp.name, "during.jsonl")
        self.encode(self.during_ranking(add), path)
        self.assertEqual(Path(path).read_bytes(), Path(clean).read_bytes())

    def test_a_recording_that_disappears_during_the_encode_refuses_the_stream(self):
        gone = os.path.join(self.tmp.name, "universe_{}.json".format(churn_days()[100]))
        path = os.path.join(self.tmp.name, "gone.jsonl")
        with self.assertRaises(encode.Conflict) as caught:
            self.encode(self.during_ranking(lambda: os.remove(gone)), path)
        self.assertIn(os.path.basename(gone), str(caught.exception))
        self.assertFalse(os.path.exists(path))

    def test_a_recording_rewritten_during_the_encode_refuses_the_stream(self):
        name = "universe_{}.json".format(churn_days()[100])

        def rewrite_it():
            rec = record.read(os.path.join(self.tmp.name, name))
            os.remove(os.path.join(self.tmp.name, name))
            rec["bars"][0]["v"] += 1
            record.write(self.tmp.name, rec)
        with self.assertRaises(encode.Conflict) as caught:
            self.encode(self.during_ranking(rewrite_it), os.path.join(self.tmp.name, "rewritten.jsonl"))
        self.assertIn(name, str(caught.exception))


class ListedReadTest(unittest.TestCase):
    """Each listed recording is opened first and its open file checked
    against the listing, so a file replaced as it is read gives either the
    listed contents or a Conflict, never the replacement; sector recordings
    are read the same way."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build_churn(self.tmp.name)
        self.clean = os.path.join(self.tmp.name, "clean.jsonl")
        churn_stream(self.tmp.name, CHURN_FIRST, self.clean)

    def tearDown(self):
        self.tmp.cleanup()

    def encode(self, ranker, path):
        envelopes = encode.iter_encode(self.tmp.name, CHURN_FIRST, CHURN_LAST, IDENTITY, required=["SPY", "IEF"],
                                       optional=["C01"], base=CHURN_FIRST, ranker=ranker)
        encode.write_stream(path, envelopes)

    def replace(self, name):
        path = os.path.join(self.tmp.name, name)
        rec = record.read(path)
        os.remove(path)
        if "bars" in rec:
            next(b for b in rec["bars"] if b["T"] == "SPY")["c"] += 1.0   # a named bar, written as it is read
        else:
            rec["as_of"] = rec.get("as_of", "") + " "   # a sector recording, written again
        record.write(self.tmp.name, rec)

    def armed_at_the_last_ranking(self, action):
        def rank(requests):
            if requests[-1]["universe"]["session"] == CHURN_LAST.isoformat():
                action()
            return churn_ranker(requests)
        return rank

    def test_a_bar_recording_replaced_as_it_is_read_is_never_read_as_the_replacement(self):
        name = "universe_{}.json".format(churn_days()[100])
        target = os.path.join(self.tmp.name, name)
        armed, real_stat, real_fstat = [], os.stat, os.fstat
        original = real_stat(target).st_ino

        def after(result):
            if armed and armed[0] and result.st_ino == original:
                armed[0] = False
                self.replace(name)
            return result

        def stat(path, *a, **kw):
            return after(real_stat(path, *a, **kw))

        def fstat(fd):
            return after(real_fstat(fd))
        path = os.path.join(self.tmp.name, "raced.jsonl")
        with mock.patch("marketdata.encode.os.stat", stat), mock.patch("marketdata.encode.os.fstat", fstat):
            try:
                self.encode(self.armed_at_the_last_ranking(lambda: armed.append(True)), path)
            except encode.Conflict as e:
                self.assertIn(name, str(e))
                return
        self.assertEqual(Path(path).read_bytes(), Path(self.clean).read_bytes())

    def latest_sector(self, kind):
        day = churn_days()[100]
        names = sorted(n for n in os.listdir(self.tmp.name)
                       if n.startswith(kind + "_") and n[len(kind) + 1:-5] <= day.isoformat())
        return names[-1]

    def test_a_sector_recording_replaced_during_the_encode_refuses_the_stream(self):
        name = self.latest_sector("sectors-iwv")
        with self.assertRaises(encode.Conflict) as caught:
            self.encode(self.armed_at_the_last_ranking(lambda: self.replace(name)),
                        os.path.join(self.tmp.name, "sector-replaced.jsonl"))
        self.assertIn(name, str(caught.exception))

    def test_a_sector_recording_removed_during_the_encode_refuses_the_stream(self):
        name = self.latest_sector("sectors-nasdaq")
        with self.assertRaises(encode.Conflict) as caught:
            self.encode(self.armed_at_the_last_ranking(lambda: os.remove(os.path.join(self.tmp.name, name))),
                        os.path.join(self.tmp.name, "sector-removed.jsonl"))
        self.assertIn(name, str(caught.exception))


class LongerLaterWindowTest(unittest.TestCase):
    """A name's closes are kept from the earliest start of all its windows,
    not only its first decision's: a later decision may ask for a longer
    window."""

    def test_a_later_longer_window_is_held_whole_in_a_stream_that_starts_late(self):
        def ranker(requests):
            out = churn_ranker(requests)
            for a in out:
                a["window"] = 5 if a["session"].startswith("2026-03") else CHURN_WINDOW
            return out
        with tempfile.TemporaryDirectory() as d:
            build_churn(d)
            late, whole = (encode.encode(d, first, CHURN_LAST, IDENTITY, required=["SPY", "IEF"], optional=["C01"],
                                         base=CHURN_FIRST, ranker=ranker)
                           for first in (D(2026, 3, 2), CHURN_FIRST))
            late, whole = list(late), list(whole)
        april = lambda stream: {e["payload"]["instrument_id"]: e["payload"] for e in of(stream, HISTORY)
                                if e["payload"]["session"] == "2026-04-30"}
        self.assertEqual(len(april(late)["C00"]["closes"]), CHURN_WINDOW)
        self.assertEqual(april(late), april(whole))


class RankerRefusalTest(unittest.TestCase):
    """A ranker that refuses, or does not answer in time, refuses the stream
    with the session it was asked about."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_a_refusal_names_the_session(self):
        with self.assertRaises(encode.RankerFailed) as caught:
            compact(self.tmp.name, ranker=encode.go_ranker(["sh", "-c", "echo 'line 1: no' >&2; exit 1"]))
        self.assertIn("universe 2026-11-30", str(caught.exception))
        self.assertIn("line 1: no", str(caught.exception))

    def test_a_ranker_that_does_not_answer_in_time_names_the_session(self):
        started = time.monotonic()
        with self.assertRaises(encode.RankerFailed) as caught:
            compact(self.tmp.name, ranker=encode.go_ranker(["sleep", "30"], timeout=0.5))
        self.assertLess(time.monotonic() - started, 10)
        self.assertIn("universe 2026-11-30", str(caught.exception))
        self.assertIn("did not answer within 0.5 s", str(caught.exception))

    def test_the_go_ranker_waits_sixty_seconds_by_default(self):
        self.assertEqual(encode.RANKER_TIMEOUT_SECONDS, 60)


def retract(d, day, reason="NYSE closed for a national day of mourning"):
    """Records the exchange's cancellation of ``day`` and removes its bars."""
    os.remove(os.path.join(d, "grouped_{}.json".format(day)))
    encode.record_retraction(d, D.fromisoformat(day), reason, recorded_at="2026-11-24T23:00:00Z")


class RetractionTest(unittest.TestCase):
    """A session the exchange cancelled: recorded once, written as a
    market.session.retracted right after the calendar, and no session at all
    to the rest of the stream."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        build(self.tmp.name)
        retract(self.tmp.name, "2026-11-25")

    def tearDown(self):
        self.tmp.cleanup()

    def test_the_calendar_is_followed_by_the_retraction_and_the_session_is_skipped(self):
        stream = compact(self.tmp.name)
        self.assertEqual([e["type"] for e in stream[:2]], ["market.session.calendar", "market.session.retracted"])
        self.assertIn("2026-11-25", stream[0]["payload"]["sessions"])   # the NYSE rules, as every run writes them
        self.assertEqual((stream[1]["payload"], stream[1]["recorded_at"]), (
            {"source": "marketdata-retraction", "session": "2026-11-25", "reason": "NYSE closed for a national day of mourning"},
            "2026-11-24T23:00:00Z"))
        self.assertEqual([e["event_time"][:10] for e in of(stream, "market.session.closed")],
                         ["2026-11-23", "2026-11-24", "2026-11-27", "2026-11-30"])
        window = [c["session"] for c in next(e for e in of(stream, HISTORY))["payload"]["closes"]]
        self.assertEqual(window, ["2026-11-23", "2026-11-24", "2026-11-27", "2026-11-30"])

    def test_a_retracted_session_with_bars_recorded_is_refused(self):
        record_bars = os.path.join(self.tmp.name, "grouped_2026-11-27.json")
        os.rename(record_bars, os.path.join(self.tmp.name, "kept.json"))
        encode.record_retraction(self.tmp.name, D(2026, 11, 27), "test", recorded_at="2026-11-24T23:00:00Z")
        os.rename(os.path.join(self.tmp.name, "kept.json"), record_bars)
        with self.assertRaises(encode.Conflict):
            compact(self.tmp.name)

    def test_a_retraction_is_recorded_once(self):
        with self.assertRaises(record.AlreadyRecorded):
            encode.record_retraction(self.tmp.name, D(2026, 11, 25), "again", recorded_at="2026-11-24T23:00:00Z")
        with self.assertRaises(ValueError):
            encode.record_retraction(self.tmp.name, D(2026, 11, 28), "a Saturday is no session", recorded_at="2026-11-24T23:00:00Z")

    def test_the_cli_records_a_retraction_without_a_key(self):
        with mock.patch("marketdata.massive.load_key", side_effect=AssertionError("key loaded")):
            status = cli.main(["retract", "--out", self.tmp.name, "--session", "2026-11-27", "--reason", "storm"])
        self.assertEqual(status, 0)
        self.assertEqual(encode.load_retractions(self.tmp.name)[D(2026, 11, 27)]["reason"], "storm")
        self.assertEqual(cli.main(["retract", "--out", self.tmp.name, "--session", "2026-11-27", "--reason", "x"]), 1)
        self.assertEqual(cli.main(["retract", "--out", self.tmp.name, "--session", "2026-11-26"]), 1)

    def test_the_retracted_stream_satisfies_the_go_validators(self):
        path = os.path.join(self.tmp.name, "retracted.jsonl")
        encode.write_stream(path, compact(self.tmp.name, ranker=go_ranker))
        with open(path) as f:
            result = subprocess.run(["go", "run", "./adapter/marketdata/tests/testdata/envelope_contract.go"],
                                    cwd=REPO, stdin=f, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


# The Go ranker over a synthetic momentum configuration: lookbacks of 3 and
# 3 closes, a universe of 2 and the encoder's own universe and sector
# sources. Its window is the 260 sessions the engine keeps (ADR 0026 point
# 5, amended 2026-10-18), longer than the fixture's recordings.
GO_RANKER = encode.go_ranker(["go", "run", "./cmd/universe-rank", "-config",
                              os.path.join(REPO, "cmd/universe-rank/testdata/momentum.json")])


def go_ranker(requests):
    cwd = os.getcwd()
    os.chdir(REPO)
    try:
        return GO_RANKER(requests)
    finally:
        os.chdir(cwd)


class GoRankerTest(unittest.TestCase):
    """The compact stream with the Go engine's own ranking, checked against
    the Go engine's own contracts."""

    def test_the_go_ranker_names_the_two_largest_common_stocks_by_dollar_volume(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            stream = compact(d, ranker=go_ranker)
        # 11-30 closes: AAA 20.30 x 1000, BBB 50 x 400, CCC 49 x 1000, DDD 7.50 x 1000.
        self.assertEqual([e["payload"]["instrument_id"] for e in of(stream, HISTORY)], ["AAA", "CCC"])
        # Every recorded session falls inside the 260-session window.
        self.assertEqual(len(of(stream, HISTORY)[0]["payload"]["closes"]), 5)

    def test_the_compact_stream_satisfies_the_go_validators(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            path = os.path.join(d, "compact.jsonl")
            encode.write_stream(path, compact(d, ranker=go_ranker))
            with open(path) as f:
                result = subprocess.run(["go", "run", "./adapter/marketdata/tests/testdata/envelope_contract.go"],
                                        cwd=REPO, stdin=f, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def decide_both_ways(self, spoil=None):
        """Encodes the recordings both ways, feeds each to a fresh Go momentum
        run (the helper fails unless both emit byte-identical records), and
        returns the decisions and the two streams' sizes."""
        with tempfile.TemporaryDirectory() as d:
            build(d)
            if spoil:
                spoil(d)
            full, small = os.path.join(d, "full.jsonl"), os.path.join(d, "compact.jsonl")
            encode.write_stream(full, run(d, optional=[], universe_members=True))
            encode.write_stream(small, compact(d, ranker=go_ranker))
            result = subprocess.run(["go", "run", "./adapter/marketdata/tests/testdata/decision_equivalence.go",
                                     "cmd/universe-rank/testdata/momentum.json", full, small],
                                    cwd=REPO, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            with open(full) as f, open(small) as g:
                sizes = (sum(1 for _ in f), sum(1 for _ in g))
        return [json.loads(line) for line in result.stdout.splitlines()], sizes

    def test_the_momentum_engine_decides_the_same_from_the_compact_stream_as_from_full_bars(self):
        """The acceptance demo: the same recordings encoded both ways, each
        fed to a fresh Go momentum run, emit byte-identical records."""
        decisions, sizes = self.decide_both_ways()
        self.assertEqual(len(decisions), 1)
        names = {n["instrument_id"]: n["status"] for n in decisions[0]["momentum"]["names"]}
        self.assertEqual(sorted(names), ["AAA", "CCC"])
        self.assertEqual(decisions[0]["momentum"]["close_session"], "2026-11-30")
        self.assertLess(sizes[1], sizes[0])

    def test_a_member_spoiled_on_the_decision_session_gives_way_to_the_next_by_dollar_volume_in_both_streams(self):
        """A member left out of the decision session for bad data is left out
        of its snapshot, so the next stock by dollar volume is ranked in its
        place, and the bars run (which needs a bar for every member it ranks)
        decides exactly as the compact run does (ADR 0026 point 5)."""
        spoil = lambda d: rewrite(d, "universe", "2026-11-30", "AAA", h=1.0)   # a high below the close
        decisions, _ = self.decide_both_ways(spoil)
        self.assertEqual(len(decisions), 1)
        # The fixture's run has no account and flat closes, so the decision is
        # withheld past the ranking, never for a missing bar or history.
        self.assertNotIn(decisions[0].get("withheld_reason"), ("missing-bars", "histories-missing"))
        self.assertEqual(decisions[0]["momentum"]["close_session"], "2026-11-30")
        self.assertEqual(sorted(n["instrument_id"] for n in decisions[0]["momentum"]["names"]), ["BBB", "CCC"])


if __name__ == "__main__":
    unittest.main()
