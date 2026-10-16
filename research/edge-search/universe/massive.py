# Research script, not production code. Builds decision D8's live universe
# (docs/live-trading/plan.md, section 4) from Massive's REST API, for the
# comparison with QuantConnect's universe that keeps Phase 1 open: every
# US-listed common stock, ranked by one session's unadjusted dollar volume
# (close x volume) on the last session before the month, price at least $5,
# the top 500. As in the backtest's Coarse step (momentum/defensive_v2.py),
# except that QuantConnect's HasFundamentalData has no equivalent here.
#
# Inputs, both as of the tested date (plan, section 4):
# - which stocks traded, and their close and volume: the grouped daily bars
#   endpoint, unadjusted, one request for the whole market;
# - which are common stock: the reference tickers endpoint with `date`,
#   filtered by type. COMMON_TYPES keeps ordinary common stock and ADRs of
#   it, since the backtest's universe holds foreign companies' US listings.
#
# The API key is read from the macOS Keychain (service MASSIVE_API_KEY) and
# sent only as a header. Responses are cached as files under the data
# directory, outside the repository, so a rerun costs no requests. The
# Basic plan allows 5 requests a minute and two years of history
# (massive.com/pricing, checked 2026-10-16).
import datetime as _dt
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = "https://api.massive.com"
COMMON_TYPES = ("CS", "ADRC")
UNIVERSE_SIZE = 500
PRICE_FLOOR = 5.0


def rank_universe(bars, common, size=UNIVERSE_SIZE, floor=PRICE_FLOOR):
    """The ``size`` tickers in ``common`` with the largest close x volume
    among ``bars`` (grouped daily rows), close at least ``floor``. OTC rows
    and rows without a close or volume are skipped; ties go by ticker."""
    rows = [r for r in bars
            if r.get("T") in common and not r.get("otc")
            and r.get("c") is not None and r.get("v") is not None and r["c"] >= floor]
    rows.sort(key=lambda r: (-r["c"] * r["v"], r["T"]))
    return [r["T"] for r in rows[:size]]


def common_tickers(pages, types=COMMON_TYPES):
    """Tickers of US exchange-listed stocks whose type is in ``types``, from
    reference-ticker response pages."""
    return {r["ticker"] for page in pages for r in page.get("results") or ()
            if r.get("type") in types and r.get("market") == "stocks" and r.get("locale") == "us"}


def candidate_sessions(year, month, back=10):
    """Weekdays before the first day of ``year``-``month``, latest first."""
    day, out = _dt.date(year, month, 1), []
    while len(out) < back:
        day -= _dt.timedelta(days=1)
        if day.weekday() < 5:
            out.append(day)
    return out


def last_session_before(client, year, month):
    """(date, bars) of the last session with bars before the month starts;
    a holiday returns no bars and is skipped."""
    for day in candidate_sessions(year, month):
        body = client.grouped(day)
        if body.get("results"):
            return day, body["results"]
    raise RuntimeError("no session with bars in the ten weekdays before {}-{:02d}".format(year, month))


class Client:
    """GET requests to Massive, cached by URL (without the key) and spaced
    ``min_interval`` seconds apart; one retry after a minute on HTTP 429."""

    def __init__(self, key, cache_dir, min_interval=12.5, opener=urllib.request.urlopen,
                 sleep=time.sleep, clock=time.monotonic):
        self.key, self.cache_dir, self.min_interval = key, cache_dir, min_interval
        self.opener, self.sleep, self.clock = opener, sleep, clock
        self.last = None
        os.makedirs(cache_dir, exist_ok=True)

    @staticmethod
    def _strip_key(url):
        parts = urllib.parse.urlsplit(url)
        query = [(k, v) for k, v in urllib.parse.parse_qsl(parts.query) if k != "apiKey"]
        return urllib.parse.urlunsplit(parts._replace(query=urllib.parse.urlencode(sorted(query))))

    def _cache_path(self, url):
        return os.path.join(self.cache_dir, re.sub(r"[^A-Za-z0-9._=-]+", "_", url[len(BASE):]).strip("_") + ".json")

    def get_url(self, url):
        url = self._strip_key(url)
        path = self._cache_path(url)
        if os.path.exists(path):
            with open(path) as f:
                return json.load(f)
        for attempt in (0, 1):
            if self.last is not None:
                wait = self.min_interval - (self.clock() - self.last)
                if wait > 0:
                    self.sleep(wait)
            self.last = self.clock()
            req = urllib.request.Request(url, headers={"Authorization": "Bearer " + self.key})
            try:
                with self.opener(req, timeout=60) as resp:
                    body = json.loads(resp.read().decode())
                break
            except urllib.error.HTTPError as e:
                if e.code != 429 or attempt:
                    raise
                self.sleep(60.0)
        with open(path, "w") as f:
            json.dump(body, f)
        return body

    def get(self, path, params):
        return self.get_url(BASE + path + "?" + urllib.parse.urlencode(sorted(params.items())))

    def grouped(self, day):
        return self.get("/v2/aggs/grouped/locale/us/market/stocks/" + day.isoformat(), {"adjusted": "false"})

    def tickers(self, day, type_):
        pages = [self.get("/v3/reference/tickers", {"date": day.isoformat(), "type": type_,
                                                    "market": "stocks", "limit": 1000})]
        while pages[-1].get("next_url"):
            pages.append(self.get_url(pages[-1]["next_url"]))
        return pages


def key_from_keychain(service="MASSIVE_API_KEY"):
    return subprocess.run(["security", "find-generic-password", "-a", os.environ["USER"], "-s", service, "-w"],
                          capture_output=True, text=True, check=True).stdout.strip()


def month_universe(client, year, month, types=COMMON_TYPES):
    """(session date, ranked tickers) for the rebalance in ``year``-``month``."""
    day, bars = last_session_before(client, year, month)
    pages = [p for t in types for p in client.tickers(day, t)]
    return day, rank_universe(bars, common_tickers(pages, types))


def main(argv):
    """massive.py FIRST LAST DATA_DIR: writes DATA_DIR/universe_YYYY-MM.json
    for each month FIRST..LAST (YYYY-MM), caching responses in DATA_DIR/cache."""
    first, last, data_dir = argv
    (y, m), end = map(int, first.split("-")), tuple(map(int, last.split("-")))
    client = Client(key_from_keychain(), os.path.join(data_dir, "cache"))
    while (y, m) <= end:
        day, names = month_universe(client, y, m)
        with open(os.path.join(data_dir, "universe_{}-{:02d}.json".format(y, m)), "w") as f:
            json.dump({"month": "{}-{:02d}".format(y, m), "session": day.isoformat(), "names": names}, f)
        print("{}-{:02d} session {} names {}".format(y, m, day, len(names)))
        y, m = (y + 1, 1) if m == 12 else (y, m + 1)


if __name__ == "__main__":
    main(sys.argv[1:])
