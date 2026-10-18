"""A stand-in for the engine on the adapter's side of the socket, writing its durable log as internal/journal does.

It commits each new input before it replies (ADR 0027 section 4): the input
and the decisions scripted for its ID go to the log file as one group closed
by a commit, in the layout relay.read_engine_log reads (the record hashes are
stand-ins; the chain is the real engine's to verify). An input it has
committed is answered from its log, byte for byte, and one with the same ID
but other content is refused, as the engine refuses it. An input out of
sequence, or of another run, is refused too: the real engine stops on one.
Everything here is synthetic.
"""
import copy
import hashlib
import json
from pathlib import Path

LOG_CHAIN = ("seed=sha256(canonical_log_header_bytes); "
             "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")
CONFIGURATION_HASH = "sha256:" + "1" * 64
STRATEGY_VERSION = "target-weight-boost/1.0.0+rehearsal"
COMPACT = (",", ":")


class EngineRefused(Exception):
    """The stand-in refused an input as the engine would, stopping its run."""


def _record_line(sequence, kind, envelope, raw_payload):
    head = json.dumps(envelope, separators=COMPACT)
    record_hash = "sha256:" + hashlib.sha256("{}:{}".format(sequence, head).encode()).hexdigest()
    return record_hash, '{{"record":{{"sequence":{},"kind":"{}","envelope":{},"payload":{}}},"record_hash":"{}"}}}}\n'.format(
        sequence, kind, head[:-1], raw_payload, record_hash)


class LoggedEngine:
    """Answers decide(envelope) for one run, logging each committed input and its decisions to log_path.

    decisions maps an input ID to the decision dicts ({"type", "payload"})
    its reply carries. Calling the engine is the relay's connect: it returns
    itself, and connects counts each call.
    """

    def __init__(self, log_path, decisions=None, configuration_hash=CONFIGURATION_HASH,
                 strategy_version=STRATEGY_VERSION):
        self.path = Path(log_path)
        self.decisions = decisions or {}
        self.configuration_hash = configuration_hash
        self.strategy_version = strategy_version
        self.committed = {}
        self.received = []
        self.connects = 0
        self.records = 0
        self.next_sequence = 1
        if not self.path.exists():
            self.path.write_text(json.dumps({"log_version": 1, "chain_algorithm": LOG_CHAIN,
                                             "configuration_hash": configuration_hash,
                                             "strategy_version": strategy_version}, separators=COMPACT) + "\n")

    def __call__(self):
        self.connects += 1
        return self

    def inputs(self, event_type=None):
        """The envelopes of every input committed, in order, of event_type if given."""
        return [e for e, _ in self.committed.values() if event_type is None or e["type"] == event_type]

    def decide(self, envelope):
        self.received.append(copy.deepcopy(envelope))
        committed = self.committed.get(envelope["id"])
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
        lines = []
        head = {k: v for k, v in envelope.items() if k != "payload"}
        head.pop("payload_hash")
        head["payload_hash"] = envelope["payload_hash"]
        self.records += 1
        last, line = _record_line(self.records, "input", head, raw)
        lines.append(line)
        for n, decision in enumerate(decisions, 1):
            draw = json.dumps(decision["payload"], separators=COMPACT, allow_nan=False)
            dhead = {"id": "{}:decision:{}".format(envelope["id"], n), "type": decision["type"], "envelope_version": 1,
                     "schema_version": 1, "event_time": envelope["event_time"], "recorded_at": envelope["recorded_at"],
                     "sequence": envelope["sequence"], "source": "engine", "strategy_version": self.strategy_version,
                     "configuration_hash": self.configuration_hash, "causation_id": envelope["id"],
                     "payload_hash": hashlib.sha256(draw.encode()).hexdigest()}
            self.records += 1
            last, line = _record_line(self.records, "decision", dhead, draw)
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
