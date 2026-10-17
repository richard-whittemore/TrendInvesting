"""Tests for the sector-label recordings (GICS from iShares IWV, Nasdaq's
screener as the fallback). Every response is synthetic; no network."""
import datetime as dt
import io
import json
import os
import tempfile
import unittest
import urllib.error

from marketdata import record, sectors, steps
from marketdata.massive import MarketDataError

D = dt.date
NOW = lambda: dt.datetime(2026, 3, 27, 22, 0, tzinfo=dt.timezone.utc)   # noqa: E731


def iwv_csv(*rows, as_of="Mar 26, 2026"):
    """An iShares holdings file: preamble, header, (ticker, sector, asset class) rows, footer."""
    lines = ["﻿iShares Russell 3000 ETF", 'Fund Holdings as of,"{}"'.format(as_of), "",
             "Ticker,Name,Sector,Asset Class,Market Value"]
    lines += ['"{}","NAME","{}","{}","1.00"'.format(t, s, a) for t, s, a in rows]
    return "\n".join(lines + ["", "The content contained herein is owned or licensed by BlackRock"])


def screener(*rows):
    return json.dumps({"data": {"headers": {}, "rows": [{"symbol": t, "name": "N", "sector": s} for t, s in rows]},
                       "status": {"rCode": 200}})


class FakeOpener:
    def __init__(self, *queue):
        self.queue, self.requests = list(queue), []

    def __call__(self, req, timeout=None):
        self.requests.append(req)
        item = self.queue.pop(0)
        if isinstance(item, int):
            raise urllib.error.HTTPError(req.full_url, item, "err", {}, io.BytesIO(b""))
        return io.BytesIO(item.encode())


def getter(*queue):
    opener = FakeOpener(*queue)
    return (lambda url: sectors.http_get(url, opener=opener)), opener


class ParseTest(unittest.TestCase):
    def test_the_iwv_file_gives_each_equitys_gics_sector_and_its_date(self):
        as_of, rows = sectors.parse_iwv(iwv_csv(("AAPL", "Information Technology", "Equity"),
                                                ("BRKB", "Financials", "Equity"),
                                                ("USD", "Cash and/or Derivatives", "Cash")))
        self.assertEqual(as_of, "Mar 26, 2026")
        self.assertEqual(rows, {"AAPL": "Information Technology", "BRKB": "Financials"})

    def test_a_page_that_is_not_a_holdings_file_is_refused(self):
        for text in ("<!DOCTYPE html><html>blocked</html>", iwv_csv(("USD", "Cash", "Cash"))):
            with self.subTest(text[:20]), self.assertRaises(sectors.Unusable):
                sectors.parse_iwv(text)

    def test_the_screener_gives_each_symbols_sector(self):
        self.assertEqual(sectors.parse_nasdaq(json.loads(screener(("BRK/B", "Finance"), ("SHOP", "Technology")))),
                         {"BRKB": "Finance", "SHOP": "Technology"})

    def test_a_screener_response_without_rows_is_refused(self):
        for body in ({}, {"data": None}, {"data": {"rows": []}}, {"data": {"rows": [{"sector": "x"}]}}):
            with self.subTest(body), self.assertRaises(sectors.Unusable):
                sectors.parse_nasdaq(body)


def records(iwv_rows, nasdaq_rows, session="2026-03-27", fetched_at=None):
    """Fetched by default at 22:00 UTC on the session's own evening (its close
    is 20:00 UTC in late March)."""
    fetched_at = fetched_at or session + "T22:00:00Z"
    return ({"kind": "sectors-iwv", "source": sectors.IWV_SOURCE, "session": session, "fetched_at": fetched_at,
             "url": sectors.IWV_URL, "as_of": "Mar 26, 2026", "body": iwv_csv(*iwv_rows)},
            {"kind": "sectors-nasdaq", "source": sectors.NASDAQ_SOURCE, "session": session, "fetched_at": fetched_at,
             "url": sectors.NASDAQ_URL, "rows": json.loads(screener(*nasdaq_rows))["data"]["rows"]})


class LabelsTest(unittest.TestCase):
    def test_gics_first_then_the_screener_in_one_vocabulary_else_no_label(self):
        iwv, nasdaq = records([("AAPL", "Information Technology", "Equity"), ("JPM", "Financials", "Equity"),
                               ("ODD", "Cash and/or Derivatives", "Equity")],
                              [("AAPL", "Finance"), ("SHOP", "Technology"), ("BRK/B", "Miscellaneous"),
                               ("ODD", "Energy"), ("NONE", "")])
        got = sectors.labels(iwv, nasdaq)
        self.assertEqual(got["AAPL"], ("Technology", sectors.IWV_SOURCE))
        self.assertEqual(got["JPM"], ("Financial Services", sectors.IWV_SOURCE))
        # GICS and the screener name technology alike, so the cap counts both.
        self.assertEqual(got["SHOP"], ("Technology", sectors.NASDAQ_SOURCE))
        # An unmapped label falls through to the next source, then to none.
        self.assertEqual(got["ODD"], ("Energy", sectors.NASDAQ_SOURCE))
        self.assertNotIn("BRKB", got)
        self.assertNotIn("NONE", got)

    def test_every_source_sector_maps_onto_a_morningstar_sector(self):
        self.assertEqual(sorted(set(sectors.GICS_SECTORS.values())), sorted(sectors.MORNINGSTAR_SECTORS))
        self.assertEqual(sorted(set(sectors.NASDAQ_SECTORS.values())), sorted(sectors.MORNINGSTAR_SECTORS))

    def test_a_ticker_two_rows_label_differently_is_left_to_the_next_source(self):
        iwv, nasdaq = records([("BF.B", "Consumer Staples", "Equity"), ("BFB", "Energy", "Equity")],
                              [("BF/B", "Consumer Staples")])
        self.assertEqual(sectors.labels(iwv, nasdaq)["BFB"], ("Consumer Defensive", sectors.NASDAQ_SOURCE))

    def test_tickers_match_without_punctuation(self):
        self.assertEqual(sectors.norm("brk.b"), "BRKB")
        self.assertEqual(sectors.norm("BF/B"), "BFB")


class RecordTest(unittest.TestCase):
    def test_both_sources_are_recorded_for_the_session(self):
        get, opener = getter(iwv_csv(("AAPL", "Information Technology", "Equity")), screener(("SHOP", "Technology")))
        with tempfile.TemporaryDirectory() as d:
            paths = sectors.record_sectors(d, D(2026, 3, 27), get=get, now=NOW)
            self.assertEqual(sorted(os.path.basename(p) for p in paths),
                             ["sectors-iwv_2026-03-27.json", "sectors-nasdaq_2026-03-27.json"])
            iwv = record.read(os.path.join(d, "sectors-iwv_2026-03-27.json"))
            self.assertEqual((iwv["source"], iwv["as_of"], iwv["fetched_at"]),
                             (sectors.IWV_SOURCE, "Mar 26, 2026", "2026-03-27T22:00:00Z"))
        self.assertEqual([r.full_url for r in opener.requests], [sectors.IWV_URL, sectors.NASDAQ_URL])
        self.assertIn("Mozilla", opener.requests[0].get_header("User-agent"))

    def test_nothing_is_written_when_either_source_fails(self):
        for queue in ((403, screener(("A", "Energy"))), (iwv_csv(("A", "Energy", "Equity")), "<html>no</html>")):
            get, _ = getter(*queue)
            with tempfile.TemporaryDirectory() as d, self.subTest(queue[0] if isinstance(queue[0], int) else "html"):
                with self.assertRaises(MarketDataError):
                    sectors.record_sectors(d, D(2026, 3, 27), get=get, now=NOW)
                self.assertEqual(os.listdir(d), [])

    def test_a_rerun_fetches_only_the_missing_source(self):
        # IWV was recorded before the run stopped; its site is now down (403
        # if asked), and the missing Nasdaq file is still recorded.
        with tempfile.TemporaryDirectory() as d:
            iwv, _ = records([("A", "Energy", "Equity")], [])
            record.write(d, iwv)
            get, opener = getter(screener(("A", "Energy")), 403)
            paths = sectors.record_sectors(d, D(2026, 3, 27), get=get, now=NOW)
            self.assertEqual([os.path.basename(p) for p in paths], ["sectors-nasdaq_2026-03-27.json"])
            self.assertEqual([r.full_url for r in opener.requests], [sectors.NASDAQ_URL])
            self.assertEqual(record.read(os.path.join(d, "sectors-iwv_2026-03-27.json")), iwv)

    def test_a_rerun_with_both_recorded_fetches_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            for rec in records([("A", "Energy", "Equity")], [("A", "Energy")]):
                record.write(d, rec)
            get, opener = getter()
            self.assertEqual(sectors.record_sectors(d, D(2026, 3, 27), get=get, now=NOW), [])
            self.assertEqual(opener.requests, [])

    def test_a_past_session_is_refused_before_any_fetch(self):
        # Live files describe today, so they are never saved under an earlier session.
        get, opener = getter(iwv_csv(("A", "Energy", "Equity")), screener(("A", "Energy")))
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(sectors.Backdated):
                sectors.record_sectors(d, D(2026, 3, 26), get=get, now=NOW)
            self.assertEqual((os.listdir(d), opener.requests), ([], []))

    def test_a_session_not_yet_settled_or_not_a_trading_day_is_refused(self):
        get, opener = getter()
        at_close = lambda: dt.datetime(2026, 3, 27, 20, 30, tzinfo=dt.timezone.utc)   # noqa: E731
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(steps.NotSettled):
                sectors.record_sectors(d, D(2026, 3, 27), get=get, now=at_close)
            with self.assertRaises(MarketDataError):
                sectors.record_sectors(d, D(2026, 3, 28), get=get, now=NOW)   # a Saturday
        self.assertEqual(opener.requests, [])

    def test_only_the_two_sources_hosts_are_fetched(self):
        with self.assertRaises(MarketDataError):
            sectors.http_get("https://example.com/x", opener=FakeOpener("x"))
        with self.assertRaises(MarketDataError):
            sectors.http_get("http://www.ishares.com/x", opener=FakeOpener("x"))


class LatestTest(unittest.TestCase):
    def test_the_latest_recording_on_or_before_the_session_within_the_age_limit(self):
        with tempfile.TemporaryDirectory() as d:
            for session in ("2026-03-20", "2026-03-25", "2026-03-30"):
                iwv, _ = records([("A", "Energy", "Equity")], [], session=session)
                record.write(d, iwv)
            self.assertEqual(sectors.latest(d, "sectors-iwv", D(2026, 3, 27))["session"], "2026-03-25")
            with self.assertRaises(steps.MissingData):
                sectors.latest(d, "sectors-iwv", D(2026, 3, 27), max_age_days=1)
            with self.assertRaises(steps.MissingData):
                sectors.latest(d, "sectors-iwv", D(2026, 3, 19))
            with self.assertRaises(steps.MissingData):
                sectors.latest(d, "sectors-nasdaq", D(2026, 3, 27))

    def test_a_recording_fetched_outside_its_sessions_window_is_refused(self):
        # A session's labels are valid only if fetched after its close
        # (20:00 UTC on 2026-03-25) and before the next session's close
        # (20:00 UTC on 2026-03-26).
        for fetched, ok in (("2026-03-25T20:00:00Z", True), ("2026-03-26T19:59:59Z", True),
                            ("2026-03-26T20:00:00Z", False), ("2026-03-27T22:00:00Z", False),
                            ("2026-03-25T19:59:59Z", False), ("x", False)):
            with tempfile.TemporaryDirectory() as d, self.subTest(fetched):
                iwv, _ = records([("A", "Energy", "Equity")], [], session="2026-03-25", fetched_at=fetched)
                record.write(d, iwv)
                if ok:
                    self.assertEqual(sectors.latest(d, "sectors-iwv", D(2026, 3, 27)), iwv)
                else:
                    with self.assertRaises(sectors.Backdated):
                        sectors.latest(d, "sectors-iwv", D(2026, 3, 27))


if __name__ == "__main__":
    unittest.main()
