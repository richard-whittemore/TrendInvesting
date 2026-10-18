"""Sector labels for the universe snapshot, recorded once per session.

Source (docs/live-trading/plan.md section 4, "Sectors", chosen 2026-10-15
on docs/research/sector-labels-2026-10.md): the GICS sector in iShares'
daily IWV (Russell 3000) holdings file where it lists a stock, else the
sector in Nasdaq's stock screener; a stock neither labels has no label, and
the momentum rule ranks it uncapped, as the backtest ranked a stock with no
Morningstar sector (ADR 0026 point 8, the owner's decision of 2026-10-18).
A file whose sector column is missing, or a session in which no label maps,
is refused rather than written as empty labels; the momentum rule withholds
a month with too many unlabelled names as labels-missing.

Both sources' sectors are mapped by name onto Morningstar's eleven sectors,
the scheme the backtest's cap counted (QuantConnect's MorningstarSectorCode),
using the mappings the sector-label study measured with
(research/edge-search/sectors/compare.py, and the Nasdaq table in the study's
part 4). One vocabulary is needed because the cap counts picks per label: a
technology stock GICS calls "Information Technology" and one Nasdaq calls
"Technology" must count against the same cap. A label a mapping does not
name (Nasdaq's "Miscellaneous", an empty sector, iShares' cash rows) is no
label from that source. The mapping is a judgment, not an official
concordance; the recordings keep each source's own label.

Neither source needs a key. Both responses are recorded whole (the iShares
CSV text, the screener's rows), so the labels can be re-derived. Only the
sources not yet recorded for the session are fetched, and nothing is
written unless every one fetched answered usably.

Both sources serve only today's file, so a session's labels are fetched
while that session is the latest one: after its close plus the settle delay
(steps.SETTLE) and before the next session's close. A past session is
refused (Backdated) before any fetch, and ``latest`` refuses a recording
whose fetch time falls outside its session's close-to-next-close window,
so a backtest never reads labels taken after the fact.
"""
import csv
import datetime as dt
import glob
import io
import json
import os
import re
import urllib.error
import urllib.parse
import urllib.request

from . import nyse_calendar, record
from .massive import MarketDataError
from .steps import SETTLE, MissingData, _now, _stamp, require_settled

IWV_SOURCE = "ishares-iwv-gics"
NASDAQ_SOURCE = "nasdaq-screener"
IWV_URL = ("https://www.ishares.com/us/products/239714/ishares-russell-3000-etf/1467271812596.ajax"
           "?fileType=csv&fileName=IWV_holdings&dataType=fund")
NASDAQ_URL = "https://api.nasdaq.com/api/screener/stocks?tableonly=true&download=true"
HOSTS = ("www.ishares.com", "api.nasdaq.com")
# Both sites refuse a request without a browser-like User-Agent.
USER_AGENT = "Mozilla/5.0 (compatible; TrendInvesting market-data step)"

MORNINGSTAR_SECTORS = ("Basic Materials", "Consumer Cyclical", "Financial Services", "Real Estate",
                       "Consumer Defensive", "Healthcare", "Utilities", "Communication Services",
                       "Energy", "Industrials", "Technology")
#: iShares' GICS sector names onto Morningstar's (compare.py's GICS_TO_MORNINGSTAR).
GICS_SECTORS = {"Materials": "Basic Materials", "Consumer Discretionary": "Consumer Cyclical",
                "Financials": "Financial Services", "Real Estate": "Real Estate",
                "Consumer Staples": "Consumer Defensive", "Health Care": "Healthcare",
                "Utilities": "Utilities", "Communication": "Communication Services",
                "Energy": "Energy", "Industrials": "Industrials", "Information Technology": "Technology"}
#: Nasdaq's screener sector names onto Morningstar's (the study's part 4 table).
NASDAQ_SECTORS = {"Basic Materials": "Basic Materials", "Consumer Discretionary": "Consumer Cyclical",
                  "Finance": "Financial Services", "Real Estate": "Real Estate",
                  "Consumer Staples": "Consumer Defensive", "Health Care": "Healthcare",
                  "Utilities": "Utilities", "Telecommunications": "Communication Services",
                  "Energy": "Energy", "Industrials": "Industrials", "Technology": "Technology"}


class Unusable(MarketDataError):
    """A source answered with something that is not its usual file."""


class Backdated(MarketDataError):
    """Labels fetched for, or recorded under, a session that was no longer
    the latest one when they were fetched."""


def norm(ticker):
    """A ticker without punctuation, upper case: Massive writes "BRK.B",
    iShares "BRKB", Nasdaq "BRK/B"."""
    return re.sub(r"[^A-Z0-9]", "", ticker.upper())


def _is_source(url):
    parts = urllib.parse.urlsplit(url)
    return parts.scheme == "https" and parts.netloc in HOSTS


class SourceOnlyRedirect(urllib.request.HTTPRedirectHandler):
    """Refuses (as the redirect's HTTP error) any redirect off HTTPS on the two
    sources' hosts, so every hop is checked, not only the first; the pattern
    of massive.MassiveOnlyRedirect."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if not _is_source(urllib.parse.urljoin(req.full_url, newurl)):
            raise urllib.error.HTTPError(req.full_url, code, "redirect off the sector sources refused", headers, fp)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


OPENER = urllib.request.build_opener(SourceOnlyRedirect())


def http_get(url, opener=OPENER.open):
    """(text, URL read) for ``url``, which, like every redirect followed from
    it, must be HTTPS on one of the two sources' hosts. The URL read is the
    last hop's, so a recording names where its body actually came from."""
    parts = urllib.parse.urlsplit(url)
    if not _is_source(url):
        raise MarketDataError("refusing a URL outside the sector sources: {}://{}".format(parts.scheme, parts.netloc))
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, "Accept": "*/*"})
    try:
        with opener(req, timeout=60) as resp:
            read_from = resp.geturl()
            if not _is_source(read_from):
                raise MarketDataError("refusing a response read outside the sector sources")
            return resp.read().decode("utf-8-sig"), read_from
    except urllib.error.HTTPError as e:
        raise MarketDataError("HTTP {} from {}".format(e.code, parts.netloc)) from None
    except (urllib.error.URLError, OSError, UnicodeDecodeError) as e:
        raise MarketDataError("request to {} failed: {}".format(parts.netloc, type(e).__name__)) from None


def parse_iwv(text):
    """(as-of text, {ticker: GICS sector}) from an iShares holdings CSV's
    equity rows."""
    lines = [line.lstrip("﻿") for line in text.splitlines()]
    start = next((i for i, line in enumerate(lines) if line.startswith("Ticker,")), None)
    if start is None:
        raise Unusable("no 'Ticker,' header: not an iShares holdings file")
    as_of = next((line.split(",", 1)[1].strip().strip('"') for line in lines[:start]
                  if line.startswith("Fund Holdings as of,")), None)
    reader = csv.DictReader(io.StringIO("\n".join(lines[start:])))
    if "Sector" not in (reader.fieldnames or ()):
        # A renamed column would read as no label for every row, which the
        # momentum rule ranks with no sector cap: refused, not emitted empty.
        raise Unusable("the iShares file has no 'Sector' column")
    rows = {}
    for row in reader:
        if row.get("Asset Class") == "Equity" and row.get("Ticker"):
            _put(rows, norm(row["Ticker"]), (row.get("Sector") or "").strip())
    if not rows:
        raise Unusable("the iShares file lists no equity")
    return as_of, _unambiguous(rows)


def parse_nasdaq(body):
    """{symbol: sector} from the screener's JSON response."""
    rows = (body.get("data") or {}).get("rows") if isinstance(body, dict) else None
    if not rows or not all(isinstance(r, dict) and r.get("symbol") for r in rows):
        raise Unusable("the screener response has no rows of symbols")
    if not any("sector" in r for r in rows):
        # As for the iShares column: a renamed field is refused, not read as
        # no label for every row.
        raise Unusable("the screener response has no 'sector' field")
    out = {}
    for r in rows:
        _put(out, norm(r["symbol"]), (r.get("sector") or "").strip())
    return _unambiguous(out)


_AMBIGUOUS = object()


def _put(rows, key, sector):
    """Two rows that normalise to one ticker with different sectors label it
    ambiguously, so neither is used."""
    if key in rows and rows[key] != sector:
        rows[key] = _AMBIGUOUS
    else:
        rows[key] = sector


def _unambiguous(rows):
    return {k: v for k, v in rows.items() if v is not _AMBIGUOUS}


def labels(iwv, nasdaq):
    """{normalised ticker: (Morningstar sector, source)} from the two
    recordings: IWV's GICS label where it maps, else Nasdaq's, else none.
    Raises Unusable when no label maps at all."""
    out = {}
    for ticker, sector in parse_nasdaq({"data": {"rows": nasdaq["rows"]}}).items():
        if sector in NASDAQ_SECTORS:
            out[ticker] = (NASDAQ_SECTORS[sector], NASDAQ_SOURCE)
    for ticker, sector in parse_iwv(iwv["body"])[1].items():
        if sector in GICS_SECTORS:
            out[ticker] = (GICS_SECTORS[sector], IWV_SOURCE)
    if not out:
        # Neither source's sectors map: a renamed vocabulary or a broken
        # file, not a session with no labelled stock. Refused rather than
        # written as a snapshot with no label (ADR 0026 point 5).
        raise Unusable("neither source's sectors map onto a Morningstar sector")
    return out


def _fetch_iwv(get, day, stamp):
    text, url = get(IWV_URL)
    as_of, _ = parse_iwv(text)
    return {"kind": "sectors-iwv", "source": IWV_SOURCE, "session": day, "fetched_at": stamp,
            "url": url, "as_of": as_of, "body": text}


def _fetch_nasdaq(get, day, stamp):
    text, url = get(NASDAQ_URL)
    try:
        body = json.loads(text)
    except ValueError:
        raise Unusable("the screener response is not JSON") from None
    parse_nasdaq(body)
    return {"kind": "sectors-nasdaq", "source": NASDAQ_SOURCE, "session": day, "fetched_at": stamp,
            "url": url, "rows": body["data"]["rows"]}


def _window(session):
    """(from, until): a session's labels are fetched at or after its close
    and before the next session's close."""
    return nyse_calendar.session_close(session), nyse_calendar.session_close(nyse_calendar.sessions_after(session, 1)[0])


def record_sectors(directory, session, get=http_get, now=_now, settle=SETTLE):
    """Records each source not yet recorded for ``session``, fetching only
    those; returns the paths written. ``session`` must be the latest NYSE
    session settled by ``now`` (see above). Writes nothing unless every
    source fetched answered usably."""
    if not nyse_calendar.is_trading_day(session):
        raise MarketDataError("{} is not an NYSE trading day".format(session))
    require_settled(session, now, settle)
    if now() >= _window(session)[1]:
        raise Backdated("{} is no longer the latest session; the sources serve only today's files".format(session))
    day = session.isoformat()
    missing = []
    for kind, fetch in (("sectors-iwv", _fetch_iwv), ("sectors-nasdaq", _fetch_nasdaq)):
        path = os.path.join(directory, record.filename({"kind": kind, "session": day}))
        if os.path.exists(path):
            record.read(path)   # raises Corrupt rather than trusting a damaged file
        else:
            missing.append(fetch)
    stamp = _stamp(now)
    fetched = [fetch(get, day, stamp) for fetch in missing]
    return [record.write(directory, rec) for rec in fetched]


def latest(directory, kind, session, max_age_days=7):
    """The newest ``kind`` recording dated on or before ``session`` and no
    more than ``max_age_days`` before it. Raises MissingData otherwise."""
    best = None
    for path in glob.glob(os.path.join(directory, kind + "_*.json")):
        day = dt.date.fromisoformat(os.path.basename(path)[len(kind) + 1:-len(".json")])
        if day <= session and (best is None or day > best[0]):
            best = (day, path)
    if best is None:
        raise MissingData("no {} recording on or before {}".format(kind, session))
    if (session - best[0]).days > max_age_days:
        raise MissingData("the latest {} recording, {}, is more than {} days before {}".format(
            kind, best[0], max_age_days, session))
    rec = record.read(best[1])
    try:
        fetched = dt.datetime.strptime(rec.get("fetched_at"), "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc)
    except (TypeError, ValueError):
        raise Backdated("{} has fetch time {!r}".format(best[1], rec.get("fetched_at"))) from None
    start, until = _window(best[0])
    if not start <= fetched < until:
        raise Backdated("{} was fetched at {}, outside {}'s window {} to {}".format(
            best[1], rec["fetched_at"], best[0], start.isoformat(), until.isoformat()))
    return rec
