"""Memory budget of the data job: a recorded session holds every instrument
Massive returns (thousands), but Boost's stream needs only its configured
instrument, so a warm-up or a daily firing keeps one recording in memory at a
time and the configured instrument's bars from the rest. The momentum stream
ranks every instrument, so it reads one session at a time and keeps the
ranked names' windows of closes (MomentumMemoryBudgetTest). Synthetic data
only; no network."""
import datetime as dt
import json
import os
import subprocess
import tempfile
import tracemalloc
import unittest
from pathlib import Path

from marketdata import __main__ as cli
from marketdata import encode, nyse_calendar, record, sectors
from marketdata.tests.test_sectors import iwv_csv, screener
from marketdata.tests.test_tidata import BASE, RUN, Job, after_settle, spy

REPO = Path(__file__).resolve().parents[3]

SESSIONS = 60        # sessions recorded
INSTRUMENTS = 3000   # instruments in each recorded session (the real day holds ~10,000)
# A parsed recording of 3,000 instruments is about 2 MB, so the whole history
# held at once is ~120 MB. One recording at a time plus the configured
# instrument's history fits well inside this budget (bytes, tracemalloc peak).
BUDGET = 16 * 1024 * 1024
PAIRED_BUDGET = 16 * 1024 * 1024


def record_history(data, days, instruments=INSTRUMENTS, kinds=("grouped",)):
    for day in days:
        bars = [spy(day)] + [{"T": "X{:05d}".format(n), "o": 10.0 + n, "h": 11.0 + n, "l": 9.0 + n,
                              "c": 10.5 + n, "v": 1000 + n, "vw": 10.4 + n, "n": 50, "t": 0}
                             for n in range(instruments)]
        for kind in kinds:
            record.write(str(data), {"kind": kind, "source": "synthetic", "session": day.isoformat(),
                                     "fetched_at": "2026-01-01T00:00:00Z", "adjusted": False, "bars": bars})


def peak_of(job, now, *extra):
    tracemalloc.start()
    try:
        tracemalloc.reset_peak()
        status, _, err = job.main(now, *extra)
        peak = tracemalloc.get_traced_memory()[1]
    finally:
        tracemalloc.stop()
    return status, err, peak


class MemoryBudgetTest(unittest.TestCase):
    def setUp(self):
        self.days = nyse_calendar.sessions(BASE, nyse_calendar.sessions_after(BASE, SESSIONS - 1)[-1])

    def test_the_warm_up_stays_inside_the_budget(self):
        with tempfile.TemporaryDirectory() as d:
            job = Job(d)
            record_history(job.data, self.days)
            status, err, peak = peak_of(job, after_settle(self.days[-2]), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 0, err)
            self.assertLess(peak, BUDGET, "warm-up peak {:.1f} MB".format(peak / 1e6))

    def test_the_daily_firing_stays_inside_the_budget(self):
        with tempfile.TemporaryDirectory() as d:
            job = Job(d)
            record_history(job.data, self.days)
            self.assertEqual(job.main(after_settle(self.days[-2]), "--backfill-from", BASE.isoformat())[0], 0)
            status, err, peak = peak_of(job, after_settle(self.days[-1]))
            self.assertEqual(status, 0, err)
            self.assertLess(peak, BUDGET, "daily peak {:.1f} MB".format(peak / 1e6))
            self.assertTrue((job.stream_dir() / "boost-{}.jsonl".format(self.days[-1])).exists())

    def test_a_day_with_both_recordings_stays_inside_the_budget(self):
        # Two recordings of one day are held together for the agreement check,
        # so the peak is two days' recordings plus the kept bars, not more.
        with tempfile.TemporaryDirectory() as d:
            job = Job(d)
            record_history(job.data, self.days, kinds=("grouped", "universe"))
            status, err, peak = peak_of(job, after_settle(self.days[-2]), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 0, err)
            print("paired-recording peak {:.2f} MB".format(peak / 1e6))
            self.assertLess(peak, PAIRED_BUDGET, "paired warm-up peak {:.1f} MB".format(peak / 1e6))

    def test_the_other_instruments_change_no_byte_of_the_stream(self):
        days = self.days[:12]
        streams = []
        for instruments in (INSTRUMENTS, 0):
            with tempfile.TemporaryDirectory() as d:
                job = Job(d)
                record_history(job.data, days, instruments)
                self.assertEqual(job.main(after_settle(days[-1]), "--backfill-from", BASE.isoformat())[0], 0)
                streams.append((job.stream_dir() / "boost-{}.jsonl".format(days[-1])).read_bytes())
        self.assertEqual(streams[0], streams[1])


# The momentum stream (the compact universe input, encode with a ranker) over
# recordings shaped like the real ones: thousands of instruments in every
# session, two years of sessions, each with its universe recording, quarterly
# dividends and a few splits. The rule ranks the whole market each session
# (dollar volume, top 500) and keeps 260 sessions of closes per ranked name
# (ADR 0026 points 1 and 5), so the data job cannot keep only some
# instruments, but it never needs every instrument of every session at once
# (docs/live-trading/lessons-2026-10.md L1: loading them all killed the 2 GB
# server). Synthetic tickers and prices only.
MOMENTUM_INSTRUMENTS = 2000   # listed stocks in each session (the real day lists ~5,000)
RANKED = 500                  # the configured universe size
WINDOW = 260                  # closes kept per ranked name (the Go ranker's window)
# Two years (502 sessions) when TI_MOMENTUM_MEMORY_FULL=1, as make check sets;
# otherwise fourteen months (291 sessions), still longer than the window, so
# the ordinary unit run checks the same bound in about half the time.
FULL = os.environ.get("TI_MOMENTUM_MEMORY_FULL") == "1"
MOMENTUM_BASE = dt.date(2025, 1, 2) if FULL else dt.date(2025, 11, 3)
MOMENTUM_LAST = dt.date(2026, 12, 31)
NAMED = ("IEF", "SHY", "SPY")
# The stated budget (tracemalloc peak in bytes, the measure the Boost tests
# use; docs/manual/market-data.md states it): one session's recordings and
# snapshot, the closes of the names ranked at the decisions whose windows
# cover the session (about RANKED x WINDOW floats, more where the ranked set
# churns), small per-ticker figures, and the span's split and dividend rows.
# It does not grow with the history beyond those rows. One budget holds at
# the real market's size: this 2,000-name fixture peaks at ~13 MB and a
# one-off run on 10,000 names measured ~43 MB, both under it. Every
# instrument of every session held at once, as before, peaked at ~2.3 GB here.
MOMENTUM_BUDGET = 64 * 1024 * 1024
# The fixture's own ceiling, so a regression that would push the real size past
# MOMENTUM_BUDGET (a peak twice the fixture's) fails here first.
FIXTURE_BUDGET = 32 * 1024 * 1024


def momentum_bar(ticker, n, i):
    c = 20.0 + (n % 7) * 0.5 + (i % 30) * 0.01
    # Dollar volume falls with n, with a monthly wobble that churns the names
    # near the ranked set's edge, as real rankings do.
    v = 1000000 - 30 * n + (n * 7919 + (i // 21) * 104729) % 600
    return {"T": ticker, "o": c, "h": c + 0.5, "l": c - 0.5, "c": c, "v": v, "vw": c, "n": 50, "t": 0}


def stamp(day):
    return "{}T23:30:00Z".format(day.isoformat())


def record_momentum_history(directory, days, instruments=MOMENTUM_INSTRUMENTS):
    names = ["M{:05d}".format(n) for n in range(instruments)]
    tickers = [{"ticker": t, "type": "CS", "market": "stocks"} for t in names]
    last_sectors = None
    for i, day in enumerate(days):
        bars = [{"T": t, "o": 100.0, "h": 103.0, "l": 99.0, "c": 100.0 + (i % 200) * 0.01, "v": 5000000, "vw": 100.0,
                 "n": 50, "t": 0} for t in NAMED]
        bars += [momentum_bar(t, n, i) for n, t in enumerate(names)]
        record.write(directory, {"kind": "universe", "source": "massive", "session": day.isoformat(),
                                 "fetched_at": stamp(day), "adjusted": False, "ticker_type": "CS",
                                 "bars": bars, "tickers": tickers})
        if last_sectors is None or (day - last_sectors).days >= 5:
            last_sectors = day
            labelled = names[:50]
            record.write(directory, {"kind": "sectors-iwv", "source": sectors.IWV_SOURCE, "session": day.isoformat(),
                                     "fetched_at": stamp(day), "url": sectors.IWV_URL, "as_of": "Jan 2, 2025",
                                     "body": iwv_csv(*((t, "Information Technology", "Equity") for t in labelled))})
            record.write(directory, {"kind": "sectors-nasdaq", "source": sectors.NASDAQ_SOURCE,
                                     "session": day.isoformat(), "fetched_at": stamp(day), "url": sectors.NASDAQ_URL,
                                     "rows": json.loads(screener(*((t, "Finance") for t in labelled)))["data"]["rows"]})
    # Every third name pays a quarterly dividend; every hundredth splits once.
    dividends = [{"id": "V{}-{}".format(n, q), "ticker": t, "ex_dividend_date": days[30 + 63 * q + n % 20].isoformat(),
                  "cash_amount": 0.1} for n, t in enumerate(names) if n % 3 == 0 for q in range((len(days) - 50) // 63)]
    splits = [{"id": "S{}".format(n), "ticker": t, "execution_date": days[100 + n % (len(days) - 150)].isoformat(),
               "split_from": 1, "split_to": 2} for n, t in enumerate(names) if n % 100 == 1]
    for kind, rows in (("dividends", dividends), ("splits", splits)):
        record.write(directory, {"kind": kind, "source": "massive", "session": days[-1].isoformat(),
                                 "from": days[0].isoformat(), "fetched_at": stamp(days[-1]), "rows": rows})


class MomentumMemoryBudgetTest(unittest.TestCase):
    """The momentum warm-up and the daily firing (one session written, its
    closes adjusted from the warm-up's base) stay inside MOMENTUM_BUDGET."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        root = Path(cls.tmp.name)
        cls.data = root / "data"
        cls.days = nyse_calendar.sessions(MOMENTUM_BASE, MOMENTUM_LAST)
        record_momentum_history(str(cls.data), cls.days)
        cls.ranker = root / "universe-rank"
        subprocess.run(["go", "build", "-o", str(cls.ranker), "./cmd/universe-rank"], cwd=REPO, check=True)
        config = json.loads((REPO / "cmd/universe-rank/testdata/momentum.json").read_text())
        config["momentum"]["universe_size"] = RANKED
        cls.config = root / "momentum.json"
        cls.config.write_text(json.dumps(config))

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def encode(self, first, out):
        argv = ["encode", "--out", str(self.data), "--first", first.isoformat(), "--asof", self.days[-1].isoformat(),
                "--base", MOMENTUM_BASE.isoformat(), "--instruments", ",".join(NAMED),
                "--ranker", "{} -config {}".format(self.ranker, self.config),
                "--configuration-hash", "hash-1", "--strategy-version", "momentum/test", "--run-id", "run-1",
                "--stream", str(out)]
        tracemalloc.start()
        try:
            tracemalloc.reset_peak()
            status = cli.main(argv)
            peak = tracemalloc.get_traced_memory()[1]
        finally:
            tracemalloc.stop()
        return status, peak

    def histories(self, out):
        """{decision session: histories}; every name trades every session
        with good data, so each history holds exactly the window's sessions:
        all WINDOW of them once the recordings reach back that far."""
        counts = {}
        with open(out) as f:
            for line in f:
                if '"type":"market.close.history"' in line:
                    e = json.loads(line)
                    day = dt.date.fromisoformat(e["payload"]["session"])
                    counts[day.isoformat()] = counts.get(day.isoformat(), 0) + 1
                    window = [d.isoformat() for d in self.days if d <= day][-WINDOW:]
                    self.assertEqual([c["session"] for c in e["payload"]["closes"]], window, e["payload"]["instrument_id"])
        return counts

    def decisions(self):
        return [d.isoformat() for d, n in zip(self.days, self.days[1:] + [dt.date(2027, 1, 4)]) if d.month != n.month]

    def check_peak(self, name, peak):
        print("momentum {} peak {:.1f} MB".format(name, peak / 1e6))
        self.assertLess(peak, MOMENTUM_BUDGET, "momentum {} peak {:.1f} MB".format(name, peak / 1e6))
        self.assertLess(peak, FIXTURE_BUDGET, "momentum {} peak {:.1f} MB".format(name, peak / 1e6))

    def test_the_momentum_warm_up_stays_inside_the_budget(self):
        out = Path(self.tmp.name) / "warm-up.jsonl"
        status, peak = self.encode(MOMENTUM_BASE, out)
        self.assertEqual(status, 0)
        self.check_peak("warm-up", peak)
        counts = self.histories(out)
        self.assertEqual(sorted(counts), self.decisions())   # a decision at each month's end
        self.assertEqual(set(counts.values()), {RANKED})

    def test_the_momentum_daily_firing_stays_inside_the_budget(self):
        # The last session ends a month, so it is a decision: its histories
        # reach back a full window, adjusted from the warm-up's base.
        out = Path(self.tmp.name) / "daily.jsonl"
        status, peak = self.encode(MOMENTUM_LAST, out)
        self.assertEqual(status, 0)
        self.check_peak("daily", peak)
        self.assertEqual(self.histories(out), {MOMENTUM_LAST.isoformat(): RANKED})


def record_stress_history(directory, days, names, broken=()):
    """Universe recordings of ``names`` for ``days``; each name in ``broken``
    has a negative volume, a malformed bar, every session."""
    tickers = [{"ticker": t, "type": "CS", "market": "stocks"} for t in names]
    last_sectors = None
    for i, day in enumerate(days):
        bars = [{"T": t, "o": 100.0, "h": 103.0, "l": 99.0, "c": 100.0 + (i % 200) * 0.01, "v": 5000000,
                 "vw": 100.0, "n": 50, "t": 0} for t in NAMED]
        bars += [dict(momentum_bar(t, n, i), v=-1 if t in broken else 1000 + n) for n, t in enumerate(names)]
        record.write(directory, {"kind": "universe", "source": "massive", "session": day.isoformat(),
                                 "fetched_at": stamp(day), "adjusted": False, "ticker_type": "CS",
                                 "bars": bars, "tickers": tickers})
        if last_sectors is None or (day - last_sectors).days >= 5:
            last_sectors = day
            record.write(directory, {"kind": "sectors-iwv", "source": sectors.IWV_SOURCE, "session": day.isoformat(),
                                     "fetched_at": stamp(day), "url": sectors.IWV_URL, "as_of": "Jan 2, 2026",
                                     "body": iwv_csv((names[0], "Information Technology", "Equity"))})
            record.write(directory, {"kind": "sectors-nasdaq", "source": sectors.NASDAQ_SOURCE,
                                     "session": day.isoformat(), "fetched_at": stamp(day), "url": sectors.NASDAQ_URL,
                                     "rows": json.loads(screener((names[0], "Finance")))["data"]["rows"]})
    for kind in ("dividends", "splits"):
        record.write(directory, {"kind": kind, "source": "massive", "session": days[-1].isoformat(),
                                 "from": days[0].isoformat(), "fetched_at": stamp(days[-1]), "rows": []})


def scripted(sets):
    """A ranker that names sets[k % len(sets)] at the k-th decision session."""
    seen = []

    def rank(requests):
        out = []
        for r in requests:
            session, nxt = r["universe"]["session"], r["next_session"]
            decision = session[:7] != nxt[:7]
            if decision:
                seen.append(session)
            out.append({"session": session, "decision_session": decision, "window": WINDOW,
                        "members": list(sets[(len(seen) - 1) % len(sets)]) if decision else []})
        return out
    return rank


class MomentumStressMemoryTest(unittest.TestCase):
    """Two shapes the healthy fixture does not reach: a feed with bad bars
    for many stocks every session, and ranked sets that change completely
    every month. Neither may grow memory by a stored item per stock per
    session (docs/live-trading/lessons-2026-10.md L1). Synthetic data only."""

    NAMES = ["S{:04d}".format(n) for n in range(900)]
    DAYS = nyse_calendar.sessions(dt.date(2026, 1, 2), dt.date(2026, 8, 31))   # 166 sessions, 8 decisions

    def peak(self, broken=(), sets=None, excluded=None):
        with tempfile.TemporaryDirectory() as d:
            record_stress_history(d, self.DAYS, self.NAMES, broken)
            ranker = scripted(sets or [self.NAMES[:110]])
            tracemalloc.start()
            try:
                tracemalloc.reset_peak()
                envelopes = encode.iter_encode(d, self.DAYS[0], self.DAYS[-1], encode.Identity("h", "v", "r"),
                                               required=list(NAMED), ranker=ranker, excluded=excluded)
                encode.write_stream(os.path.join(d, "out.jsonl"), envelopes)
                return tracemalloc.get_traced_memory()[1]
            finally:
                tracemalloc.stop()

    def test_bad_bars_for_many_stocks_every_session_add_no_memory_per_session(self):
        broken, excluded = self.NAMES[450:], {}
        healthy, spoiled = self.peak(), self.peak(broken=broken, excluded=excluded)
        print("bad-bar peak {:.2f} MB, healthy {:.2f} MB".format(spoiled / 1e6, healthy / 1e6))
        self.assertLess(spoiled - healthy, 1024 * 1024)
        self.assertEqual(sorted(excluded), sorted(broken))
        for ticker in broken:   # the first reasons are kept, the rest are one run
            runs = excluded[ticker]
            self.assertLessEqual(len(runs), encode.REASONS_KEPT + 1, ticker)
            self.assertEqual(sum(r.sessions for r in runs), len(self.DAYS), ticker)

    def test_ranked_sets_that_change_every_month_keep_compact_windows(self):
        sets = [self.NAMES[110 * k:110 * (k + 1)] for k in range(8)]   # disjoint: each name ranked once
        steady, rotating = self.peak(), self.peak(sets=sets)
        print("rotating-set peak {:.2f} MB, steady {:.2f} MB".format(rotating / 1e6, steady / 1e6))
        # About 64,000 more closes held (each set from the base to its
        # decision): about 0.5 MB as 8-byte closes, several times that as one
        # Python object per close.
        self.assertLess(rotating - steady, 1024 * 1024)


if __name__ == "__main__":
    unittest.main()
