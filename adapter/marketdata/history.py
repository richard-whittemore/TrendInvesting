"""Raw bar history and corporate actions, recorded once and read back.

History is Massive's grouped daily endpoint, one recorded request per
session for every US stock (unadjusted), so 253 sessions of closes for the
whole market cost 253 requests (about 53 minutes at 5 a minute). Splits and
dividends come from the bulk reference endpoints, filtered by date and
paginated, and are recorded per window. Recordings are never overwritten, so
a backfill is resumable. Closed
days come from the NYSE calendar (nyse_calendar.py), never from an empty
response: an empty response on a trading day is MissingData, records nothing
and is retried on the next run. A trading day with no recording is a Gap and
fails the read.
"""
import datetime as dt
import glob
import os

from . import nyse_calendar, record
from .steps import MissingData, SOURCE, _now, _stamp


class Gap(Exception):
    """A weekday in the requested range has no recording."""


def _path(directory, day):
    return os.path.join(directory, "grouped_{}.json".format(day.isoformat()))


def _weekdays_back(asof):
    day = asof
    while True:
        if nyse_calendar.is_trading_day(day):
            yield day
        day -= dt.timedelta(days=1)


def backfill(client, directory, asof, sessions, max_weekdays=None, now=_now):
    """Records the grouped bars of the last ``sessions`` sessions on or before
    ``asof``, skipping any day already on disk. Returns counts."""
    max_weekdays = max_weekdays if max_weekdays is not None else int(sessions * 1.5) + 10
    have = fetched = 0
    for n, day in enumerate(_weekdays_back(asof)):
        if have == sessions:
            break
        if n >= max_weekdays:
            raise MissingData("found {} of {} sessions in {} weekdays up to {}".format(have, sessions, max_weekdays, asof))
        path = _path(directory, day)
        if os.path.exists(path):
            if not record.read(path).get("bars"):
                raise Gap("recording for {} has no bars".format(day))
            have += 1
            continue
        bars = client.grouped(day).get("results") or []
        if not bars:
            raise MissingData("no bars for trading day {}".format(day))
        record.write(directory, {"kind": "grouped", "source": SOURCE, "session": day.isoformat(),
                                 "fetched_at": _stamp(now), "adjusted": False, "bars": bars})
        fetched += 1
        have += 1
    return {"sessions": have, "fetched": fetched}


def load(directory, first, last):
    """[(session, bars)] oldest first for the trading days in first..last.
    Raises Gap for a trading day with no recording or a recording with no bars."""
    out, day = [], first
    while day <= last:
        if nyse_calendar.is_trading_day(day):
            path = _path(directory, day)
            if not os.path.exists(path):
                raise Gap("no recording for {}".format(day.isoformat()))
            bars = record.read(path)["bars"]
            if not bars:
                raise Gap("recording for {} has no bars".format(day.isoformat()))
            out.append((day, bars))
        day += dt.timedelta(days=1)
    return out


def record_actions(client, first, last, directory, now=_now):
    """Records splits and dividends dated first..last as two files; returns
    their paths. Nothing is written if either fetch fails."""
    fetched = [("splits", client.splits(first, last)), ("dividends", client.all_dividends(first, last))]
    stamp = _stamp(now)
    paths = []
    for kind, pages in fetched:
        rows = [r for p in pages for r in (p.get("results") or ())]
        paths.append(record.write(directory, {"kind": kind, "source": SOURCE, "session": last.isoformat(),
                                              "from": first.isoformat(), "fetched_at": stamp, "rows": rows}))
    return paths


def load_actions(directory, kind):
    """All recorded rows of ``kind`` (splits or dividends), oldest window first."""
    rows = []
    for path in sorted(glob.glob(os.path.join(directory, kind + "_*_*.json"))):
        rows.extend(record.read(path)["rows"])
    return rows
