"""The run's market-data stream, delivered to the engine through the adapter's outbox, after the broker snapshot.

The data job (adapter/marketdata/tidata.py, its file contract) writes each
run's stream to STREAMS/<run>/<engine>-<last session>.jsonl, where <run> is
the engine's durable log's name without ".log": one envelope per line, in
delivery order, each file's ``sequence`` a file-local ordinal 1..N, and each
file covering the sessions since the one before it. Only the deliverer
knows the engine's sequence, so it sets each envelope's sequence as it sends
it (loop.AdapterLoop._envelope) and sends every other field as written.

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
sends each once, in order (loop.AdapterLoop._drain).

Exactly once: before the outbox write, the delivery record (DELIVERED_SUFFIX
beside the pending file, read strictly) names the files, their SHA-256 and
the snapshot's input ID, marked queuing; after it, queued. A stop between
the two leaves it queuing, and the next start looks for the snapshot's ID in
the outbox and among the inputs the engine committed: found, the write was
made (it is one write) and the batch is marked queued; not found, nothing
of it was recorded or sent, and it is queued again from a fresh snapshot
under the same ID. A file the record names is never queued again, and the
outbox's own rules send nothing twice across a stop (an entry the engine
committed is struck off unsent; one awaiting its reply is asked for again
under its ID, ADR 0027). A new batch waits until no stream entry is left in
the outbox, so a partly sent file resumes where it stopped.

Fail closed (AGENTS.md): a file that is not as the data job writes it (JSON
shape, envelope fields, a payload that does not match its hash, an input ID
twice, the file-local sequence not 1..N), one whose run identity
(configuration hash, strategy version, run name) is not the engine's run
now running, or one whose session is not after every file already
delivered, is refused (StreamRefused): nothing of that batch is recorded or
sent, and the adapter stops for a person, naming the file and how to settle
it. A run whose stream folder does not exist yet stops the start: its
warm-up has not been written.
"""
import hashlib
import importlib.util
import json
import logging
import os
import re
from collections import namedtuple
from datetime import date, datetime, time as clock_time

from relay import ENVELOPE_VERSION, PAYLOAD_HASH, TIME, _is_count, _is_text, _is_time, read_engine_log
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


def _calendar():
    """The committed NYSE calendar the data job uses (adapter/marketdata/nyse_calendar.py), loaded from its file.

    The adapter is installed beside it in the same layout (provision.sh
    adapter), so both read one calendar.
    """
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir, "marketdata", "nyse_calendar.py")
    spec = importlib.util.spec_from_file_location("nyse_calendar", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


nyse_calendar = _calendar()


class StreamRefused(Exception):
    """A stream file, or the run's stream folder, is not one the adapter delivers; it stops for a person."""


class DeliveryRecordInvalid(ValueError):
    """The delivery record is not as the adapter writes it; the adapter stops before delivering anything."""


Stream = namedtuple("Stream", "name session sha256 envelopes")
"""One stream file read strictly: its file name, its last session, the SHA-256 of its bytes and its envelopes."""


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
    return Stream(name, session, hashlib.sha256(data).hexdigest(), envelopes)


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
    of an earlier run that a stop left QUEUING before its outbox write, so
    nothing of it was ever recorded or sent. Read strictly:
    anything else raises DeliveryRecordInvalid. Written whole, atomically
    and synced, only by the adapter holding the pending file's lock.
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
    """The run's name: its durable log's file name without ".log", as the data job names its stream folder."""
    name = os.path.basename(engine_log_path)
    return name[:-len(".log")] if name.endswith(".log") else name


class Deliverer:
    """Delivers the stream folder <streams>/<run> of the run whose durable log is engine_log_path (see the module).

    outbox is the adapter's relay.Outbox; record_path the delivery record's
    file; snapshot() returns the broker snapshot's snapshot.Record, or None
    when the link went during the read (nothing is then delivered); new_id
    an input ID for an event type.
    """

    def __init__(self, streams, record_path, outbox, engine_log_path, snapshot, new_id):
        self.run = run_name(engine_log_path)
        self.folder = os.path.join(streams, self.run)
        self.record = DeliveryRecord(record_path)
        self._outbox = outbox
        self._engine_log_path = engine_log_path
        self._snapshot = snapshot
        self._new_id = new_id

    def check(self):
        """At start: the run's stream folder exists and the delivery record reads; StreamRefused or DeliveryRecordInvalid."""
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
        delivered = [f["name"] for b in batches if b["run"] == self.run for f in b["files"]]
        latest = max((_session_of(n) for n in delivered), default=None)
        new = sorted((_session_of(n), n) for n in os.listdir(self.folder)
                     if STREAM_FILE.fullmatch(n) and n not in delivered)
        if not new:
            return
        engine_log = read_engine_log(self._engine_log_path)
        streams = []
        for session, name in new:
            path = os.path.join(self.folder, name)
            if latest is not None and session <= latest:
                raise _refused(path, "its session {} is not after {}, the last session already delivered".format(
                    session, latest))
            streams.append(read_stream(path, self.run, engine_log.configuration_hash, engine_log.strategy_version))
        snapshot = self._snapshot()
        if snapshot is None:
            return
        batch = {"run": self.run, "snapshot": self._new_id(BROKER_SNAPSHOT_TYPE),
                 "files": [{"name": s.name, "sha256": s.sha256, "lines": len(s.envelopes)} for s in streams]}
        self.record.begin(batch)
        self._outbox.extend(_entries(batch["snapshot"], snapshot, streams))
        self.record.mark_queued()
        log.info("ibkr stream queued run=%s files=%s inputs=%d", self.run, ",".join(s.name for s in streams),
                 1 + sum(len(s.envelopes) for s in streams))

    def _finish(self, batch):
        """Settle a batch a stop left QUEUING; True once it is QUEUED."""
        recorded = {e["id"] for e in self._outbox.load()}
        if batch["snapshot"] not in recorded and batch["snapshot"] not in {
                i.id for i in read_engine_log(self._engine_log_path).inputs}:
            if batch["run"] != self.run:
                # Never recorded or sent, and its run has ended: the run now
                # running has its own stream from its warm-up.
                log.info("ibkr stream batch of run %s was never queued; dropped, since the engine now runs %s",
                         batch["run"], self.run)
                self.record.mark_dropped()
                return True
            engine_log = read_engine_log(self._engine_log_path)
            streams = []
            for entry in batch["files"]:
                path = os.path.join(self.folder, entry["name"])
                stream = read_stream(path, self.run, engine_log.configuration_hash, engine_log.strategy_version)
                if stream.sha256 != entry["sha256"]:
                    raise _refused(path, "it changed since the adapter recorded it for delivery")
                streams.append(stream)
            snapshot = self._snapshot()
            if snapshot is None:
                return False
            self._outbox.extend(_entries(batch["snapshot"], snapshot, streams))
        self.record.mark_queued()
        return True


def _session_of(name):
    return date.fromisoformat(STREAM_FILE.fullmatch(name).group(1))


def _entries(snapshot_id, snapshot, streams):
    """The outbox entries of one batch: the broker snapshot, then every envelope of each file, in order."""
    entries = [{"id": snapshot_id, "event_type": snapshot.event_type, "schema_version": snapshot.schema_version,
                "event_time": snapshot.payload["as_of"], "payload": snapshot.payload}]
    for stream in streams:
        entries.extend({"id": e["id"], "event_type": e["type"], "schema_version": e["schema_version"],
                        "event_time": e["event_time"], "payload": e["payload"], "envelope": e}
                       for e in stream.envelopes)
    return entries
