"""Engine inputs from the market-data recordings: the envelope encoding.

Reads only recorded files (record.py), never the network, so a replay of
the same recordings writes the same bytes. For the sessions first..last it
writes, in order:

1. one ``market.session.calendar`` v1 (source ``nyse-calendar``): every
   session from first to ``ahead`` sessions past last, from the committed
   NYSE rules (nyse_calendar.py). The momentum run refuses a session the
   calendar cannot see past, so ``ahead`` is at least 1;
2. per session, oldest first:
   - a ``market.bar.completed`` v2 for each requested instrument with a bar
     that session, sorted by instrument (event.CompletedBarAdjustedPayload):
     the raw view is the recorded grouped bar (unadjusted); the
     split-adjusted and the split- and dividend-adjusted views are adjusted
     forward from ``base`` by adjust.forward_factors, with the bulk splits
     and dividends recorded for the whole span (history.load_actions: each
     action once by its id, the windows covering the span), so a bar is
     never restated by a later run;
   - a ``market.universe.snapshot`` v1 for a session with a universe
     recording: one row per type-CS reference ticker with a bar that
     session, its unadjusted close and volume, and its sector label and
     source (sectors.py), sorted by instrument;
   - a ``market.session.closed`` v1 naming exactly the bars written.

Bars come from ``grouped_<session>.json`` (the backfill) or, for a session
the daily universe step recorded instead, the bars of
``universe_<session>.json``; two recordings of one session must agree. A
bar's ``recorded_at`` is when its recording was fetched; event times are the
session's close (4 p.m. New York, 1 p.m. on an early close).

Everything fails closed, and nothing is returned: a trading day with no
recording (Gap), a required instrument without a bar or a universe without
recent sector recordings (MissingData), action recordings that do not cover
the span (MissingData), two different copies of one action or two
disagreeing recordings of a session (Conflict), a malformed bar or row
(Malformed), or anything adjust.forward_factors cannot adjust exactly;
except that a ticker written only as a universe member is left out instead
(see encode).

``engine`` picks the inputs written. ``"momentum"`` (the default) writes
all of the above. ``"boost"`` writes only the bars of its one required
instrument and the session closes: the Boost handler reads one instrument
and refuses every other input type (targetweight.Handler, "Boost's inputs,
in stream order"), so it gets no calendar and no universe snapshot.

The stream file is written in full or not at all: a temporary file in the
same folder is synced, then hard-linked to the final name, which must not
exist, and the folder is synced (ADR 0018, as record.write does). A failed
write leaves nothing under the final name, so a retry can write it.

Envelopes carry the run identity the caller gives (each engine's own
configuration hash and strategy version) and contiguous sequences from
``first_sequence``; a composition that interleaves another producer's
inputs restamps them.
"""
import collections
import datetime as dt
import hashlib
import json
import math
import os
import tempfile

from . import adjust, history, nyse_calendar, record, sectors
from .massive import MarketDataError
from .steps import MissingData

SOURCE = "marketdata"
CALENDAR_SOURCE = "nyse-calendar"
# Go's event type and schema-version constants (internal/event), ADR 0015.
CALENDAR = ("market.session.calendar", 1)     # SessionCalendarSchemaVersion
UNIVERSE = ("market.universe.snapshot", 1)    # UniverseSnapshotSchemaVersion
BAR = ("market.bar.completed", 2)             # CompletedBarAdjustedVersion
SESSION_CLOSED = ("market.session.closed", 1)  # SessionClosedSchemaVersion

ENGINES = ("momentum", "boost")

Identity = collections.namedtuple("Identity", "configuration_hash strategy_version run_id")


class Gap(MarketDataError):
    """A trading day in the span has no recording, or one with no bars."""


class Conflict(MarketDataError):
    """Two recordings disagree about the same fact."""


class Malformed(MarketDataError):
    """A recorded row cannot be encoded as a valid input."""


Session = collections.namedtuple("Session", "day bars fetched_at universe")


def _utc(t):
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def _fetched_at(rec, path):
    text = rec.get("fetched_at")
    try:
        dt.datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ")
    except (TypeError, ValueError):
        raise Malformed("{} has fetch time {!r}".format(path, text)) from None
    return text


def _canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def load_sessions(directory, first, last):
    """[Session] for the trading days first..last, oldest first."""
    out = []
    for day in nyse_calendar.sessions(first, last):
        found = []
        for kind in ("grouped", "universe"):
            path = os.path.join(directory, "{}_{}.json".format(kind, day.isoformat()))
            if os.path.exists(path):
                found.append((kind, path, record.read(path)))
        if not found:
            raise Gap("no recording for trading day {}".format(day))
        if len(found) == 2 and (sorted(map(_canonical, found[0][2]["bars"]))
                                != sorted(map(_canonical, found[1][2]["bars"]))):
            raise Conflict("the grouped and universe recordings of {} hold different bars".format(day))
        _, path, rec = found[0]
        if not rec.get("bars"):
            raise Gap("the recording for {} has no bars".format(day))
        bars = {}
        for row in rec["bars"]:
            ticker = row.get("T")
            if not isinstance(ticker, str) or not ticker or ticker in bars:
                raise Malformed("{} holds a missing or repeated ticker {!r}".format(path, ticker))
            bars[ticker] = row
        universe = next((r for kind, _, r in found if kind == "universe"), None)
        out.append(Session(day, bars, _fetched_at(rec, path), universe))
    return out


def _actions(directory, kind, first, last):
    """history.load_actions, with its refusals as this module's errors."""
    try:
        return history.load_actions(directory, kind, first, last)
    except history.Gap as e:
        raise MissingData(str(e)) from e
    except history.ConflictingActions as e:
        raise Conflict(str(e)) from e


def _finite(value, positive):
    return (isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)
            and (value > 0 if positive else value >= 0))


def _check_bar(ticker, day, r):
    o, h, l, c, v = (r.get(k) for k in "ohlcv")
    if not all(_finite(x, True) for x in (o, h, l, c)) or not _finite(v, False):
        raise Malformed("{} bar on {} has o/h/l/c/v {!r}".format(ticker, day, (o, h, l, c, v)))
    if h < max(o, l, c) or l > min(o, c):
        raise Malformed("{} bar on {} is inconsistent: o/h/l/c {!r}".format(ticker, day, (o, h, l, c)))


def _view(label, r, price, shares):
    return {"view": label, "open": float(r["o"]) * price, "high": float(r["h"]) * price,
            "low": float(r["l"]) * price, "close": float(r["c"]) * price, "volume": float(r["v"]) / shares}


def _universe_rows(directory, day, rec, max_age_days):
    types = {}
    for t in rec.get("tickers") or ():
        sym, kind = t.get("ticker"), t.get("type")
        if not isinstance(sym, str) or not sym or not isinstance(kind, str) or not kind:
            raise Malformed("universe {} has a reference ticker {!r}".format(day, t))
        types[sym] = kind
    if not types:
        raise MissingData("universe {} has no reference tickers".format(day))
    labels = sectors.labels(sectors.latest(directory, "sectors-iwv", day, max_age_days),
                            sectors.latest(directory, "sectors-nasdaq", day, max_age_days))
    rows = {}
    for b in rec["bars"]:
        sym = b.get("T")
        if sym not in types:
            continue
        if sym in rows:
            raise Malformed("universe {} repeats {}".format(day, sym))
        c, v = b.get("c"), b.get("v")
        if not _finite(c, False) or not _finite(v, False):
            raise Malformed("universe {} row {} has close {!r} and volume {!r}".format(day, sym, c, v))
        sector, source = labels.get(sectors.norm(sym), ("", ""))
        rows[sym] = {"instrument_id": sym, "type": types[sym], "close": float(c), "volume": float(v),
                     "sector": sector, "sector_source": source}
    return [rows[s] for s in sorted(rows)]


def _factors(ticker, loaded, days, actions, base):
    """adjust.forward_factors for ``ticker``, after checking each of its bars;
    raises Malformed for a bad bar or anything that cannot be adjusted."""
    closes = {}
    for s in loaded:
        if ticker in s.bars:
            _check_bar(ticker, s.day, s.bars[ticker])
            closes[s.day] = float(s.bars[ticker]["c"])
    splits, dividends = actions
    try:
        return adjust.forward_factors(ticker, days, closes, splits, dividends, base)
    except adjust.CannotAdjust as e:
        raise Malformed(str(e)) from None


class _Stream:
    def __init__(self, identity, first_sequence):
        if not all(identity):
            raise ValueError("run identity is required")
        self.identity, self.sequence, self.out = identity, first_sequence, []

    def add(self, kind, id_, event_time, recorded_at, payload):
        raw = json.dumps(payload, separators=(",", ":"), allow_nan=False)
        self.out.append({
            "id": id_, "type": kind[0], "envelope_version": 1, "schema_version": kind[1],
            "event_time": event_time, "recorded_at": recorded_at, "sequence": self.sequence,
            "correlation_id": self.identity.run_id, "source": SOURCE,
            "strategy_version": self.identity.strategy_version,
            "configuration_hash": self.identity.configuration_hash,
            "payload_hash": hashlib.sha256(raw.encode()).hexdigest(), "payload": payload})
        self.sequence += 1


def encode(directory, first, last, identity, required=("SPY", "IEF", "SHY"), optional=(),
           universe_members=False, base=None, ahead=1, first_sequence=2, max_sector_age_days=7,
           engine="momentum", excluded=None):
    """The envelopes for first..last, as described above. ``required``
    instruments must have a bar every session; ``optional`` ones (and, with
    ``universe_members``, every type-CS ticker of the span's universe
    recordings) are written where they traded. ``base`` (default ``first``)
    is the session the adjusted views are based at; keep it fixed for the
    life of an engine's journal. ``engine`` is "momentum" or "boost", as
    described above.

    A ticker written only because it is a universe member, whose bars
    cannot be encoded (a malformed bar) or adjusted exactly (CannotAdjust),
    is left out: none of its bars is written, so the momentum rule finds no
    window for it and leaves it out of the ranking, recording why (ADR 0026
    point 5, "A universe stock missing any session ... is left out"); its
    universe snapshot row is still written. It is named, with the reason,
    in ``excluded`` (a dict, if given). A required or optional instrument
    that cannot be encoded still refuses the stream."""
    if engine not in ENGINES:
        raise ValueError("engine {!r} is not one of {}".format(engine, ", ".join(ENGINES)))
    boost = engine == "boost"
    if boost and (len(required) != 1 or optional or universe_members):
        raise ValueError("a boost stream names exactly one required instrument and nothing else")
    base = base or first
    if base > first or not nyse_calendar.is_trading_day(base):
        raise ValueError("base {} must be a session on or before the first {}".format(base, first))
    if first > last or ahead < 1:
        raise ValueError("need first <= last and ahead >= 1")
    loaded = load_sessions(directory, base, last)
    days = [s.day for s in loaded]
    span = [s for s in loaded if s.day >= first]
    if not span:
        raise MissingData("no session in {}..{}".format(first, last))
    named = set(required) | set(optional)
    wanted = set(named)
    if universe_members:
        for s in span:
            if s.universe is not None:
                cs = {t.get("ticker") for t in s.universe.get("tickers") or () if t.get("type") == "CS"}
                wanted |= cs & set(s.bars)
    start = base + dt.timedelta(days=1)
    by_ticker = collections.defaultdict(lambda: ([], []))
    for r in _actions(directory, "splits", start, last):
        by_ticker[r.get("ticker")][0].append(r)
    for r in _actions(directory, "dividends", start, last):
        by_ticker[r.get("ticker")][1].append(r)

    factors, left_out = {}, {}
    for ticker in sorted(wanted):
        try:
            factors[ticker] = _factors(ticker, loaded, days, by_ticker[ticker], base)
        except Malformed as e:
            if ticker in named:
                raise
            left_out[ticker] = str(e)
    wanted -= set(left_out)
    if excluded is not None:
        excluded.update(left_out)
    for ticker in required:
        missing = [s.day.isoformat() for s in span if ticker not in s.bars]
        if missing:
            raise MissingData("{} has no bar on {}".format(ticker, ", ".join(missing)))

    stream = _Stream(identity, first_sequence)
    through = nyse_calendar.sessions_after(last, ahead)[-1]
    opening = _utc(nyse_calendar.session_close(span[0].day).astimezone(dt.timezone.utc))
    if not boost:
        stream.add(CALENDAR, "marketdata:calendar:{}:{}".format(span[0].day, through), opening, opening,
                   {"source": CALENDAR_SOURCE,
                    "sessions": [d.isoformat() for d in nyse_calendar.sessions(span[0].day, through)]})
    for s in span:
        end, session = _utc(nyse_calendar.session_close(s.day).astimezone(dt.timezone.utc)), s.day.isoformat()
        ids = sorted(t for t in wanted if t in s.bars)
        if not ids:
            raise MissingData("no requested instrument has a bar on {}".format(session))
        for ticker in ids:
            r = s.bars[ticker]
            split_m, total_m = factors[ticker][s.day]
            stream.add(BAR, "marketdata:bar:{}:{}".format(ticker, session), end, s.fetched_at, {
                "instrument_id": ticker, "period_end": end,
                "split_adjusted": _view("split-adjusted", r, split_m, split_m),
                "raw": _view("raw", r, 1.0, 1.0),
                "adjusted": _view("split-and-dividend-adjusted", r, total_m, split_m),
                "adjusted_base": base.isoformat()})
        if s.universe is not None and not boost:
            stream.add(UNIVERSE, "marketdata:universe:{}".format(session), end,
                       _fetched_at(s.universe, "universe " + session),
                       {"session": session, "source": s.universe.get("source"),
                        "rows": _universe_rows(directory, s.day, s.universe, max_sector_age_days)})
        stream.add(SESSION_CLOSED, "marketdata:session-closed:{}".format(session), end, s.fetched_at,
                   {"period_end": end, "instrument_ids": ids})
    return stream.out


def write_stream(path, envelopes):
    """Writes the envelopes one per line to a new file, never overwritten,
    in full or not at all (see above). Raises FileExistsError if ``path``
    exists, and record.NotDurable if the folder sync fails after the file is
    installed."""
    directory = os.path.dirname(os.path.abspath(path))
    fd, tmp = tempfile.mkstemp(dir=directory, suffix=".part")
    try:
        with os.fdopen(fd, "w") as f:
            for e in envelopes:
                f.write(json.dumps(e, separators=(",", ":"), allow_nan=False) + "\n")
            f.flush()
            os.fsync(f.fileno())
        os.link(tmp, path)   # fails if the stream exists
    finally:
        os.remove(tmp)
    try:
        record._sync_dir(directory)
    except OSError as e:
        raise record.NotDurable(path, e) from None
    return path
