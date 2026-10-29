"""ti-data: the daily data job behind ti-data.service (ADR 0026 section 8:
one shared data step records each day's market data once, and each engine
reads the recordings as its input stream).

For the Boost engine, each run:

1. picks the latest settled session: the latest NYSE trading day whose close
   (4 p.m. New York, 1 p.m. on an early close) plus ``--settle-minutes``
   has passed, so a bar still moving is never recorded as a close
   (steps.require_settled; plan.md section 4). ``--session`` names one
   instead, and is refused until it has settled;
2. reads the run's identity from the engine's durable log (ADR 0027): the
   header's configuration hash and strategy version, which every input
   carries (ADR 0016), and the run's name, the log's file name without
   ``.log``, as the stream's correlation id. The log's first committed input
   must be the run's Boost configuration, and the ``--config`` file must
   agree with it on every field the stream depends on, so a stream is never
   stamped with a hash its history was not built for. The run's streams
   live in ``STREAMS/boost/<run>/``, found by the run's identity
   (stream_run): a crash resume runs under a new log name with the crashed
   run's identity, and keeps writing into the folder of the run that began
   it, under that run's name, so its earlier files still re-encode byte for
   byte; a fresh run after an earlier one has its own identity, so its own
   folder and its own warm-up;
3. records Massive's grouped daily bars (the encoder's bar source, one
   request a session) for every session after the run's last stream, and the
   bulk splits and dividends for a window reaching back over the last
   ``ACTION_OVERLAP`` days (history.record_actions), so an action published
   a few days late is still recorded and the windows stay contiguous;
4. re-verifies every stream already written for the run: each must still
   re-encode byte for byte from the recordings under the current identity.
   One that does not (an action recorded after its file was delivered, a
   changed engine log header, a file edited or removed) stops the job for a
   person: the engine has already read that file, so a revision after
   delivery is the owner's decision, never the job's;
5. encodes the Boost stream (encode.encode with engine "boost": its one
   instrument's bars, then each session's close, so the session closes
   before its decision, ADR 0021) for the sessions after the previous stream
   through the session, adjusted forward from the configuration's
   ``warm_up_start`` (the first session whose close feeds the indicators),
   to ``boost-<last session>.jsonl``.

The file contract: one envelope per line, in delivery order. Each file's
``sequence`` fields number its own envelopes 1, 2, ... (a file-local
ordinal), never the engine's sequence: that is global across every source of
the run's inputs (the configuration, the Run approval, fills, snapshots), so
only the deliverer, which sends one input at a time and reads the engine's
reply, can know it. The deliverer reads the files in session order and sets
each envelope's ``sequence`` to the engine's next input sequence as it sends
it; everything else in the envelope is sent as written. No sequence is ever
written into a file that another source could already have taken.

A run's first stream is the warm-up (``--backfill-from`` the configured
``warm_up_start``): it records every session from the warm-up start, checks
the history has no gap and is at least as long as the longest indicator
window (trend_days, exit_sma, rsi_period + 1), and writes the whole history
as one stream. A daily run before it is refused.

Fail closed (AGENTS.md rule 5): a missing key, an unsettled session, an
unreadable engine log, a configuration the log does not hold, a missing
recording, an empty answer on a trading day, a gap the encoder finds, a
history too short, or an earlier stream that no longer re-encodes is refused
with exit status 1 and a message, and no stream is written
(encode.write_stream installs the file whole or not at all). Recordings
already made are kept: they are immutable inputs (record.py) and the next
run reuses them. The key and an unsettled session are checked before
anything is written. The key is never printed: main replaces it in every
message it prints.

Idempotent: when a stream already holds the session, the run fetches
nothing, re-verifies every stream as in step 4, and returns that stream.

With ``--engine momentum --instance <instance>`` (ti-data@<instance>.service,
an IRA's momentum engine) the run writes that engine's compact stream
instead (run_momentum): the same session, run identity and stream folder
rules, in ``STREAMS/<instance>/<run>/<instance>-<last session>.jsonl``, and
the same file contract, with these differences:

- the configuration is the momentum one the engine logged (strategy_id
  "momentum", ``universe_input`` "compact"); its required instruments are
  the trend and risk-off instruments and the bond funds;
- the warm-up (``--backfill-from DATE``) names the adjusted views' base,
  which no momentum configuration holds: the first stream starts at it, and
  every later file is based at the first file's calendar's first session,
  so the base is fixed for the life of the run (encode.py);
- first, before any request to Massive, the latest settled session's
  sector labels (sectors.record_sectors: the sources serve only that
  session's files, so a morning whose Massive requests fail still keeps the
  labels a later catch-up needs; a source that fails is named);
- per session after the last stream the grouped bars are recorded once
  (history.backfill, the request Boost's job makes; a session either job
  has recorded is not fetched again, and a retracted session never is);
  then, for every such session with the labels of both sources within the
  week up to it, its universe: the common-stock listing dated that session
  with that same recording's bars (steps.run_universe, so the two recordings
  agree). A session without labels gets no universe, so the engine withholds
  a decision on it (missing-universe) rather than every later stream being
  refused for a universe that could never be labelled. So a month-start
  morning whose firings all failed is caught up the next morning with the
  month-end universe and histories;
- Boost's job and the IRAs' may run at once (a Persistent catch-up after a
  boot, a start by hand, a long catch-up still running at the next timer):
  a recording another job writes between this job's check and its write is
  taken as recorded (record.AlreadyRecorded), as a recording is written once
  (record.py);
- the holdings that get their own daily bars (encode's ``optional``) are
  read from the engine's own durable log (_Holdings), a line at a time: the
  positions of its last account snapshot, else of its Run approval's
  starting book, plus every instrument filled since; a log with none holds
  nothing, and a snapshot, fill or approval of a version kinds.py does not
  name, or a log whose record hash chain breaks, is refused;
- the ranker reads a private copy of the configuration bytes checked
  against the engine's log, never the file's path;
- the stream is encode's compact stream (``ranker``: cmd/universe-rank
  with the instance's configuration, go_ranker), read one session at a time
  within the stated memory budget (docs/live-trading/lessons-2026-10.md L1;
  docs/manual/market-data.md);
- every firing holds the instance's lock (_instance_lock) and checks the
  run's folder against its manifest (_published: each published file's
  sessions, length and digest, written whole beside them), so a file
  truncated, changed or gone, the newest included, stops the job and is
  never written again; before a new file is written, every published file a
  split or dividend recording that appeared, changed or went since the
  manifest's check could reach (however old: another account's backfill may
  record a window long past) is re-encoded alone, each with the holdings its
  own bars name, and must match byte for byte (_reencode_changed; more than
  MAX_REENCODED is a run by hand, --reencode-all);
- retractions (one rule, on every path): a published file covers sessions
  first..last and is encoded under exactly the retractions recorded for
  sessions on or before last (encode's ``retracted_through=last``), when it
  is published and every time it is checked. A retraction of a later
  session travels with the next file (its calendar and retraction). One of
  a session on or before last recorded after the file was published
  changes a stream the engine has already read, so the file no longer
  encodes the same and the job stops for a person. The firing's session is
  the last real close on or before the latest settled session (a file is
  never named after a cancelled day, and a cancelled day with bars recorded
  refuses); when that session's file exists the firing returns it
  unchanged, after the same checks a new session's firing makes (the
  folder against its manifest, the file encoded again, the action
  recordings read before it). So a closure announced in advance, or one
  recorded before a delayed file is written, gives that file the calendar
  of the rules through the cancelled day, and the next file brings the
  retraction. If the cancelled day ended its month, the session before it
  becomes the decision session after its file was written without that
  month's histories, so the month is withheld (histories-missing);
- the latest session is not published without its universe before the last
  morning firing (its labels can still be fetched); once published without
  it, a file is never rewritten;
- a fresh run approved without a starting book holds the previous run's
  book, read through the previous log its approval names.
"""
import argparse
import contextlib
import datetime as dt
import fcntl
import glob
import hashlib
import json
import os
import re
import stat
import sys
import tempfile
from collections import namedtuple

from . import encode, engine_log, history, kinds, massive, nyse_calendar, record, sectors, steps

ENGINE = "boost"
MOMENTUM = "momentum"
STREAM = re.compile(r"boost-(\d{4}-\d{2}-\d{2})\.jsonl")
# The run's manifest of the files it published, beside them, and the
# instance's lock (run_momentum).
MANIFEST = "manifest.json"
MANIFEST_VERSION = 1
LOCK = ".ti-data.lock"
# How many runs back a fresh run's inherited book is followed (_Holdings).
MAX_PREVIOUS_LOGS = 64
# The most published files one firing encodes again after split and dividend
# recordings change: a daily firing's own two-week window reaches about ten,
# and each takes a whole-history encoding, so more is a run by hand
# (--reencode-all) rather than a morning past the 09:00 cutoff.
MAX_REENCODED = 15
# An instance's name: its stream folder and file prefix, its engine folder
# and its configuration's name (ti-data@<instance>.service).
INSTANCE = re.compile(r"[a-z][a-z0-9-]*")
# The inputs the holdings are read from (internal/event; their versions in
# kinds.py): the adapter's account snapshot, the owner's Run approval and its
# starting book, and the engine's fills.
SNAPSHOT_TYPE = kinds.ACCOUNT_SNAPSHOT[0]
FILL_TYPE = kinds.TARGET_WEIGHT_FILL[0]
# How far back each run's action window reaches: an action Massive publishes
# up to this late is still recorded before a later session needs it.
ACTION_OVERLAP = dt.timedelta(days=14)
KEY_NAME = "MASSIVE_API_KEY"
CONFIGURATION_TYPE = "strategy.target-weight.configuration"
# The owner's Run approval, which a fresh run after an earlier one records
# (cmd/engine; ADR 0027, the restart model).
RUN_APPROVED = engine_log.RUN_APPROVED
# The engine's durable log (internal/journal durable_log.go, ADR 0027), read
# by the one reader the data job shares with adapter/ibkr (engine_log.py).
LOG_VERSION = engine_log.LOG_VERSION
LOG_CHAIN_ALGORITHM = engine_log.LOG_CHAIN_ALGORITHM
LOG_HEADER_FIELDS = engine_log.LOG_HEADER_FIELDS
# Stands in for the key in every message main prints.
KEY_MASK = "[" + KEY_NAME + "]"

try:   # written by deploy/build-data.sh into the packed file
    from ._build import STAMP
except ImportError:
    STAMP = "dev"

EngineLog = namedtuple("EngineLog",
                       "configuration_hash strategy_version run_id boost identity inputs record_hashes chain_break market "
                       "holdings", defaults=((),))
"""The engine's durable log as the job reads it: the header's identity, the
log's own run name (its file name without ``.log``), the Boost
configuration, the run's identity (see read_engine_log), the IDs of its
committed inputs in order, every committed record's hash in order, its
hash chain (a crash resume's begin with the crashed run's), and the
sequence of the first committed record whose stored hash is not the one
LOG_CHAIN_ALGORITHM gives over the records before it (engine_log.chain_next), or None
when every one's is, and the count of committed delivered inputs (the data
job's kinds and the adapter's broker snapshot, kinds.DELIVERED_KINDS), the history a fresh run's log cannot hold,
and, for a momentum log, the instruments it holds (holdings). For a momentum log ``boost`` is its Momentum
configuration."""


class Boost(namedtuple("Boost", "instrument warm_up_start trend_days exit_sma rsi_period")):
    """The Boost configuration's fields the stream depends on (internal/event
    BoostConfiguration)."""

    @property
    def min_sessions(self):
        """The history the indicators need: the longest of the trend average,
        the exit average and the RSI's period plus one."""
        return max(self.trend_days, self.exit_sma, self.rsi_period + 1)


class Momentum(namedtuple("Momentum", "required trend_closes settings")):
    """The momentum configuration's fields the stream depends on (internal/event
    TargetWeightConfigurationPayload): the instruments every session must have a
    bar for (the trend and risk-off instruments and the bond funds), the trend's
    closes, which a warm-up must hold, and the momentum and glide sections whole
    (canonical JSON), which the ranker reads."""


class Refused(Exception):
    """The job refused to run; nothing was written for this session."""


class NotAvailableYet(Exception):
    """Massive's plan does not serve the latest session's bars yet; nothing was
    written, and the next firing asks again."""


# The free plan serves a session's grouped bars the next morning, so a 403 for
# the latest settled session is a wait from the session's own day until this
# hour on the next trading day (the morning firings of ti-data.timer before
# it); at or after it, the last firing's 403 is a failure that pages (ADR
# 0019, "Alerting"). Only the latest settled session is exempt: an explicitly
# requested older one, or an older one of a catch-up, is a refusal.
# TimerAgreementTest checks the timer and this hour agree.
QUIET_BEFORE = dt.time(8, 0)


def quiet_until(session):
    """The New York time before which a 403 for ``session`` is a wait."""
    following = nyse_calendar.sessions_after(session, 1)[0]
    return dt.datetime.combine(following, QUIET_BEFORE, tzinfo=nyse_calendar.NEW_YORK)


class _LatestSession:
    """Wraps the Massive client: a 403 for the grouped bars of ``session`` alone
    is NotAvailableYet; every other error passes through unchanged."""

    def __init__(self, client, session):
        self._client, self._session = client, session

    def grouped(self, day):
        try:
            return self._client.grouped(day)
        except massive.MarketDataError as e:
            if e.status == 403 and day == self._session:
                raise NotAvailableYet(str(e)) from None
            raise

    def __getattr__(self, name):
        return getattr(self._client, name)


def settled_session(now, settle):
    """The latest NYSE trading day whose close plus ``settle`` is at or before ``now``."""
    day = now.astimezone(nyse_calendar.NEW_YORK).date()
    while not nyse_calendar.is_trading_day(day) or nyse_calendar.session_close(day) + settle > now:
        day -= dt.timedelta(days=1)
    return day


def read_key(path):
    """MASSIVE_API_KEY from the secrets file (KEY=VALUE lines, as
    deploy/etc/secrets.env.example). The value is never put in a message."""
    if not path:
        raise Refused("no secrets file: {} is read from the file systemd passes (LoadCredential)".format(KEY_NAME))
    try:
        with open(path) as f:
            lines = f.read().splitlines()
    except OSError as e:
        raise Refused("cannot read {} from the secrets file {}: {}".format(KEY_NAME, path, e.strerror)) from None
    for line in lines:
        name, sep, value = line.partition("=")
        if sep and name.strip() == KEY_NAME and value.strip():
            return value.strip()
    raise Refused("{} is not set in the secrets file {}".format(KEY_NAME, path))


def _log_error(path, n, why):
    return Refused("the engine's durable log {} cannot be read at line {}: {}".format(path, n, why))


def _refused(err):
    """Refused for the shared reader's engine_log.LogUnreadable."""
    if err.line is None:
        return Refused("cannot read the engine's durable log {}: {}".format(err.path, err.why))
    return _log_error(err.path, err.line, err.why)


def read_engine_log(path, engine=ENGINE):
    """The run's identity and configuration from the engine's durable log, read strictly by the reader the
    adapter shares (engine_log.read): a header line, then per input a group of records closed by a commit naming
    the group's last record and its hash; only committed groups count (the tail may be a group still being
    written). The first committed group's input must be the run's Boost configuration. A log not of that shape
    is refused, never read as empty.

    The run's identity is the ID of its Run approval, else its first
    committed input's ID and recorded time (engine_log.identity, as
    adapter/ibkr/relay.py's run_identity): a crash resume's log holds a copy
    of the crashed run's records, its approval included (ADR 0027 section
    6), so the resume keeps the identity, and a fresh run after an earlier
    one has its own approval.

    A momentum log (``engine`` "momentum") is read keeping only its
    configuration and, of every later input, its ID, type, version and
    recorded time, with the holdings worked out as each group is committed
    (_Holdings), so memory holds one line of the log, never its universe
    snapshots and histories (docs/live-trading/lessons-2026-10.md L1). One
    whose record hash chain breaks is refused: its holdings would come from
    records changed after the engine wrote them (AGENTS.md rule 5)."""
    run = os.path.basename(path)[:-len(".log")]
    if not path.endswith(".log") or not run:
        raise Refused("{} is not an engine durable log (<run>.log)".format(path))
    held = _Holdings(path) if engine == MOMENTUM else None
    try:
        log = engine_log.read(path, keep=held.keep if held else None)
    except engine_log.LogUnreadable as err:
        raise _refused(err) from None
    if not log.groups:
        raise Refused("the engine's durable log {} holds no committed configuration input: start the engine "
                      "first".format(path))
    if held and log.chain_break is not None:
        raise Refused("the engine's durable log {} does not follow its record hash chain at record {}: the hash it "
                      "states is not the one its records give (LOG_CHAIN_ALGORITHM), so it was changed after the "
                      "engine wrote it, and neither its configuration nor its holdings are read. Restore it from the "
                      "backup as the engine wrote it; never edit it (docs/manual/market-data.md, \"A crash "
                      "resume\")".format(path, log.chain_break))
    n, _, first = log.groups[0][0]
    configuration = _logged_configuration(path, log.header, n, first, engine)
    inputs = [group[0][2] for group in log.groups]
    market = sum((e.get("type"), e.get("schema_version")) in kinds.DELIVERED_KINDS for e in inputs)
    return EngineLog(log.header["configuration_hash"], log.header["strategy_version"], run, configuration,
                     engine_log.identity(log.groups), tuple(e.get("id") for e in inputs), log.record_hashes,
                     log.chain_break, market, held.held() if held else ())


def _positions(path, n, positions):
    """The instruments of a positions list (account.snapshot versions 3 and 4, a starting book); Refused if it
    is not one, never read as holding nothing."""
    if not isinstance(positions, list):
        raise _log_error(path, n, "cannot read holdings: its positions are not a list")
    out = set()
    for p in positions:
        ticker = p.get("instrument_id") if isinstance(p, dict) else None
        if not isinstance(ticker, str) or not ticker:
            raise _log_error(path, n, "cannot read holdings: its positions name a holding without an instrument: "
                                      "{!r}".format(p))
        out.add(ticker)
    return out


class _Holdings:
    """The instruments a momentum run holds by its own durable log, which need their own daily bars (encode's
    ``optional``: ADR 0026 point 5, amended 2026-10-18, scores a holding across a gap only from its own bars):
    the positions of the last committed account snapshot (kinds.POSITIONS_SNAPSHOTS), else of the Run
    approval's starting book (the book the owner confirmed the run starts from, ADR 0027), plus every
    instrument of a fill committed after it, which a later snapshot will show. A log with none of these (a
    first run before its first reading) holds nothing. A snapshot, fill or approval of a version this table
    does not name is refused, never skipped (ADR 0015).

    keep is engine_log.read's ``keep``: it reads each committed group as it is committed and keeps the first
    (the configuration) whole and of the others only the input's ID, type, version and recorded time."""

    def __init__(self, path, depth=0):
        self.path, self.current, self.groups, self.depth = path, set(), 0, depth
        # (previous log's name, its final record hash) while the holdings are the previous run's book.
        self.inherit = None

    def keep(self, group):
        n, kind, e = group[0]
        self.groups += 1
        if self.groups == 1:
            return group
        kind_version = (e.get("type"), e.get("schema_version"))
        payload = e.get("payload")
        if e.get("type") == SNAPSHOT_TYPE:
            if kind_version not in kinds.POSITIONS_SNAPSHOTS:
                raise _log_error(self.path, n, "cannot read holdings: account.snapshot version {} (this job reads "
                                               "versions {})".format(e.get("schema_version"), ", ".join(
                                                   str(v) for _, v in kinds.POSITIONS_SNAPSHOTS)))
            self.current = _positions(self.path, n, payload.get("positions") if isinstance(payload, dict) else None)
            self.inherit = None
        elif e.get("type") == RUN_APPROVED:
            if e.get("schema_version") not in kinds.RUN_APPROVED_VERSIONS:
                raise _log_error(self.path, n, "cannot read holdings: {} version {}".format(
                    RUN_APPROVED, e.get("schema_version")))
            payload = payload if isinstance(payload, dict) else {}
            book = payload.get("starting_book") if e.get("schema_version") >= kinds.RUN_APPROVED_BOOK_FROM else None
            if book is not None:
                self.current = _positions(self.path, n, book.get("positions") if isinstance(book, dict) else None)
                self.inherit = None
            elif payload.get("previous_log"):
                # A fresh run that takes the previous run's book (ADR 0027): its holdings are that run's.
                self.current, self.inherit = set(), (payload.get("previous_log"), payload.get("previous_log_final_hash"))
            else:
                raise _log_error(self.path, n, "cannot read holdings: its Run approval states no starting book and "
                                               "names no previous log")
        elif e.get("type") == FILL_TYPE:
            if kind_version != kinds.TARGET_WEIGHT_FILL:
                raise _log_error(self.path, n, "cannot read holdings: {} version {}".format(
                    FILL_TYPE, e.get("schema_version")))
            ticker = payload.get("instrument_id") if isinstance(payload, dict) else None
            if not isinstance(ticker, str) or not ticker:
                raise _log_error(self.path, n, "cannot read holdings: its fill names no instrument")
            self.current.add(ticker)
        return [(n, kind, {k: e.get(k) for k in ("id", "type", "schema_version", "recorded_at")})]

    def held(self):
        """The holdings, the previous runs' book read through their logs when a fresh run took it, each
        through this reader, its record hash chain and its final hash the approval names checked."""
        if self.inherit is None:
            return tuple(sorted(self.current))
        name, final_hash = self.inherit
        if not isinstance(name, str) or os.path.basename(name) != name or not name.endswith(".log"):
            raise Refused("cannot read holdings: the Run approval in {} names the previous log {!r}, not a log's "
                          "file name".format(self.path, name))
        if self.depth >= MAX_PREVIOUS_LOGS:
            raise Refused("cannot read holdings: more than {} previous logs before {}".format(MAX_PREVIOUS_LOGS,
                                                                                          self.path))
        previous = os.path.join(os.path.dirname(os.path.abspath(self.path)), name)
        before = _Holdings(previous, self.depth + 1)
        try:
            log = engine_log.read(previous, keep=before.keep)
        except engine_log.LogUnreadable as err:
            raise Refused("cannot read holdings: the run of {} takes the book of the previous run, whose log cannot "
                          "be read: {}".format(self.path, err)) from None
        if log.chain_break is not None or log.final_hash != final_hash:
            raise Refused("cannot read holdings: the previous log {} is not the one the Run approval in {} names "
                          "(its final record hash {}, the approval's {}{}); restore it from the backup as the engine "
                          "wrote it".format(previous, self.path, log.final_hash, final_hash,
                                            "; its record hash chain breaks" if log.chain_break is not None else ""))
        return tuple(sorted(set(before.held()) | self.current))


def head_identity(path):
    """The run identity the first records of the durable log at path name, read no further than its second commit.

    As adapter/ibkr/relay.py's head_identity, by the same reader
    (engine_log.head): a run's identity is in its log's first two committed
    inputs (the configuration, and a fresh run's Run approval, which
    cmd/engine records second, before it listens), so this is
    read_engine_log's identity for every log the engine writes, read without
    the rest of the log. Only the first lines' shape is checked; a log the
    head names as the run's is then read strictly (read_engine_log).
    Refused when the first lines cannot be read as the engine writes them:
    they name no run."""
    try:
        found = engine_log.identity(engine_log.head(path))
    except engine_log.LogUnreadable as err:
        raise _refused(err) from None
    if found is None:
        raise Refused("the engine's durable log {} holds no committed input, so it names no run".format(path))
    return found


def stream_run(engine_streams, engine_log, log, warn=None, engine=ENGINE):
    """The name of the run's stream folder in engine_streams (STREAMS/boost):
    the run's, found by its identity, never by its log's name alone.

    The adapter finds the same folder by the same rule (adapter/ibkr/
    delivery.py, stream_run). A folder is the run's when the log beside
    engine_log named after it (<folder>.log; the run's own log for the
    folder of its own name) has the run's identity (read_engine_log: its Run
    approval's ID, else its configuration and as-of time). A fresh run's
    folder bears its own name, made by its warm-up. A crash resume runs
    under a new name (a new TI_RUN; a log is never reused) and keeps the
    folder of the run that began it: its log begins with a copy of the
    crashed run's records, its approval included (ADR 0027 section 6), so
    the folder, and each envelope's correlation id, is the first run's name
    of a chain of resumes, and an abandoned resume, which has no folder, is
    never taken for it.

    Only the logs of folders are read, each first by its head
    (head_identity): one whose head names another run is read no further,
    and one whose head cannot be read cannot be matched to any run: warn
    (by default a line on stderr) names its folder (an older build's log, or
    one damaged after its run ended). With a log skipped, a folder of the
    run's own name found or not, the job is refused: that log may be the resumed run's own
    first log, its head damaged, and the run is never sent to a warm-up
    under its new name. A log whose head names the run is read whole and
    strictly, and refused if it cannot be. The record hashes compared are
    recomputed first (engine_log.chain_next), in both logs, and a log whose stored
    hashes are not its chain's is refused: it was changed after the engine
    wrote it. The job is also refused (fail closed, AGENTS.md) with two
    folders of the run, its own name's among them (one run's stream is
    never split across two folders), and with one folder of another name
    whose log is not the head of engine_log's record hash chain (engine
    -resume writes the crashed run's records record for record under the
    same header, so a resume's log begins with that log's committed records,
    each with the same record_hash, in the same order), or holds its
    configuration alone (a second start of a first run from the same
    run.env, with no approval, repeats that record hash for hash: it is a
    fresh run, never handed the first run's folder without its own warm-up,
    ADR 0027). With no folder of the run, the run's own name is returned (a
    fresh run before its warm-up) when the log holds no committed input
    beyond its configuration and approval; a log that does holds copied
    history a fresh run's cannot, so it is a crash resume whose first run's
    log is missing, and it is refused with that case named, never sent to a
    warm-up (_own_or_resume)."""
    if warn is None:
        def warn(text):
            print("ti-data warning: " + text, file=sys.stderr)
    logs = os.path.dirname(os.path.abspath(engine_log))
    names = sorted(os.listdir(engine_streams)) if os.path.isdir(engine_streams) else []
    found, chains, skipped = [], {}, []
    for name in names:
        path = os.path.join(logs, name + ".log")
        if not os.path.isdir(os.path.join(engine_streams, name)) or not os.path.isfile(path):
            continue
        if name == log.run_id:
            found.append(name)
            continue
        try:
            head = head_identity(path)
        except Refused as e:
            skipped.append(name)
            warn("stream folder {} skipped: its log's first records cannot be read, so it cannot be matched to any "
                 "run ({}). Once its run has ended, {}".format(os.path.join(engine_streams, name), e,
                                                              _move_aside(engine_streams, logs)))
            continue
        if head != log.identity:
            continue
        try:
            chains[name] = read_engine_log(path, engine)
        except Refused as e:
            raise Refused("the log {} of the stream folder {} names this run in its first records but cannot be "
                          "read: {}. Nothing is written: restore that log as the engine wrote it "
                          "(docs/manual/market-data.md, \"A crash resume\")".format(
                              path, os.path.join(engine_streams, name), e)) from None
        if chains[name].identity == log.identity:
            found.append(name)
    if len(found) > 1:
        raise Refused("the run of {} has more than one stream folder: {}. One run's stream is in one folder, made "
                      "by its warm-up; this stops the job for a person (docs/manual/market-data.md, \"A crash "
                      "resume\")".format(engine_log, ", ".join(os.path.join(engine_streams, n) for n in found)))
    if skipped:
        raise Refused(
            "nothing is written for the run of {}: the logs of {} cannot be matched to any run (their first records cannot "
            "be read): one may be this run's own first log, damaged, so no stream folder is trusted "
            "and no warm-up is to be run for this run's name. If this run is a crash resume, restore its first "
            "run's log from the backup as the engine wrote it; if a log is an old run's, once its run has ended, {}".format(
                engine_log, ", ".join(os.path.join(engine_streams, n) for n in skipped),
                _move_aside(engine_streams, logs)))
    if found == [log.run_id]:
        return log.run_id
    if not found:
        return _own_or_resume(engine_streams, logs, engine_log, log)
    earlier = chains[found[0]]
    broken = [(path, chain.chain_break) for path, chain in ((os.path.join(logs, found[0] + ".log"), earlier),
                                                            (engine_log, log))
              if chain.chain_break is not None and chain.chain_break <= len(earlier.record_hashes)]
    if broken:
        raise Refused(
            "the log {} does not follow its record hash chain at record {}: the hash it states is not the one its "
            "records give (LOG_CHAIN_ALGORITHM), so it was changed after the engine wrote it, nothing shows the run "
            "of {} is the crash resume of the run of the stream folder {}, and nothing is written. Restore it from "
            "the backup as the engine wrote it; never edit it (docs/manual/market-data.md, \"A crash "
            "resume\")".format(broken[0][0], broken[0][1], engine_log, os.path.join(engine_streams, found[0])))
    if len(earlier.inputs) < 2:
        why = "that run committed nothing but its configuration"
    elif log.record_hashes[:len(earlier.record_hashes)] != earlier.record_hashes:
        why = "this log does not begin with that run's {} committed records, record hash for record hash".format(
            len(earlier.record_hashes))
    else:
        return found[0]
    raise Refused(
        "the run of {} has the identity of the run of {}, but {}, so nothing shows it is that run's crash resume "
        "and it is never handed that run's stream folder {}. It is one of three cases: a second start of a first "
        "run without a Run approval, from the same configuration and as-of time (stop it and start a fresh run "
        "with its approval and its own warm-up); a resume of a first run that committed only its configuration "
        "(nothing of that run is kept: start a fresh run with its approval and its own warm-up); or a log that is "
        "not a copy of that run's records (not its resume: resume the crashed run's own log with engine -resume; "
        "while the engine is still copying, run this again once it listens) (docs/manual/market-data.md, \"A "
        "crash resume\")".format(engine_log, os.path.join(logs, found[0] + ".log"), why,
                                 os.path.join(engine_streams, found[0])))


def _own_or_resume(engine_streams, logs, engine_log, log):
    """The run's own name for a log of a fresh run; Refused for a log that holds history, no folder being found.

    The adapter's rule is the same (adapter/ibkr/delivery.py,
    _own_or_resume): a log holding committed delivered inputs
    holds copied history (operator, approval and configuration inputs never
    count), so it is a crash
    resume whose first run's log is not in the engine's log folder, and a
    warm-up under its new name would make a second folder."""
    history = log.market
    if history <= 0:
        return log.run_id
    unmatched = sorted(n for n in (os.listdir(engine_streams) if os.path.isdir(engine_streams) else [])
                       if n != log.run_id and os.path.isdir(os.path.join(engine_streams, n))
                       and not os.path.isfile(os.path.join(logs, n + ".log")))
    hint = ""
    if unmatched:
        hint = " The stream folder {} has no log beside it: it is likely the first run's.".format(
            ", ".join(os.path.join(engine_streams, n) for n in unmatched))
    raise Refused(
        "the log {} already holds {} committed delivered inputs beyond its configuration; if it is a crash resume of an "
        "earlier run, restore that run's log beside it and start again; do not run a warm-up under this name (a "
        "second stream folder splits one run's stream).{} (docs/manual/market-data.md, \"A crash "
        "resume\")".format(engine_log, history, hint))


def _move_aside(engine_streams, logs):
    """The remedy for a log that cannot be matched to any run, as the adapter's (delivery.move_aside)."""
    return ("move the folder out of {} and its log out of the engine's log folder {}, keeping both as evidence in "
            "a folder outside both (docs/manual/market-data.md, \"A crash resume\")".format(engine_streams, logs))


def _logged_configuration(path, header, n, envelope, engine=ENGINE):
    """The Boost (or, with ``engine`` "momentum", the Momentum) configuration the run's first input carries
    (cmd/engine composeTargetWeight: id "configuration:<hash>", the header's identity)."""
    if (envelope.get("type") != CONFIGURATION_TYPE
            or envelope.get("id") != "configuration:" + header["configuration_hash"]
            or envelope.get("configuration_hash") != header["configuration_hash"]
            or envelope.get("strategy_version") != header["strategy_version"]):
        raise _log_error(path, n, "the run's first input is not its configuration")
    try:
        return (_momentum if engine == MOMENTUM else _boost)(envelope.get("payload"))
    except Refused as e:
        raise _log_error(path, n, "its configuration input: {}".format(e)) from None


def _boost(cfg):
    """The Boost fields of a configuration object; Refused if it is not one."""
    try:
        boost = cfg["boost"]
        out = Boost(boost["instrument_id"], dt.date.fromisoformat(boost["warm_up_start"]), int(boost["trend_days"]),
                    int(boost["exit_sma"]), int(boost["rsi_period"]))
    except (ValueError, KeyError, TypeError) as e:
        raise Refused("it is not a Boost configuration: {!r}".format(e)) from None
    if cfg.get("strategy_id") != ENGINE or not isinstance(out.instrument, str) or not out.instrument:
        raise Refused("it is not a Boost configuration")
    return out


def _momentum(cfg):
    """The Momentum fields of a configuration object; Refused if it is not a momentum configuration reading the
    compact universe input, the only stream this job writes for it."""
    try:
        if cfg.get("strategy_id") != MOMENTUM:
            raise KeyError("strategy_id")
        momentum, glide = cfg["momentum"], cfg["glide"]
        required = {momentum["trend_instrument"], momentum["risk_off_instrument"]}
        required |= {f["instrument_id"] for f in glide["bond_funds"]}
        trend_closes = int(momentum["trend_closes"])
        settings = json.dumps({"momentum": momentum, "glide": glide}, sort_keys=True, separators=(",", ":"))
    except (ValueError, KeyError, TypeError, AttributeError) as e:
        raise Refused("it is not a momentum configuration: {!r}".format(e)) from None
    if not all(isinstance(t, str) and t for t in required):
        raise Refused("it is not a momentum configuration: an instrument is not named")
    if momentum.get("universe_input") != "compact":
        raise Refused("its universe_input is {!r}: the data job writes the compact stream (encode --ranker) "
                      "only, so the configuration must say \"compact\"".format(momentum.get("universe_input")))
    return Momentum(tuple(sorted(required)), trend_closes, settings)


def read_momentum_config(path):
    """The Momentum fields of the momentum configuration file at path."""
    try:
        with open(path) as f:
            cfg = json.load(f)
    except (OSError, ValueError) as e:
        raise Refused("cannot read the momentum configuration {}: {}".format(path, e)) from None
    try:
        return _momentum(cfg)
    except Refused as e:
        raise Refused("{}: {}".format(path, e)) from None


def read_config(path):
    """The Boost configuration's fields the stream depends on, and the
    history its indicators need (internal/event BoostConfiguration)."""
    try:
        with open(path) as f:
            cfg = json.load(f)
    except (OSError, ValueError) as e:
        raise Refused("cannot read the Boost configuration {}: {}".format(path, e)) from None
    try:
        boost = _boost(cfg)
    except Refused as e:
        raise Refused("{}: {}".format(path, e)) from None
    if not nyse_calendar.is_trading_day(boost.warm_up_start):
        raise Refused("warm_up_start {} in {} is not an NYSE session".format(boost.warm_up_start, path))
    return boost


def _streams(directory, prefix=ENGINE):
    """[(last session, path)] of the run's streams (``<prefix>-<session>.jsonl``), oldest first."""
    out = []
    pattern = STREAM if prefix == ENGINE else re.compile(re.escape(prefix) + r"-(\d{4}-\d{2}-\d{2})\.jsonl")
    for path in glob.glob(os.path.join(directory, glob.escape(prefix) + "-*.jsonl")):
        m = pattern.fullmatch(os.path.basename(path))
        if m:
            out.append((dt.date.fromisoformat(m.group(1)), path))
    return sorted(out)


def _bytes(envelopes):
    return "".join(json.dumps(e, separators=(",", ":"), allow_nan=False) + "\n" for e in envelopes).encode()


def _encode(data, first, last, cfg, identity):
    """One stream file's envelopes, numbered from 1 (the file contract above)."""
    return encode.encode(data, first, last, identity, required=[cfg.instrument], base=cfg.warm_up_start,
                         engine=ENGINE, first_sequence=1)


def _session_of(envelope):
    """The session a Boost envelope belongs to: its id ends with it (a bar's
    "marketdata:bar:<instrument>:<session>", a close's
    "marketdata:session-closed:<session>")."""
    return dt.date.fromisoformat(envelope["id"].rsplit(":", 1)[1])


def _verify(existing, data, cfg, identity, engine_log):
    """Refuses unless every written stream, oldest first, re-encodes byte for
    byte; each file's first session follows the one before it.

    The recordings are encoded once, from the warm-up start through the
    newest file's last session, and each file is compared with its sessions'
    slice of that encoding, numbered from 1 again. That equals encoding the
    file alone: the adjustment is forward from the warm-up start, so a
    session's envelopes depend only on the sessions and actions up to it
    (adjust.forward_factors), and a file's sequence is its own ordinal (the
    file contract above). One encoding a run keeps the work linear in the
    sessions, not files times sessions."""
    if not existing:
        return
    try:
        whole = _encode(data, cfg.warm_up_start, existing[-1][0], cfg, identity)
    except Exception as e:   # a recording missing, a gap or a conflict: the files cannot be re-derived
        raise Refused("the run's files through {} cannot be encoded again from their recordings ({}: {}); this "
                      "stops the job for a person".format(existing[-1][1], type(e).__name__, e)) from None
    previous = None
    for last, path in existing:
        first = cfg.warm_up_start if previous is None else nyse_calendar.sessions_after(previous, 1)[0]
        part = [e for e in whole if first <= _session_of(e) <= last]
        again = _bytes(dict(e, sequence=n) for n, e in enumerate(part, 1))
        with open(path, "rb") as f:
            if f.read() != again:
                raise Refused("{} differs from the stream its recordings and the engine log {} give now; the "
                              "engine has already read it, so it is not rewritten and nothing more is written "
                              "for the run: this stops the job for a person (docs/manual/market-data.md, "
                              "\"A file already written differs\")".format(path, engine_log))
        previous = last


def _actions_reach(data, start):
    """The last day both kinds' recorded windows cover contiguously from ``start``, or the day before it."""
    reach = []
    for kind in history.KINDS:
        windows = sorted((dt.date.fromisoformat(r["from"]), dt.date.fromisoformat(r["session"]))
                         for r in map(record.read, glob.glob(os.path.join(data, kind + "_*_*.json"))))
        end = start - dt.timedelta(days=1)
        for a, b in windows:
            if a > end + dt.timedelta(days=1):
                break
            end = max(end, b)
        reach.append(end)
    return min(reach)


def run(data, streams, config, engine_log, key, now, settle, session=None, backfill_from=None,
        client_factory=massive.Client):
    """Runs the job once (see above) and returns the stream's path."""
    cfg = read_config(config)
    if backfill_from is not None and backfill_from != cfg.warm_up_start:
        raise Refused("--backfill-from {} is not the configuration's warm_up_start {}: the indicators are seeded "
                      "from that session".format(backfill_from, cfg.warm_up_start))
    if session is None:
        session = settled_session(now(), settle)
    else:
        steps.require_settled(session, now, settle)
    if session < cfg.warm_up_start:
        raise Refused("session {} precedes the warm-up start {}".format(session, cfg.warm_up_start))
    log = read_engine_log(engine_log)
    if log.boost != cfg:
        raise Refused("the Boost configuration {} is not the one the engine's durable log {} holds ({} there, {} "
                      "here): the stream would carry the run's configuration hash over another history".format(
                          config, engine_log, log.boost, cfg))
    run_id = stream_run(os.path.join(streams, ENGINE), engine_log, log)
    identity = encode.Identity(log.configuration_hash, log.strategy_version, run_id)
    directory = os.path.join(streams, ENGINE, run_id)
    existing = _streams(directory)
    for last, path in existing:
        if session <= last:
            _verify(existing, data, cfg, identity, engine_log)
            return path
    previous = existing[-1][0] if existing else None
    if previous is None and backfill_from is None:
        raise Refused("run {} has no stream yet: run the warm-up once first, ti-data --backfill-from {}".format(
            run_id, cfg.warm_up_start))
    first = cfg.warm_up_start if previous is None else nyse_calendar.sessions_after(previous, 1)[0]
    span = nyse_calendar.sessions(first, session)
    if previous is None and len(span) < cfg.min_sessions:
        raise Refused("the history {}..{} holds {} sessions; the indicators need {}".format(
            first, session, len(span), cfg.min_sessions))
    latest = settled_session(now(), settle)
    # Only the latest settled session is awaited quietly (None matches no day).
    client = _LatestSession(client_factory(key), session if session == latest else None)
    try:
        history.backfill(client, data, session, len(span), now=now, settle=settle)
    except NotAvailableYet as e:
        if now() >= quiet_until(session):
            raise Refused("Massive still refuses session {} at or after {} New York on the next trading day, when "
                          "the plan should serve it ({}); check the plan and the key".format(
                              session, QUIET_BEFORE.strftime("%H:%M"), e)) from None
        raise NotAvailableYet("session {} is not available yet on this plan ({}); the next firing asks again"
                              .format(session, e)) from None
    start = cfg.warm_up_start + dt.timedelta(days=1)
    window = min(_actions_reach(data, start) + dt.timedelta(days=1), session - ACTION_OVERLAP)
    try:
        history.record_actions(client, max(window, start), session, data, now=now, settle=settle)
    except record.AlreadyRecorded:
        pass
    # After the actions are recorded, so one published late against a file
    # already delivered stops the job here.
    _verify(existing, data, cfg, identity, engine_log)
    envelopes = _encode(data, first, session, cfg, identity)
    os.makedirs(directory, exist_ok=True)
    return encode.write_stream(os.path.join(directory, "boost-{}.jsonl".format(session)), envelopes)


def _stream_base(path):
    """The adjusted views' base of a momentum run: the first session of its first file's calendar, which the
    compact stream starts at the base (encode.py)."""
    with open(path) as f:
        first = f.readline()
    try:
        envelope = json.loads(first)
        if envelope["type"] != encode.CALENDAR[0]:
            raise ValueError("its first input is {}, not the calendar".format(envelope["type"]))
        return dt.date.fromisoformat(envelope["payload"]["sessions"][0])
    except (ValueError, KeyError, TypeError, IndexError) as e:
        raise Refused("the run's first file {} does not begin with its calendar, so its base cannot be read ({}); "
                      "this stops the job for a person".format(path, e)) from None


def _named_in(path, required):
    """The holdings a written momentum file was encoded with: the instruments its bars name beyond the
    required ones. A compact stream carries bars for the named instruments only, and an optional instrument
    with no bar in a file's sessions changes none of its bytes, so this re-derives the file's own set."""
    out = set()
    with open(path) as f:
        for line in f:
            if '"type":"market.bar.completed"' in line:
                out.add(json.loads(line)["payload"]["instrument_id"])
    return sorted(out - set(required))


def _digest_of(envelopes):
    digest = hashlib.sha256()
    for e in envelopes:
        digest.update((json.dumps(e, separators=(",", ":"), allow_nan=False) + "\n").encode())
    return digest.hexdigest()


def _verify_momentum(files, data, cfg, base, identity, engine_log, ranker):
    """Refuses unless each of ``files`` ([(first session, last session, path)]) re-encodes alone, byte for
    byte, from the recordings under the current identity, with the holdings its own bars name."""
    for first, last, path in files:
        try:
            envelopes = encode.iter_encode(data, first, last, identity, required=list(cfg.required),
                                           optional=_named_in(path, cfg.required), base=base, first_sequence=1,
                                           engine=MOMENTUM, ranker=ranker, retracted_through=last)
            again = _digest_of(envelopes)
        except Exception as e:   # a recording missing, a gap or a conflict: the file cannot be re-derived
            raise Refused("the run's file {} cannot be encoded again from its recordings ({}: {}); this stops the "
                          "job for a person".format(path, type(e).__name__, e)) from None
        if _file_digest(path) != again:
            raise Refused("{} differs from the stream its recordings and the engine log {} give now; the "
                          "engine may already have read it, so it is not rewritten and nothing more is written "
                          "for the run: this stops the job for a person (docs/manual/market-data.md, "
                          "\"A file already written differs\")".format(path, engine_log))


def _file_digest(path):
    """The SHA-256 of a written file, read a megabyte at a time: a warm-up file of a long history is far
    larger than the memory budget (docs/live-trading/lessons-2026-10.md L1)."""
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _spans(existing, base):
    """[(first session, last session, path)] of the written files, oldest first: each follows the one before."""
    out, previous = [], None
    for last, path in existing:
        first = base if previous is None else nyse_calendar.sessions_after(previous, 1)[0]
        out.append((first, last, path))
        previous = last
    return out


def _has_bars(data, day):
    return any(os.path.exists(os.path.join(data, "{}_{}.json".format(kind, day.isoformat())))
               for kind in ("grouped", "universe"))


def _check_chain(spans, retracted):
    """Refuses unless the written files ([(first, last, path)], oldest first) are one unbroken chain: each
    begins at the first session after the one before it ends (the first at the run's base), read from its
    own calendar's time, never inferred from its neighbours. A file missing between two others, or the
    warm-up's, is a published history that is gone, and stops the job for a person."""
    for first, last, path in spans:
        expected = first
        while expected in retracted:
            expected = nyse_calendar.sessions_after(expected, 1)[0]
        with open(path) as f:
            line = f.readline()
        try:
            envelope = json.loads(line)
            if envelope["type"] != encode.CALENDAR[0]:
                raise ValueError("its first input is {}, not the calendar".format(envelope["type"]))
            begins = dt.date.fromisoformat(envelope["event_time"][:10])
        except (ValueError, KeyError, TypeError) as e:
            raise Refused("the run's file {} does not begin with its calendar ({}); this stops the job for a "
                          "person".format(path, e)) from None
        if begins != expected and begins in retracted and first <= begins < expected:
            # The file begins at a session cancelled since it was published: the retraction rule (a
            # retraction of a session on or before a published file's last stops the job), not a lost file.
            raise Refused("{} begins at {}, but a retraction of {} was recorded after it was published: the engine "
                          "has already read that session, so nothing more is written for the run: this stops the "
                          "job for a person (docs/manual/market-data.md, \"A file already written differs\")".format(
                              path, begins, begins))
        if begins != expected:
            raise Refused("the run's files are not one unbroken chain: {} begins at {}, but the sessions from {} "
                          "are in no file, so a file written for the run is missing; nothing more is written for "
                          "the run: this stops the job for a person (restore the file from the backup, unchanged: "
                          "docs/manual/market-data.md, \"A job for the IRAs\")".format(path, begins, expected))


def _session_before(day):
    """The NYSE session before ``day`` by the committed rules calendar (nyse_calendar.py, plan.md section 4).
    It does not know recorded retractions: a caller stepping off a cancelled day skips those itself
    (_run_momentum's session, encode.py's retraction rule)."""
    day -= dt.timedelta(days=1)
    while not nyse_calendar.is_trading_day(day):
        day -= dt.timedelta(days=1)
    return day


def _labelled(day, kinds_dates):
    """Whether both sector sources have a recording dated within the week up to ``day``, the labels the
    encoder reads a universe session with (encode._universe_rows, sectors.latest's 7 days); the checks of its
    fetch time are the encoder's, here only whether one could serve."""
    return all(any(day - dt.timedelta(days=7) <= d <= day for d in dates) for dates in kinds_dates.values())


def _sector_dates(data):
    out = {}
    for kind in ("sectors-iwv", "sectors-nasdaq"):
        out[kind] = {dt.date.fromisoformat(os.path.basename(p)[len(kind) + 1:-len(".json")])
                     for p in glob.glob(os.path.join(data, kind + "_*.json"))}
    return out


def _record_sectors(data, session, sectors_get, now, settle, warn):
    """The session's sector labels, before any request to Massive, so a morning whose Massive requests fail
    still records the labels a later catch-up's universe needs (the sources serve only the latest session's
    files). A recording another job writes first is taken as recorded and the other source fetched."""
    for _ in range(3):
        try:
            sectors.record_sectors(data, session, get=sectors_get, now=now, settle=settle)
            return
        except record.AlreadyRecorded:
            continue
        except Exception as e:   # a source down: a universe session needs labels within a week (_labelled)
            warn("the sector labels of {} were not recorded ({}: {})".format(session, type(e).__name__, e))
            return


def _record_universe(client, data, day, now, settle, warn):
    """The universe of ``day``: its common-stock listing from Massive's reference tickers dated that day, with
    the bars of its whole-market recording, so the two recordings agree. One another job records first is
    taken as recorded."""
    grouped = os.path.join(data, "grouped_{}.json".format(day.isoformat()))
    if not os.path.exists(grouped):   # a universe recording alone holds the session's bars already
        return
    recorded = record.read(grouped)
    try:
        steps.run_universe(client, day, data, now=now, grouped_body={"results": recorded["bars"]}, settle=settle)
    except record.AlreadyRecorded:
        pass


def run_momentum(data, streams, config, engine_log, key, now, settle, instance, ranker_bin=None, ranker=None,
                 session=None, backfill_from=None, client_factory=massive.Client, sectors_get=sectors.http_get,
                 warn=None, reencode_all=False, rebuild_manifest=False):
    """Runs the momentum engine's job once (see above) and returns the stream's path. The ranker is
    ``ranker`` if given, else cmd/universe-rank at ``ranker_bin``, run on a private copy of the configuration's
    bytes checked against the engine's log, never on the file's path, which may change while the job runs."""
    if warn is None:
        def warn(text):
            print("ti-data warning: " + text, file=sys.stderr)
    try:
        with open(config, "rb") as f:
            raw = f.read()
        cfg = _momentum(json.loads(raw))
    except (OSError, ValueError) as e:
        raise Refused("cannot read the momentum configuration {}: {}".format(config, e)) from None
    except Refused as e:
        raise Refused("{}: {}".format(config, e)) from None
    if session is None:
        session = settled_session(now(), settle)
    else:
        steps.require_settled(session, now, settle)
    log = read_engine_log(engine_log, MOMENTUM)
    if log.boost != cfg:
        raise Refused("the momentum configuration {} is not the one the engine's durable log {} holds: the stream "
                      "would carry the run's configuration hash over another ranking".format(config, engine_log))
    try:
        with tempfile.TemporaryDirectory(prefix="ti-data-") as private, _instance_lock(streams, instance):
            if ranker is None:
                checked = os.path.join(private, "configuration.json")
                with open(checked, "wb") as f:
                    f.write(raw)
                ranker = encode.go_ranker([ranker_bin, "-config", checked])
            return _run_momentum(data, streams, engine_log, key, now, settle, instance, ranker, session,
                                 backfill_from, client_factory, sectors_get, warn, cfg, log, reencode_all,
                                 rebuild_manifest)
    except Busy as e:
        # At the last morning firing nothing asks again before the 09:00 cutoff, and the run holding the lock
        # (a warm-up or a run by hand) has no OnFailure of its own: the firing fails, so it pages
        # (docs/live-trading/lessons-2026-10.md L2; ADR 0019, "Alerting").
        if now() >= quiet_until(settled_session(now(), settle)):
            raise Refused("{}, and this is the last morning firing: nothing asks again before the 09:00 cutoff, "
                          "so it fails (check the run holding the lock)".format(e)) from None
        raise


class Busy(Exception):
    """Another run of the instance's job holds its lock; this one writes nothing."""


@contextlib.contextmanager
def _instance_lock(streams, instance):
    """The instance's lock (STREAMS/<instance>/.ti-data.lock), held from the reading of the run's folder to the
    publication of its file and manifest, so a start by hand, a warm-up and a timer firing never publish
    overlapping files (ADR 0027: one run's stream is one chain of files). Not waited for: a firing that finds
    it held writes nothing, quietly before the last morning firing, which asks again, and as a failure that
    pages at the last one (run_momentum; docs/live-trading/lessons-2026-10.md L2)."""
    folder = os.path.join(streams, instance)
    os.makedirs(folder, exist_ok=True)
    with open(os.path.join(folder, LOCK), "a") as f:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise Busy("another run of the {} data job holds its lock; this one writes nothing (the run holding it "
                       "writes the file, or fails and pages)".format(instance)) from None
        try:
            yield
        finally:
            fcntl.flock(f, fcntl.LOCK_UN)


def _action_set(data):
    """{name: sha256} of every split and dividend recording: the action recordings an encoding reads."""
    out = {}
    for kind in history.KINDS:
        for path in glob.glob(os.path.join(data, kind + "_*_*.json")):
            out[os.path.basename(path)] = _file_digest(path)
    return out


def _action_from(name):
    """The first day of an action recording's window, from its name (record.filename: kind_from_session)."""
    return dt.date.fromisoformat(name.split("_")[1])


def _manifest_entry(path, first, last, without):
    return {"name": os.path.basename(path), "first": first.isoformat(), "last": last.isoformat(),
            "bytes": os.path.getsize(path), "sha256": _file_digest(path),
            "without_universe": [d.isoformat() for d in without]}


MANIFEST_REMEDY = ("restore manifest.json from the backup, unchanged, or, without one, rebuild it by hand with "
                   "--rebuild-manifest, which takes in every file only if it encodes again byte for byte "
                   "(docs/manual/market-data.md, \"A job for the IRAs\")")


def _read_manifest(directory):
    """The run's manifest, every field checked; None when there is none; Refused, naming the field and the
    remedy, for one that cannot be read (a hand edit, a later version, a disk fault). The manifest is the
    job's record of the inputs it published to the run (ADR 0027: an input the engine may have read is never
    rewritten), written whole or not at all (ADR 0018), so a manifest that does not read is never guessed at
    (AGENTS.md rule 5)."""
    path = os.path.join(directory, MANIFEST)

    def need(table, key, kind, where):
        if not isinstance(table, dict) or key not in table:
            raise ValueError("{} lacks the field {}".format(where, key))
        value = table[key]
        if kind == "date":
            if not isinstance(value, str):
                raise ValueError("{}'s {} is not a date".format(where, key))
            dt.date.fromisoformat(value)
        elif not isinstance(value, kind) or isinstance(value, bool):
            raise ValueError("{}'s {} is not a {}".format(where, key, kind.__name__))
        return value
    try:
        with open(path) as f:
            manifest = json.load(f)
        if need(manifest, "manifest_version", int, "it") != MANIFEST_VERSION:
            raise ValueError("its version is {!r}, not {}".format(manifest["manifest_version"], MANIFEST_VERSION))
        need(manifest, "base", "date", "it")
        need(manifest, "actions", dict, "it")
        for n, e in enumerate(need(manifest, "files", list, "it"), 1):
            where = "its file entry {}".format(n)
            need(e, "name", str, where)
            need(e, "first", "date", where)
            need(e, "last", "date", where)
            need(e, "bytes", int, where)
            need(e, "sha256", str, where)
            for d in need(e, "without_universe", list, where):
                if not isinstance(d, str):
                    raise ValueError("{}'s without_universe holds {!r}, not a date".format(where, d))
                dt.date.fromisoformat(d)
    except FileNotFoundError:
        return None
    except (OSError, ValueError, TypeError) as e:
        raise Refused("the run's manifest {} cannot be read: {}; this stops the job for a person: {}".format(
            path, e, MANIFEST_REMEDY)) from None
    return manifest


def _last_session_of(path):
    """The session of a stream file's last line, its market.session.closed: each session's close is its last
    input (ADR 0021: a session closes before its decision), so a file that ends otherwise, or at another
    session than its manifest lists, is not the file the job published (AGENTS.md rule 5). Only the end of
    the file is read (docs/live-trading/lessons-2026-10.md L1)."""
    with open(path, "rb") as f:
        f.seek(0, os.SEEK_END)
        end = f.tell()
        size, tail = 1 << 16, b""
        while True:
            start = max(0, end - size)
            f.seek(start)
            tail = f.read(end - start)
            lines = tail.rstrip(b"\n").split(b"\n")
            if len(lines) > 1 or start == 0:
                break
            size *= 2
    try:
        envelope = json.loads(lines[-1])
        if envelope["type"] != encode.SESSION_CLOSED[0]:
            raise ValueError("its last input is {}, not a session's close".format(envelope["type"]))
        return dt.date.fromisoformat(envelope["event_time"][:10])
    except (ValueError, KeyError, TypeError) as e:
        raise Refused("the run's file {} does not end with a session's close ({}); this stops the job for a "
                      "person".format(path, e)) from None


def _write_manifest(directory, manifest):
    """The manifest, written whole or not at all (a synced temporary file renamed over it, then the folder
    synced: ADR 0018)."""
    fd, tmp = tempfile.mkstemp(dir=directory, prefix=".manifest-", suffix=".part")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(manifest, f, sort_keys=True, indent=1)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, os.path.join(directory, MANIFEST))
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise
    record._sync_dir(directory)


def _published(directory, instance, existing, base, data, cfg, identity, engine_log, ranker, retracted,
               rebuild=False):
    """The run's published files as its manifest lists them, checked (AGENTS.md rule 5; ADR 0027: an input
    the engine may have read is never rewritten): every listed file is there with the length and digest the
    manifest gives, named for its last session and holding exactly the first and last sessions it lists
    (its own calendar and its own last close), the files form one unbroken chain from the base, and no
    other stream file is in the folder, except one newest file published by a run that stopped before its
    manifest was updated (the first file included: the manifest is written before it), which is taken in
    only if it encodes again byte for byte. With ``rebuild`` (by hand, --rebuild-manifest) every file is
    taken in by that rule, in chain order, into a new manifest. Returns (manifest, [(first, last, path)]);
    refuses anything else, so a published file truncated, changed or gone, the newest included, is never
    written again from today's recordings and holdings."""
    try:
        manifest = _read_manifest(directory)
    except Refused:
        if not rebuild:
            raise
        manifest = None   # unreadable: what --rebuild-manifest is for
    if rebuild and manifest is not None:
        raise Refused("--rebuild-manifest refused: the run's manifest {} can be read, so it is kept as the record of "
                      "what the job published (a file it lists that is gone is restored from the backup, never "
                      "forgotten); the rebuild is only for a lost or unreadable manifest".format(
                          os.path.join(directory, MANIFEST)))
    # The split and dividend recordings the rebuild checks the files against, read before it encodes any.
    checked_actions = _action_set(data) if rebuild else None
    if manifest is None:
        if existing and not rebuild:
            raise Refused("the run's folder {} holds stream files but no manifest of what the job published; this "
                          "stops the job for a person: {}".format(directory, MANIFEST_REMEDY))
        if not existing:
            return None, []
        manifest = {"manifest_version": MANIFEST_VERSION, "base": base.isoformat(), "files": [], "actions": {}}
    if dt.date.fromisoformat(manifest["base"]) != base:
        raise Refused("the run's manifest gives the base {}, but {} {}".format(
            manifest["base"], "the run's first file's calendar" if existing else "--backfill-from", base))
    pattern = re.compile(re.escape(instance) + r"-(\d{4}-\d{2}-\d{2})\.jsonl")
    listed = []
    for e in manifest["files"]:
        path = os.path.join(directory, e["name"])
        first, last = dt.date.fromisoformat(e["first"]), dt.date.fromisoformat(e["last"])
        named = pattern.fullmatch(e["name"])
        if not named or dt.date.fromisoformat(named.group(1)) != last or first > last:
            raise Refused("the run's manifest lists {} for {}..{}, which is not that file's name or a span of "
                          "sessions; this stops the job for a person: {}".format(path, first, last, MANIFEST_REMEDY))
        if not os.path.exists(path):
            raise Refused("{} is missing: the job published it for the run (its manifest lists it), so nothing more "
                          "is written for the run: this stops the job for a person (restore it from the backup, "
                          "unchanged; docs/manual/market-data.md, \"A job for the IRAs\")".format(path))
        if os.path.getsize(path) != e["bytes"] or _file_digest(path) != e["sha256"]:
            raise Refused("{} changed since it was published (its manifest gives {} bytes, sha256 {}); nothing more is "
                          "written for the run: this stops the job for a person (restore it from the backup, "
                          "unchanged)".format(path, e["bytes"], e["sha256"]))
        if _last_session_of(path) != last:
            raise Refused("{} ends at {}, not at the {} its manifest lists; this stops the job for a person: {}".format(
                path, _last_session_of(path), last, MANIFEST_REMEDY))
        listed.append((first, last, path))
    names = {os.path.basename(p) for _, _, p in listed}
    unlisted = [(last, path) for last, path in existing if os.path.basename(path) not in names]
    spans = _spans([(last, path) for _, last, path in listed], base)
    if [(a, b) for a, b, _ in spans] != [(a, b) for a, b, _ in listed]:
        raise Refused("the run's manifest {} does not list one unbroken chain of files from the base {}; this stops "
                      "the job for a person: {}".format(os.path.join(directory, MANIFEST), base, MANIFEST_REMEDY))
    _check_chain(spans, retracted)
    newest = spans[-1][1] if spans else None
    if unlisted and not rebuild and (len(unlisted) > 1 or (newest is not None and unlisted[0][0] <= newest)):
        raise Refused("the run's folder {} holds stream files its manifest does not list ({}); this stops the "
                      "job for a person".format(directory, ", ".join(os.path.basename(p) for _, p in unlisted)))
    for last, path in unlisted:
        first = base if newest is None else nyse_calendar.sessions_after(newest, 1)[0]
        _check_chain(spans + [(first, last, path)], retracted)
        if _last_session_of(path) != last:
            raise Refused("{} ends at {}, not at the session its name gives; this stops the job for a person".format(
                path, _last_session_of(path)))
        _verify_momentum([(first, last, path)], data, cfg, base, identity, engine_log, ranker)
        manifest["files"].append(_manifest_entry(path, first, last, _without_universe(data, first, last, retracted)))
        spans.append((first, last, path))
        newest = last
    if rebuild:
        if _action_set(data) != checked_actions:
            raise Refused("split or dividend recordings changed during the rebuild (another job recorded one), so "
                          "the files were not all checked against them; nothing is written: run --rebuild-manifest "
                          "again")
        manifest["actions"] = checked_actions
        _write_manifest(directory, manifest)
    return manifest, spans


def _without_universe(data, first, last, retracted):
    return [d for d in nyse_calendar.sessions(first, last)
            if d not in retracted and not os.path.exists(os.path.join(data, "universe_{}.json".format(d.isoformat())))]


def _runs(days):
    """Consecutive sessions as "first..last" runs, for one line naming many."""
    out, run = [], []
    for d in days:
        if run and nyse_calendar.sessions_after(run[-1], 1)[0] != d:
            out.append(run)
            run = []
        run.append(d)
    if run:
        out.append(run)
    return ", ".join(str(r[0]) if len(r) == 1 else "{}..{}".format(r[0], r[-1]) for r in out)


def _reencode_changed(manifest, spans, data, cfg, base, identity, engine_log, ranker, reencode_all, now_set=None):
    """Encodes again (AGENTS.md rule 5; ADR 0027: an input the engine may have read is never rewritten, only
    checked) every published file that a split or dividend recording which appeared, changed or went
    since the manifest's check could reach (its last session on or after the earliest window start among
    them), however old, and returns the action recordings now read. More than MAX_REENCODED files is more
    than one morning firing can check before the open (docs/live-trading/lessons-2026-10.md L1 and L2):
    refused, naming the remedy (a run by hand with
    --reencode-all), unless ``reencode_all``. ``now_set`` is the action recordings read before any file the
    caller checked against them was encoded (never a set read afterwards, which could hold a recording no
    file was checked against); by default they are read here, before this encodes any."""
    if now_set is None:
        now_set = _action_set(data)
    before = manifest["actions"] if manifest else {}
    changed = {k for k in set(now_set) | set(before) if now_set.get(k) != before.get(k)}
    if not changed or not spans:
        return now_set
    reach = min(_action_from(k) for k in changed)
    affected = [s for s in spans if s[1] >= reach]
    if len(affected) > MAX_REENCODED and not reencode_all:
        raise Refused("split and dividend recordings appeared or changed since the run's files were checked ({}), "
                      "reaching back to {}: the {} files published from {} on would have to be encoded again, more "
                      "than one firing checks ({}); nothing is written. Run the job once by hand with --reencode-all "
                      "(docs/manual/market-data.md, \"A job for the IRAs\")".format(
                          ", ".join(sorted(changed)), reach, len(affected), affected[0][0], MAX_REENCODED))
    _verify_momentum(affected, data, cfg, base, identity, engine_log, ranker)
    return now_set


def _run_momentum(data, streams, engine_log, key, now, settle, instance, ranker, session, backfill_from,
                  client_factory, sectors_get, warn, cfg, log, reencode_all, rebuild_manifest=False):
    run_id = stream_run(os.path.join(streams, instance), engine_log, log, engine=MOMENTUM)
    identity = encode.Identity(log.configuration_hash, log.strategy_version, run_id)
    directory = os.path.join(streams, instance, run_id)
    existing = _streams(directory, instance)
    base = _stream_base(existing[0][1]) if existing else backfill_from
    if existing and backfill_from is not None and backfill_from != base:
        raise Refused("--backfill-from {} is not the run's base {}, where its first file starts: the base is fixed "
                      "for the life of a run".format(backfill_from, base))
    if base is None:
        raise Refused("run {} has no stream yet: run the warm-up once first, ti-data --engine momentum --instance {} "
                      "... --backfill-from <the first session of its history> (docs/manual/market-data.md, \"A job "
                      "for the IRAs\")".format(run_id, instance))
    if not nyse_calendar.is_trading_day(base):
        raise Refused("--backfill-from {} is not an NYSE session".format(base))
    if session < base:
        raise Refused("session {} precedes the run's base {}".format(session, base))
    retracted = encode.load_retractions(data)
    # A file is never named after a session the exchange cancelled: the job's session, and the latest, are
    # the last real closes on or before them (encode.py treats a retracted day as no session).
    while session in retracted:
        # The cancelled day was recorded with no bars (encode.py refuses a retracted day with bars, and so
        # does this job before it steps over it).
        for kind in ("grouped", "universe"):
            if os.path.exists(os.path.join(data, "{}_{}.json".format(kind, session.isoformat()))):
                raise Refused("{} is retracted, but its {} bars are recorded; this stops the job for a person "
                              "(docs/manual/market-data.md, \"A session the exchange cancels\")".format(session, kind))
        session = _session_before(session)
    if session < base:
        raise Refused("session {} precedes the run's base {}".format(session, base))
    manifest, spans = _published(directory, instance, existing, base, data, cfg, identity, engine_log, ranker,
                                 retracted, rebuild=rebuild_manifest)
    latest = settled_session(now(), settle)
    while latest in retracted:
        latest = _session_before(latest)
    for first, last, path in spans:
        if first <= session <= last:
            # The requested file is always encoded again: any recording it reads (a retraction, a bar, a
            # universe or a sector recording) may have changed, not only the action recordings tracked. The
            # action recordings are read first, so one recorded meanwhile is never acknowledged unchecked.
            snapshot = _action_set(data)
            _verify_momentum([(first, last, path)], data, cfg, base, identity, engine_log, ranker)
            others = [s for s in spans if s[2] != path]   # the requested file is encoded once, above
            manifest["actions"] = _reencode_changed(manifest, others, data, cfg, base, identity, engine_log, ranker,
                                                    reencode_all, now_set=snapshot)
            _write_manifest(directory, manifest)
            entry = next(e for e in manifest["files"] if e["name"] == os.path.basename(path))
            recent = [d for d in entry["without_universe"] if dt.date.fromisoformat(d) >= latest - dt.timedelta(days=7)]
            if recent:
                warn("{} was written without the universe of {}: a decision on it is withheld "
                     "(missing-universe)".format(path, ", ".join(recent)))
            return path
    previous = spans[-1][1] if spans else None
    first = base if previous is None else nyse_calendar.sessions_after(previous, 1)[0]
    span = [d for d in nyse_calendar.sessions(first, session) if d not in retracted]
    if previous is None and len([d for d in nyse_calendar.sessions(base, session) if d not in retracted]) < cfg.trend_closes:
        raise Refused("the history {}..{} holds fewer sessions than the {} closes of the trend".format(
            base, session, cfg.trend_closes))
    if session == latest:
        _record_sectors(data, session, sectors_get, now, settle, warn)
    client = _LatestSession(client_factory(key), session if session == latest else None)
    try:
        for day in span:
            if not _has_bars(data, day):
                history.backfill(client, data, day, 1, now=now, settle=settle)
        labels = _sector_dates(data)
        for day in span:
            if (not os.path.exists(os.path.join(data, "universe_{}.json".format(day.isoformat())))
                    and _labelled(day, labels)):
                _record_universe(client, data, day, now, settle, warn)
    except NotAvailableYet as e:
        if now() >= quiet_until(session):
            raise Refused("Massive still refuses session {} at or after {} New York on the next trading day, when "
                          "the plan should serve it ({}); check the plan and the key".format(
                              session, QUIET_BEFORE.strftime("%H:%M"), e)) from None
        raise NotAvailableYet("session {} is not available yet on this plan ({}); the next firing asks again"
                              .format(session, e)) from None
    without = _without_universe(data, first, session, retracted)
    if session in without and session == latest and now() < quiet_until(session):
        # The latest session's labels can still be fetched until its next close: the file is not published
        # without its universe before the last morning firing (a month-end decision would be lost for good).
        raise NotAvailableYet("the sector labels of {} are not recorded yet, so neither is its universe; the file is "
                              "not written before the last morning firing, which asks again".format(session))
    if without:
        warn("no universe is recorded for {} session(s) without sector labels of both sources within the week up to "
             "them: {}; a decision on any of them is withheld (missing-universe)".format(len(without), _runs(without)))
    start = base + dt.timedelta(days=1)
    window = max(min(_actions_reach(data, start) + dt.timedelta(days=1), session - ACTION_OVERLAP), start)
    try:
        history.record_actions(client, window, session, data, now=now, settle=settle)
    except record.AlreadyRecorded:
        pass
    # After the actions are recorded, so one published late against a file
    # already delivered, or one another job recorded, stops the job here.
    actions = _reencode_changed(manifest, spans, data, cfg, base, identity, engine_log, ranker, reencode_all)
    excluded = {}
    # Under the retractions of sessions up to its last only, as every later check encodes it (the rule above).
    envelopes = encode.iter_encode(data, first, session, identity, required=list(cfg.required),
                                   optional=[t for t in log.holdings if t not in cfg.required], base=base,
                                   first_sequence=1, engine=MOMENTUM, excluded=excluded, ranker=ranker,
                                   retracted_through=session)
    os.makedirs(directory, exist_ok=True)
    if manifest is None:
        # Written before the run's first file, so a stop between the two leaves a file the next firing takes
        # in by the rule of _published (ADR 0018: each write is whole or not at all).
        manifest = {"manifest_version": MANIFEST_VERSION, "base": base.isoformat(), "files": [], "actions": {}}
        _write_manifest(directory, manifest)
    path = encode.write_stream(os.path.join(directory, "{}-{}.jsonl".format(instance, session)), envelopes)
    manifest["files"].append(_manifest_entry(path, first, session, without))
    manifest["actions"] = actions
    _write_manifest(directory, manifest)
    # After the stream is written, so every exclusion its encoding found is named.
    for ticker in sorted(excluded):
        for left in excluded[ticker]:
            print("ti-data: excluded {} from {} to {} ({} sessions): {}".format(
                ticker, left.first, left.last, left.sessions, left.reason), file=sys.stderr)
    return path


def parse(argv):
    ap = argparse.ArgumentParser(prog="ti-data", description=(
        "The daily data job: record SPY's settled session from Massive and write the Boost engine's stream "
        "to STREAMS/boost/<run>/boost-<session>.jsonl. Exit 1, with nothing written for the session, on any "
        "refusal."))
    ap.add_argument("--version", action="version", version="ti-data " + STAMP)
    ap.add_argument("--data", default=os.environ.get("TI_DATA_DIR"), help="the recordings folder (TI_DATA_DIR)")
    ap.add_argument("--streams", help="the streams folder (default DATA/streams)")
    ap.add_argument("--config", required=True, help="the Boost engine's configuration")
    ap.add_argument("--engine-log", required=True, help="the engine's durable log, <run>.log")
    ap.add_argument("--run-env", help="the engine's run.env: while it is missing or empty and the engine's folder holds no "
                                      "run log, no run has been started, and the job prints one waiting line and exits "
                                      "0, writing nothing; with a run log, a missing or unreadable run.env is refused")
    credentials = os.environ.get("CREDENTIALS_DIRECTORY")
    ap.add_argument("--secrets-file", default=os.path.join(credentials, "secrets") if credentials else None,
                    help="KEY=VALUE secrets holding MASSIVE_API_KEY (default $CREDENTIALS_DIRECTORY/secrets)")
    ap.add_argument("--settle-minutes", type=int, default=int(steps.SETTLE.total_seconds() // 60),
                    help="minutes after the close before a session may be recorded")
    ap.add_argument("--session", type=dt.date.fromisoformat, help="the session (default: the latest settled)")
    ap.add_argument("--backfill-from", type=dt.date.fromisoformat, metavar="DATE",
                    help="the one-time warm-up: record every session from DATE (the configuration's "
                         "warm_up_start; for a momentum engine, the run's base) and write the run's first stream")
    ap.add_argument("--engine", choices=(ENGINE, MOMENTUM), default=ENGINE,
                    help="the engine whose stream is written (default boost; momentum: an IRA's compact stream)")
    ap.add_argument("--instance", help="momentum: the engine's instance (trad-ira), its stream folder and file prefix")
    ap.add_argument("--ranker", help="momentum: cmd/universe-rank, run with -config CONFIG")
    ap.add_argument("--rebuild-manifest", action="store_true",
                    help="momentum, by hand: rebuild the run's lost or damaged manifest, taking in every file only if "
                         "it encodes again byte for byte")
    ap.add_argument("--reencode-all", action="store_true",
                    help="momentum, by hand: encode again every published file that changed split or dividend "
                         "recordings reach, however many (a morning firing checks at most {})".format(MAX_REENCODED))
    args = ap.parse_args(argv)
    if args.engine == ENGINE and (args.instance or args.ranker or args.reencode_all or args.rebuild_manifest):
        ap.error("--instance, --ranker, --reencode-all and --rebuild-manifest are for --engine momentum")
    if args.engine == MOMENTUM:
        if not args.instance or not INSTANCE.fullmatch(args.instance) or args.instance == ENGINE:
            ap.error("--engine momentum needs --instance, an IRA's name such as trad-ira")
        if not args.ranker:
            ap.error("--engine momentum needs --ranker, the universe-rank program")
    if args.settle_minutes < 0:
        ap.error("--settle-minutes must not be negative")
    if not args.data:
        ap.error("--data or TI_DATA_DIR is required")
    if not args.streams:
        args.streams = os.path.join(args.data, "streams")
    return args


MAX_RUN_ENV = 64 * 1024


class RunEnvRefused(Exception):
    """run.env is lost, unreadable, not a regular file or too large."""


def awaiting_first_run(path, engine_streams=None):
    """True only when no run has been started: run.env is missing or holds
    nothing, the engine's folder holds no run log (*.log) and engine_streams,
    this job's own output for the engine, holds no run stream folder; the firing
    then waits rather than failing. A run.env that is missing or empty while
    any of those exists was lost, and one that is not a regular file, is over
    64 KiB or cannot be read is bad: all of them raise, to be refused like any
    other fault (docs/manual/server.md, "The reporter before the first run":
    the wait is only for an account that has never run). run.env is absent only when lstat finds nothing at the path (a dangling
    link is present and refused). It is opened
    without blocking and its descriptor checked, so a FIFO swapped in after a
    path check cannot hold the job."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    except FileNotFoundError:
        # Absent means lstat finds nothing at the path: a link whose target is
        # missing is present, and cannot be read.
        try:
            os.lstat(path)
        except FileNotFoundError:
            empty = True
        except OSError as e:
            raise RunEnvRefused("{} cannot be read: {}".format(path, e))
        else:
            raise RunEnvRefused("{} is present but cannot be opened (a link to nothing?)".format(path))
    except OSError as e:
        raise RunEnvRefused("{} cannot be read: {}".format(path, e))
    else:
        try:
            st = os.fstat(fd)
            if not stat.S_ISREG(st.st_mode):
                raise RunEnvRefused("{} is not a regular file".format(path))
            if st.st_size > MAX_RUN_ENV:
                raise RunEnvRefused("{} is larger than {} bytes and is not read".format(path, MAX_RUN_ENV))
            try:
                raw = os.read(fd, MAX_RUN_ENV + 1)
                empty = not raw.decode("utf-8").strip()
            except (OSError, UnicodeDecodeError) as e:
                raise RunEnvRefused("{} cannot be read: {}".format(path, e))
            if len(raw) > MAX_RUN_ENV:
                raise RunEnvRefused("{} is larger than {} bytes and is not read".format(path, MAX_RUN_ENV))
        finally:
            os.close(fd)
    if not empty:
        return False
    folder = os.path.dirname(os.path.abspath(path))
    try:
        logs = [n for n in os.listdir(folder) if n.endswith(".log")]
    except FileNotFoundError:
        logs = []
    except OSError as e:
        raise RunEnvRefused("the settings file {} is missing or empty and its folder cannot be listed: {}".format(path, e))
    streams = []
    if engine_streams:
        try:
            for n in os.listdir(engine_streams):
                try:
                    if stat.S_ISDIR(os.stat(os.path.join(engine_streams, n)).st_mode):
                        streams.append(n)
                except FileNotFoundError:
                    continue   # the entry vanished between the listing and the stat: no longer there
        except FileNotFoundError:
            streams = []
        except OSError as e:
            raise RunEnvRefused("the settings file {} is missing or empty and {} cannot be listed or inspected: {}"
                                .format(path, engine_streams, e))
    if logs or streams:
        raise RunEnvRefused("the run's settings file is missing but a run exists: {} is missing or empty and {}"
                            .format(path, "the engine folder holds the run log " + sorted(logs)[0] if logs
                                    else "{} holds the run stream folder {}".format(engine_streams, sorted(streams)[0])))
    return True


def main(argv, now=steps._now, client_factory=massive.Client, ranker=None, sectors_get=sectors.http_get):
    args = parse(argv)
    print("ti-data " + STAMP, file=sys.stderr)
    key = None   # held here, so every message printed below can have it replaced
    momentum = args.engine == MOMENTUM
    try:
        if args.run_env and awaiting_first_run(args.run_env, os.path.join(args.streams,
                                                                          args.instance if momentum else ENGINE)):
            print("ti-data: waiting for the account's first run: {} is missing or empty (start the {} engine once)"
                  .format(args.run_env, args.instance if momentum else "Boost"), file=sys.stderr)
            return 0
        key = read_key(args.secrets_file)
        if momentum:
            path = run_momentum(args.data, args.streams, args.config, args.engine_log, key, now,
                                dt.timedelta(minutes=args.settle_minutes), args.instance,
                                ranker_bin=args.ranker, ranker=ranker, reencode_all=args.reencode_all,
                                rebuild_manifest=args.rebuild_manifest,
                                session=args.session, backfill_from=args.backfill_from,
                                client_factory=client_factory, sectors_get=sectors_get)
        else:
            path = run(args.data, args.streams, args.config, args.engine_log, key, now,
                       dt.timedelta(minutes=args.settle_minutes), session=args.session,
                       backfill_from=args.backfill_from, client_factory=client_factory)
    except (NotAvailableYet, Busy) as e:
        print("ti-data: " + (str(e).replace(key, KEY_MASK) if key else str(e)), file=sys.stderr)
        return 0
    except Exception as e:   # every refusal, including MissingData, Gap, NotSettled and NotDurable
        text = "{}: {}".format(type(e).__name__, e)
        if key:
            text = text.replace(key, KEY_MASK)
        print("ti-data refused: " + text, file=sys.stderr)
        return 1
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
