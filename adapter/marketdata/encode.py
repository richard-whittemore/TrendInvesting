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
     recording: one row per reference ticker, of any type, with a bar
     that session, its unadjusted close and volume, and its sector label and
     source (sectors.py), sorted by instrument; a row of a ticker not named
     as required or optional whose close or volume is missing or negative,
     or of a member left out of the session for bad data, is left out (see
     encode), while a bad row of a required or optional instrument refuses
     the stream;
   - a ``market.session.closed`` v1 naming exactly the bars written.

A session the exchange cancelled is recorded once (record_retraction, the
CLI's ``retract``) and needs no bar recording. The calendar is still written
from the NYSE rules, as every run writes it, followed at once by one
``market.session.retracted`` v1 per recorded retraction it covers; the rest of
the stream treats the day as no session, and the calendar reaches far enough
past the last session to list ``ahead`` sessions that are not retracted. A
retracted day with bars recorded is a Conflict. (Boost streams carry no
calendar, so they read no retraction.)

With a ``ranker`` the stream is the compact universe input instead (the
owner's 2026-10-16 decision on the momentum input shape): bars only for the
required and optional instruments, the one universe snapshot per recorded
session as above, and, at a session the ranker calls a decision session, one
``market.close.history`` v1 per member it names, sorted by instrument, after
the snapshot and before the close. A history carries the member's split- and
dividend-adjusted closes, computed exactly as its bar's adjusted view would
be, on each session of the ranker's window (calendar sessions ending at the
decision session) that it traded and was not left out of. Which members, and
the window, are Go rules (targetweight.MomentumHistoryRequest, served by
cmd/universe-rank through go_ranker), so nothing here ranks; the engine ranks
the snapshot again and refuses a history set that differs. The recordings
are read one session at a time, three times over (_compact), so memory holds
one session's recordings and the closes of the ranked names' windows, never
every instrument of every session (docs/live-trading/lessons-2026-10.md L1;
iter_encode writes the envelopes as they are made). The calendar then
starts at ``base``, since histories reach back to it. A decision session with
no universe recording gets no histories, and the engine withholds that
decision (missing-universe).

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
(see encode). A bad row of an optional instrument refuses the stream, as a
required one's does.

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
import array
import bisect
import collections
import datetime as dt
import glob
import hashlib
import json
import math
import os
import subprocess
import tempfile

from . import adjust, history, nyse_calendar, record, sectors
# The event types, schema versions and source the stream carries: one table
# with the adapter that delivers it.
from .kinds import BAR, CALENDAR, HISTORY, RETRACTED, SESSION_CLOSED, SOURCE, UNIVERSE
from .massive import MarketDataError
from .steps import MissingData, _now

CALENDAR_SOURCE = "nyse-calendar"
RETRACTION_SOURCE = "marketdata-retraction"

ENGINES = ("momentum", "boost")

Identity = collections.namedtuple("Identity", "configuration_hash strategy_version run_id")


class Gap(MarketDataError):
    """A trading day in the span has no recording, or one with no bars."""


class Conflict(MarketDataError):
    """Two recordings disagree about the same fact."""


class Malformed(MarketDataError):
    """A recorded row cannot be encoded as a valid input."""


class RankerFailed(MarketDataError):
    """The ranker refused a request or answered something unusable."""


# How long one ranker call may take: a call ranks one session, normally in
# well under a second, so a ranker still running after this is stuck, and the
# stream is refused rather than the data job left waiting.
RANKER_TIMEOUT_SECONDS = 60


def go_ranker(argv, timeout=RANKER_TIMEOUT_SECONDS):
    """A ranker that asks cmd/universe-rank (``argv``, for example
    ["bin/universe-rank", "-config", "momentum.json"]): one JSON request per
    line on its stdin, one answer per line on its stdout, within ``timeout``
    seconds a call (the process it started is stopped after that). The
    ranking rule and the window are the Go engine's own
    (targetweight.MomentumHistoryRequest), so nothing here ranks."""
    def rank(requests):
        text = "".join(json.dumps(r, separators=(",", ":"), allow_nan=False) + "\n" for r in requests)
        try:
            done = subprocess.run(argv, input=text, capture_output=True, text=True, check=False, timeout=timeout)
        except subprocess.TimeoutExpired:
            raise RankerFailed("the ranker {} did not answer within {} s".format(argv, timeout)) from None
        except OSError as e:
            raise RankerFailed("the ranker {} did not run: {}".format(argv, e)) from None
        if done.returncode != 0:
            raise RankerFailed("the ranker refused: {}".format(done.stderr.strip()))
        try:
            return [json.loads(line) for line in done.stdout.splitlines()]
        except ValueError as e:
            raise RankerFailed("the ranker's answer is not JSON lines: {}".format(e)) from None
    return rank


def _check_answers(requests, answers):
    """The ranker's answers, checked against the requests: one per request, in
    order, naming the request's session, a window of at least one session,
    and distinct members, each a row of that session's snapshot."""
    if not isinstance(answers, list) or len(answers) != len(requests):
        raise RankerFailed("{} requests, but the ranker answered {!r}".format(len(requests), answers))
    for req, ans in zip(requests, answers):
        u = req["universe"]
        rows = {r["instrument_id"] for r in u["rows"]}
        if not isinstance(ans, dict):
            raise RankerFailed("the ranker's answer for {} is unusable: {!r}".format(u["session"], ans))
        members, window = ans.get("members"), ans.get("window")
        if (ans.get("session") != u["session"] or not isinstance(ans.get("decision_session"), bool)
                or not isinstance(window, int) or isinstance(window, bool) or window < 1
                or not isinstance(members, list) or len(set(members)) != len(members)
                or not set(members) <= rows or (members and not ans["decision_session"])):
            raise RankerFailed("the ranker's answer for {} is unusable: {!r}".format(u["session"], ans))
    return answers


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


def record_retraction(directory, day, reason, recorded_at=None, now=_now):
    """Records that the exchange cancelled the session ``day``, with why, as
    ``retraction_<day>.json``: never overwritten (record.write), so a
    retraction is recorded once. ``day`` must be a session by the NYSE rules.
    The encoder then writes it as a market.session.retracted and treats the
    day as no session."""
    if not nyse_calendar.is_trading_day(day):
        raise ValueError("{} is not a session by the NYSE rules, so there is nothing to retract".format(day))
    if not reason:
        raise ValueError("a retraction needs a reason")
    return record.write(directory, {"kind": "retraction", "source": RETRACTION_SOURCE, "session": day.isoformat(),
                                    "fetched_at": recorded_at or _utc(now()), "reason": reason})


def load_retractions(directory):
    """{day: recording} for every recorded retraction."""
    out = {}
    for path in sorted(glob.glob(os.path.join(directory, "retraction_*.json"))):
        rec = record.read(path)
        try:
            day = dt.date.fromisoformat(rec.get("session"))
        except (TypeError, ValueError):
            raise Malformed("{} names session {!r}".format(path, rec.get("session"))) from None
        if not nyse_calendar.is_trading_day(day) or not rec.get("reason"):
            raise Malformed("{} retracts {} with reason {!r}".format(path, day, rec.get("reason")))
        _fetched_at(rec, path)
        out[day] = rec
    return out


def load_sessions(directory, first, last, retracted=(), tickers=None):
    """[Session] for the trading days first..last, oldest first, leaving out
    each ``retracted`` day, which must have no bar recording.

    With ``tickers`` (a set), each session keeps only those tickers' bars and
    no universe recording. A day's recordings are read and checked in full
    (a ticker is not repeated; when both the grouped and universe recordings
    exist, they are held together for the agreement check, with canonical
    copies of their bars). Then only the named tickers' bars are kept, and
    the recordings are released before the next day is read. So memory holds
    at most one day's recordings (two when both exist) plus the kept bars,
    not every instrument of every session."""
    return list(iter_sessions(directory, first, last, retracted, tickers))


def list_recordings(directory):
    """{file name: (inode, size, modification time)} for every bar and sector
    recording in ``directory``: the listing a multi-pass reader takes once,
    so every pass reads the same files (iter_sessions)."""
    out = {}
    for pattern in ("grouped_*.json", "universe_*.json", "sectors-*_*.json"):
        for path in glob.glob(os.path.join(directory, pattern)):
            st = os.stat(path)
            out[os.path.basename(path)] = (st.st_ino, st.st_size, st.st_mtime_ns)
    return out


def _read_listed(directory, name, listing):
    """record.read of a file in ``listing``, refused as a Conflict when it
    has disappeared or been replaced since the listing was taken."""
    path = os.path.join(directory, name)
    try:
        st = os.stat(path)
    except FileNotFoundError:
        raise Conflict("{} disappeared during the encode".format(name)) from None
    if (st.st_ino, st.st_size, st.st_mtime_ns) != listing[name]:
        raise Conflict("{} changed during the encode".format(name))
    return record.read(path)


def iter_sessions(directory, first, last, retracted=(), tickers=None, listing=None):
    """The sessions of load_sessions, read, checked and yielded one at a
    time, so a caller that keeps only part of each holds one day's
    recordings at once (docs/live-trading/lessons-2026-10.md L1). With
    ``listing`` (list_recordings), only the files it names are read, and one
    that has disappeared or changed since is a Conflict, so passes over the
    same span read the same recordings."""
    def exists(name):
        return name in listing if listing is not None else os.path.exists(os.path.join(directory, name))

    for day in nyse_calendar.sessions(first, last):
        if day in retracted:
            for kind in ("grouped", "universe"):
                if exists("{}_{}.json".format(kind, day.isoformat())):
                    raise Conflict("{} is retracted, but its {} bars are recorded".format(day, kind))
            continue
        found = []
        for kind in ("grouped", "universe"):
            name = "{}_{}.json".format(kind, day.isoformat())
            if exists(name):
                path = os.path.join(directory, name)
                found.append((kind, path, record.read(path) if listing is None else _read_listed(directory, name, listing)))
        if not found:
            raise Gap("no recording for trading day {}".format(day))
        if len(found) == 2 and (sorted(map(_canonical, found[0][2]["bars"]))
                                != sorted(map(_canonical, found[1][2]["bars"]))):
            raise Conflict("the grouped and universe recordings of {} hold different bars".format(day))
        _, path, rec = found[0]
        if not rec.get("bars"):
            raise Gap("the recording for {} has no bars".format(day))
        bars, seen = {}, set()
        for row in rec["bars"]:
            ticker = row.get("T")
            if not isinstance(ticker, str) or not ticker or ticker in seen:
                raise Malformed("{} holds a missing or repeated ticker {!r}".format(path, ticker))
            seen.add(ticker)
            if tickers is None or ticker in tickers:
                bars[ticker] = row
        universe = None if tickers is not None else next((r for kind, _, r in found if kind == "universe"), None)
        session = Session(day, bars, _fetched_at(rec, path), universe)
        del found, rec, seen, bars, universe
        yield session
        del session


def _actions(directory, kind, first, last, tickers=None):
    """history.load_actions, with its refusals as this module's errors."""
    try:
        return history.load_actions(directory, kind, first, last, tickers)
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


def _universe_rows(directory, day, rec, max_age_days, named, listing=None):
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
    paths = None if listing is None else lambda kind: [os.path.join(directory, n) for n in listing
                                                       if n.startswith(kind + "_")]
    labels = sectors.labels(
        sectors.latest(directory, "sectors-iwv", day, max_age_days, paths=paths and paths("sectors-iwv")),
        sectors.latest(directory, "sectors-nasdaq", day, max_age_days, paths=paths and paths("sectors-nasdaq")))
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
    {session: (reason, sessions)}, the first session left out, why, and how
    many sessions from it are left out. A malformed bar is not used and
    leaves out that one session; the momentum rule's own 253-close window
    then keeps the ticker out of every ranking that reaches it (ADR 0026
    point 5). A split or dividend that cannot be adjusted exactly is not
    applied, so the price series jumps at its ex-date: the factors are still
    exact within each side (the levels differ by one constant, returns are
    unchanged), and the first session on or after the ex-date is left out.
    No complete 253-close window spans the jump, because the ex-date session
    is missing from every such window (ADR 0026 point 5; the Go tests
    TestScoreLeavesOutANameWithAGapInItsWindow and
    TestDecideRecordsEveryRankedName in internal/targetweight/momentum pin
    that a name with bars on both sides of one missing session, even with
    more than 253 closes, is a gap and is not ranked). When that session
    already holds a malformed bar it is still one session, with both reasons
    recorded. Only the action is dropped; that session's close is kept for
    the next action. An action with no readable date cannot be placed, so
    every session is bad."""
    bad, closes = {}, {}
    for s in loaded:
        if ticker in s.bars:
            try:
                _check_bar(ticker, s.day, s.bars[ticker])
            except Malformed as e:
                bad[s.day] = (str(e), 1)
                continue
            closes[s.day] = float(s.bars[ticker]["c"])
    return _member_factors_from(ticker, closes, bad, days, actions, base)


def _member_factors_from(ticker, closes, bad, days, actions, base):
    """_member_factors from the ticker's closes ({session: raw close} of its
    well-formed bars) and ``bad`` (its malformed sessions, as
    _member_factors records them; extended in place). ``closes`` may be any
    subset of the well-formed closes that holds the first one and the one
    before each of the ticker's dividend ex-dates: adjust.forward_factors
    reads no other close, so the sessions left out are the same, and the
    factors are the same on every session ``closes`` holds."""
    splits, dividends = actions
    while True:
        try:
            return adjust.forward_factors(ticker, days, closes, splits, dividends, base), bad
        except adjust.CannotAdjust as e:
            if e.day is None:
                return {}, {days[0]: (str(e), len(days))}
            kept = ([r for r in splits if _event_day(r, "execution_date") != e.day],
                    [r for r in dividends if _event_day(r, "ex_dividend_date") != e.day])
            if kept != (splits, dividends):
                day = next(d for d in days if d >= e.day)
                bad[day] = ("; ".join(filter(None, (bad.get(day, ("", 1))[0], str(e)))), 1)
                splits, dividends = kept
            elif e.day in closes:   # a refused close: leave out that session only
                bad.setdefault(e.day, (str(e), 1))
                closes.pop(e.day)
            else:
                return {}, {days[0]: (str(e), len(days))}   # nothing left to drop: refuse every session


def _left_out(days, bad):
    """{session: reason} for every session left out by ``bad`` (see
    _member_factors)."""
    out = {}
    for i, day in enumerate(days):
        if day in bad:
            reason, n = bad[day]
            for later in days[i:i + n]:
                out.setdefault(later, reason)
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


def _history(ticker, day, window, by_day, factors, left_out, base):
    """The market.close.history payload of ``ticker`` at decision session
    ``day``: its split- and dividend-adjusted close, computed exactly as its
    bar's adjusted view would be, on each window session it traded and was
    not left out of (see encode)."""
    closes = []
    for d in window:
        s = by_day.get(d)
        if s is not None and ticker in s.bars and d not in left_out:
            closes.append({"session": d.isoformat(), "close": float(s.bars[ticker]["c"]) * factors[d][1]})
    return {"instrument_id": ticker, "session": day.isoformat(), "adjusted_base": base.isoformat(), "closes": closes}


class _Stream:
    def __init__(self, identity, first_sequence):
        if not all(identity):
            raise ValueError("run identity is required")
        self.identity, self.sequence, self.out = identity, first_sequence, []

    def make(self, kind, id_, event_time, recorded_at, payload):
        """The next envelope, with the next sequence."""
        raw = json.dumps(payload, separators=(",", ":"), allow_nan=False)
        envelope = {
            "id": id_, "type": kind[0], "envelope_version": 1, "schema_version": kind[1],
            "event_time": event_time, "recorded_at": recorded_at, "sequence": self.sequence,
            "correlation_id": self.identity.run_id, "source": SOURCE,
            "strategy_version": self.identity.strategy_version,
            "configuration_hash": self.identity.configuration_hash,
            "payload_hash": hashlib.sha256(raw.encode()).hexdigest(), "payload": payload}
        self.sequence += 1
        return envelope

    def add(self, kind, id_, event_time, recorded_at, payload):
        self.out.append(self.make(kind, id_, event_time, recorded_at, payload))


def _check_request(first, last, required, optional, universe_members, base, ahead, engine, ranker):
    """(boost, compact, base) for an encode request, or ValueError."""
    if engine not in ENGINES:
        raise ValueError("engine {!r} is not one of {}".format(engine, ", ".join(ENGINES)))
    boost = engine == "boost"
    if boost and (len(required) != 1 or optional or universe_members):
        raise ValueError("a boost stream names exactly one required instrument and nothing else")
    compact = ranker is not None
    if compact and (boost or universe_members):
        raise ValueError("a compact stream is a momentum stream that writes ranked members' histories, not their bars")
    base = base or first
    if base > first or not nyse_calendar.is_trading_day(base):
        raise ValueError("base {} must be a session on or before the first {}".format(base, first))
    if first > last or ahead < 1:
        raise ValueError("need first <= last and ahead >= 1")
    return boost, compact, base


def encode(directory, first, last, identity, required=("SPY", "IEF", "SHY"), optional=(),
           universe_members=False, base=None, ahead=1, first_sequence=2, max_sector_age_days=7,
           engine="momentum", excluded=None, ranker=None):
    """The envelopes for first..last, as described above. ``required``
    instruments must have a bar every session; ``optional`` ones (and, with
    ``universe_members``, every reference ticker, of any type, with a bar
    in the span's universe recordings) are written where they traded. ``base`` (default ``first``)
    is the session the adjusted views are based at; keep it fixed for the
    life of an engine's journal. ``engine`` is "momentum" or "boost", as
    described above.

    A ticker written only because it is a universe member is left out of
    some sessions when its data is bad: no bar is written for them and the
    session's ``market.session.closed`` does not name it.

    - A bar that cannot be encoded (malformed) leaves out that session only.
      The momentum rule leaves a stock missing any session of its 253-close
      window out of that ranking (ADR 0026 point 5, "left out of that
      month's ranking"), so it keeps the ticker out of every ranking that
      reaches the bad session, and the ticker is ranked again when its
      window is complete under that rule.
    - A split or dividend that cannot be adjusted exactly (CannotAdjust) is
      not applied, so the price series is discontinuous at its ex-date. The
      ticker is left out of the first session on or after the ex-date only,
      like a malformed bar. No complete 253-close window spans the jump,
      because the ex-date session is missing from every such window, so the
      momentum rule ranks the ticker again once its window lies wholly after
      the jump (ADR 0026 point 5). The Go tests
      TestScoreLeavesOutANameWithAGapInItsWindow and
      TestDecideRecordsEveryRankedName (internal/targetweight/momentum) pin
      that one missing session inside the window leaves the name out of the
      ranking even when 253 or more of its bars have arrived. The engine
      scores a stock it holds across a gap (ADR 0026 point 5, amended
      2026-10-18) only in the compact stream and only from its own bars, so
      a holding must be named in ``optional``, where the same action refuses
      the stream instead. A stream with universe members' bars gives every
      member bars, named or not, so the engine scores no holding across a
      gap from it.

    Its bars before the bad session are written, so later bad data never
    changes an earlier session's bars. A universe snapshot row whose close
    or volume is missing or negative is left out of that session's snapshot
    (for a member, its bar that session is malformed too), and so is the row
    of a member left out of that session, whichever stream is written: the
    snapshot records that it was left out, so every stream ranks the same
    members and a momentum run configured for bars can require a bar for
    every member it ranks (ADR 0026 point 5). The ticker's other rows are
    written. Each ticker left out of any session of the
    stream is named in ``excluded`` (a dict, if given) with an [Exclusion]
    per run of sessions. A required or optional instrument with bad data
    still refuses the stream.

    ``ranker`` (go_ranker, or any callable with its contract) makes the
    stream compact, as described above: it is called once per universe
    session, oldest first, with a list of one request {"universe": snapshot
    payload, "next_session": YYYY-MM-DD}, and must answer it with a list of
    one {"session", "decision_session", "window", "members"}; an answer
    that does not fit its request (RankerFailed) refuses the stream. The
    compact stream is read one session at a time (_compact), so its memory
    is bounded by one session's recordings and the ranked names' windows of
    closes, not by every instrument of every session. A member with bad data is
    left out of the same sessions as above, in its history instead of its
    bars, and named in ``excluded`` the same way."""
    boost, compact, base = _check_request(first, last, required, optional, universe_members, base, ahead,
                                          engine, ranker)
    if compact:
        return list(_compact(directory, first, last, identity, required, optional, base, ahead, first_sequence,
                             max_sector_age_days, excluded, ranker))
    retractions = {} if boost else load_retractions(directory)
    # A boost stream reads its one instrument's bars only, so the other
    # instruments of each recording are not kept.
    loaded = load_sessions(directory, base, last, retractions, set(required) if boost else None)
    days = [s.day for s in loaded]
    by_day = {s.day: s for s in loaded}
    span = [s for s in loaded if s.day >= first]
    if not span:
        raise MissingData("no session in {}..{}".format(first, last))
    named = set(required) | set(optional)
    wanted = set(named)
    # Every listed ticker with a bar in a universe session, of any type: the
    # run ranks its configured listing type, which the stream does not know,
    # so each is checked before the snapshot filter below and no ranked
    # member's bad session can be found after it (ADR 0026 point 5).
    members = set()
    if not boost:
        for s in span:
            if s.universe is not None:
                listed = {t.get("ticker") for t in s.universe.get("tickers") or ()}
                members |= listed & set(s.bars)
    if universe_members:
        wanted |= members
    start = base + dt.timedelta(days=1)
    by_ticker = collections.defaultdict(lambda: ([], []))
    # A boost stream reads its one instrument's actions only, so only theirs
    # are checked for conflicting copies; a momentum stream may encode any
    # listed ticker, so every action is.
    scope = set(required) if boost else None
    for r in _actions(directory, "splits", start, last, scope):
        by_ticker[r.get("ticker")][0].append(r)
    for r in _actions(directory, "dividends", start, last, scope):
        by_ticker[r.get("ticker")][1].append(r)

    factors, out = {}, {}   # out: ticker -> {session left out: reason}
    for ticker in sorted(wanted | members):
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
    after = nyse_calendar.sessions_after(last, ahead)
    while len([d for d in after if d not in retractions]) < ahead:
        after = nyse_calendar.sessions_after(last, len(after) + 1)
    through = after[-1]
    opening = _utc(nyse_calendar.session_close(span[0].day).astimezone(dt.timezone.utc))
    # A compact stream's histories reach back to the base, so its calendar
    # does too: the engine refuses a close on a date its calendar does not list.
    # The calendar is written from the NYSE rules, as every run writes it,
    # and each recorded retraction it covers follows it; the rest of the
    # stream treats a retracted day as no session.
    rules = nyse_calendar.sessions(base if compact else span[0].day, through)
    calendar = [d for d in rules if d not in retractions]
    if not boost:
        stream.add(CALENDAR, "marketdata:calendar:{}:{}".format(rules[0], through), opening, opening,
                   {"source": CALENDAR_SOURCE, "sessions": [d.isoformat() for d in rules]})
        for day in sorted(d for d in retractions if rules[0] <= d <= through):
            rec = retractions[day]
            stream.add(RETRACTED, "marketdata:retracted:{}".format(day), opening, rec["fetched_at"],
                       {"source": rec["source"], "session": day.isoformat(), "reason": rec["reason"]})
    universes = {s.day: _universe_rows(directory, s.day, s.universe, max_sector_age_days, named)
                 for s in span if s.universe is not None and not boost}
    # A member left out of a session for bad data is left out of that
    # session's snapshot too, so the snapshot records it and a run configured
    # for bars can require a bar for every member it ranks (ADR 0026 point 5).
    universes = {day: ([r for r in rows if day not in out.get(r["instrument_id"], ())], skipped)
                 for day, (rows, skipped) in universes.items()}
    answers = {}
    if compact and universes:
        requests = [{"universe": {"session": day.isoformat(), "source": by_day[day].universe.get("source"),
                                  "rows": rows},
                     "next_session": calendar[calendar.index(day) + 1].isoformat()}
                    for day, (rows, _) in sorted(universes.items())]
        for req, ans in zip(requests, _check_answers(requests, ranker(requests))):
            answers[dt.date.fromisoformat(req["universe"]["session"])] = ans
        # Each member ranked is a row of its snapshot (_check_answers), so a
        # listed ticker with a bar that session, checked above.
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
        if s.day in universes:
            rows, skipped = universes[s.day]
            for ticker, reason in skipped.items():
                out.setdefault(ticker, {}).setdefault(s.day, reason)
            fetched = _fetched_at(s.universe, "universe " + session)
            stream.add(UNIVERSE, "marketdata:universe:{}".format(session), end, fetched,
                       {"session": session, "source": s.universe.get("source"), "rows": rows})
            answer = answers.get(s.day)
            if answer is not None and answer["decision_session"]:
                window = [d for d in calendar if d <= s.day][-answer["window"]:]
                for ticker in sorted(answer["members"]):
                    stream.add(HISTORY, "marketdata:history:{}:{}".format(ticker, session), end, fetched,
                               _history(ticker, s.day, window, by_day, factors[ticker], out.get(ticker, {}), base))
        stream.add(SESSION_CLOSED, "marketdata:session-closed:{}".format(session), end, s.fetched_at,
                   {"period_end": end, "instrument_ids": ids})
    if excluded is not None:
        for ticker in sorted(out):
            runs = _exclusions([s.day for s in span], out[ticker])
            if runs:
                excluded[ticker] = runs
    return stream.out


# How many malformed bars of one ticker keep their own reason in a compact
# stream; the rest share one reason, so a feed broken for many stocks every
# session costs a few runs per stock, not an entry per stock per session
# (docs/live-trading/lessons-2026-10.md L1).
REASONS_KEPT = 3


class _Runs:
    """The sessions one ticker is left out of, with why, as runs of
    consecutive sessions (indexes into the loaded sessions) sharing a
    reason, plus single sessions set apart. Reads like the
    {session: reason} map encode keeps (``in``, get, setdefault), at a size
    set by the number of runs, not of sessions."""

    __slots__ = ("index", "firsts", "runs", "extra", "count")

    def __init__(self, index):
        self.index, self.firsts, self.runs, self.extra, self.count = index, [], [], {}, 0

    def add(self, i, reason):
        """Adds session ``i``, after every session added so far."""
        self.count += 1
        if self.runs and self.runs[-1][1] == i - 1 and self.runs[-1][2] == reason:
            self.runs[-1][1] = i
        else:
            self.firsts.append(i)
            self.runs.append([i, i, reason])

    def put(self, day, reason):
        self.extra[self.index[day]] = reason

    def get(self, day, default=None):
        i = self.index.get(day)
        if i is None:
            return default
        if i in self.extra:
            return self.extra[i]
        k = bisect.bisect_right(self.firsts, i) - 1
        if k >= 0 and self.runs[k][1] >= i:
            return self.runs[k][2]
        return default

    def __contains__(self, day):
        return self.get(day) is not None

    def setdefault(self, day, reason):
        found = self.get(day)
        if found is None:
            self.put(day, reason)
            return reason
        return found


def _compact(directory, first, last, identity, required, optional, base, ahead, first_sequence,
             max_sector_age_days, excluded, ranker):
    """The compact stream of encode, read one session at a time in three
    passes over the recordings, so memory holds one session's recordings,
    the closes of the names ranked at a decision whose window covers the
    session (8 bytes a close), and per-ticker figures of a few sessions or
    runs each, never every instrument of every session
    (docs/live-trading/lessons-2026-10.md L1; the budget is stated in
    docs/manual/market-data.md):

    1. every session base..last: the named instruments' bars, the universe
       members, and per listed ticker its malformed sessions (as runs, the
       first REASONS_KEPT with their own reason), its first well-formed
       close and its close before each of its dividend ex-dates, which is
       all _member_factors_from reads to find the sessions a member is left
       out of;
    2. every universe session of the span: its snapshot, ranked by the
       ranker one session at a time;
    3. from the earliest window start: the closes of the names ranked at a
       decision whose window covers the session, and, from ``first``, the
       envelopes, made one at a time (the returned generator).

    The recordings are listed once, before pass 1 (list_recordings), and
    every pass reads only those, so a recording written meanwhile is not
    read and one removed or replaced meanwhile refuses the stream. Passes 1
    and 2 run before this returns and raise what encode raises, in the
    order it raises them, so the generator only writes. The envelopes are
    byte for byte encode's own before it read this way (the recorded
    fixture in the compact stream's tests pins that)."""
    listing = list_recordings(directory)
    retractions = load_retractions(directory)
    named = set(required) | set(optional)
    start = base + dt.timedelta(days=1)
    by_ticker = collections.defaultdict(lambda: ([], []))
    # The actions are read first, to know whose closes pass 1 keeps, but
    # encode refuses the recordings before the actions, so whatever reading
    # them raised is raised once pass 1 has checked the recordings.
    actions_failed = None
    try:
        for r in _actions(directory, "splits", start, last, None):
            by_ticker[r.get("ticker")][0].append(r)
        for r in _actions(directory, "dividends", start, last, None):
            by_ticker[r.get("ticker")][1].append(r)
    except Exception as e:
        actions_failed = e
    # The session before each dividend ex-date, whose close the dividend's
    # factor reads (adjust.forward_factors).
    expected = [d for d in nyse_calendar.sessions(base, last) if d not in retractions]
    priors = collections.defaultdict(set)   # session -> tickers whose close on it a factor reads
    if actions_failed is None:
        for ticker, (_, dividends) in by_ticker.items():
            for r in dividends:
                d = _event_day(r, "ex_dividend_date")
                i = bisect.bisect_left(expected, d) if d is not None else 0
                if i > 0:
                    priors[expected[i - 1]].add(ticker)

    # Pass 1.
    days, index, kept, members = [], {}, [], set()
    malformed = {}                             # ticker -> _Runs of its malformed sessions
    closes = collections.defaultdict(dict)     # ticker -> {session: raw close}: first, and before ex-dates
    for s in iter_sessions(directory, base, last, retractions, listing=listing):
        i = index[s.day] = len(days)
        days.append(s.day)
        kept.append(Session(s.day, {t: s.bars[t] for t in sorted(named) if t in s.bars}, s.fetched_at, None))
        if s.day >= first and s.universe is not None:
            listed = {t.get("ticker") for t in s.universe.get("tickers") or ()}
            members |= listed & set(s.bars)
        wanted_close = priors.get(s.day, ())
        for ticker, row in s.bars.items():
            if ticker in named:
                continue
            try:
                _check_bar(ticker, s.day, row)
            except Malformed as e:
                runs = malformed.get(ticker)
                if runs is None:
                    runs = malformed[ticker] = _Runs(index)
                runs.add(i, str(e) if runs.count < REASONS_KEPT else
                         "{} has more malformed bars; only its first {} reasons are kept".format(ticker, REASONS_KEPT))
                continue
            if ticker not in closes or ticker in wanted_close:
                closes[ticker].setdefault(s.day, float(row["c"]))
        del s
    if not [d for d in days if d >= first]:
        raise MissingData("no session in {}..{}".format(first, last))
    if actions_failed is not None:
        raise actions_failed

    factors, out = {}, {}   # out: ticker -> the sessions it is left out of, with why
    for ticker in sorted(named | members):
        if ticker in named:
            factors[ticker] = _factors(ticker, kept, days, by_ticker[ticker], base)
            continue
        runs = malformed.get(ticker)
        # A malformed session that an action also leaves out carries both
        # reasons, as _member_factors records them.
        seeds = {}
        splits, dividends = by_ticker[ticker]
        for r, key in [(r, "execution_date") for r in splits] + [(r, "ex_dividend_date") for r in dividends]:
            d = _event_day(r, key)
            j = bisect.bisect_left(days, d) if d is not None else len(days)
            if j < len(days) and runs is not None and days[j] in runs:
                seeds[days[j]] = (runs.get(days[j]), 1)
        _, bad = _member_factors_from(ticker, dict(closes.get(ticker, {})), seeds, days, by_ticker[ticker], base)
        if any(n > 1 for _, n in bad.values()):   # nothing could be adjusted: every session is left out
            left = _Runs(index)
            reason = next(iter(bad.values()))[0]
            for i in range(len(days)):
                left.add(i, reason)
        else:
            left = runs if runs is not None else _Runs(index)
            for day, (reason, _) in bad.items():
                left.put(day, reason)
        if left.runs or left.extra:
            out[ticker] = left
    span = [s for s in kept if s.day >= first]
    for ticker in required:
        missing = [s.day.isoformat() for s in span if ticker not in s.bars]
        if missing:
            raise MissingData("{} has no bar on {}".format(ticker, ", ".join(missing)))
    for ticker in list(closes):   # only a member's figures are read again
        if ticker not in members:
            del closes[ticker]
    for ticker in list(malformed):
        if ticker not in members:
            del malformed[ticker]

    stream = _Stream(identity, first_sequence)
    after = nyse_calendar.sessions_after(last, ahead)
    while len([d for d in after if d not in retractions]) < ahead:
        after = nyse_calendar.sessions_after(last, len(after) + 1)
    through = after[-1]
    opening = _utc(nyse_calendar.session_close(span[0].day).astimezone(dt.timezone.utc))
    rules = nyse_calendar.sessions(base, through)
    calendar = [d for d in rules if d not in retractions]

    # Pass 2: each universe session's snapshot, ranked one session at a time.
    # encode ranks once every snapshot is built, so a ranker refusal waits
    # until every snapshot has been checked.
    answers, skipped_by_day, fetched_by_day, ranker_failed = {}, {}, {}, None
    for s in iter_sessions(directory, first, last, retractions, listing=listing):
        if s.universe is None:
            continue
        rows, skipped = _universe_rows(directory, s.day, s.universe, max_sector_age_days, named, listing)
        rows = [r for r in rows if s.day not in out.get(r["instrument_id"], ())]
        # A skipped row's bar is malformed too, so it is normally left out
        # already; only the rest are kept, to be added to the left-out sessions.
        skipped = {t: r for t, r in skipped.items() if s.day not in out.get(t, ())}
        if skipped:
            skipped_by_day[s.day] = skipped
        try:
            fetched_by_day[s.day] = _fetched_at(s.universe, "universe " + s.day.isoformat())
        except Malformed as e:
            fetched_by_day[s.day] = e
        if ranker_failed is None:
            requests = [{"universe": {"session": s.day.isoformat(), "source": s.universe.get("source"), "rows": rows},
                         "next_session": calendar[calendar.index(s.day) + 1].isoformat()}]
            try:
                answers[s.day] = _check_answers(requests, ranker(requests))[0]
            except RankerFailed as e:
                # One session per call, so the ranker's own message cannot
                # say which session it refused.
                ranker_failed = RankerFailed("universe {}: {}".format(s.day.isoformat(), e))
        del s, rows
    if ranker_failed is not None:
        raise ranker_failed
    for s in span:   # what encode refuses as it writes, refused before anything is written
        if not any(t in s.bars for t in named):
            raise MissingData("no requested instrument has a bar on {}".format(s.day.isoformat()))
        if isinstance(fetched_by_day.get(s.day), Malformed):
            raise fetched_by_day[s.day]
    for day, skipped in sorted(skipped_by_day.items()):
        for ticker, reason in skipped.items():
            out.setdefault(ticker, {}).setdefault(day, reason)
    if excluded is not None:
        for ticker in sorted(out):
            runs = _exclusions([s.day for s in span], out[ticker])
            if runs:
                excluded[ticker] = runs

    # The closes pass 3 keeps: per ranked name, the window of each decision
    # that ranks it, (first session, decision session) as indexes, oldest
    # first.
    needs = collections.defaultdict(list)
    for day in sorted(answers):
        answer = answers[day]
        if answer["decision_session"]:
            window = [d for d in calendar if d <= day][-answer["window"]:]
            for ticker in answer["members"]:
                needs[ticker].append((index[window[0]], index[day]))
        else:
            del answers[day]
    reach = min([first] + [days[spans[0][0]] for spans in needs.values()])
    del kept, span

    def emit():
        yield stream.make(CALENDAR, "marketdata:calendar:{}:{}".format(rules[0], through), opening, opening,
                          {"source": CALENDAR_SOURCE, "sessions": [d.isoformat() for d in rules]})
        for day in sorted(d for d in retractions if rules[0] <= d <= through):
            rec = retractions[day]
            yield stream.make(RETRACTED, "marketdata:retracted:{}".format(day), opening, rec["fetched_at"],
                              {"source": rec["source"], "session": day.isoformat(), "reason": rec["reason"]})
        # Ranked name -> [index of its first held session, its raw closes from
        # there, NaN where none is used]: 8 bytes a session, from the start of
        # the earliest window still to be written.
        held = {}
        for s in iter_sessions(directory, reach, last, retractions, listing=listing):
            i = index[s.day]
            for ticker, spans in needs.items():
                if min(a for a, _ in spans) > i:
                    continue
                entry = held.get(ticker)
                if entry is None:
                    entry = held[ticker] = [i, array.array("d")]
                row = s.bars.get(ticker)
                use = (row is not None and any(a <= i <= b for a, b in spans)
                       and s.day not in malformed.get(ticker, ()))
                entry[1].append(float(row["c"]) if use else math.nan)
            if s.day < first:
                continue
            end, session = _utc(nyse_calendar.session_close(s.day).astimezone(dt.timezone.utc)), s.day.isoformat()
            for ticker in sorted(t for t in named if t in s.bars):
                r = s.bars[ticker]
                split_m, total_m = factors[ticker][s.day]
                yield stream.make(BAR, "marketdata:bar:{}:{}".format(ticker, session), end, s.fetched_at, {
                    "instrument_id": ticker, "period_end": end,
                    "split_adjusted": _view("split-adjusted", r, split_m, split_m),
                    "raw": _view("raw", r, 1.0, 1.0),
                    "adjusted": _view("split-and-dividend-adjusted", r, total_m, split_m),
                    "adjusted_base": base.isoformat()})
            ids = sorted(t for t in named if t in s.bars)
            if s.universe is not None:
                rows, _ = _universe_rows(directory, s.day, s.universe, max_sector_age_days, named, listing)
                rows = [r for r in rows if s.day not in out.get(r["instrument_id"], ())]
                fetched = fetched_by_day[s.day]
                yield stream.make(UNIVERSE, "marketdata:universe:{}".format(session), end, fetched,
                                  {"session": session, "source": s.universe.get("source"), "rows": rows})
                del rows
                answer = answers.get(s.day)
                if answer is not None:
                    window = [d for d in calendar if d <= s.day][-answer["window"]:]
                    for ticker in sorted(answer["members"]):
                        first_held, values = held.get(ticker, (i + 1, ()))
                        known = {days[first_held + k]: v for k, v in enumerate(values) if not math.isnan(v)}
                        yield stream.make(HISTORY, "marketdata:history:{}:{}".format(ticker, session), end, fetched,
                                          _held_history(ticker, s.day, window, known, named, factors,
                                                        closes, days, by_ticker, out, base))
                        del known
                        spans = needs[ticker]
                        spans.pop(0)
                        if not spans:
                            del needs[ticker]
                            held.pop(ticker, None)
                            continue
                        keep_from = min(a for a, _ in spans)
                        entry = held.get(ticker)
                        if entry is not None and keep_from > i:   # its next window has not started
                            del held[ticker]
                        elif entry is not None and keep_from > entry[0]:
                            del entry[1][:keep_from - entry[0]]
                            entry[0] = keep_from
            yield stream.make(SESSION_CLOSED, "marketdata:session-closed:{}".format(session), end, s.fetched_at,
                              {"period_end": end, "instrument_ids": ids})
            del s

    return emit()


def _held_history(ticker, day, window, held, named, factors, closes, days, by_ticker, out, base):
    """_history from the closes pass 3 held for ``ticker`` (see _compact): a
    named instrument's factors are its own, a member's come from
    _member_factors_from over its pass-1 closes and the held ones (the
    factors do not depend on the sessions left out, so none are passed)."""
    if ticker in named:
        f = factors[ticker]
    else:
        known = dict(closes.get(ticker, {}))
        known.update(held)
        f, _ = _member_factors_from(ticker, known, {}, days, by_ticker[ticker], base)
    left_out = out.get(ticker, {})
    return {"instrument_id": ticker, "session": day.isoformat(), "adjusted_base": base.isoformat(),
            "closes": [{"session": d.isoformat(), "close": held[d] * f[d][1]}
                       for d in window if d in held and d not in left_out]}


def iter_encode(directory, first, last, identity, required=("SPY", "IEF", "SHY"), optional=(),
                universe_members=False, base=None, ahead=1, first_sequence=2, max_sector_age_days=7,
                engine="momentum", excluded=None, ranker=None):
    """The envelopes of encode (same arguments), as an iterator for
    write_stream. A compact stream (with ``ranker``) is checked and ranked
    in full before this returns, so it fails as encode does and
    ``excluded`` is filled; its envelopes are then made one at a time as
    they are written, so the stream is never held whole (docs/live-trading/
    lessons-2026-10.md L1). Any other stream is encode's list."""
    if ranker is None:
        return iter(encode(directory, first, last, identity, required, optional, universe_members, base, ahead,
                           first_sequence, max_sector_age_days, engine, excluded))
    _, _, base = _check_request(first, last, required, optional, universe_members, base, ahead, engine, ranker)
    return _compact(directory, first, last, identity, required, optional, base, ahead, first_sequence,
                    max_sector_age_days, excluded, ranker)


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
