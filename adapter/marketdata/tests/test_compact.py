"""Tests for the compact universe stream (encode.encode with a ranker): one
universe snapshot per session, bars only for the named instruments, and at
a decision session one close history per member the Go ranker names.
Synthetic data only; no network."""
import datetime as dt
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from marketdata import __main__ as cli
from marketdata import encode, record
from marketdata.tests.test_encode import ALL_SESSIONS, FIRST, REPO, bar_of, break_bar, build, of, rewrite, run, unadjustable_ddd

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
# 3 closes (a window of 4 sessions), a universe of 2 and the encoder's own
# universe and sector sources.
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
        self.assertEqual(len(of(stream, HISTORY)[0]["payload"]["closes"]), 4)

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
