"""A stand-in for the engine on the adapter's side of the socket, writing its durable log as internal/journal does.

A new log begins, as cmd/engine's does, with the run's configuration
committed as its first input (ID "configuration:" + the configuration hash,
recorded at the run's as-of time, started_at); a log that exists already is
a resumed run's copy, and the stand-in takes its next sequence from it.
It commits each new input before it replies (ADR 0027 section 4): the input
and the decisions scripted for its ID go to the log file as one group closed
by a commit, in the layout relay.read_engine_log reads, each record's hash
chained as LOG_CHAIN_ALGORITHM states (relay.log_chain_next; a real
engine's resume is checked against it in test_delivery's
RealEngineResumeTests). An input it has
committed is answered from its log, byte for byte, and one with the same ID
but other content is refused, as the engine refuses it. An input out of
sequence, or of another run, is refused too: the real engine stops on one.
Everything here is synthetic.
"""
import copy
import hashlib
import json
from datetime import datetime, timezone
from pathlib import Path

from relay import log_chain_next, log_chain_seed

LOG_CHAIN = ("seed=sha256(canonical_log_header_bytes); "
             "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")
CONFIGURATION_HASH = "sha256:" + "1" * 64
CONFIGURATION_TYPE = "strategy.target-weight.configuration"
STARTED_AT = "2026-10-16T13:30:00Z"
STRATEGY_VERSION = "target-weight-boost/1.0.0+rehearsal"
COMPACT = (",", ":")
BROKER_SNAPSHOT_TYPE = "account.broker.snapshot"


RUN_APPROVED = "operator.run.approved"


def run_approval(account, previous_log="", previous_log_final_hash="", starting_book=None, latched=False):
    """An operator.run.approved payload (schema version 3): a fresh run's naming previous_log, or a first run's.

    starting_book is (positions, total_cash): a dict of symbol to shares and
    the total cash the owner confirms, in USD; a first run's approval states
    one (ADR 0019 and ADR 0027, amended 2026-10-18).
    """
    book = None
    if starting_book is not None:
        positions, cash = starting_book
        book = {"positions": [{"instrument_id": s, "quantity": q} for s, q in sorted(positions.items())],
                "total_cash": cash, "currency": "USD"}
    return {"account_id": account, "previous_log": previous_log, "previous_log_final_hash": previous_log_final_hash,
            "incident_id": "", "approved_by": "owner", "approved_at": "2026-10-17T14:00:00Z",
            "boost": {"latched": latched}, "incident_in_force": None, "starting_book": book}


def final_hash(log_path):
    """The record hash of the last commit in the durable log at log_path, as the engine approves it."""
    commits = [json.loads(line)["commit"] for line in Path(log_path).read_text(encoding="utf-8").splitlines()
               if line.startswith('{"commit"')]
    return commits[-1]["record_hash"]


class EngineRefused(Exception):
    """The stand-in refused an input as the engine would, stopping its run."""


def _record_line(previous, sequence, kind, envelope, raw_payload):
    """(record hash, line) of a record after the chain's previous hash (raw bytes), as internal/journal writes one."""
    head = json.dumps(envelope, separators=COMPACT)
    digest = log_chain_next(previous, kind, envelope, raw_payload.encode("utf-8"))
    record_hash = "sha256:" + digest.hex()
    return record_hash, '{{"record":{{"sequence":{},"kind":"{}","envelope":{},"payload":{}}},"record_hash":"{}"}}}}\n'.format(
        sequence, kind, head[:-1], raw_payload, record_hash)


class LoggedEngine:
    """Answers decide(envelope) for one run, logging each committed input and its decisions to log_path.

    decisions maps an input ID to the decision dicts ({"type", "payload"})
    its reply carries. configuration is the configuration input's payload (a
    bare Boost strategy ID when not given; the data job reads a whole Boost
    configuration from it). Calling the engine is the relay's connect: it
    returns itself, and connects counts each call.

    With snapshot_age (a timedelta), the decisions scripted for an input are
    committed only when the latest broker snapshot committed before it was
    taken at or before the input's recorded time and no longer before it
    than snapshot_age: ADR 0019's snapshot rule, which the engine applies to
    a sell (orderplan.StaleDetail), applied here to every scripted decision.
    An input that fails it commits no decisions, and withheld names it with
    the reason.
    """

    def __init__(self, log_path, decisions=None, configuration_hash=CONFIGURATION_HASH,
                 strategy_version=STRATEGY_VERSION, started_at=STARTED_AT, configuration=None, snapshot_age=None,
                 approval=None, approval_version=3):
        self.path = Path(log_path)
        self.approval_version = approval_version
        self.decisions = decisions or {}
        self.snapshot_age = snapshot_age
        self.withheld = {}
        self._snapshot_as_of = None
        self.configuration_hash = configuration_hash
        self.strategy_version = strategy_version
        self.committed = {}
        # A resumed run's copy of the crashed run's committed inputs, each
        # answered from the log as the engine answers it (ADR 0027 section 4).
        self.copied = {}
        self.received = []
        self.connects = 0
        self.records = 0
        self.next_sequence = 1
        # The instance lock every engine creates in its state folder before
        # its log and holds while it runs (cmd/engine lockInstance); the
        # double creates it and never holds it, as an engine that has ended.
        if self.path.parent.is_dir():
            (self.path.parent / "engine.lock").touch(exist_ok=True)
        if self.path.exists():
            lines = self.path.read_text(encoding="utf-8").splitlines()[1:]
            self.records = sum(1 for line in lines if line.startswith('{"record"'))
            self.next_sequence = sum(1 for line in lines if line.startswith('{"commit"')) + 1
            records = [json.loads(line)["record"] for line in lines if line.startswith('{"record"')]
            self._chain = bytes.fromhex(records[-1]["record_hash"][len("sha256:"):]) if records else None
            return
        header = {"log_version": 1, "chain_algorithm": LOG_CHAIN, "configuration_hash": configuration_hash,
                  "strategy_version": strategy_version}
        self._chain = log_chain_seed(header)
        self.path.write_text(json.dumps(header, separators=COMPACT) + "\n")
        raw = json.dumps(configuration or {"strategy_id": "boost"}, separators=COMPACT)
        head = {"id": "configuration:" + configuration_hash, "type": CONFIGURATION_TYPE, "envelope_version": 1,
                "schema_version": 7, "event_time": started_at, "recorded_at": started_at, "sequence": 1,
                "source": "engine", "strategy_version": strategy_version, "configuration_hash": configuration_hash,
                "payload_hash": hashlib.sha256(raw.encode()).hexdigest()}
        self.records = 1
        last, line = self._record(1, "input", head, raw)
        with open(self.path, "a", encoding="utf-8") as f:
            f.write(line + json.dumps({"commit": {"sequence": 1, "record_hash": last}}, separators=COMPACT) + "\n")
        self.next_sequence = 2
        if approval is not None:
            self._approve(approval, started_at)

    def _approve(self, payload, started_at):
        """Commit the owner's Run approval as the log's second input, before the socket opens, as cmd/engine does.

        Named, as cmd/engine names it, by the previous log's final hash, or,
        for a first run's (no previous log, the starting book stated), by the
        run's start and its own payload's hash (schema version 3, or the
        approval_version the double was made with).
        """
        raw = json.dumps(payload, separators=COMPACT)
        digest = hashlib.sha256(raw.encode()).hexdigest()
        ident = ("operator-run-approved:first-run:{}:{}".format(started_at, digest) if not payload["previous_log"]
                 else "operator-run-approved:" + payload["previous_log_final_hash"])
        head = {"id": ident, "type": RUN_APPROVED, "envelope_version": 1, "schema_version": self.approval_version,
                "event_time": started_at, "recorded_at": started_at, "sequence": 2, "source": "engine",
                "strategy_version": self.strategy_version, "configuration_hash": self.configuration_hash,
                "payload_hash": digest}
        self.records += 1
        last, line = self._record(self.records, "input", head, raw)
        with open(self.path, "a", encoding="utf-8") as f:
            f.write(line + json.dumps({"commit": {"sequence": self.records, "record_hash": last}},
                                      separators=COMPACT) + "\n")
        self.next_sequence = 3

    @classmethod
    def resumed(cls, crashed_log, log_path, decisions=None):
        """A crash resume of the run whose log is crashed_log, in a new log log_path (a new TI_RUN, engine -resume).

        As cmd/engine's resume: the new log holds a copy of the crashed log's
        records, so the run's identity carries over (relay.run_identity), its
        next sequence follows them, and an input the crashed run committed is
        answered from the copy, never committed again (ADR 0027 section 6).
        """
        data = Path(crashed_log).read_bytes()
        Path(log_path).write_bytes(data)
        engine = cls(log_path, decisions)
        lines = [json.loads(line) for line in data.decode("utf-8").splitlines()[1:]]
        group = []
        for line in lines:
            if "record" in line:
                group.append(line["record"])
                continue
            envelope = group[0]["envelope"]
            decided = [{"type": r["envelope"]["type"], "payload": r["envelope"]["payload"]} for r in group[1:]]
            reply = {"id": "decisions:" + envelope["id"], "type": "engine.decisions", "envelope_version": 1,
                     "schema_version": 1, "causation_id": envelope["id"], "sequence": envelope["sequence"],
                     "configuration_hash": engine.configuration_hash, "strategy_version": engine.strategy_version,
                     "payload": {"decisions": decided}}
            engine.copied[envelope["id"]] = (envelope, reply)
            group = []
        return engine

    def __call__(self):
        self.connects += 1
        return self

    def inputs(self, event_type=None):
        """The envelopes of every input committed, in order, of event_type if given."""
        return [e for e, _ in self.committed.values() if event_type is None or e["type"] == event_type]

    def decide(self, envelope):
        self.received.append(copy.deepcopy(envelope))
        committed = self.committed.get(envelope["id"]) or self.copied.get(envelope["id"])
        if committed is not None:
            if json.dumps(committed[0], sort_keys=True) != json.dumps(envelope, sort_keys=True):
                raise EngineRefused("input {} was committed with other content".format(envelope["id"]))
            return copy.deepcopy(committed[1])
        if envelope.get("sequence") != self.next_sequence:
            raise EngineRefused("input {} has sequence {}, not {}".format(envelope["id"], envelope.get("sequence"),
                                                                          self.next_sequence))
        if (envelope.get("configuration_hash"), envelope.get("strategy_version")) != (
                self.configuration_hash, self.strategy_version):
            raise EngineRefused("input {} is not this run's".format(envelope["id"]))
        raw = json.dumps(envelope["payload"], separators=COMPACT, allow_nan=False)
        if hashlib.sha256(raw.encode()).hexdigest() != envelope.get("payload_hash"):
            raise EngineRefused("input {} payload does not match its hash".format(envelope["id"]))
        decisions = copy.deepcopy(self.decisions.get(envelope["id"], []))
        if decisions and self.snapshot_age is not None:
            stale = self._stale(envelope)
            if stale:
                self.withheld[envelope["id"]] = stale
                decisions = []
        if envelope["type"] == BROKER_SNAPSHOT_TYPE:
            self._snapshot_as_of = _instant(envelope["payload"]["as_of"])
        lines = []
        head = {k: v for k, v in envelope.items() if k != "payload"}
        head.pop("payload_hash")
        head["payload_hash"] = envelope["payload_hash"]
        self.records += 1
        last, line = self._record(self.records, "input", head, raw)
        lines.append(line)
        for n, decision in enumerate(decisions, 1):
            draw = json.dumps(decision["payload"], separators=COMPACT, allow_nan=False)
            dhead = {"id": "{}:decision:{}".format(envelope["id"], n), "type": decision["type"], "envelope_version": 1,
                     "schema_version": 1, "event_time": envelope["event_time"], "recorded_at": envelope["recorded_at"],
                     "sequence": envelope["sequence"], "source": "engine", "strategy_version": self.strategy_version,
                     "configuration_hash": self.configuration_hash, "causation_id": envelope["id"],
                     "payload_hash": hashlib.sha256(draw.encode()).hexdigest()}
            self.records += 1
            last, line = self._record(self.records, "decision", dhead, draw)
            lines.append(line)
        lines.append(json.dumps({"commit": {"sequence": self.records, "record_hash": last}}, separators=COMPACT) + "\n")
        with open(self.path, "a", encoding="utf-8") as f:
            f.write("".join(lines))
        self.next_sequence += 1
        reply = {"id": "decisions:" + envelope["id"], "type": "engine.decisions", "envelope_version": 1,
                 "schema_version": 1, "causation_id": envelope["id"],
                 "sequence": envelope["sequence"], "configuration_hash": self.configuration_hash,
                 "strategy_version": self.strategy_version, "payload": {"decisions": decisions}}
        self.committed[envelope["id"]] = (copy.deepcopy(envelope), reply)
        return copy.deepcopy(reply)

    def _record(self, sequence, kind, head, raw):
        record_hash, line = _record_line(self._chain, sequence, kind, head, raw)
        self._chain = bytes.fromhex(record_hash[len("sha256:"):])
        return record_hash, line

    def _stale(self, envelope):
        """Why the latest broker snapshot is stale at envelope's recorded time, or "" when it is fresh."""
        if self._snapshot_as_of is None:
            return "no broker snapshot is committed before it"
        age = _instant(envelope["recorded_at"]) - self._snapshot_as_of
        if age.total_seconds() < 0:
            return "the broker snapshot was taken {} after it was recorded".format(-age)
        if age > self.snapshot_age:
            return "the broker snapshot is {} old at it".format(age)
        return ""

    def decisions_for(self, input_id):
        """The decisions the durable log commits for input_id, as written: [(type, payload)]."""
        out = []
        for line in self.path.read_text(encoding="utf-8").splitlines()[1:]:
            record = json.loads(line).get("record")
            if record and record["kind"] == "decision" and record["envelope"].get("causation_id") == input_id:
                out.append((record["envelope"]["type"], record["envelope"]["payload"]))
        return out


def _instant(text):
    """An RFC 3339 UTC time as the adapter and the data job stamp it ("...Z", whole seconds) as a datetime."""
    return datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
