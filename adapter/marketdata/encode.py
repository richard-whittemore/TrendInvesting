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
     source (sectors.py), sorted by instrument; a non-required row whose
     close or volume is missing or negative is left out (see encode);
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
except that a ticker written only as a universe member, or listed only in a
universe snapshot, is left out of the sessions its bad data reaches instead
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

# The momentum rule ranks a universe stock on its last 253 closes and leaves
# out a stock missing any of them, for that ranking only (ADR 0026 point 5;
# the 21/252/253-close lookbacks of its configuration, section 6).
WINDOW = 253

Identity = collections.namedtuple("Identity", "configuration_hash strategy_version run_id")


class Gap(MarketDataError):
    """A trading day in the span has no recording, or one with no bars."""


class Conflict(MarketDataError):
    """Two recordings disagree about the same fact."""


class Malformed(MarketDataError):
    """A recorded row cannot be encoded as a valid input."""


Session = collections.namedtuple("Session", "day bars fetched_at universe")

# A run of consecutive sessions of the stream a ticker is left out of:
# the first and last session, how many sessions, and why.
Exclusion = collections.namedtuple("Exclusion", "first last sessions reason")


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


def _universe_rows(directory, day, rec, max_age_days, named):
    """(rows, skipped): the snapshot rows, and {ticker: reason} for each row
    whose close or volume is missing or negative. Such a row is left out of
    this session's snapshot, unless the ticker is ``named`` (required or
    optional), which refuses the stream."""
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
    rows, skipped = {}, {}
    for b in rec["bars"]:
        sym = b.get("T")
        if sym not in types:
            continue
        if sym in rows or sym in skipped:
            raise Malformed("universe {} repeats {}".format(day, sym))
        c, v = b.get("c"), b.get("v")
        if not _finite(c, False) or not _finite(v, False):
            reason = "universe {} row {} has close {!r} and volume {!r}".format(day, sym, c, v)
            if sym in named:
                raise Malformed(reason)
            skipped[sym] = reason
            continue
        sector, source = labels.get(sectors.norm(sym), ("", ""))
        rows[sym] = {"instrument_id": sym, "type": types[sym], "close": float(c), "volume": float(v),
                     "sector": sector, "sector_source": source}
    return [rows[s] for s in sorted(rows)], skipped


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


def _event_day(row, key):
    try:
        return dt.date.fromisoformat(row.get(key))
    except (TypeError, ValueError):
        return None


def _member_factors(ticker, loaded, days, actions, base):
    """(factors, bad) for a ticker written only as a universe member: bad is
    {session: reason} for each session whose data is bad, a malformed bar
    or the first session on or after a split or dividend that cannot be
    adjusted exactly. A bad bar is not used and such an action is not
    applied, so the factors are still exact across any window holding no
    bad session: on either side of an action left unapplied the levels
    differ by one constant, and returns within a side are unchanged. An
    action with no readable date cannot be placed, so every session is
    bad."""
    bad, closes = {}, {}
    for s in loaded:
        if ticker in s.bars:
            try:
                _check_bar(ticker, s.day, s.bars[ticker])
            except Malformed as e:
                bad[s.day] = str(e)
                continue
            closes[s.day] = float(s.bars[ticker]["c"])
    splits, dividends = actions
    while True:
        try:
            return adjust.forward_factors(ticker, days, closes, splits, dividends, base), bad
        except adjust.CannotAdjust as e:
            if e.day is None:
                return {}, dict.fromkeys(days, str(e))
            bad.setdefault(next(d for d in days if d >= e.day), str(e))
            kept = ([r for r in splits if _event_day(r, "execution_date") != e.day],
                    [r for r in dividends if _event_day(r, "ex_dividend_date") != e.day])
            if e.day not in closes and kept == (splits, dividends):
                return {}, dict.fromkeys(days, str(e))   # nothing left to drop: refuse every session
            closes.pop(e.day, None)
            splits, dividends = kept


def _left_out(days, bad):
    """{session: reason} for every session whose WINDOW-close window, the
    WINDOW sessions ending at it, holds a bad session."""
    out = {}
    for i, day in enumerate(days):
        if day in bad:
            for later in days[i:i + WINDOW]:
                out.setdefault(later, bad[day])
    return out


def _exclusions(span_days, out):
    """[Exclusion] for the runs of consecutive span sessions in ``out``."""
    runs, previous = [], None
    for day in span_days:
        reason = out.get(day)
        if reason is not None and reason == previous:
            first, _, n, _ = runs[-1]
            runs[-1] = Exclusion(first, day, n + 1, reason)
        elif reason is not None:
            runs.append(Exclusion(day, day, 1, reason))
        previous = reason
    return runs


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

    A ticker written only because it is a universe member, with a bar that
    cannot be encoded (malformed) or a split or dividend that cannot be
    adjusted exactly (CannotAdjust, at the first session on or after it), is
    left out of every session whose WINDOW-close window holds that bad
    session, and only those: no bar is written for them and the session's
    ``market.session.closed`` does not name it, so the momentum rule leaves
    it out of those rankings (ADR 0026 point 5, "left out of that month's
    ranking"). Its bars before the bad session, and from the session whose
    window is clean again, are written, so later bad data never changes an
    earlier session's bars. A universe snapshot row whose close or volume is
    missing or negative is left out of that session's snapshot; the ticker's
    other rows are written. Each ticker left out of any session of the
    stream is named in ``excluded`` (a dict, if given) with an [Exclusion]
    per run of sessions. A required or optional instrument with bad data
    still refuses the stream."""
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

    factors, out = {}, {}   # out: ticker -> {session left out: reason}
    for ticker in sorted(wanted):
        if ticker in named:
            factors[ticker] = _factors(ticker, loaded, days, by_ticker[ticker], base)
            continue
        factors[ticker], bad = _member_factors(ticker, loaded, days, by_ticker[ticker], base)
        if bad:
            out[ticker] = _left_out(days, bad)
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
        ids = sorted(t for t in wanted if t in s.bars and s.day not in out.get(t, ()))
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
            rows, skipped = _universe_rows(directory, s.day, s.universe, max_sector_age_days, named)
            for ticker, reason in skipped.items():
                out.setdefault(ticker, {}).setdefault(s.day, reason)
            stream.add(UNIVERSE, "marketdata:universe:{}".format(session), end,
                       _fetched_at(s.universe, "universe " + session),
                       {"session": session, "source": s.universe.get("source"), "rows": rows})
        stream.add(SESSION_CLOSED, "marketdata:session-closed:{}".format(session), end, s.fetched_at,
                   {"period_end": end, "instrument_ids": ids})
    if excluded is not None:
        for ticker in sorted(out):
            runs = _exclusions([s.day for s in span], out[ticker])
            if runs:
                excluded[ticker] = runs
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
