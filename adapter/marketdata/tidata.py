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
"""
import argparse
import datetime as dt
import glob
import hashlib
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
CONFIGURATION_TYPE = "strategy.target-weight.configuration"
# The owner's Run approval, which a fresh run after an earlier one records
# (cmd/engine; ADR 0027, the restart model).
RUN_APPROVED = "operator.run.approved"
# The engine's durable log header (internal/journal durable_log.go, ADR 0027),
# as adapter/ibkr's reader of the same log checks it.
LOG_VERSION = 1
LOG_CHAIN_ALGORITHM = ("seed=sha256(canonical_log_header_bytes); "
                       "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")
LOG_HEADER_FIELDS = ("chain_algorithm", "configuration_hash", "log_version", "strategy_version")
LOG_RECORD_FIELDS = ("envelope", "kind", "record_hash", "sequence")
LOG_COMMIT_FIELDS = ("record_hash", "sequence")
RECORD_HASH = re.compile(r"sha256:[0-9a-f]{64}")
# The envelope fields the chain's canonical bytes cover (internal/event
# CanonicalEnvelopeBytes), the two left out of the envelope when empty, and
# an RFC 3339 time's parts.
ENVELOPE_FIELDS = ("configuration_hash", "envelope_version", "event_time", "id", "payload_hash", "recorded_at",
                   "schema_version", "sequence", "source", "strategy_version", "type")
ENVELOPE_OPTIONAL = ("causation_id", "correlation_id")
TIME_PARTS = re.compile(r"([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]{1,9}))?"
                        r"(?:Z|([+-])([0-9]{2}):([0-9]{2}))")
# Stands in for the key in every message main prints.
KEY_MASK = "[" + KEY_NAME + "]"

try:   # written by deploy/build-data.sh into the packed file
    from ._build import STAMP
except ImportError:
    STAMP = "dev"

EngineLog = namedtuple("EngineLog",
                       "configuration_hash strategy_version run_id boost identity inputs record_hashes chain_break")
"""The engine's durable log as the job reads it: the header's identity, the
log's own run name (its file name without ``.log``), the Boost
configuration, the run's identity (see read_engine_log), the IDs of its
committed inputs in order, every committed record's hash in order, its
hash chain (a crash resume's begin with the crashed run's), and the
sequence of the first committed record whose stored hash is not the one
LOG_CHAIN_ALGORITHM gives over the records before it (_chain_next), or None
when every one's is."""


class Boost(namedtuple("Boost", "instrument warm_up_start trend_days exit_sma rsi_period")):
    """The Boost configuration's fields the stream depends on (internal/event
    BoostConfiguration)."""

    @property
    def min_sessions(self):
        """The history the indicators need: the longest of the trend average,
        the exit average and the RSI's period plus one."""
        return max(self.trend_days, self.exit_sma, self.rsi_period + 1)


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
    """The run's identity and configuration from the engine's durable log: a
    header line, then per input a group of records closed by a commit naming
    the group's last record and its hash; only committed groups count (the
    tail may be a group still being written). The first committed group's
    input must be the run's Boost configuration. A log not of that shape is
    refused, never read as empty.

    The run's identity is the ID of its Run approval, else its first
    committed input's ID and recorded time, as adapter/ibkr/relay.py's
    run_identity defines it: a crash resume's log holds a copy of the
    crashed run's records, its approval included (ADR 0027 section 6), so
    the resume keeps the identity, and a fresh run after an earlier one has
    its own approval."""
    run = os.path.basename(path)[:-len(".log")]
    if not path.endswith(".log") or not run:
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
    sequence, last_hash, group, configuration, first, approved, inputs = 0, None, [], None, None, None, []
    hashes = []
    chain, chain_break, pending_break = _chain_seed(header), None, None
    for n, line in enumerate(parsed[1:], 2):
        if not isinstance(line, dict) or len(line) != 1 or not set(line) <= {"record", "commit"}:
            raise _log_error(path, n, "a line is exactly one of a record and a commit")
        if "record" in line:
            rec = line["record"]
            if (not isinstance(rec, dict) or tuple(sorted(rec)) != LOG_RECORD_FIELDS
                    or not isinstance(rec["record_hash"], str) or not RECORD_HASH.fullmatch(rec["record_hash"])
                    or not isinstance(rec["envelope"], dict)):
                raise _log_error(path, n, "it is not a record as the engine writes one")
            if rec["sequence"] != sequence + 1:
                raise _log_error(path, n, "its sequence does not follow {}".format(sequence))
            if rec["kind"] != ("decision" if group else "input"):
                raise _log_error(path, n, "a group is one input followed by its decisions")
            if pending_break is None and chain_break is None:
                chain = _chain_next(chain, rec, lines[n - 1])
                if chain is None or "sha256:" + chain.hex() != rec["record_hash"]:
                    pending_break = rec["sequence"]
            sequence, last_hash = rec["sequence"], rec["record_hash"]
            group.append((n, rec["envelope"], last_hash))
            continue
        commit = line["commit"]
        if (not isinstance(commit, dict) or tuple(sorted(commit)) != LOG_COMMIT_FIELDS or not group
                or commit["sequence"] != sequence or commit["record_hash"] != last_hash):
            raise _log_error(path, n, "it is not the commit of the group before it")
        if configuration is None:
            configuration = _logged_configuration(path, header, *group[0][:2])
            first = group[0][1]
        if approved is None and group[0][1].get("type") == RUN_APPROVED:
            approved = group[0][1].get("id")
        inputs.append(group[0][1].get("id"))
        hashes.extend(h for _, _, h in group)
        group = []
        chain_break = chain_break if chain_break is not None else pending_break
    if configuration is None:
        raise Refused("the engine's durable log {} holds no committed configuration input: start the engine "
                      "first".format(path))
    identity = approved if approved else "{}@{}".format(first.get("id"), first.get("recorded_at"))
    return EngineLog(header["configuration_hash"], header["strategy_version"], run, configuration, identity,
                     tuple(inputs), tuple(hashes), chain_break)


def _chain_seed(header):
    """The seed of the log's record hash chain (LOG_CHAIN_ALGORITHM), as adapter/ibkr/relay.py's log_chain_seed."""
    return hashlib.sha256(_canonical_bytes({k: header[k] for k in LOG_HEADER_FIELDS})).digest()


def _chain_next(previous, rec, raw):
    """The record hash, raw SHA-256 bytes, of rec (its log line raw) after previous, else None; as relay's
    log_chain_next: sha256(previous || kind || internal/event CanonicalEnvelopeBytes), every envelope field
    covered, both times in UTC as RFC 3339 with nanoseconds, an absent correlation or causation ID as "", and
    the payload as its bytes exactly as stored (the writer puts it last in the envelope, the record hash last in
    the record). None when the record cannot be rendered so: it follows no chain."""
    envelope = rec["envelope"]
    try:
        marker = '"payload_hash":"{}","payload":'.format(envelope["payload_hash"]).encode()
        suffix = '}},"record_hash":"{}"}}}}'.format(rec["record_hash"]).encode()
        start = raw.find(marker)
        if start < 0 or not raw.endswith(suffix) or not isinstance(rec["kind"], str):
            return None
        return _record_digest(previous, rec["kind"], envelope,
                              raw[start + len(marker):len(raw) - len(suffix)].decode("utf-8"))
    except (KeyError, TypeError, ValueError, AttributeError, OverflowError):
        return None


def _record_digest(previous, kind, envelope, payload):
    """sha256(previous || kind || the canonical bytes of envelope with payload, its stored text): _chain_next's."""
    fields = {k: envelope[k] for k in ENVELOPE_FIELDS}
    fields.update((k, envelope.get(k, "")) for k in ENVELOPE_OPTIONAL)
    fields["event_time"], fields["recorded_at"] = _utc(envelope["event_time"]), _utc(envelope["recorded_at"])
    fields["payload"] = payload
    return hashlib.sha256(previous + kind.encode("utf-8") + _canonical_bytes(fields)).digest()


def _canonical_bytes(fields):
    """fields (text and whole-number values) as internal/event canonicalJSON renders them: keys sorted, compact,
    strings as Go's encoding/json writes them (UTF-8 kept; <, >, &, U+2028 and U+2029 escaped)."""
    parts = []
    for key in sorted(fields):
        value = fields[key]
        if isinstance(value, bool) or not isinstance(value, (int, str)):
            raise TypeError("a canonical field is text or a whole number")
        parts.append(_go_string(key) + ":" + (str(value) if isinstance(value, int) else _go_string(value)))
    return ("{" + ",".join(parts) + "}").encode("utf-8")


def _go_string(text):
    return (json.dumps(text, ensure_ascii=False).replace("<", "\\u003c").replace(">", "\\u003e")
            .replace("&", "\\u0026").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029"))


def _utc(text):
    """An RFC 3339 time as Go's Time.UTC().Format(time.RFC3339Nano) renders it: UTC, "Z", no trailing zeros."""
    year, month, day, hour, minute, second, fraction, sign, hours, minutes = TIME_PARTS.fullmatch(text).groups()
    at = dt.datetime(int(year), int(month), int(day), int(hour), int(minute), int(second), tzinfo=dt.timezone.utc)
    if sign:
        offset = dt.timedelta(hours=int(hours), minutes=int(minutes))
        at = at - offset if sign == "+" else at + offset
    fraction = (fraction or "").rstrip("0")
    return "{:04d}-{:02d}-{:02d}T{:02d}:{:02d}:{:02d}{}Z".format(at.year, at.month, at.day, at.hour, at.minute,
                                                              at.second, "." + fraction if fraction else "")


def head_identity(path):
    """The run identity the first records of the durable log at path name, read no further than its second commit.

    As adapter/ibkr/relay.py's head_identity: a run's identity is in its
    log's first two committed inputs (the configuration, and a fresh run's
    Run approval, which cmd/engine records second, before it listens), so
    this is read_engine_log's identity for every log the engine writes, read
    without the rest of the log. Only the first lines' shape is checked; a
    log the head names as the run's is then read strictly (read_engine_log).
    Refused when the first lines cannot be read as the engine writes them:
    they name no run."""
    inputs, group = [], None
    try:
        with open(path, "rb") as f:
            lines = iter(f)
            first = next(lines, b"")
            if not first.endswith(b"\n"):
                raise _log_error(path, 1, "it has no complete header line")
            header = _head_json(path, 1, first)
            if not isinstance(header, dict) or header.get("log_version") != LOG_VERSION:
                raise _log_error(path, 1, "it is not a durable log header of version {}".format(LOG_VERSION))
            for n, raw in enumerate(lines, 2):
                if len(inputs) == 2 or not raw.endswith(b"\n"):
                    break
                line = _head_json(path, n, raw)
                record = line.get("record") if isinstance(line, dict) and len(line) == 1 else None
                envelope = record.get("envelope") if isinstance(record, dict) else None
                if (isinstance(envelope, dict) and record.get("kind") == ("decision" if group else "input")
                        and all(isinstance(envelope.get(k), str) and envelope[k]
                                for k in ("id", "type", "recorded_at"))):
                    group = group or envelope
                elif isinstance(line, dict) and len(line) == 1 and "commit" in line and group:
                    inputs.append(group)
                    group = None
                else:
                    raise _log_error(path, n, "it is not a record or commit as the engine writes one")
    except OSError as e:
        raise Refused("cannot read the engine's durable log {}: {}".format(path, e.strerror)) from None
    if not inputs:
        raise Refused("the engine's durable log {} holds no committed input, so it names no run".format(path))
    approved = [e["id"] for e in inputs if e["type"] == RUN_APPROVED]
    return approved[0] if approved else "{}@{}".format(inputs[0]["id"], inputs[0]["recorded_at"])


def _head_json(path, n, raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except ValueError:
        raise _log_error(path, n, "it is not JSON") from None


def stream_run(engine_streams, engine_log, log, warn=None):
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
    and one whose head cannot be read cannot be matched to any run, so,
    when the run's folder is found, it is skipped and warn (by default a
    line on stderr) names its folder (an older build's log, or one damaged
    after its run ended). With no folder of the run found and a log
    skipped, the job is refused: that log may be the resumed run's own
    first log, its head damaged, and the run is never sent to a warm-up
    under its new name. A log whose head names the run is read whole and
    strictly, and refused if it cannot be. The record hashes compared are
    recomputed first (_chain_next), in both logs, and a log whose stored
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
    fresh run before its warm-up)."""
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
            chains[name] = read_engine_log(path)
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
    if not found and skipped:
        raise Refused(
            "no stream folder of the run of {} is found, and the logs of {} cannot be matched to any run (their "
            "first records cannot be read): one may be this run's own first log, damaged, so nothing is written "
            "and no warm-up is to be run for this run's name. If this run is a crash resume, restore its first "
            "run's log from the backup as the engine wrote it; if a log is an old run's, {}".format(
                engine_log, ", ".join(os.path.join(engine_streams, n) for n in skipped),
                _move_aside(engine_streams, logs)))
    if not found or found == [log.run_id]:
        return log.run_id
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


def _move_aside(engine_streams, logs):
    """The remedy for a log that cannot be matched to any run, as the adapter's (delivery.move_aside)."""
    return ("move the folder out of {} and its log out of the engine's log folder {}, keeping both as evidence in "
            "a folder outside both (docs/manual/market-data.md, \"A crash resume\")".format(engine_streams, logs))


def _logged_configuration(path, header, n, envelope):
    """The Boost configuration the run's first input carries (cmd/engine
    composeTargetWeight: id "configuration:<hash>", the header's identity)."""
    if (envelope.get("type") != CONFIGURATION_TYPE
            or envelope.get("id") != "configuration:" + header["configuration_hash"]
            or envelope.get("configuration_hash") != header["configuration_hash"]
            or envelope.get("strategy_version") != header["strategy_version"]):
        raise _log_error(path, n, "the run's first input is not its configuration")
    try:
        return _boost(envelope.get("payload"))
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


def _streams(directory):
    """[(last session, path)] of the run's streams, oldest first."""
    out = []
    for path in glob.glob(os.path.join(directory, "boost-*.jsonl")):
        m = STREAM.fullmatch(os.path.basename(path))
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
    client = client_factory(key)
    history.backfill(client, data, session, len(span), now=now, settle=settle)
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
    print("ti-data " + STAMP, file=sys.stderr)
    key = None   # held here, so every message printed below can have it replaced
    try:
        key = read_key(args.secrets_file)
        path = run(args.data, args.streams, args.config, args.engine_log, key, now,
                   dt.timedelta(minutes=args.settle_minutes), session=args.session,
                   backfill_from=args.backfill_from, client_factory=client_factory)
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
