"""The engine's durable log, read as the engine writes it: the one reader the data job and the IBKR adapter share.

internal/journal durable_log.go; ADR 0027; docs/runbooks/durable-log.md.
The engine streams its log as it runs: a header line, then for each input
one group (the input's record, the records of the decisions it caused, then
a commit naming the group's last record). Only a committed group was made
durable; the lines after the last commit are a group the engine is still
writing, or a crash's torn tail, and hold no input.

read gives the committed groups, every committed record's hash and where
the record hash chain (LOG_CHAIN_ALGORITHM, recomputed over each record as
internal/journal does) first breaks; head reads no further than the first
two commits, where a run's identity is (identity). Fail closed
(AGENTS.md): a log not as the engine writes it raises LogUnreadable, never
read as holding no inputs.

The data job (tidata.py) imports this module from its package; the IBKR
adapter (adapter/ibkr/relay.py) loads it by its path, installed beside it by
provision.sh adapter, as it loads kinds.py. Standard library only, and no
imports of the package: the adapter loads it on its own.
"""
import hashlib
import json
import re
from collections import namedtuple
from datetime import datetime, timedelta, timezone

LOG_VERSION = 1
LOG_CHAIN_ALGORITHM = ("seed=sha256(canonical_log_header_bytes); "
                       "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")
LOG_HEADER_FIELDS = ("chain_algorithm", "configuration_hash", "log_version", "strategy_version")
LOG_RECORD_FIELDS = ("envelope", "kind", "record_hash", "sequence")
LOG_COMMIT_FIELDS = ("record_hash", "sequence")
LOG_INPUT = "input"
LOG_DECISION = "decision"
RECORD_HASH = re.compile(r"sha256:[0-9a-f]{64}")
# An event envelope (internal/event Envelope): its required fields, and the
# two it leaves out when empty.
ENVELOPE_VERSION = 1
ENVELOPE_FIELDS = frozenset(("id", "type", "envelope_version", "schema_version", "event_time", "recorded_at",
                             "sequence", "source", "strategy_version", "configuration_hash", "payload_hash",
                             "payload"))
ENVELOPE_OPTIONAL = frozenset(("correlation_id", "causation_id"))
PAYLOAD_HASH = re.compile(r"[0-9a-f]{64}")
TIME = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})")
TIME_PARTS = re.compile(r"([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]{1,9}))?"
                        r"(?:Z|([+-])([0-9]{2}):([0-9]{2}))")
# The owner's Run approval, which names a fresh run after an earlier one
# (internal/event OperatorRunApprovedEventType).
RUN_APPROVED = "operator.run.approved"

CommittedLog = namedtuple("CommittedLog", "header groups final_hash record_hashes chain_break")
"""A durable log's header, its committed groups (each a list of (line number, kind, envelope)), its last
commit's record hash (None with no commit), every committed record's stored hash in order, and the sequence
of the first committed record whose stored hash is not the one LOG_CHAIN_ALGORITHM gives over the records
before it, or None when every one's is."""


class LogUnreadable(ValueError):
    """The log at path cannot be read as the engine writes it: at line (None when the file cannot be read), why."""

    def __init__(self, path, line, why):
        super().__init__("the engine's durable log {} cannot be read{}: {}".format(
            path, "" if line is None else " at line {}".format(line), why))
        self.path, self.line, self.why = path, line, why


def read(path, keep=None):
    """The durable log at path as a CommittedLog, read strictly; LogUnreadable otherwise (see the module).

    The log is read a line at a time, so memory holds one line and what is
    kept of each committed group, never the file (docs/live-trading/
    lessons-2026-10.md L1: an IRA's log grows by a universe snapshot a day).
    ``keep``, if given, is called with each committed group as it is
    committed and returns what CommittedLog.groups holds for it (by default
    the group itself); a caller that needs only a few fields of each input
    passes one that drops the rest."""
    try:
        with open(path, "rb") as f:
            return _read_lines(path, f, keep)
    except OSError as err:
        raise LogUnreadable(path, None, err.strerror or str(err)) from None


def _complete(f):
    """The file's complete lines, without their newline: the last one, when it has none, is a line the engine
    is still writing, and is not read."""
    for raw in f:
        if not raw.endswith(b"\n"):
            return
        yield raw[:-1]


def _read_lines(path, f, keep):
    lines = _complete(f)
    first = next(lines, None)
    if first is None:
        raise LogUnreadable(path, 1, "it has no complete header line")
    header = _json(path, 1, first)
    if isinstance(header, dict) and "journal_version" in header:
        raise LogUnreadable(path, 1, "it is the journal the engine writes when it stops, not the durable log it "
                                     "streams as it runs (its -log file)")
    if (not isinstance(header, dict) or tuple(sorted(header)) != LOG_HEADER_FIELDS
            or not is_count(header["log_version"]) or header["log_version"] != LOG_VERSION
            or header["chain_algorithm"] != LOG_CHAIN_ALGORITHM or not is_text(header["configuration_hash"])
            or not is_text(header["strategy_version"])):
        raise LogUnreadable(path, 1, "it is not a durable log header of version {}".format(LOG_VERSION))
    groups, group, hashes, pending = [], [], [], []
    sequence, last_hash, final_hash = 0, None, None
    chain = chain_seed(header)
    chain_break = pending_break = None
    for n, raw in enumerate(lines, 2):
        line = _json(path, n, raw)
        if not isinstance(line, dict) or len(line) != 1 or not set(line) <= {"record", "commit"}:
            raise LogUnreadable(path, n, "a line is exactly one of a record and a commit")
        if "record" in line:
            record = line["record"]
            if not isinstance(record, dict) or tuple(sorted(record)) != LOG_RECORD_FIELDS:
                raise LogUnreadable(path, n, "it is not a record as the engine writes one")
            if not is_count(record["sequence"]) or record["sequence"] != sequence + 1:
                raise LogUnreadable(path, n, "its sequence does not follow {}".format(sequence))
            if record["kind"] != (LOG_DECISION if group else LOG_INPUT):
                raise LogUnreadable(path, n, "a group is one input followed by its decisions")
            if not isinstance(record["record_hash"], str) or not RECORD_HASH.fullmatch(record["record_hash"]):
                raise LogUnreadable(path, n, "its record hash is not a SHA-256")
            payload = _payload(path, n, raw, record)
            if pending_break is None and chain_break is None:
                chain = chain_next(chain, record["kind"], record["envelope"], payload)
                if chain is None or "sha256:" + chain.hex() != record["record_hash"]:
                    pending_break = record["sequence"]
            sequence, last_hash = record["sequence"], record["record_hash"]
            group.append((n, record["kind"], record["envelope"]))
            pending.append(last_hash)
            continue
        commit = line["commit"]
        if (not isinstance(commit, dict) or tuple(sorted(commit)) != LOG_COMMIT_FIELDS or not group
                or commit["sequence"] != sequence or commit["record_hash"] != last_hash):
            raise LogUnreadable(path, n, "it is not the commit of the group before it")
        groups.append(group if keep is None else keep(group))
        hashes.extend(pending)
        group, pending = [], []
        final_hash = last_hash
        chain_break = chain_break if chain_break is not None else pending_break
    return CommittedLog(header, groups, final_hash, tuple(hashes), chain_break)


def head(path):
    """The first two committed groups of the durable log at path, read no further than its second commit.

    A run's identity is in its log's first two committed inputs (the
    configuration, and a fresh run's Run approval, which cmd/engine records
    second, before it listens; a resume copies both first), so identity
    over head is identity over read for every log the engine writes. Only
    the first lines' shape is checked, so a log the head names as a run's
    is then read strictly (read). LogUnreadable when the first lines cannot
    be read as the engine writes them: they name no run.
    """
    groups, group = [], []
    try:
        with open(path, "rb") as f:
            lines = iter(f)
            first = next(lines, b"")
            if not first.endswith(b"\n"):
                raise LogUnreadable(path, 1, "it has no complete header line")
            header = _json(path, 1, first)
            if not isinstance(header, dict) or header.get("log_version") != LOG_VERSION:
                raise LogUnreadable(path, 1, "it is not a durable log header of version {}".format(LOG_VERSION))
            for n, raw in enumerate(lines, 2):
                if len(groups) == 2 or not raw.endswith(b"\n"):
                    break
                line = _json(path, n, raw)
                record = line.get("record") if isinstance(line, dict) and len(line) == 1 else None
                envelope = record.get("envelope") if isinstance(record, dict) else None
                if (isinstance(envelope, dict) and record.get("kind") == (LOG_DECISION if group else LOG_INPUT)
                        and all(is_text(envelope.get(k)) for k in ("id", "type", "recorded_at"))):
                    group.append((n, record["kind"], record["envelope"]))
                elif isinstance(line, dict) and len(line) == 1 and "commit" in line and group:
                    groups.append(group)
                    group = []
                else:
                    raise LogUnreadable(path, n, "it is not a record or commit as the engine writes one")
    except OSError as err:
        raise LogUnreadable(path, None, err.strerror or str(err)) from None
    return groups


def identity(groups):
    """The run identity of committed groups (all of a log's, or its head's): its Run approval's ID, else its first
    committed input's ID and recorded time; None for no group, which names no run.

    A crash resume's log holds a copy of the crashed run's records, its
    approval included (ADR 0027 section 6), so the resume keeps the
    identity, and a fresh run after an earlier one has its own approval.
    """
    for group in groups:
        _, kind, envelope = group[0]
        if kind == LOG_INPUT and envelope.get("type") == RUN_APPROVED:
            return envelope["id"]
    if not groups:
        return None
    first = groups[0][0][2]
    return "{}@{}".format(first["id"], first["recorded_at"])


def chain_seed(header):
    """The seed of a durable log's record hash chain: sha256 of its header's canonical bytes (LOG_CHAIN_ALGORITHM).

    internal/journal newLogChain: the header's four fields through the
    project's one canonical encoder (event.CanonicalBytes, ADR 0016).
    """
    return hashlib.sha256(canonical_bytes({k: header[k] for k in LOG_HEADER_FIELDS})).digest()


def chain_next(previous, kind, envelope, payload):
    """The record hash, as raw SHA-256 bytes, of a record of kind after previous (LOG_CHAIN_ALGORITHM), else None.

    internal/journal Chain.Next: sha256(previous || kind ||
    event.CanonicalEnvelopeBytes(envelope)), where the canonical bytes cover
    every envelope field, both times rendered in UTC as RFC 3339 with
    nanoseconds, an absent correlation or causation ID as "", and the
    payload as its bytes exactly as stored (payload, which its hash
    attests). None when the envelope cannot be rendered so (a time the
    engine would not write): such a record follows no chain.
    """
    try:
        fields = {k: envelope[k] for k in ENVELOPE_FIELDS if k != "payload"}
        fields.update((k, envelope.get(k, "")) for k in ENVELOPE_OPTIONAL)
        fields["event_time"], fields["recorded_at"] = utc(envelope["event_time"]), utc(envelope["recorded_at"])
        fields["payload"] = payload.decode("utf-8")
        canonical = canonical_bytes(fields)
    except (KeyError, TypeError, ValueError, OverflowError, AttributeError):
        return None
    return hashlib.sha256(previous + kind.encode("utf-8") + canonical).digest()


def canonical_bytes(fields):
    """fields (text and whole-number values) as internal/event canonicalJSON renders them: keys sorted, compact."""
    parts = []
    for key in sorted(fields):
        value = fields[key]
        if isinstance(value, bool) or not isinstance(value, (int, str)):
            raise TypeError("a canonical field is text or a whole number")
        parts.append(go_string(key) + ":" + (str(value) if isinstance(value, int) else go_string(value)))
    return ("{" + ",".join(parts) + "}").encode("utf-8")


def go_string(text):
    """text as Go's encoding/json writes a string: UTF-8 kept, and <, >, &, U+2028 and U+2029 escaped."""
    return (json.dumps(text, ensure_ascii=False).replace("<", "\\u003c").replace(">", "\\u003e")
            .replace("&", "\\u0026").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029"))


def utc(text):
    """An RFC 3339 time as Go's Time.UTC().Format(time.RFC3339Nano) renders it: UTC, "Z", no trailing zeros."""
    year, month, day, hour, minute, second, fraction, sign, hours, minutes = TIME_PARTS.fullmatch(text).groups()
    at = datetime(int(year), int(month), int(day), int(hour), int(minute), int(second), tzinfo=timezone.utc)
    if sign:
        if int(hours) > 23 or int(minutes) > 59:
            raise ValueError("a zone offset is 00:00 to 23:59")
        offset = timedelta(hours=int(hours), minutes=int(minutes))
        at = at - offset if sign == "+" else at + offset
    fraction = (fraction or "").rstrip("0")
    return "{:04d}-{:02d}-{:02d}T{:02d}:{:02d}:{:02d}{}Z".format(at.year, at.month, at.day, at.hour, at.minute,
                                                              at.second, "." + fraction if fraction else "")


def is_count(value):
    """Whether value is a whole number above zero (not a bool)."""
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def is_text(value):
    """Whether value is a non-empty string."""
    return isinstance(value, str) and value != ""


def is_time(value, shape):
    """Whether value is an RFC 3339 time with its zone, of the given shape, naming a real instant."""
    if not isinstance(value, str) or not shape.fullmatch(value):
        return False
    try:
        datetime.strptime(value[:19], "%Y-%m-%dT%H:%M:%S")
    except ValueError:
        return False
    return value[-1] == "Z" or (int(value[-5:-3]) <= 23 and int(value[-2:]) <= 59)


def _json(path, n, raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except ValueError as err:
        raise LogUnreadable(path, n, "it is not JSON ({})".format(err)) from None


def _payload(path, n, raw, record):
    """The payload's bytes as stored; LogUnreadable for an envelope not as internal/event writes one, or whose
    payload's bytes do not match its hash."""
    envelope = record["envelope"]
    if (not isinstance(envelope, dict) or not ENVELOPE_FIELDS <= set(envelope)
            or not set(envelope) <= ENVELOPE_FIELDS | ENVELOPE_OPTIONAL
            or not all(is_text(envelope[k]) for k in ("id", "type", "source", "strategy_version",
                                                      "configuration_hash"))
            or envelope["envelope_version"] != ENVELOPE_VERSION or not is_count(envelope["envelope_version"])
            or not is_count(envelope["schema_version"]) or not is_count(envelope["sequence"])
            or not all(is_time(envelope[k], TIME) for k in ("event_time", "recorded_at"))
            or not all(isinstance(envelope.get(k, ""), str) for k in ENVELOPE_OPTIONAL)
            or not isinstance(envelope["payload_hash"], str) or not PAYLOAD_HASH.fullmatch(envelope["payload_hash"])):
        raise LogUnreadable(path, n, "its envelope is not an event envelope as the engine writes one")
    # The payload's bytes exactly as stored, which its hash attests: the
    # writer puts payload last in the envelope, straight after its hash,
    # and the record hash last in the record.
    marker = '"payload_hash":"{}","payload":'.format(envelope["payload_hash"]).encode()
    suffix = '}},"record_hash":"{}"}}}}'.format(record["record_hash"]).encode()
    start = raw.find(marker)
    payload = raw[start + len(marker):len(raw) - len(suffix)]
    if start < 0 or not raw.endswith(suffix) or hashlib.sha256(payload).hexdigest() != envelope["payload_hash"]:
        raise LogUnreadable(path, n, "its payload does not match its payload hash")
    return payload
