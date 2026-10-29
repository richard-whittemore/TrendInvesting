"""Raw bar history and corporate actions, recorded once and read back.

History is Massive's grouped daily endpoint, one recorded request per
session for every US stock (unadjusted), so 260 sessions of closes for the
whole market cost 260 requests (about 54 minutes at 5 a minute). Splits and
dividends come from the bulk reference endpoints, filtered by date and
paginated, and are recorded per window; windows overlap, so an action read
from several is counted once (by Massive's id, or the whole row when it has
none), two different copies of one id are refused, and the windows must
cover the requested range. Recordings are never overwritten, so a backfill
and an interrupted action window are resumable. A session is fetched only
after its close plus the settle delay. Closed
days come from the NYSE calendar (nyse_calendar.py), never from an empty
response: an empty response on a trading day is MissingData, records nothing
and is retried on the next run. A trading day with no recording is a Gap and
fails the read.
"""
import datetime as dt
import glob
import json
import os

from . import nyse_calendar, record
from .steps import SETTLE, SOURCE, MissingData, _now, _stamp, require_settled

KINDS = ("splits", "dividends")


class Gap(Exception):
    """A trading day, or a day of an action range, has no recording."""


class ConflictingActions(Exception):
    """Two recordings hold different copies of one corporate action."""


def _path(directory, day):
    return os.path.join(directory, "grouped_{}.json".format(day.isoformat()))


def _weekdays_back(asof):
    day = asof
    while True:
        if nyse_calendar.is_trading_day(day):
            yield day
        day -= dt.timedelta(days=1)


def backfill(client, directory, asof, sessions, max_weekdays=None, now=_now, settle=SETTLE):
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
            _check_bars(path, day)
            have += 1
            continue
        require_settled(day, now, settle)
        bars = client.grouped(day).get("results") or []
        if not bars:
            raise MissingData("no bars for trading day {}".format(day))
        try:
            record.write(directory, {"kind": "grouped", "source": SOURCE, "session": day.isoformat(),
                                     "fetched_at": _stamp(now), "adjusted": False, "bars": bars})
        except record.AlreadyRecorded:
            # Another job (Boost's and the IRAs' data jobs may run at once)
            # recorded the session between the check above and this write: a
            # recording is written once, so that one stands (record.py), and
            # it is checked as one found on disk is.
            _check_bars(path, day)
            have += 1
            continue
        fetched += 1
        have += 1
    return {"sessions": have, "fetched": fetched}


def _check_bars(path, day):
    """A grouped recording found on disk must read (record.read raises Corrupt) and hold bars."""
    if not record.read(path).get("bars"):
        raise Gap("recording for {} has no bars".format(day))


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


def record_actions(client, first, last, directory, now=_now, settle=SETTLE):
    """Records splits and dividends dated first..last as two files; returns
    their paths. A kind already recorded for the window is verified and kept,
    so an interrupted window resumes; both recorded is AlreadyRecorded.
    Nothing is written if a fetch fails, and nothing is fetched before the
    last session in the window has closed plus ``settle``."""
    session = last
    while not nyse_calendar.is_trading_day(session):
        session -= dt.timedelta(days=1)
    require_settled(session, now, settle)
    paths = [os.path.join(directory, record.filename({"kind": k, "session": last.isoformat(),
                                                      "from": first.isoformat()})) for k in KINDS]
    present = [os.path.exists(p) for p in paths]
    for p, there in zip(paths, present):
        if there:
            record.read(p)   # raises Corrupt
    if all(present):
        raise record.AlreadyRecorded(paths[0])
    fetch = {"splits": client.splits, "dividends": client.all_dividends}
    fetched = [(k, fetch[k](first, last)) for k, there in zip(KINDS, present) if not there]
    stamp = _stamp(now)
    written = {}
    for kind, pages in fetched:
        rows = [r for p in pages for r in (p.get("results") or ())]
        try:
            written[kind] = record.write(directory, {"kind": kind, "source": SOURCE, "session": last.isoformat(),
                                                     "from": first.isoformat(), "fetched_at": stamp, "rows": rows})
        except record.AlreadyRecorded as e:
            # Another job recorded this kind's window first (Boost's and the
            # IRAs' data jobs may run at once): that recording stands, is
            # checked as one found on disk is, and the other kind is still
            # written.
            record.read(str(e))
            written[kind] = str(e)
    return [written.get(k, p) for k, p in zip(KINDS, paths)]


def _canonical(row):
    return json.dumps(row, sort_keys=True, separators=(",", ":"))


def load_actions(directory, kind, first, last, tickers=None):
    """Every recorded row of ``kind`` (splits or dividends), oldest window
    first, each action once. Raises Gap unless the recorded windows cover
    every day of first..last, and ConflictingActions if two recordings hold
    different rows for one action.

    With ``tickers`` (a set), only those tickers' rows are returned and a
    conflict is raised only when either copy is one of theirs, so a revised
    figure for an instrument the caller never reads does not refuse it; a
    copy that moves an action onto or off one of them still does."""
    windows, rows, seen = [], [], {}
    for path in sorted(glob.glob(os.path.join(directory, kind + "_*_*.json"))):
        rec = record.read(path)
        windows.append((dt.date.fromisoformat(rec["from"]), dt.date.fromisoformat(rec["session"])))
        for r in rec["rows"]:
            body = _canonical(r)
            key = ("id", r["id"]) if r.get("id") else ("row", body)
            if key in seen:
                kept, ticker = seen[key]
                if kept != body and (tickers is None or ticker in tickers or r.get("ticker") in tickers):
                    raise ConflictingActions("{} {} differs between recordings".format(kind, key[1]))
                continue
            seen[key] = (body, r.get("ticker"))
            if tickers is None or r.get("ticker") in tickers:
                rows.append(r)
    reach = first - dt.timedelta(days=1)
    for a, b in sorted(windows):
        if a > reach + dt.timedelta(days=1):
            break
        reach = max(reach, b)
    if reach < last:
        raise Gap("no {} recording covers {}".format(kind, (reach + dt.timedelta(days=1)).isoformat()))
    return rows
