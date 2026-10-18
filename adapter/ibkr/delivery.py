"""The run's market-data stream, delivered to the engine through the adapter's outbox, after the broker snapshot.

The data job (adapter/marketdata/tidata.py, its file contract) writes each
run's stream to STREAMS/<run>/<engine>-<last session>.jsonl, where <run> is
the engine's durable log's name without ".log", found by the run's identity
(stream_run): a crash resume, under a new log name with the crashed run's
identity (ADR 0027, the restart model), keeps the folder, and the name, of
the run that began it, so its batch left in the outbox is sent on to it and
any of it the copied log already commits is struck off unsent, as every
committed outbox entry is; a fresh run has its own: one envelope per line, in
delivery order, each file's ``sequence`` a file-local ordinal 1..N, and each
file covering the sessions since the one before it. Only the deliverer
knows the engine's sequence, so it sets each envelope's sequence as it sends
it (loop.AdapterLoop._envelope), stamps its recorded_at with the send time,
and sends every other field as written.

While the link is up and reconciled, the adapter (Deliverer.deliver) picks
up the files of its run it has not delivered, oldest session first, reads
each strictly (read_stream), takes one broker snapshot (the account read,
snapshot.read_account, with IBKR's mark for each holding, snapshot.
read_marks, and the market's state at its time, market_open), and records
the snapshot and then every envelope of those files in the outbox in one
write (relay.Outbox.extend), so the engine journals the snapshot before the
bars, and each session's bars before its close: ADR 0019's snapshot rule
(the latest snapshot at or before the trigger, by journal sequence) and ADR
0021 (a session closes before its entries are decided). The outbox then
sends each once, in order (loop.AdapterLoop._drain), every input stamped
with its send time as its recorded time, so the snapshot is never recorded
after an input it serves. The snapshot's entry names the run it was read
for, as every stream entry's envelope does, so an earlier run's batch is
never sent to a fresh run.

Exactly once: before the outbox write, the delivery record (DELIVERED_SUFFIX
beside the pending file, read strictly) names the files, their SHA-256 and
the snapshot's input ID, marked queuing; after it, queued. A stop between
the two leaves it queuing, and the next start looks for the snapshot's ID in
the outbox and among the inputs the engine committed: found, the write was
made (it is one write) and the batch is marked queued; not found, nothing
of it was recorded or sent, and it is queued again from a fresh snapshot
under the same ID. A file a queued batch names is never queued again, and
the outbox's own rules send nothing twice across a stop (an entry the
engine committed is struck off unsent; one awaiting its reply is asked for
again under its ID, ADR 0027). A new batch waits until no stream entry is
left in the outbox, so a partly sent file resumes where it stopped.

A batch not delivered to its run is marked dropped: one a stop left queuing
before its outbox write when a fresh run then started (nothing of it was
recorded or sent), and one whose entries a fresh run struck off the outbox
unsent (loop.AdapterLoop._drain, through dropped). Its files are not
delivered: if that run is resumed (a crash resumes the same run, ADR 0027,
under its identity, stream_run),
they are queued for it again behind a fresh snapshot, and any of their
inputs its engine already committed is struck off unsent, as every
committed outbox entry is.

Fail closed (AGENTS.md): every file of a batch is read and checked before
anything of the batch is written. A file that is not as the data job writes
it (JSON shape, envelope fields, a type, schema version or source the data
job does not write, kinds.py, a payload that does not match its hash, an
input ID twice, the file-local sequence not 1..N, or a last line that is not
the close of the session its name gives), one whose run identity
(configuration hash, strategy version, run name) is not the engine's run
now running, one whose first session is not after the last session already
delivered or of the file before it in the batch, and one naming an input
another file of the batch or the outbox names, is refused (StreamRefused),
except a session retraction a later file repeats with the same type,
schema version and payload, as the data job writes each one its calendar
covers into every file: that one is recorded once. Any of these refusals:
nothing of that batch is recorded or sent, and the adapter stops for a
person, naming the file and how to settle it. A run whose stream folder
does not exist yet stops the start: its warm-up has not been written; so do
two folders of one run (stream_run).
"""
import hashlib
import importlib.util
import json
import logging
import os
import re
from collections import namedtuple
from datetime import date, datetime, time as clock_time, timedelta, timezone

from relay import (ENVELOPE_VERSION, LOG_INPUT, PAYLOAD_HASH, TIME, ResolveRefused, _committed_groups, _is_count, _is_text,
                   _is_time, head_identity, read_engine_log, run_identity)
from snapshot import BROKER_SNAPSHOT_TYPE

log = logging.getLogger("ibkr")

# The delivery record, beside the pending file.
DELIVERED_SUFFIX = ".delivered"
# A stream file's name: <engine>-<last session>.jsonl (the data job writes
# boost-YYYY-MM-DD.jsonl); every other name in the folder (the data job's
# temporary *.part file, say) is left alone.
STREAM_FILE = re.compile(r"[a-z][a-z0-9-]*-([0-9]{4}-[0-9]{2}-[0-9]{2})\.jsonl")
# The fields of one envelope as the data job writes it (encode._Stream.add).
STREAM_FIELDS = ("configuration_hash", "correlation_id", "envelope_version", "event_time", "id", "payload",
                 "payload_hash", "recorded_at", "schema_version", "sequence", "source", "strategy_version", "type")
BATCH_FIELDS = ("files", "run", "snapshot", "state")
FILE_FIELDS = ("lines", "name", "sha256")
QUEUING, QUEUED, DROPPED = "queuing", "queued", "dropped"
SHA256 = re.compile(r"[0-9a-f]{64}")
# The regular session opens at 9:30 New York on every NYSE trading day; it
# closes at the calendar's close (4 p.m., 1 p.m. on an early close).
SESSION_OPEN = clock_time(9, 30)
# How a person settles a refused file (docs/manual/market-data.md, "What
# happens to the file").
SETTLE = ("with the adapter stopped, move the file out of the run's stream folder (keep it: it is evidence, never "
          "edit it), run the data job again (sudo systemctl start ti-data.service), and start the adapter again "
          "(sudo systemctl start ti-adapter-boost) (docs/manual/market-data.md, \"What happens to the file\")")


def _marketdata(name):
    """The data job's module adapter/marketdata/<name>.py, loaded from its file.

    The adapter is installed beside it in the same layout (provision.sh
    adapter), so both read one NYSE calendar (nyse_calendar.py) and one
    table of the inputs the data job writes (kinds.py).
    """
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir, "marketdata", name + ".py")
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


nyse_calendar = _marketdata("nyse_calendar")
kinds = _marketdata("kinds")


class StreamRefused(Exception):
    """A stream file, or the run's stream folder, is not one the adapter delivers; it stops for a person."""


class SnapshotRefused(StreamRefused):
    """The account could not be read in full for the broker snapshot within its attempts; nothing is delivered."""


class DeliveryRecordInvalid(ValueError):
    """The delivery record is not as the adapter writes it; the adapter stops before delivering anything."""


Stream = namedtuple("Stream", "name session first sha256 envelopes")
"""One stream file read strictly: its file name, its last session (which its name gives and its last line
closes), the first session it closes, the SHA-256 of its bytes and its envelopes."""


def market_open(at):
    """Whether the NYSE regular session was open at the instant at (an aware datetime): the broker snapshot's statement."""
    ny = at.astimezone(nyse_calendar.NEW_YORK)
    day = ny.date()
    if not nyse_calendar.is_trading_day(day):
        return False
    opens = datetime.combine(day, SESSION_OPEN, nyse_calendar.NEW_YORK)
    return opens <= ny < nyse_calendar.session_close(day)


def _refused(path, why):
    return StreamRefused("the stream file {} is refused: {}. Nothing from it, or from any file after it, is sent; {}"
                         .format(path, why, SETTLE))


def read_stream(path, run, configuration_hash, strategy_version):
    """The stream file at path as a Stream, read strictly against the run now running; StreamRefused otherwise."""
    name = os.path.basename(path)
    matched = STREAM_FILE.fullmatch(name)
    try:
        session = date.fromisoformat(matched.group(1)) if matched else None
    except ValueError:
        session = None
    if session is None:
        raise _refused(path, "its name is not <engine>-<session>.jsonl")
    try:
        with open(path, "rb") as f:
            data = f.read()
    except OSError as err:
        raise _refused(path, "it cannot be read ({})".format(err.strerror or err)) from None
    if not data:
        raise _refused(path, "it holds no envelopes")
    if not data.endswith(b"\n"):
        raise _refused(path, "its last line is not complete")
    envelopes, seen = [], set()
    for n, raw in enumerate(data.split(b"\n")[:-1], 1):
        try:
            envelope = json.loads(raw.decode("utf-8"))
        except ValueError:
            raise _refused(path, "line {} is not JSON".format(n)) from None
        why = _envelope_fault(envelope, n)
        if why:
            raise _refused(path, "line {} {}".format(n, why))
        if envelope["id"] in seen:
            raise _refused(path, "line {} names input {} a second time".format(n, envelope["id"]))
        seen.add(envelope["id"])
        for field, want in (("correlation_id", run), ("configuration_hash", configuration_hash),
                            ("strategy_version", strategy_version)):
            if envelope[field] != want:
                raise _refused(path, "it is not this run's: line {} carries {} {!r}, and the engine now runs {!r} "
                                     "(a file the data job wrote for another run or another configuration)".format(
                                         n, field, envelope[field], want))
        envelopes.append(envelope)
    closed = []
    for n, envelope in enumerate(envelopes, 1):
        if (envelope["type"], envelope["schema_version"]) == kinds.SESSION_CLOSED:
            day = _closed_session(envelope["payload"])
            if day is None:
                raise _refused(path, "line {} closes a session with no period_end that can be read".format(n))
            closed.append(day)
    if (envelopes[-1]["type"], envelopes[-1]["schema_version"]) != kinds.SESSION_CLOSED:
        raise _refused(path, "its last line is not a session's close; the data job ends every file with the "
                             "close of its last session")
    if closed[-1] != session:
        raise _refused(path, "its name gives session {}, but its last line closes session {}".format(
            session, closed[-1]))
    return Stream(name, session, closed[0], hashlib.sha256(data).hexdigest(), envelopes)


def _closed_session(payload):
    """The New York date of a session close's period_end, or None if it is not a time with its zone."""
    text = payload.get("period_end")
    if not _is_time(text, TIME):
        return None
    zone = TIME.fullmatch(text).group(2)
    offset = timedelta(0) if zone == "Z" else (1 if zone[0] == "+" else -1) * timedelta(
        hours=int(zone[1:3]), minutes=int(zone[4:6]))
    at = datetime.strptime(text[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc) - offset
    return at.astimezone(nyse_calendar.NEW_YORK).date()


def _envelope_fault(envelope, n):
    """Why line n's envelope is not one the data job writes, or "" when it is."""
    if not isinstance(envelope, dict):
        return "is not a JSON object"
    if tuple(sorted(envelope)) != STREAM_FIELDS:
        return "does not carry exactly the fields {}".format(", ".join(STREAM_FIELDS))
    if not all(_is_text(envelope[k]) for k in ("id", "type", "source", "correlation_id", "strategy_version",
                                                "configuration_hash")):
        return "has an empty or non-text identifying field"
    if envelope["envelope_version"] != ENVELOPE_VERSION or not _is_count(envelope["envelope_version"]):
        return "is not envelope version {}".format(ENVELOPE_VERSION)
    if not _is_count(envelope["schema_version"]):
        return "has no schema version"
    if (envelope["type"], envelope["schema_version"]) not in kinds.KINDS:
        return "is {} version {}, which the data job does not write".format(envelope["type"],
                                                                            envelope["schema_version"])
    if envelope["source"] != kinds.SOURCE:
        return "names source {!r}; the data job writes {!r}".format(envelope["source"], kinds.SOURCE)
    if not all(_is_time(envelope[k], TIME) for k in ("event_time", "recorded_at")):
        return "has an event or recorded time that is not an RFC 3339 time with its zone"
    if envelope["sequence"] != n or not _is_count(envelope["sequence"]):
        return "has sequence {!r}; a file's envelopes are numbered 1, 2, ... in order".format(envelope["sequence"])
    if not isinstance(envelope["payload"], dict):
        return "has a payload that is not a JSON object"
    try:
        raw = json.dumps(envelope["payload"], separators=(",", ":"), allow_nan=False).encode("utf-8")
    except ValueError:
        return "has a payload holding a number JSON cannot carry (NaN or infinity)"
    if (not isinstance(envelope["payload_hash"], str) or not PAYLOAD_HASH.fullmatch(envelope["payload_hash"])
            or hashlib.sha256(raw).hexdigest() != envelope["payload_hash"]):
        return "has a payload that does not match its payload_hash"
    return ""


class DeliveryRecord:
    """The batches of stream files the adapter has recorded in its outbox, oldest first, in a JSON file.

    {"batches": [{"run", "snapshot", "files": [{"name", "sha256", "lines"}],
    "state"}]}: the run the files were written for, the input ID of the
    broker snapshot recorded before them, the files in delivery order, and
    QUEUING until the outbox write is made, then QUEUED; DROPPED for a batch
    not delivered to its run (see the module): its files may be named again
    by a later batch of that run. Read strictly: anything else raises
    DeliveryRecordInvalid. Written whole, atomically and synced, only by the
    adapter holding the pending file's lock.
    """

    def __init__(self, path):
        self.path = path

    def load(self):
        try:
            with open(self.path, encoding="utf-8") as f:
                record = json.load(f)
        except FileNotFoundError:
            return []
        except ValueError as err:
            raise self._invalid("it is not JSON ({})".format(err)) from None
        if not isinstance(record, dict) or tuple(sorted(record)) != ("batches",) or not isinstance(
                record["batches"], list):
            raise self._invalid("it is not an object holding only its batches")
        named = set()
        for n, batch in enumerate(record["batches"], 1):
            if (not isinstance(batch, dict) or tuple(sorted(batch)) != BATCH_FIELDS or not _is_text(batch["run"])
                    or not _is_text(batch["snapshot"]) or batch["state"] not in (QUEUING, QUEUED, DROPPED)
                    or not isinstance(batch["files"], list) or not batch["files"]):
                raise self._invalid("batch {} is not a batch as the adapter records one".format(n))
            if batch["state"] == QUEUING and n != len(record["batches"]):
                raise self._invalid("batch {} is still queuing, but a later batch follows it".format(n))
            for entry in batch["files"]:
                if (not isinstance(entry, dict) or tuple(sorted(entry)) != FILE_FIELDS
                        or not isinstance(entry["name"], str) or not STREAM_FILE.fullmatch(entry["name"])
                        or not isinstance(entry["sha256"], str) or not SHA256.fullmatch(entry["sha256"])
                        or not _is_count(entry["lines"])):
                    raise self._invalid("batch {} names a file not as the adapter records one".format(n))
                key = (batch["run"], entry["name"])
                if batch["state"] == DROPPED:
                    continue
                if key in named:
                    raise self._invalid("it names {} of run {} twice".format(entry["name"], batch["run"]))
                named.add(key)
        return record["batches"]

    def begin(self, batch):
        """Record batch, about to be written to the outbox, as QUEUING."""
        self._write(self.load() + [dict(batch, state=QUEUING)])

    def mark_queued(self):
        """Mark the last batch QUEUED: its outbox write was made."""
        self._mark(QUEUED)

    def mark_dropped(self):
        """Mark the last batch DROPPED: an earlier run's, never written to the outbox."""
        self._mark(DROPPED)

    def drop_queued(self, run):
        """Mark run's last batch DROPPED if it is QUEUED: a fresh run struck its entries off the outbox unsent."""
        batches = self.load()
        last = max((n for n, b in enumerate(batches) if b["run"] == run), default=None)
        if last is None or batches[last]["state"] != QUEUED:
            return False
        batches[last] = dict(batches[last], state=DROPPED)
        self._write(batches)
        return True

    def _mark(self, state):
        batches = self.load()
        batches[-1] = dict(batches[-1], state=state)
        self._write(batches)

    def _invalid(self, why):
        return DeliveryRecordInvalid(
            "the delivery record {} cannot be read: {}. Nothing is delivered: never edit it; restore it from the "
            "backup as it was (docs/manual/market-data.md, \"What happens to the file\")".format(self.path, why))

    def _write(self, batches):
        tmp = self.path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump({"batches": batches}, f, separators=(",", ":"), allow_nan=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)
        fd = os.open(os.path.dirname(os.path.abspath(self.path)), os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


def run_name(engine_log_path):
    """The log's run name: its durable log's file name without ".log" (TI_RUN)."""
    name = os.path.basename(engine_log_path)
    return name[:-len(".log")] if name.endswith(".log") else name


def stream_run(streams, engine_log_path):
    """The name of the stream folder in streams of the run whose durable log is engine_log_path, found by its identity.

    The data job applies the same rule (adapter/marketdata/tidata.py,
    stream_run). A folder is a run's when the log beside engine_log_path
    named after it (<folder>.log; the run's own log for the folder of its
    own name) has the run's identity (relay.run_identity: its Run approval's
    ID, else its configuration and as-of time). A fresh run's folder bears
    its own name, made by its warm-up. A crash resume runs under a new name
    (a new TI_RUN) and keeps the folder of the run that began it: its log
    begins with a copy of the crashed run's records, its approval included
    (ADR 0027 section 6), so the folder, and every stream envelope's
    correlation id, is the first run's name of a chain of resumes; an
    abandoned resume has no folder and is never taken for it.

    Only the logs of folders are read, and each first by its head
    (relay.head_identity: its first two committed inputs, where a run's
    identity is): a log whose head names another run is read no further,
    and one whose head cannot be read cannot be matched to any run: a
    warning names its folder (an older build's log, or one damaged after its
    run ended) and the start is refused, as below. A log whose head names this run is read whole and strictly. Fail closed
    (StreamRefused, AGENTS.md):

    - this run's log, or a log whose head names this run, cannot be read;
    - a log was skipped, a folder of the run's own name or none: it may be
      this resumed run's own first log, its head damaged, and the run is
      never sent to a warm-up under its new name, nor handed that folder;
    - two folders of the run, its own name's among them: one run's stream
      is never split across two folders;
    - one folder of another name whose log is not the head of this log's
      record hash chain (ADR 0027: engine -resume writes the crashed run's
      records into the new log record for record under the same header, so
      a resume's log begins with that log's committed records, each with
      the same record_hash, in the same order; any other record is not that
      run's), or holds its configuration alone (a second start of a first
      run from the same run.env, with no approval, repeats its
      configuration record hash for hash, so the chain cannot tell them
      apart, and nothing of such a run is kept; ADR 0027: a run that is not
      a crash's resume is a fresh run, with its approval). The record
      hashes compared are recomputed first (relay.log_chain_next,
      LOG_CHAIN_ALGORITHM), in both logs: a stored hash vouches for its
      record only when it is the one the chain gives over it, so a log
      edited after the engine wrote it is refused. The log is read once the
      engine listens, so a resume's copy is then whole.

    With no folder of the run, a log holding no history beyond its
    configuration, approval and operator inputs is a fresh run's before its
    warm-up, and its own name is returned (check then refuses: its warm-up is
    not written); a log already holding committed market inputs holds copied
    history, so it is a crash resume whose first run's log is missing, and it
    is refused with that case named, never sent to a warm-up
    (_own_or_resume).
    """
    own = run_name(engine_log_path)
    logs = os.path.dirname(os.path.abspath(engine_log_path))
    candidates = [n for n in (sorted(os.listdir(streams)) if os.path.isdir(streams) else [])
                  if os.path.isdir(os.path.join(streams, n)) and os.path.isfile(os.path.join(logs, n + ".log"))]
    if not candidates:
        return _own_or_resume(streams, logs, engine_log_path, own)
    try:
        mine = _committed_groups(engine_log_path)
        identity = run_identity(engine_log_path)
    except ResolveRefused as err:
        raise StreamRefused("the run's stream folder is found by the run's identity, from its durable log, and "
                            "that log cannot be read: {}".format(err)) from None
    found, chains, skipped = [], {}, []
    for name in candidates:
        if name == own:
            found.append(name)
            continue
        path = os.path.join(logs, name + ".log")
        try:
            head = head_identity(path)
        except ResolveRefused as err:
            skipped.append(name)
            log.warning("ibkr stream folder %s skipped: its log's first records cannot be read, so it cannot be "
                        "matched to any run (%s). Once its run has ended, %s", os.path.join(streams, name), err,
                        move_aside(streams, logs))
            continue
        if head != identity:
            continue
        try:
            chains[name] = _committed_groups(path)
            whole = run_identity(path)
        except ResolveRefused as err:
            raise StreamRefused(
                "the log {} of the stream folder {} names this run in its first records but cannot be read: {}. "
                "Nothing is delivered: restore that log as the engine wrote it (docs/manual/market-data.md, "
                "\"A crash resume\")".format(path, os.path.join(streams, name), err)) from None
        if whole == identity:
            found.append(name)
    if len(found) > 1:
        raise StreamRefused(
            "the run of {} has more than one stream folder: {}. One run's stream is in one folder, made by "
            "its warm-up; nothing is delivered until a person decides which is the run's "
            "(docs/manual/market-data.md, \"A crash resume\")".format(
                engine_log_path, ", ".join(os.path.join(streams, n) for n in found)))
    if skipped:
        raise StreamRefused(
            "nothing is delivered for the run of {}: the logs of {} cannot be matched to any run (their first records cannot "
            "be read): one may be this run's own first log, damaged, so no stream folder is trusted "
            "and no warm-up is to be run for this run's name. If this run is a crash resume, restore its first "
            "run's log from the backup as the engine wrote it; if a log is an old run's, once its run has ended, {}".format(
                engine_log_path, ", ".join(os.path.join(streams, n) for n in skipped), move_aside(streams, logs)))
    if found == [own]:
        return own
    if not found:
        return _own_or_resume(streams, logs, engine_log_path, own)
    earlier = chains[found[0]]
    broken = [(path, chain.chain_break) for path, chain in ((os.path.join(logs, found[0] + ".log"), earlier),
                                                            (engine_log_path, mine))
              if chain.chain_break is not None and chain.chain_break <= len(earlier.record_hashes)]
    if broken:
        raise StreamRefused(
            "the log {} does not follow its record hash chain at record {}: the hash it states is not the one its "
            "records give (LOG_CHAIN_ALGORITHM), so it was changed after the engine wrote it, nothing shows the run "
            "of {} is the crash resume of the run of the stream folder {}, and nothing is delivered. Restore it "
            "from the backup as the engine wrote it; never edit it (docs/manual/market-data.md, \"A crash "
            "resume\")".format(broken[0][0], broken[0][1], engine_log_path, os.path.join(streams, found[0])))
    if len(earlier.groups) < 2:
        why = "that run committed nothing but its configuration"
    elif mine.record_hashes[:len(earlier.record_hashes)] != earlier.record_hashes:
        why = "this log does not begin with that run's {} committed records, record hash for record hash".format(
            len(earlier.record_hashes))
    else:
        return found[0]
    raise StreamRefused(no_whole_copy(engine_log_path, os.path.join(logs, found[0] + ".log"),
                                      os.path.join(streams, found[0]), why))


def move_aside(streams, logs):
    """The remedy for a log that cannot be matched to any run: the data job's warning says the same (tidata.py)."""
    return ("move the folder out of {} and its log out of the engine's log folder {}, "
            "keeping both as evidence in a folder outside both (docs/manual/market-data.md, \"A crash "
            "resume\")".format(streams, logs))


def _own_or_resume(streams, logs, engine_log_path, own):
    """own for a log of a fresh run; StreamRefused for a log that holds history, no folder being found.

    A fresh run's log holds its configuration, its Run approval, if any, and
    any operator input; only committed market inputs (the data job's kinds,
    kinds.KINDS) are history, which a fresh run's log cannot hold (ADR 0027),
    so a log holding one is a crash resume whose first run's
    log is not in the engine's log folder, and a warm-up under its new name
    would make the second folder the manual forbids.
    """
    try:
        groups = _committed_groups(engine_log_path).groups
    except ResolveRefused as err:
        raise StreamRefused("the run's stream folder is found by the run's identity, from its durable log, and "
                            "that log cannot be read: {}".format(err)) from None
    history = sum(1 for g in groups
                  if g[0][1] == LOG_INPUT and (g[0][2]["type"], g[0][2]["schema_version"]) in kinds.KINDS)
    if history <= 0:
        return own
    unmatched = sorted(n for n in (os.listdir(streams) if os.path.isdir(streams) else [])
                       if n != own and os.path.isdir(os.path.join(streams, n))
                       and not os.path.isfile(os.path.join(logs, n + ".log")))
    hint = ""
    if unmatched:
        hint = " The stream folder {} has no log beside it: it is likely the first run's.".format(
            ", ".join(os.path.join(streams, n) for n in unmatched))
    raise StreamRefused(
        "the log {} already holds {} committed market inputs beyond its configuration; if it is a crash resume of an "
        "earlier run, restore that run's log beside it and start again; do not run a warm-up under this name "
        "(a second stream folder splits one run's stream).{} (docs/manual/market-data.md, \"A crash "
        "resume\")".format(engine_log_path, history, hint))


def no_whole_copy(engine_log, earlier, folder, why):
    """The refusal of a run that shares an earlier run's identity but is not shown to be its crash resume.

    The data job's refusal says the same (adapter/marketdata/tidata.py,
    stream_run): one text names every case that leads to it, each with its
    remedy (docs/manual/market-data.md, "A crash resume").
    """
    return ("the run of {} has the identity of the run of {}, but {}, so nothing shows it is that run's crash resume "
            "and it is never handed that run's stream folder {}. It is one of three cases: a second start of a "
            "first run without a Run approval, from the same configuration and as-of time (stop it and start a "
            "fresh run with its approval and its own warm-up); a resume of a first run that committed only its "
            "configuration (nothing of that run is kept: start a fresh run with its approval and its own "
            "warm-up); or a log that is not a copy of that run's records (not its resume: resume the crashed "
            "run's own log with engine -resume; while the engine is still copying, run this again once it "
            "listens) (docs/manual/market-data.md, \"A crash resume\")".format(engine_log, earlier, why, folder))


class Deliverer:
    """Delivers the stream folder <streams>/<run> of the run whose durable log is engine_log_path (see the module).

    <run> is stream_run's, found at check: the run's own name, or for a
    crash resume the name of the run that began it.

    outbox is the adapter's relay.Outbox; record_path the delivery record's
    file; snapshot() returns the broker snapshot's snapshot.Record, or None
    when it cannot be taken now (the link went during the read, say; nothing
    is then delivered, and the next step tries again), and raises
    SnapshotRefused when it cannot be taken at all; new_id an input ID for
    an event type.
    """

    def __init__(self, streams, record_path, outbox, engine_log_path, snapshot, new_id):
        self._streams = streams
        # The log's own run name until check finds the run's folder.
        self.run = run_name(engine_log_path)
        self.folder = os.path.join(streams, self.run)
        self.record = DeliveryRecord(record_path)
        self._outbox = outbox
        self._engine_log_path = engine_log_path
        self._snapshot = snapshot
        self._new_id = new_id

    def check(self):
        """At start, once the engine listens (its log is whole): the run's stream folder exists and the delivery
        record reads; StreamRefused or DeliveryRecordInvalid."""
        self.run = stream_run(self._streams, self._engine_log_path)
        self.folder = os.path.join(self._streams, self.run)
        if not os.path.isdir(self.folder):
            raise StreamRefused(
                "the run's stream folder {} does not exist: the data job has not written run {}'s warm-up yet. "
                "Run it once (ti-data ... --backfill-from <the configuration's warm_up_start>; "
                "docs/manual/market-data.md, \"The one-time warm-up\"), then start the adapter".format(
                    self.folder, self.run))
        self.record.load()

    def deliver(self):
        """Record the next batch of undelivered files, after a fresh broker snapshot, in the outbox (see the module)."""
        batches = self.record.load()
        if batches and batches[-1]["state"] == QUEUING:
            if not self._finish(batches[-1]):
                return
            batches = self.record.load()
        if any("envelope" in e for e in self._outbox.load()):
            return  # a batch is still being sent: the next waits behind it
        delivered = [f["name"] for b in batches if b["run"] == self.run and b["state"] != DROPPED
                     for f in b["files"]]
        latest = max((_session_of(n) for n in delivered), default=None)
        new = sorted((_session_of(n), n) for n in os.listdir(self.folder)
                     if STREAM_FILE.fullmatch(n) and n not in delivered)
        if not new:
            return
        for session, name in new:
            if latest is not None and session <= latest:
                raise _refused(os.path.join(self.folder, name),
                               "its session {} is not after {}, the last session already delivered".format(
                                   session, latest))
        streams = self._read_batch([name for _, name in new], latest)
        snapshot = self._snapshot()
        if snapshot is None:
            return
        batch = {"run": self.run, "snapshot": self._new_id(BROKER_SNAPSHOT_TYPE),
                 "files": [{"name": s.name, "sha256": s.sha256, "lines": len(s.envelopes)} for s in streams]}
        self.record.begin(batch)
        entries = _entries(self.run, batch["snapshot"], snapshot, streams)
        self._outbox.extend(entries)
        self.record.mark_queued()
        log.info("ibkr stream queued run=%s files=%s inputs=%d", self.run, ",".join(s.name for s in streams),
                 len(entries))

    def dropped(self, run):
        """A fresh run struck run's entries off the outbox unsent: its last batch is not delivered (see the module)."""
        if self.record.drop_queued(run):
            log.info("ibkr stream batch of run %s struck off unsent; dropped, so a resumed run %s gets its files "
                     "again", run, run)

    def _read_batch(self, names, latest):
        """The files names of this run, in order, each read strictly and checked against the others (see the module)."""
        engine_log = read_engine_log(self._engine_log_path)
        recorded = {e["id"] for e in self._outbox.load()}
        streams, named, previous = [], {}, latest
        for name in names:
            path = os.path.join(self.folder, name)
            stream = read_stream(path, self.run, engine_log.configuration_hash, engine_log.strategy_version)
            if previous is not None and stream.first <= previous:
                raise _refused(path, "its first session {} is not after {}, the last session before it".format(
                    stream.first, previous))
            for envelope in stream.envelopes:
                if envelope["id"] in named:
                    earlier, first = named[envelope["id"]]
                    if not _repeats(first, envelope):
                        raise _refused(path, "it names input {}, which {} names too".format(envelope["id"], earlier))
                    continue
                if envelope["id"] in recorded:
                    raise _refused(path, "it names input {}, which the outbox already holds".format(envelope["id"]))
                named[envelope["id"]] = (name, envelope)
            streams.append(stream)
            previous = stream.session
        return streams

    def _finish(self, batch):
        """Settle a batch a stop left QUEUING; True once it is QUEUED (or DROPPED, an earlier run's)."""
        recorded = {e["id"] for e in self._outbox.load()}
        if batch["snapshot"] not in recorded and batch["snapshot"] not in {
                i.id for i in read_engine_log(self._engine_log_path).inputs}:
            if batch["run"] != self.run:
                # Never recorded or sent, and its run is not running: the run
                # now running has its own stream from its warm-up.
                log.info("ibkr stream batch of run %s was never queued; dropped, since the engine now runs %s",
                         batch["run"], self.run)
                self.record.mark_dropped()
                return True
            batches = self.record.load()
            delivered = [f["name"] for b in batches[:-1] if b["run"] == self.run and b["state"] != DROPPED
                         for f in b["files"]]
            latest = max((_session_of(n) for n in delivered), default=None)
            streams = self._read_batch([entry["name"] for entry in batch["files"]], latest)
            for entry, stream in zip(batch["files"], streams):
                if stream.sha256 != entry["sha256"]:
                    raise _refused(os.path.join(self.folder, entry["name"]),
                                   "it changed since the adapter recorded it for delivery")
            snapshot = self._snapshot()
            if snapshot is None:
                return False
            self._outbox.extend(_entries(self.run, batch["snapshot"], snapshot, streams))
        self.record.mark_queued()
        return True


def _repeats(first, envelope):
    """Whether envelope is a later file's copy of first that the data job writes again: a retraction, as it was.

    A compact stream's calendar reaches back to the run's base, and every
    file writes each recorded retraction that calendar covers under its
    stable ID (encode.encode), so successive files repeat it. Its type,
    schema version and payload must be first's (its times are each file's
    own); any other input named twice is refused.
    """
    return ((envelope["type"], envelope["schema_version"]) == kinds.RETRACTED
            and (first["type"], first["schema_version"], first["payload"]) == (
                envelope["type"], envelope["schema_version"], envelope["payload"]))


def _session_of(name):
    return date.fromisoformat(STREAM_FILE.fullmatch(name).group(1))


def _entries(run, snapshot_id, snapshot, streams):
    """The outbox entries of one batch: the broker snapshot, naming the run it was read for, then every envelope of each file, in order."""
    entries = [{"id": snapshot_id, "event_type": snapshot.event_type, "schema_version": snapshot.schema_version,
                "event_time": snapshot.payload["as_of"], "payload": snapshot.payload, "run": run}]
    # A retraction a later file repeats (_repeats) is recorded once, as the
    # first file wrote it.
    seen = set()
    for stream in streams:
        for e in stream.envelopes:
            if e["id"] in seen:
                continue
            seen.add(e["id"])
            entries.append({"id": e["id"], "event_type": e["type"], "schema_version": e["schema_version"],
                            "event_time": e["event_time"], "payload": e["payload"], "envelope": e})
    return entries
