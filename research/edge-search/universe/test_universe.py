"""Tests for the D8 universe built from Massive's daily bars (massive.py),
QuantConnect's logged universe (qc_universe.py) and their comparison
(compare.py). Every input is synthetic; no market data."""
import datetime as _dt
import io
import json
import os
import sys
import tempfile
import types
import unittest
import urllib.error

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import compare  # noqa: E402
import massive  # noqa: E402


def _bar(t, c, v, otc=False):
    row = {"T": t, "c": c, "v": v, "o": c, "h": c, "l": c, "t": 0}
    if otc:
        row["otc"] = True
    return row


class RankTest(unittest.TestCase):
    def test_ranks_common_stock_by_dollar_volume_at_or_above_the_floor(self):
        bars = [_bar("AAA", 10.0, 100), _bar("BBB", 50.0, 100), _bar("CCC", 4.99, 10_000),
                _bar("DDD", 5.0, 300), _bar("FUND", 100.0, 1_000)]
        common = {"AAA", "BBB", "CCC", "DDD"}
        # BBB 5000, DDD 1500, AAA 1000; CCC under $5; FUND not common stock.
        self.assertEqual(massive.rank_universe(bars, common, size=10), ["BBB", "DDD", "AAA"])

    def test_keeps_the_top_size_and_breaks_ties_by_ticker(self):
        bars = [_bar("ZZZ", 10.0, 100), _bar("AAA", 10.0, 100), _bar("MMM", 20.0, 100)]
        self.assertEqual(massive.rank_universe(bars, {"ZZZ", "AAA", "MMM"}, size=2), ["MMM", "AAA"])

    def test_skips_otc_rows_and_rows_missing_a_close_or_volume(self):
        bars = [_bar("AAA", 10.0, 100, otc=True), {"T": "BBB", "v": 100}, {"T": "CCC", "c": 10.0},
                _bar("DDD", 10.0, 1)]
        self.assertEqual(massive.rank_universe(bars, {"AAA", "BBB", "CCC", "DDD"}), ["DDD"])

    def test_common_tickers_keeps_only_the_chosen_types_of_us_stocks(self):
        pages = [{"results": [
            {"ticker": "AAA", "type": "CS", "market": "stocks", "locale": "us"},
            {"ticker": "ADR", "type": "ADRC", "market": "stocks", "locale": "us"},
            {"ticker": "ETF1", "type": "ETF", "market": "stocks", "locale": "us"},
            {"ticker": "PFD", "type": "PFD", "market": "stocks", "locale": "us"},
            {"ticker": "OTC1", "type": "CS", "market": "otc", "locale": "us"},
        ]}, {"results": [{"ticker": "BBB", "type": "CS", "market": "stocks", "locale": "us"}]}, {}]
        self.assertEqual(massive.common_tickers(pages), {"AAA", "ADR", "BBB"})
        self.assertEqual(massive.common_tickers(pages, types=("CS",)), {"AAA", "BBB"})


class SessionTest(unittest.TestCase):
    def test_candidate_sessions_walk_back_from_the_day_before_the_month_over_weekdays(self):
        # 2026-03-01 is a Sunday: the last weekdays before it are Fri 27 Feb, Thu 26 Feb.
        days = massive.candidate_sessions(2026, 3)
        self.assertEqual(days[:2], [_dt.date(2026, 2, 27), _dt.date(2026, 2, 26)])
        self.assertTrue(all(d.weekday() < 5 for d in days))

    def test_last_session_skips_days_with_no_bars(self):
        empty = {_dt.date(2026, 2, 27)}

        class Fake:
            def grouped(self, day):
                return {"resultsCount": 0, "results": []} if day in empty else {
                    "resultsCount": 1, "results": [_bar("AAA", 10.0, 1)]}
        day, bars = massive.last_session_before(Fake(), 2026, 3)
        self.assertEqual(day, _dt.date(2026, 2, 26))
        self.assertEqual([b["T"] for b in bars], ["AAA"])


class _Resp(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class ClientTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.calls, self.slept, self.now = [], [], [100.0]

    def client(self, bodies):
        def opener(req, timeout=None):
            self.calls.append(req)
            return _Resp(json.dumps(bodies.pop(0)).encode())
        return massive.Client("k3y", self.dir, min_interval=12.0, opener=opener,
                              sleep=lambda s: (self.slept.append(s), self.now.__setitem__(0, self.now[0] + s)),
                              clock=lambda: self.now[0])

    def test_sends_the_key_as_a_header_never_in_the_url_or_the_cache(self):
        c = self.client([{"resultsCount": 0, "results": []}])
        c.grouped(_dt.date(2026, 2, 27))
        req = self.calls[0]
        self.assertEqual(req.get_header("Authorization"), "Bearer k3y")
        self.assertNotIn("k3y", req.full_url)
        self.assertIn("/v2/aggs/grouped/locale/us/market/stocks/2026-02-27", req.full_url)
        self.assertIn("adjusted=false", req.full_url)
        for name in os.listdir(self.dir):
            with open(os.path.join(self.dir, name)) as f:
                self.assertNotIn("k3y", name + f.read())

    def test_a_cached_response_is_not_fetched_again(self):
        c = self.client([{"resultsCount": 1, "results": [_bar("AAA", 1.0, 1)]}])
        first = c.grouped(_dt.date(2026, 2, 27))
        again = c.grouped(_dt.date(2026, 2, 27))
        self.assertEqual(first, again)
        self.assertEqual(len(self.calls), 1)

    def test_requests_are_spaced_by_the_minimum_interval(self):
        c = self.client([{"results": []}, {"results": []}])
        c.grouped(_dt.date(2026, 2, 26))
        c.grouped(_dt.date(2026, 2, 27))
        self.assertEqual(self.slept, [12.0])

    def test_tickers_follow_next_url_and_drop_any_key_it_carries(self):
        nxt = massive.BASE + "/v3/reference/tickers?cursor=abc&apiKey=k3y"
        c = self.client([{"results": [{"ticker": "AAA"}], "next_url": nxt}, {"results": [{"ticker": "BBB"}]}])
        pages = c.tickers(_dt.date(2026, 2, 27), "CS")
        self.assertEqual([p["results"][0]["ticker"] for p in pages], ["AAA", "BBB"])
        self.assertIn("date=2026-02-27", self.calls[0].full_url)
        self.assertIn("type=CS", self.calls[0].full_url)
        self.assertIn("cursor=abc", self.calls[1].full_url)
        self.assertNotIn("k3y", self.calls[1].full_url)

    def test_a_rate_limited_request_waits_a_minute_and_retries_once(self):
        state = {"n": 0}

        def opener(req, timeout=None):
            state["n"] += 1
            if state["n"] == 1:
                raise urllib.error.HTTPError(req.full_url, 429, "Too Many", {}, None)
            return _Resp(json.dumps({"results": []}).encode())
        c = massive.Client("k", self.dir, min_interval=0.0, opener=opener,
                           sleep=self.slept.append, clock=lambda: 0.0)
        self.assertEqual(c.grouped(_dt.date(2026, 2, 27)), {"results": []})
        self.assertEqual(self.slept, [60.0])


class QCUniverseLogTest(unittest.TestCase):
    def setUp(self):
        stub = types.ModuleType("AlgorithmImports")
        stub.QCAlgorithm = type("QCAlgorithm", (), {})
        stub.Universe = types.SimpleNamespace(Unchanged="unchanged")
        sys.modules["AlgorithmImports"] = stub
        import qc_universe
        self.module = qc_universe

    def test_lines_are_tagged_with_the_month_bounded_and_closed(self):
        names = ["T%03d" % i for i in range(120)]
        lines = self.module.universe_lines("2026-03", names)
        self.assertTrue(all(l.startswith("UNIV 2026-03 ") and l.endswith("|") and len(l) <= 190 for l in lines))
        self.assertEqual(";".join(l[len("UNIV 2026-03 "):-1] for l in lines).split(";"), names)

    def test_coarse_keeps_the_backtests_filter_and_logs_each_chosen_month_once(self):
        row = lambda t, p, dv, fund=True: types.SimpleNamespace(
            Symbol=types.SimpleNamespace(Value=t), Price=p, DollarVolume=dv, HasFundamentalData=fund)
        algo = self.module.UniverseLog()
        algo.MONTHS = ((2026, 3),)
        algo.UNIVERSE_SIZE = 2
        algo.logged, algo.counts, algo.last_month = [], {}, None
        algo.Log = algo.logged.append
        coarse = [row("AAA", 10.0, 100.0), row("BBB", 4.0, 900.0), row("CCC", 20.0, 300.0),
                  row("DDD", 30.0, 50.0), row("NOF", 50.0, 999.0, fund=False)]
        algo.Time = _dt.datetime(2026, 3, 2)
        picked = algo.Coarse(coarse)
        self.assertEqual([s.Value for s in picked], ["CCC", "AAA"])
        self.assertEqual(algo.logged, ["UNIV 2026-03 AAA;CCC|"])
        algo.Time = _dt.datetime(2026, 3, 3)
        algo.Coarse(coarse)
        algo.Time = _dt.datetime(2026, 4, 1)
        algo.Coarse(coarse)
        self.assertEqual(len(algo.logged), 1)
        self.assertEqual(algo.counts, {"2026-03": 2})


class CompareTest(unittest.TestCase):
    def test_parses_logged_months_and_refuses_a_month_whose_count_is_wrong(self):
        log = ["2026-03-01 00:00:00 UNIV 2026-03 AAA;BBB|", "2026-03-01 00:00:00 UNIV 2026-03 CCC|",
               "2026-04-01 00:00:00 UNIV 2026-04 AAA|", "2026-04-01 00:00:00 UNIV 2026-04 BBB"]
        months = compare.parse_qc_log(log)
        self.assertEqual(months["2026-03"], ["AAA", "BBB", "CCC"])
        with self.assertRaises(ValueError):
            compare.check_counts(months, {"2026-03": 3, "2026-04": 2})
        compare.check_counts({"2026-03": months["2026-03"]}, {"2026-03": 3})

    def test_overlap_normalises_share_class_separators(self):
        row = compare.overlap(["BRK.B", "AAA", "BBB", "CCC"], ["BRK/B", "AAA", "BBB", "DDD"])
        self.assertEqual(row["shared"], 3)
        self.assertEqual(row["share"], 0.75)
        self.assertEqual(row["only_qc"], ["CCC"])
        self.assertEqual(row["only_live"], ["DDD"])



class VariantsTest(unittest.TestCase):
    def test_the_log_runs_cover_24_consecutive_months_each_starting_before_its_first(self):
        sys.path.insert(0, os.path.dirname(HERE))
        import build_variants
        runs = {k: v for k, v in build_variants.VARIANTS.items() if k.startswith("U_LOG_")}
        months = [m for _, (t, p) in sorted(runs.items()) for m in p["MONTHS"]]
        self.assertEqual(len(months), 24)
        self.assertEqual(months[0], (2024, 11))
        self.assertEqual(months[-1], (2026, 10))
        for (y, m), (y2, m2) in zip(months, months[1:]):
            self.assertEqual((y2, m2), (y + 1, 1) if m == 12 else (y, m + 1))
        for _, (template, p) in runs.items():
            self.assertEqual(template, "universe/qc_universe.py")
            self.assertLess(_dt.date(*p["START"]), _dt.date(*p["MONTHS"][0], 1))
            self.assertGreater(_dt.date(*p["END"]), _dt.date(*p["MONTHS"][1], 1))

if __name__ == "__main__":
    unittest.main()
