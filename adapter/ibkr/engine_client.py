"""The IBKR adapter's client of the engine's socket (ADR 0014; ADR 0025 point 2).

A Unix-domain socket with newline-delimited JSON, one exchange at a time:
the adapter sends one input envelope and reads one reply, the engine's
decision envelope or an error. Nothing on the wire pairs a reply with its
request beyond order, so a connection is abandoned the moment an exchange
does not complete, and every reply is checked before it is used: it names
the input just sent (causation_id), it is the run's own (configuration hash
and strategy version), it is in an envelope version and an engine.decisions
schema version this adapter reads (ACCEPTED_ENVELOPE_VERSIONS,
ACCEPTED_SCHEMA_VERSIONS: event.CurrentEnvelopeVersion and cmd/engine's
decisionsSchemaVersion; another shape could carry other orders under the
same keys), and its payload's exact bytes match its payload_hash
(event.Envelope.Validate's rule; Go's encoding is not re-created here, so
the bytes are taken from the line as received).

Any failure to get a usable reply raises relay.EngineUnavailable: the relay
keeps the input on disk and asks for it again under its ID (ADR 0027
section 4). An error the engine states about the input raises
EngineRejected, which is its answer, not a lost one: the adapter stops.
"""
import hashlib
import json
import socket

from relay import EngineUnavailable

MAX_FRAME_BYTES = 1 << 20
CODE_UNAVAILABLE = "unavailable"
DECISIONS_TYPE = "engine.decisions"
ACCEPTED_ENVELOPE_VERSIONS = (1,)
ACCEPTED_SCHEMA_VERSIONS = (1,)
_PAYLOAD_KEY = b'"payload":'


class EngineRejected(Exception):
    """The engine refused the input; the adapter stops for a person (the input stays on disk)."""

    def __init__(self, code, message):
        super().__init__("the engine refused the input: {}: {}".format(code, message))
        self.code = code


def _raw_payload(line):
    """The exact bytes of the decision envelope's own payload: the first '"payload":' key, before any nested one."""
    at = line.find(_PAYLOAD_KEY)
    if at < 0:
        raise EngineUnavailable("the engine's reply carries no payload")
    text = line[at + len(_PAYLOAD_KEY):].decode("utf-8")
    _, end = json.JSONDecoder().raw_decode(text)
    return text[:end].encode("utf-8")


class EngineClient:
    """One connection to the engine's socket at path; every socket wait is bounded by timeout seconds."""

    def __init__(self, path, timeout):
        self._buffer = b""
        self._broken = False
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.settimeout(timeout)
        try:
            self._sock.connect(path)
        except OSError as err:
            self.close()
            raise EngineUnavailable("cannot connect to the engine's socket {}: {}".format(
                path, err.strerror or err)) from None

    def close(self):
        self._broken = True
        try:
            self._sock.close()
        except OSError:
            pass

    def decide(self, envelope):
        """Send one input envelope; return the engine's decision envelope for it (see the module's checks)."""
        if self._broken:
            raise EngineUnavailable("the connection to the engine was abandoned by an earlier failure")
        try:
            return self._exchange(envelope)
        except EngineRejected:
            raise
        except EngineUnavailable:
            self.close()
            raise
        except (OSError, ValueError) as err:
            self.close()
            raise EngineUnavailable("the exchange with the engine failed: {}".format(err)) from None

    def _exchange(self, envelope):
        frame = json.dumps(envelope, separators=(",", ":"), allow_nan=False).encode("utf-8") + b"\n"
        if len(frame) > MAX_FRAME_BYTES:
            raise EngineUnavailable("input {} is {} bytes, above the {}-byte frame limit".format(
                envelope.get("id"), len(frame), MAX_FRAME_BYTES))
        self._sock.sendall(frame)
        line = self._read_line()
        reply = json.loads(line)
        if not isinstance(reply, dict):
            raise EngineUnavailable("the engine's reply is not a JSON object")
        error = reply.get("error")
        if error is not None:
            if not isinstance(error, dict):
                raise EngineUnavailable("the engine's error is not a JSON object")
            causation = error.get("causation_id", "")
            if causation and causation != envelope["id"]:
                raise EngineUnavailable("the engine's error names {}, not {}".format(causation, envelope["id"]))
            if error.get("code") == CODE_UNAVAILABLE:
                raise EngineUnavailable(error.get("message", "the engine is shutting down"))
            raise EngineRejected(error.get("code", ""), error.get("message", ""))
        decision = reply.get("envelope")
        if not isinstance(decision, dict):
            raise EngineUnavailable("the engine's reply carries neither a decision envelope nor an error")
        for key, want in (("causation_id", envelope["id"]), ("type", DECISIONS_TYPE),
                          ("configuration_hash", envelope["configuration_hash"]),
                          ("strategy_version", envelope["strategy_version"])):
            if decision.get(key) != want:
                raise EngineUnavailable("the engine's reply has {} {!r}, not {!r}".format(key, decision.get(key), want))
        for key, accepted in (("envelope_version", ACCEPTED_ENVELOPE_VERSIONS),
                              ("schema_version", ACCEPTED_SCHEMA_VERSIONS)):
            value = decision.get(key)
            if not isinstance(value, int) or isinstance(value, bool) or value not in accepted:
                raise EngineUnavailable("the engine's reply has {} {!r}; this adapter reads only {}".format(
                    key, value, ", ".join(map(str, accepted))))
        if hashlib.sha256(_raw_payload(line)).hexdigest() != decision.get("payload_hash"):
            raise EngineUnavailable("the engine's decision payload does not match its payload_hash")
        if not isinstance(decision.get("payload"), dict) or not isinstance(decision["payload"].get("decisions"), list):
            raise EngineUnavailable("the engine's decision payload carries no decisions list")
        return decision

    def _read_line(self):
        while b"\n" not in self._buffer:
            chunk = self._sock.recv(65536)
            if not chunk:
                raise EngineUnavailable("the engine closed the connection mid-exchange")
            self._buffer += chunk
            if len(self._buffer) > MAX_FRAME_BYTES:
                raise EngineUnavailable("the engine's reply exceeds the frame limit")
        line, _, self._buffer = self._buffer.partition(b"\n")
        return line


def connector(path, timeout):
    """The relay's connect: a callable opening a new EngineClient to path (EngineUnavailable if none listens)."""
    return lambda: EngineClient(path, timeout)
