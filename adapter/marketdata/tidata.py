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
   ``.log``, as the stream's correlation id. The run's streams live in
   ``STREAMS/boost/<run>/``;
3. records Massive's grouped daily bars (the encoder's bar source, one
   request a session) for every session after the run's last stream, and the
   bulk splits and dividends for a window reaching back over the last
   ``ACTION_OVERLAP`` days (history.record_actions), so an action published
   a few days late is still recorded and the windows stay contiguous;
4. encodes the Boost stream (encode.encode with engine "boost": its one
   instrument's bars, then each session's close, so the session closes
   before its decision, ADR 0021) for the sessions after the previous stream
   through the session, adjusted forward from the configuration's
   ``warm_up_start`` (the first session whose close feeds the indicators),
   to ``boost-<last session>.jsonl``. Sequences continue the previous
   stream's; the first stream starts at the log's next sequence.

A run's first stream is the warm-up (``--backfill-from`` the configured
``warm_up_start``): it records every session from the warm-up start, checks
the history has no gap and is at least as long as the longest indicator
window (trend_days, exit_sma, rsi_period + 1), and writes the whole history
as one stream. A daily run before it is refused.

Fail closed (AGENTS.md rule 5): a missing key, an unsettled session, an
unreadable engine log, a missing recording, an empty answer on a trading
day, a gap the encoder finds, or a history too short is refused with exit
status 1 and a message, and no stream is written (encode.write_stream
installs the file whole or not at all). Recordings already made are kept:
they are immutable inputs (record.py) and the next run reuses them. The key
and an unsettled session are checked before anything is written.

Idempotent: when a stream already holds the session, the run fetches
nothing, encodes that stream again from the recordings with the same first
sequence, and refuses unless the bytes are identical.
"""
import argparse
import datetime as dt
import glob
import json
import os
import re
import sys
from collections import namedtuple

from . import encode, history, massive, nyse_calendar, record, steps

ENGINE = "boost"
STREAM = re.compile(r"boost-(\d{4}-\d{2}-\d{2})\.jsonl")
# How far back each run's action window reaches: an action Massive publishes
# up to this late is still recorded before a later session needs it.
ACTION_OVERLAP = dt.timedelta(days=14)
KEY_NAME = "MASSIVE_API_KEY"
# The engine's durable log header (internal/journal durable_log.go, ADR 0027),
# as adapter/ibkr's reader of the same log checks it.
LOG_VERSION = 1
LOG_CHAIN_ALGORITHM = ("seed=sha256(canonical_log_header_bytes); "
                       "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")
LOG_HEADER_FIELDS = ("chain_algorithm", "configuration_hash", "log_version", "strategy_version")

EngineLog = namedtuple("EngineLog", "configuration_hash strategy_version run_id next_sequence")
Boost = namedtuple("Boost", "instrument warm_up_start min_sessions")


class Refused(Exception):
    """The job refused to run; nothing was written for this session."""


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


def read_engine_log(path):
    """The run's identity and next input sequence from the engine's durable
    log: a header line, then per input a group of records closed by a
    commit; only committed groups count (the tail may be a group still being
    written). A log not of that shape is refused, never read as empty."""
    if not path.endswith(".log"):
        raise Refused("{} is not an engine durable log (<run>.log)".format(path))
    try:
        with open(path, "rb") as f:
            data = f.read()
    except OSError as e:
        raise Refused("cannot read the engine's durable log {}: {}".format(path, e.strerror)) from None
    lines = data.split(b"\n")[:-1]
    if not lines:
        raise _log_error(path, 1, "it has no complete header line")
    parsed = []
    for n, raw in enumerate(lines, 1):
        try:
            parsed.append(json.loads(raw.decode("utf-8")))
        except ValueError:
            raise _log_error(path, n, "it is not JSON") from None
    header = parsed[0]
    if (not isinstance(header, dict) or tuple(sorted(header)) != LOG_HEADER_FIELDS
            or header["log_version"] != LOG_VERSION or header["chain_algorithm"] != LOG_CHAIN_ALGORITHM
            or not all(isinstance(header[k], str) and header[k] for k in ("configuration_hash", "strategy_version"))):
        raise _log_error(path, 1, "it is not a durable log header of version {}".format(LOG_VERSION))
    sequence = committed = 0
    open_group = False
    for n, line in enumerate(parsed[1:], 2):
        if not isinstance(line, dict) or len(line) != 1:
            raise _log_error(path, n, "a line is exactly one of a record and a commit")
        if "record" in line:
            rec = line["record"]
            if not isinstance(rec, dict) or rec.get("sequence") != sequence + 1:
                raise _log_error(path, n, "its sequence does not follow {}".format(sequence))
            if rec.get("kind") != ("decision" if open_group else "input"):
                raise _log_error(path, n, "a group is one input followed by its decisions")
            sequence, open_group = rec["sequence"], True
        elif "commit" in line:
            if not open_group or not isinstance(line["commit"], dict) or line["commit"].get("sequence") != sequence:
                raise _log_error(path, n, "it is not the commit of the group before it")
            committed, open_group = sequence, False
        else:
            raise _log_error(path, n, "a line is exactly one of a record and a commit")
    inputs = [line["record"]["envelope"].get("sequence") for line in parsed[1:]
              if "record" in line and line["record"]["kind"] == "input" and line["record"]["sequence"] <= committed]
    last = max((s for s in inputs if isinstance(s, int)), default=0)
    run = os.path.basename(path)[:-len(".log")]
    return EngineLog(header["configuration_hash"], header["strategy_version"], run, last + 1)


def read_config(path):
    """The Boost configuration's instrument, warm-up start and the history its
    indicators need (internal/event BoostConfiguration)."""
    try:
        with open(path) as f:
            cfg = json.load(f)
        boost = cfg["boost"]
        start = dt.date.fromisoformat(boost["warm_up_start"])
        need = max(int(boost["trend_days"]), int(boost["exit_sma"]), int(boost["rsi_period"]) + 1)
        instrument = boost["instrument_id"]
    except (OSError, ValueError, KeyError, TypeError) as e:
        raise Refused("cannot read the Boost configuration {}: {}".format(path, e)) from None
    if cfg.get("strategy_id") != ENGINE or not isinstance(instrument, str) or not instrument:
        raise Refused("{} is not a Boost configuration".format(path))
    if not nyse_calendar.is_trading_day(start):
        raise Refused("warm_up_start {} in {} is not an NYSE session".format(start, path))
    return Boost(instrument, start, need)


def _streams(directory):
    """[(last session, path)] of the run's streams, oldest first."""
    out = []
    for path in glob.glob(os.path.join(directory, "boost-*.jsonl")):
        m = STREAM.fullmatch(os.path.basename(path))
        if m:
            out.append((dt.date.fromisoformat(m.group(1)), path))
    return sorted(out)


def _first_sequence(path):
    with open(path) as f:
        return json.loads(f.readline())["sequence"]


def _last_sequence(path):
    with open(path) as f:
        return json.loads(f.read().splitlines()[-1])["sequence"]


def _bytes(envelopes):
    return "".join(json.dumps(e, separators=(",", ":"), allow_nan=False) + "\n" for e in envelopes).encode()


def _encode(data, first, last, cfg, identity, first_sequence):
    return encode.encode(data, first, last, identity, required=[cfg.instrument], base=cfg.warm_up_start,
                         engine=ENGINE, first_sequence=first_sequence)


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


def run(data, streams, config, engine_log, secrets_file, now, settle, session=None, backfill_from=None,
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
    identity = encode.Identity(log.configuration_hash, log.strategy_version, log.run_id)
    directory = os.path.join(streams, ENGINE, log.run_id)
    existing = _streams(directory)
    previous = None
    for last, path in existing:
        if session <= last:
            first = cfg.warm_up_start if previous is None else nyse_calendar.sessions_after(previous, 1)[0]
            again = _bytes(_encode(data, first, last, cfg, identity, _first_sequence(path)))
            with open(path, "rb") as f:
                if f.read() != again:
                    raise Refused("{} differs from the stream its recordings and the engine log {} give; it is "
                                  "not rewritten: find out why before the adapter reads it".format(path, engine_log))
            return path
        previous = last
    if previous is None and backfill_from is None:
        raise Refused("run {} has no stream yet: run the warm-up once first, ti-data --backfill-from {}".format(
            log.run_id, cfg.warm_up_start))
    first = cfg.warm_up_start if previous is None else nyse_calendar.sessions_after(previous, 1)[0]
    span = nyse_calendar.sessions(first, session)
    if previous is None and len(span) < cfg.min_sessions:
        raise Refused("the history {}..{} holds {} sessions; the indicators need {}".format(
            first, session, len(span), cfg.min_sessions))
    client = client_factory(read_key(secrets_file))
    history.backfill(client, data, session, len(span), now=now, settle=settle)
    start = cfg.warm_up_start + dt.timedelta(days=1)
    window = min(_actions_reach(data, start) + dt.timedelta(days=1), session - ACTION_OVERLAP)
    try:
        history.record_actions(client, max(window, start), session, data, now=now, settle=settle)
    except record.AlreadyRecorded:
        pass
    sequence = log.next_sequence if previous is None else _last_sequence(existing[-1][1]) + 1
    envelopes = _encode(data, first, session, cfg, identity, sequence)
    os.makedirs(directory, exist_ok=True)
    return encode.write_stream(os.path.join(directory, "boost-{}.jsonl".format(session)), envelopes)


def parse(argv):
    ap = argparse.ArgumentParser(prog="ti-data", description=(
        "The daily data job: record SPY's settled session from Massive and write the Boost engine's stream "
        "to STREAMS/boost/<run>/boost-<session>.jsonl. Exit 1, with nothing written for the session, on any "
        "refusal."))
    ap.add_argument("--data", default=os.environ.get("TI_DATA_DIR"), help="the recordings folder (TI_DATA_DIR)")
    ap.add_argument("--streams", help="the streams folder (default DATA/streams)")
    ap.add_argument("--config", required=True, help="the Boost engine's configuration")
    ap.add_argument("--engine-log", required=True, help="the engine's durable log, <run>.log")
    credentials = os.environ.get("CREDENTIALS_DIRECTORY")
    ap.add_argument("--secrets-file", default=os.path.join(credentials, "secrets") if credentials else None,
                    help="KEY=VALUE secrets holding MASSIVE_API_KEY (default $CREDENTIALS_DIRECTORY/secrets)")
    ap.add_argument("--settle-minutes", type=int, default=int(steps.SETTLE.total_seconds() // 60),
                    help="minutes after the close before a session may be recorded")
    ap.add_argument("--session", type=dt.date.fromisoformat, help="the session (default: the latest settled)")
    ap.add_argument("--backfill-from", type=dt.date.fromisoformat, metavar="DATE",
                    help="the one-time warm-up: record every session from DATE (the configuration's "
                         "warm_up_start) and write the run's first stream")
    args = ap.parse_args(argv)
    if args.settle_minutes < 0:
        ap.error("--settle-minutes must not be negative")
    if not args.data:
        ap.error("--data or TI_DATA_DIR is required")
    if not args.streams:
        args.streams = os.path.join(args.data, "streams")
    return args


def main(argv, now=steps._now, client_factory=massive.Client):
    args = parse(argv)
    try:
        path = run(args.data, args.streams, args.config, args.engine_log, args.secrets_file, now,
                   dt.timedelta(minutes=args.settle_minutes), session=args.session,
                   backfill_from=args.backfill_from, client_factory=client_factory)
    except Exception as e:   # every refusal, including MissingData, Gap, NotSettled and NotDurable
        print("ti-data refused: {}: {}".format(type(e).__name__, e), file=sys.stderr)
        return 1
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
