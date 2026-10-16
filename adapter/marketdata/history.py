"""Raw bar history and corporate actions, recorded once and read back.

History is Massive's grouped daily endpoint, one recorded request per
session for every US stock (unadjusted), so 253 sessions of closes for the
whole market cost 253 requests (about 53 minutes at 5 a minute). Splits and
dividends come from the bulk reference endpoints, filtered by date and
paginated, and are recorded per window. Recordings are never overwritten, so
a backfill is resumable. A weekday with no bars is recorded as closed (a
holiday); a weekday with no recording at all is a Gap and fails the read.
"""
import datetime as dt
import glob
import os

from . import record
from .steps import MissingData, SOURCE, _now, _stamp


class Gap(Exception):
    """A weekday in the requested range has no recording."""


def _path(directory, day):
    return os.path.join(directory, "grouped_{}.json".format(day.isoformat()))


def _weekdays_back(asof):
    day = asof
    while True:
        if day.weekday() < 5:
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
            have += 0 if record.read(path).get("closed") else 1
            continue
        bars = client.grouped(day).get("results") or []
        record.write(directory, {"kind": "grouped", "source": SOURCE, "session": day.isoformat(),
                                 "fetched_at": _stamp(now), "adjusted": False,
                                 "closed": not bars, "bars": bars})
        fetched += 1
        have += 1 if bars else 0
    return {"sessions": have, "fetched": fetched}


def load(directory, first, last):
    """[(session, bars)] oldest first for the sessions in first..last, closed
    days left out. Raises Gap for a weekday with no recording."""
    out, day = [], first
    while day <= last:
        if day.weekday() < 5:
            path = _path(directory, day)
            if not os.path.exists(path):
                raise Gap("no recording for {}".format(day.isoformat()))
            rec = record.read(path)
            if not rec.get("closed"):
                out.append((day, rec["bars"]))
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
