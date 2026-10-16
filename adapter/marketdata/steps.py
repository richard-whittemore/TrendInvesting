"""The fetch steps. Each returns a record (raw rows, source, fetch time) or
raises MissingData; nothing is ranked here (ranking is a Go rule), and
nothing is written unless the whole fetch succeeded.

Daily-bar source: Massive, not IBKR. Per instrument, three requests: raw
(unadjusted) bars, split-adjusted bars, and dividend events (the
dividend-adjusted view is derived downstream from raw bars and dividends).
At 5 requests a minute that is about 40 seconds an instrument, so SPY, IEF,
SHY and a few dozen held stocks fit one overnight run; fetching all 500
ranked names (about 1500 requests, over 5 hours) does not, and needs IBKR
history or a paid plan.
"""
import datetime as dt
import zoneinfo

from . import nyse_calendar, record as recording
from .massive import MarketDataError

NEW_YORK = zoneinfo.ZoneInfo("America/New_York")
SOURCE = "massive"


class MissingData(MarketDataError):
    """Required data was absent; the step failed closed and wrote nothing."""


def _stamp(now):
    return now().astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _now():
    return dt.datetime.now(dt.timezone.utc)


def _rows(pages):
    return [r for p in pages for r in (p.get("results") or ())]


def find_session(client, asof):
    """(day, grouped body) of the latest NYSE trading day on or before
    ``asof``. Holidays come from the calendar; an empty response on a trading
    day (data not yet published, or a transient fault) is MissingData."""
    day = asof
    while not nyse_calendar.is_trading_day(day):
        day -= dt.timedelta(days=1)
    body = client.grouped(day)
    if not body.get("results"):
        raise MissingData("no bars for trading day {}".format(day))
    return day, body


last_session = find_session


def fetch_universe(client, session, now=_now, grouped_body=None):
    """The raw inputs of the universe ranking for ``session``: every stock's
    unadjusted bar and the type-CS reference tickers dated that session."""
    body = grouped_body if grouped_body is not None else client.grouped(session)
    bars = body.get("results") or []
    if not bars:
        raise MissingData("no grouped daily bars for session {}".format(session))
    tickers = _rows(client.tickers(session, "CS"))
    if not tickers:
        raise MissingData("no common-stock reference tickers for session {}".format(session))
    return {"kind": "universe", "source": SOURCE, "session": session.isoformat(),
            "fetched_at": _stamp(now), "adjusted": False, "ticker_type": "CS",
            "bars": bars, "tickers": tickers}


def _bar_day(row):
    return dt.datetime.fromtimestamp(row["t"] / 1000, NEW_YORK).date()


def fetch_bars(client, instruments, session, lookback_days=400, now=_now):
    """Daily bars for ``instruments`` ending at ``session``: raw, split-adjusted
    and dividend events. Every instrument must have the session's bar in both
    views, or the whole step fails."""
    if not instruments:
        raise MissingData("no instruments configured")
    first = session - dt.timedelta(days=lookback_days)
    out = {}
    for sym in instruments:
        raw = _rows(client.daily_bars(sym, first, session, adjusted=False))
        split = _rows(client.daily_bars(sym, first, session, adjusted=True))
        divs = _rows(client.dividends(sym, first, session))
        for view, rows in (("raw", raw), ("split-adjusted", split)):
            if not any(_bar_day(r) == session for r in rows):
                raise MissingData("{} has no {} bar for session {}".format(sym, view, session))
        out[sym] = {"raw": raw, "split_adjusted": split, "dividends": divs}
    return {"kind": "bars", "source": SOURCE, "session": session.isoformat(), "fetched_at": _stamp(now),
            "lookback_days": lookback_days, "instruments": out}


def run_universe(client, session, directory, now=_now, grouped_body=None):
    return recording.write(directory, fetch_universe(client, session, now=now, grouped_body=grouped_body))


def run_bars(client, instruments, session, directory, lookback_days=400, now=_now):
    return recording.write(directory, fetch_bars(client, instruments, session, lookback_days, now=now))
