"""Tests for the market-data step. Every response is synthetic; no network."""
import datetime as dt
import io
import json
import logging
import os
import tempfile
import unittest
from unittest import mock
import urllib.error
import urllib.request
import zoneinfo

from marketdata import massive, record, steps

NEW_YORK = zoneinfo.ZoneInfo("America/New_York")
KEY = "SECRET-KEY-do-not-leak-0123456789"
DAY = dt.date(2026, 3, 27)  # a Friday


class FakeResponse:
    def __init__(self, body):
        self.body = json.dumps(body).encode()

    def read(self):
        return self.body

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class FakeOpener:
    """Serves queued responses (dict body, or an int HTTP error code) in order
    and keeps every Request it saw."""

    def __init__(self, *queue):
        self.queue, self.requests = list(queue), []

    def __call__(self, req, timeout=None):
        self.requests.append(req)
        item = self.queue.pop(0)
        if isinstance(item, int):
            raise urllib.error.HTTPError(req.full_url, item, "err", {}, io.BytesIO(b""))
        return FakeResponse(item)


class Clock:
    def __init__(self):
        self.now, self.sleeps = 1000.0, []

    def monotonic(self):
        return self.now

    def sleep(self, s):
        self.sleeps.append(s)
        self.now += s


def make_client(*queue, retries=2):
    clock, opener = Clock(), FakeOpener(*queue)
    client = massive.Client(KEY, opener=opener, sleep=clock.sleep, clock=clock.monotonic,
                            max_attempts=retries + 1)
    return client, opener, clock


def bar(t, c=10.0, v=100):
    return {"T": t, "c": c, "v": v, "o": c, "h": c, "l": c, "t": 0}


def grouped(*rows):
    return {"status": "OK", "resultsCount": len(rows), "results": list(rows)}


def ticker(sym, type_="CS"):
    return {"ticker": sym, "type": type_, "market": "stocks", "locale": "us"}


def agg(day, c):
    # Massive stamps a daily bar with midnight New York time, in milliseconds.
    ms = int(dt.datetime(day.year, day.month, day.day, tzinfo=NEW_YORK).timestamp() * 1000)
    return {"o": c, "h": c, "l": c, "c": c, "v": 1, "t": ms}


class ClientTest(unittest.TestCase):
    def test_key_goes_in_a_header_never_in_the_url(self):
        client, opener, _ = make_client(grouped(bar("AAA")))
        client.grouped(DAY)
        req = opener.requests[0]
        self.assertEqual(req.get_header("Authorization"), "Bearer " + KEY)
        self.assertNotIn(KEY, req.full_url)
        self.assertIn("/v2/aggs/grouped/locale/us/market/stocks/2026-03-27", req.full_url)
        self.assertIn("adjusted=false", req.full_url)

    def test_requests_are_spaced_to_five_a_minute(self):
        client, _, clock = make_client(grouped(bar("A")), grouped(bar("A")), grouped(bar("A")))
        for _ in range(3):
            client.grouped(DAY)
        self.assertEqual(len(clock.sleeps), 2)
        self.assertTrue(all(s >= 12.0 for s in clock.sleeps))

    def test_a_429_waits_a_minute_and_retries(self):
        client, opener, clock = make_client(429, grouped(bar("A")))
        self.assertEqual(client.grouped(DAY)["results"][0]["T"], "A")
        self.assertEqual(len(opener.requests), 2)
        self.assertIn(60.0, clock.sleeps)

    def test_repeated_429s_fail_closed(self):
        client, opener, _ = make_client(429, 429, 429)
        with self.assertRaises(massive.MarketDataError) as cm:
            client.grouped(DAY)
        self.assertEqual(len(opener.requests), 3)
        self.assertNotIn(KEY, str(cm.exception))

    def test_other_http_errors_are_not_retried_and_do_not_carry_the_key(self):
        client, opener, _ = make_client(403)
        with self.assertRaises(massive.MarketDataError) as cm:
            client.grouped(DAY)
        self.assertEqual(len(opener.requests), 1)
        self.assertNotIn(KEY, str(cm.exception))

    def test_an_error_status_body_fails(self):
        client, _, _ = make_client({"status": "ERROR", "error": "nope"})
        with self.assertRaises(massive.MarketDataError):
            client.grouped(DAY)

    def test_pagination_follows_next_url_and_strips_an_echoed_key(self):
        page1 = {"status": "OK", "results": [ticker("AAA")],
                 "next_url": "https://api.massive.com/v3/reference/tickers?cursor=abc&apiKey=" + KEY}
        page2 = {"status": "OK", "results": [ticker("BBB")]}
        client, opener, _ = make_client(page1, page2)
        pages = client.tickers(DAY, "CS")
        self.assertEqual([r["ticker"] for p in pages for r in p["results"]], ["AAA", "BBB"])
        self.assertIn("cursor=abc", opener.requests[1].full_url)
        self.assertNotIn(KEY, opener.requests[1].full_url)
        self.assertNotIn(KEY, json.dumps(pages))

    def test_next_url_on_another_host_is_refused(self):
        page1 = {"status": "OK", "results": [], "next_url": "https://evil.example/x?cursor=1"}
        client, opener, _ = make_client(page1)
        with self.assertRaises(massive.MarketDataError):
            client.tickers(DAY, "CS")
        self.assertEqual(len(opener.requests), 1)

    def test_a_repeated_cursor_fails_instead_of_looping(self):
        loop = {"status": "OK", "results": [ticker("AAA")],
                "next_url": "https://api.massive.com/v3/reference/tickers?cursor=same"}
        client, opener, _ = make_client(loop, loop, loop)
        with self.assertRaises(massive.MarketDataError):
            client.tickers(DAY, "CS")
        self.assertEqual(len(opener.requests), 2)

    def redirect(self, newurl):
        req = urllib.request.Request("https://api.massive.com/v3/x", headers={"Authorization": "Bearer " + KEY})
        return massive.MassiveOnlyRedirect().redirect_request(req, io.BytesIO(b""), 302, "Found", {}, newurl)

    def test_a_redirect_off_massive_is_refused_so_the_key_is_not_forwarded(self):
        for url in ("https://evil.example/x", "http://api.massive.com/x", "https://api.massive.com.evil.example/x"):
            with self.assertRaises(urllib.error.HTTPError, msg=url):
                self.redirect(url)

    def test_a_redirect_within_massive_is_followed(self):
        self.assertEqual(self.redirect("https://api.massive.com/v3/y").full_url, "https://api.massive.com/v3/y")

    def test_the_default_opener_uses_the_massive_only_redirect(self):
        handlers = massive.OPENER.handlers
        self.assertTrue(any(isinstance(h, massive.MassiveOnlyRedirect) for h in handlers))
        self.assertFalse(any(type(h) is urllib.request.HTTPRedirectHandler for h in handlers))
        self.assertEqual(massive.Client(KEY)._opener, massive.OPENER.open)

    def test_the_key_is_never_logged(self):
        client, _, _ = make_client(429, grouped(bar("A")))
        stream = io.StringIO()
        handler = logging.StreamHandler(stream)
        log = logging.getLogger("marketdata")
        log.addHandler(handler)
        log.setLevel(logging.DEBUG)
        try:
            client.grouped(DAY)
        finally:
            log.removeHandler(handler)
        self.assertTrue(stream.getvalue())
        self.assertNotIn(KEY, stream.getvalue())
        self.assertNotIn(KEY, repr(client))


class KeyTest(unittest.TestCase):
    def test_key_from_secrets_file_strips_whitespace(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "key")
            with open(p, "w") as f:
                f.write(KEY + "\n")
            self.assertEqual(massive.load_key(env={}, secrets_file=p, keychain=None), KEY)

    def test_environment_wins_then_file_then_keychain(self):
        self.assertEqual(massive.load_key(env={"MASSIVE_API_KEY": KEY}, secrets_file=None,
                                          keychain=lambda: "other"), KEY)
        self.assertEqual(massive.load_key(env={}, secrets_file=None, keychain=lambda: KEY), KEY)

    def test_no_key_anywhere_fails_without_naming_a_value(self):
        with self.assertRaises(massive.MarketDataError):
            massive.load_key(env={}, secrets_file=None, keychain=lambda: "")


NOW = dt.datetime(2026, 3, 27, 21, 5, tzinfo=dt.timezone.utc)
LATER = dt.datetime(2026, 4, 10, 12, 0, tzinfo=dt.timezone.utc)


class UniverseTest(unittest.TestCase):
    def test_records_raw_rows_for_the_session(self):
        client, opener, _ = make_client(
            grouped(bar("AAA", 10, 100), bar("ETF1", 50, 9)),
            {"status": "OK", "results": [ticker("AAA"), ticker("ETF1", "ETF"), ticker("CCC")],
             "next_url": "https://api.massive.com/v3/reference/tickers?cursor=2"},
            {"status": "OK", "results": [ticker("DDD")]})
        rec = steps.fetch_universe(client, DAY, now=lambda: NOW)
        self.assertEqual(rec["kind"], "universe")
        self.assertEqual(rec["source"], "massive")
        self.assertEqual(rec["session"], "2026-03-27")
        self.assertEqual(rec["fetched_at"], "2026-03-27T21:05:00Z")
        self.assertEqual([r["T"] for r in rec["bars"]], ["AAA", "ETF1"])   # raw rows kept, unranked
        self.assertEqual([r["ticker"] for r in rec["tickers"]], ["AAA", "ETF1", "CCC", "DDD"])
        self.assertEqual(len(opener.requests), 3)
        self.assertIn("type=CS", opener.requests[1].full_url)
        self.assertIn("date=2026-03-27", opener.requests[1].full_url)

    def test_a_missing_session_raises_and_fetches_nothing_more(self):
        client, opener, _ = make_client({"status": "OK", "resultsCount": 0, "results": []})
        with self.assertRaises(steps.MissingData):
            steps.fetch_universe(client, DAY, now=lambda: NOW)
        self.assertEqual(len(opener.requests), 1)

    def test_no_common_tickers_raises(self):
        client, _, _ = make_client(grouped(bar("AAA")), {"status": "OK", "results": []})
        with self.assertRaises(steps.MissingData):
            steps.fetch_universe(client, DAY, now=lambda: NOW)

    def test_last_session_skips_an_exchange_holiday_without_a_request(self):
        good_friday = dt.date(2026, 4, 3)
        client, opener, _ = make_client(grouped(bar("A")))
        day, body = steps.last_session(client, good_friday, now=lambda: LATER)
        self.assertEqual(day, dt.date(2026, 4, 2))
        self.assertEqual(body["results"][0]["T"], "A")
        self.assertIn("2026-04-02", opener.requests[0].full_url)
        self.assertEqual(len(opener.requests), 1)

    def test_last_session_over_a_weekend_starts_at_friday(self):
        client, opener, _ = make_client(grouped(bar("A")))
        self.assertEqual(steps.last_session(client, dt.date(2026, 3, 29), now=lambda: NOW)[0], DAY)
        self.assertIn("2026-03-27", opener.requests[0].full_url)

    def test_an_empty_response_on_a_trading_day_is_missing_data_not_a_skip(self):
        weekday = dt.date(2026, 4, 2)
        client, opener, _ = make_client({"status": "OK", "resultsCount": 0, "results": []}, grouped(bar("A")))
        with self.assertRaises(steps.MissingData):
            steps.last_session(client, weekday, now=lambda: LATER)
        self.assertEqual(len(opener.requests), 1)


def ny(day, hour, minute=0):
    return lambda: dt.datetime(day.year, day.month, day.day, hour, minute, tzinfo=NEW_YORK)


class SettleTest(unittest.TestCase):
    def test_a_run_during_the_session_requests_and_writes_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            client, opener, _ = make_client(grouped(bar("A")), {"status": "OK", "results": [ticker("A")]})
            with self.assertRaises(steps.NotSettled):
                steps.find_session(client, DAY, now=ny(DAY, 10))
            with self.assertRaises(steps.NotSettled):
                steps.run_universe(client, DAY, d, now=ny(DAY, 10))
            with self.assertRaises(steps.NotSettled):
                steps.run_bars(client, ["SPY"], DAY, d, now=ny(DAY, 10))
            self.assertEqual(opener.requests, [])
            self.assertEqual(os.listdir(d), [])

    def test_not_settled_is_missing_data_so_the_cli_fails_closed(self):
        self.assertTrue(issubclass(steps.NotSettled, steps.MissingData))

    def test_the_settle_delay_runs_from_the_close(self):
        steps.require_settled(DAY, ny(DAY, 16, 30), settle=dt.timedelta(minutes=30))
        with self.assertRaises(steps.NotSettled):
            steps.require_settled(DAY, ny(DAY, 16, 29), settle=dt.timedelta(minutes=30))
        with self.assertRaises(steps.NotSettled):
            steps.require_settled(DAY, ny(DAY, 16, 59))   # default delay is an hour
        steps.require_settled(DAY, ny(DAY, 17, 0))

    def test_an_early_close_settles_from_one_oclock(self):
        early = dt.date(2026, 11, 27)
        steps.require_settled(early, ny(early, 14, 0))
        with self.assertRaises(steps.NotSettled):
            steps.require_settled(early, ny(early, 13, 59))

    def test_a_past_session_is_settled(self):
        steps.require_settled(DAY, ny(DAY + dt.timedelta(days=3), 9))

    def test_after_close_plus_delay_the_universe_is_recorded(self):
        with tempfile.TemporaryDirectory() as d:
            client, _, _ = make_client(grouped(bar("A")), {"status": "OK", "results": [ticker("A")]})
            path = steps.run_universe(client, DAY, d, now=ny(DAY, 17, 5))
            self.assertTrue(os.path.exists(path))

    def test_the_default_asof_is_the_new_york_date_not_the_utc_date(self):
        # 01:30 UTC on Saturday is 21:30 on Friday in New York.
        utc = dt.datetime(2026, 3, 28, 1, 30, tzinfo=dt.timezone.utc)
        self.assertEqual(steps.new_york_date(lambda: utc), DAY)
        # 23:30 New York on Friday is already Saturday in UTC; still Friday.
        self.assertEqual(steps.new_york_date(ny(DAY, 23, 30)), DAY)

    def test_the_cli_defaults_asof_to_the_new_york_date(self):
        from marketdata import __main__ as cli
        args = cli.parse(["universe", "--out", "x"], now=lambda: dt.datetime(2026, 3, 28, 1, 30, tzinfo=dt.timezone.utc))
        self.assertEqual(args.asof, DAY)
        self.assertEqual(cli.parse(["universe", "--out", "x", "--asof", "2026-03-20"]).asof, dt.date(2026, 3, 20))
        self.assertEqual(cli.parse(["universe", "--out", "x", "--settle-minutes", "90"]).settle_minutes, 90)


class BarsTest(unittest.TestCase):
    def test_records_raw_split_adjusted_and_dividends_per_instrument(self):
        client, opener, _ = make_client(
            {"status": "OK", "results": [agg(DAY, 100.0)]},                 # SPY raw
            {"status": "OK", "results": [agg(DAY, 50.0)]},                  # SPY split-adjusted
            {"status": "OK", "results": [{"ex_dividend_date": "2026-03-20", "cash_amount": 1.5}]},
            {"status": "OK", "results": [agg(DAY, 90.0)]},                  # IEF raw
            {"status": "OK", "results": [agg(DAY, 90.0)]},
            {"status": "OK", "results": []})                                # no dividends is fine
        rec = steps.fetch_bars(client, ["SPY", "IEF"], DAY, lookback_days=30, now=lambda: NOW)
        self.assertEqual(rec["kind"], "bars")
        self.assertEqual(rec["instruments"]["SPY"]["raw"][0]["c"], 100.0)
        self.assertEqual(rec["instruments"]["SPY"]["split_adjusted"][0]["c"], 50.0)
        self.assertEqual(rec["instruments"]["SPY"]["dividends"][0]["cash_amount"], 1.5)
        self.assertEqual(rec["instruments"]["IEF"]["dividends"], [])
        self.assertEqual(len(opener.requests), 6)   # 3 per instrument
        self.assertIn("adjusted=false", opener.requests[0].full_url)
        self.assertIn("adjusted=true", opener.requests[1].full_url)

    def test_an_instrument_without_the_sessions_bar_fails_the_whole_step(self):
        old = DAY - dt.timedelta(days=1)
        client, _, _ = make_client({"status": "OK", "results": [agg(old, 1.0)]},
                                   {"status": "OK", "results": [agg(old, 1.0)]},
                                   {"status": "OK", "results": []})
        with self.assertRaises(steps.MissingData):
            steps.fetch_bars(client, ["SPY"], DAY, lookback_days=30, now=lambda: NOW)

    def test_no_instruments_is_an_error(self):
        client, _, _ = make_client()
        with self.assertRaises(steps.MissingData):
            steps.fetch_bars(client, [], DAY, lookback_days=30, now=lambda: NOW)


class RecordTest(unittest.TestCase):
    def rec(self):
        return {"kind": "universe", "source": "massive", "session": "2026-03-27",
                "fetched_at": "2026-03-27T21:05:00Z", "bars": [bar("A")], "tickers": []}

    def test_writes_a_named_file_that_reads_back_with_its_digest(self):
        with tempfile.TemporaryDirectory() as d:
            path = record.write(d, self.rec())
            self.assertEqual(os.path.basename(path), "universe_2026-03-27.json")
            self.assertEqual(record.read(path), self.rec())

    def test_a_recording_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as d:
            record.write(d, self.rec())
            with self.assertRaises(record.AlreadyRecorded):
                record.write(d, self.rec())

    def test_a_tampered_recording_is_refused_on_read(self):
        with tempfile.TemporaryDirectory() as d:
            path = record.write(d, self.rec())
            with open(path) as f:
                doc = json.load(f)
            doc["record"]["bars"][0]["c"] = 99.0
            with open(path, "w") as f:
                json.dump(doc, f)
            with self.assertRaises(record.Corrupt):
                record.read(path)

    def test_the_directory_and_parents_of_created_directories_are_synced_after_the_link(self):
        with tempfile.TemporaryDirectory() as d:
            target = os.path.join(d, "a", "b")
            synced = []
            with mock.patch.object(record, "_sync_dir", side_effect=lambda p: synced.append(
                    (p, os.path.exists(os.path.join(target, "universe_2026-03-27.json"))))):
                record.write(target, self.rec())
            self.assertEqual(synced, [(target, True), (os.path.join(d, "a"), True), (d, True)])

    def test_an_existing_directory_alone_is_synced(self):
        with tempfile.TemporaryDirectory() as d:
            with mock.patch.object(record, "_sync_dir") as sync:
                record.write(d, self.rec())
            self.assertEqual([c.args[0] for c in sync.call_args_list], [d])

    def test_a_failed_sync_after_the_link_says_the_file_is_recorded(self):
        with tempfile.TemporaryDirectory() as d:
            with mock.patch.object(record, "_sync_dir", side_effect=OSError(5, "EIO")):
                with self.assertRaises(record.NotDurable) as cm:
                    record.write(d, self.rec())
            path = os.path.join(d, "universe_2026-03-27.json")
            self.assertEqual(cm.exception.path, path)
            self.assertIn("is recorded", str(cm.exception))
            self.assertEqual(record.read(path), self.rec())
            self.assertEqual(os.listdir(d), ["universe_2026-03-27.json"])

    def test_no_partial_file_is_left_when_serialising_fails(self):
        bad = self.rec()
        bad["bars"] = [object()]
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(TypeError):
                record.write(d, bad)
            self.assertEqual(os.listdir(d), [])


class EndToEndTest(unittest.TestCase):
    def test_a_failed_fetch_writes_nothing_and_a_good_one_leaves_no_key(self):
        key_bearing = {"status": "OK", "results": [ticker("AAA")],
                       "next_url": "https://api.massive.com/v3/reference/tickers?cursor=2&apiKey=" + KEY}
        with tempfile.TemporaryDirectory() as d:
            client, _, _ = make_client({"status": "OK", "resultsCount": 0, "results": []})
            with self.assertRaises(steps.MissingData):
                steps.run_universe(client, DAY, d, now=lambda: NOW)
            self.assertEqual(os.listdir(d), [])
            client, _, _ = make_client(grouped(bar("AAA")), key_bearing, {"status": "OK", "results": []})
            path = steps.run_universe(client, DAY, d, now=lambda: NOW)
            with open(path) as f:
                self.assertNotIn(KEY, f.read())


if __name__ == "__main__":
    unittest.main()
