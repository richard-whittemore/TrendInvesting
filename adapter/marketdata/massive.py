"""Massive REST client for the market-data step.

The API key is sent only as a bearer header. It is never put in a URL,
logged, cached, printed or included in an error message. Requests are spaced
to Massive's Basic plan limit of 5 a minute; a 429 waits a minute and
retries, and gives up (fail closed) after ``max_attempts``. A redirect is
followed only to Massive itself over HTTPS: urllib copies the Authorization
header onto a redirected request, so a redirect anywhere else would hand
the key to that host.
"""
import datetime as dt
import json
import logging
import os
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = "https://api.massive.com"
KEYCHAIN_SERVICE = "MASSIVE_API_KEY"
OK_STATUSES = ("OK", "DELAYED")
log = logging.getLogger("marketdata")


class MarketDataError(Exception):
    """A request failed or returned something unusable. Never carries the key.
    ``status`` is the HTTP status when Massive answered with an error one, else None."""

    def __init__(self, message, status=None):
        super().__init__(message)
        self.status = status


def keychain_key(service=KEYCHAIN_SERVICE):
    """The key from the macOS Keychain, or "" when it is not there."""
    out = subprocess.run(["security", "find-generic-password", "-a", os.environ.get("USER", ""),
                          "-s", service, "-w"], capture_output=True, text=True)
    return out.stdout.strip() if out.returncode == 0 else ""


def load_key(env=None, secrets_file=None, keychain=keychain_key):
    """The API key: the MASSIVE_API_KEY environment variable, else the first
    line of ``secrets_file`` (a server's secrets file, mode 600, outside the
    repository), else the Keychain (the Mac)."""
    env = os.environ if env is None else env
    key = (env.get("MASSIVE_API_KEY") or "").strip()
    if not key and secrets_file:
        try:
            with open(secrets_file) as f:
                key = f.read().strip()
        except OSError as e:
            raise MarketDataError("cannot read secrets file {}: {}".format(secrets_file, e.strerror))
    if not key and keychain:
        key = keychain()
    if not key:
        raise MarketDataError("no Massive API key: set MASSIVE_API_KEY, a secrets file, or the Keychain item")
    return key


def _is_massive(url):
    parts = urllib.parse.urlsplit(url)
    return (parts.scheme, parts.netloc) == ("https", urllib.parse.urlsplit(BASE).netloc)


class MassiveOnlyRedirect(urllib.request.HTTPRedirectHandler):
    """Refuses (as the redirect's HTTP error) any redirect off Massive. An
    allowed redirect is one more request, so it waits on the request's
    ``massive_pace`` (the client's rate limit) and carries it to the next hop."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if not _is_massive(urllib.parse.urljoin(req.full_url, newurl)):
            raise urllib.error.HTTPError(req.full_url, code, "redirect off Massive refused", headers, fp)
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        pace = getattr(req, "massive_pace", None)
        if new is not None and pace is not None:
            pace()
            new.massive_pace = pace
        return new


OPENER = urllib.request.build_opener(MassiveOnlyRedirect())


def _strip_key(url):
    parts = urllib.parse.urlsplit(url)
    query = [(k, v) for k, v in urllib.parse.parse_qsl(parts.query) if k != "apiKey"]
    return urllib.parse.urlunsplit(parts._replace(query=urllib.parse.urlencode(query)))


class Client:
    def __init__(self, key, min_interval=12.5, max_attempts=3, retry_wait=60.0,
                 opener=OPENER.open, sleep=time.sleep, clock=time.monotonic):
        self._key = key
        self.min_interval, self.max_attempts, self.retry_wait = min_interval, max_attempts, retry_wait
        self._opener, self._sleep, self._clock = opener, sleep, clock
        self._last = None

    def __repr__(self):
        return "Client(min_interval={})".format(self.min_interval)

    def _pace(self):
        """Waits until ``min_interval`` has passed since the last request."""
        if self._last is not None:
            wait = self.min_interval - (self._clock() - self._last)
            if wait > 0:
                self._sleep(wait)
        self._last = self._clock()

    def get_url(self, url):
        url = _strip_key(url)
        parts = urllib.parse.urlsplit(url)
        if not _is_massive(url):
            raise MarketDataError("refusing a URL outside Massive: {}".format(parts.netloc))
        for attempt in range(1, self.max_attempts + 1):
            self._pace()
            log.debug("GET %s attempt %d", parts.path, attempt)
            req = urllib.request.Request(url, headers={"Authorization": "Bearer " + self._key})
            req.massive_pace = self._pace
            try:
                with self._opener(req, timeout=60) as resp:
                    body = json.loads(resp.read().decode())
                break
            except urllib.error.HTTPError as e:
                if e.code == 429 and attempt < self.max_attempts:
                    log.warning("429 from %s; waiting %ss", parts.path, self.retry_wait)
                    self._sleep(self.retry_wait)
                    continue
                raise MarketDataError("HTTP {} from {}".format(e.code, parts.path), status=e.code) from None
            except (urllib.error.URLError, OSError, ValueError) as e:
                raise MarketDataError("request to {} failed: {}".format(parts.path, type(e).__name__)) from None
        if not isinstance(body, dict) or body.get("status", "OK") not in OK_STATUSES:
            raise MarketDataError("unusable response from {}".format(parts.path))
        if isinstance(body.get("next_url"), str):
            body["next_url"] = _strip_key(body["next_url"])   # never keep a key the API echoes
        return body

    def get(self, path, params):
        return self.get_url(BASE + path + "?" + urllib.parse.urlencode(sorted(params.items())))

    def pages(self, path, params):
        out, seen = [self.get(path, params)], set()
        while out[-1].get("next_url"):
            cursor = out[-1]["next_url"]
            if cursor in seen:
                raise MarketDataError("pagination of {} repeated a cursor".format(path))
            seen.add(cursor)
            out.append(self.get_url(cursor))
        return out

    def grouped(self, day):
        """Every US stock's unadjusted daily bar for ``day``; one request."""
        return self.get("/v2/aggs/grouped/locale/us/market/stocks/" + day.isoformat(), {"adjusted": "false"})

    def tickers(self, day, type_):
        return self.pages("/v3/reference/tickers", {"date": day.isoformat(), "type": type_,
                                                    "market": "stocks", "limit": 1000})

    def daily_bars(self, ticker, first, last, adjusted):
        """Daily bars first..last; ``adjusted`` is Massive's split adjustment."""
        return self.pages("/v2/aggs/ticker/{}/range/1/day/{}/{}".format(ticker, first.isoformat(), last.isoformat()),
                          {"adjusted": "true" if adjusted else "false", "sort": "asc", "limit": 50000})

    def dividends(self, ticker, first, last):
        return self.pages("/v3/reference/dividends", {"ticker": ticker, "limit": 1000,
                                                      "ex_dividend_date.gte": first.isoformat(),
                                                      "ex_dividend_date.lte": last.isoformat()})

    def splits(self, first, last):
        """Every split with an execution date in first..last (all tickers)."""
        return self.pages("/v3/reference/splits", {"limit": 1000, "execution_date.gte": first.isoformat(),
                                                   "execution_date.lte": last.isoformat()})

    def all_dividends(self, first, last):
        """Every cash dividend with an ex-dividend date in first..last (all tickers)."""
        return self.pages("/v3/reference/dividends", {"limit": 1000, "ex_dividend_date.gte": first.isoformat(),
                                                      "ex_dividend_date.lte": last.isoformat()})
