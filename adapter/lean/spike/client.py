"""Python side of the LEAN-to-Go transport spike (issue #26).

This is throwaway measurement code, not the adapter. It exists to answer one
question with numbers: can a QCAlgorithm running inside the LEAN Docker
container exchange a completed-bar envelope and a decision envelope with a Go
process, per bar, reliably enough for a daily-bar strategy?

The wire contract mirrors the Go package `transport`:

  request  one JSON-encoded envelope, then "\\n"
  reply    one JSON object {"envelope": ...} or {"error": {...}}, then "\\n"

Kept compatible with Python 3.9 so the same file runs on the host and inside
the LEAN container without change.
"""

import hashlib
import json
import socket
import time

# Mirrors transport.DefaultMaxFrameBytes.
DEFAULT_MAX_FRAME_BYTES = 1 << 20

# Mirrors the protocol error codes in transport/protocol.go.
CODE_INVALID_FRAME = "invalid_frame"
CODE_INVALID_ENVELOPE = "invalid_envelope"
CODE_OVERSIZED = "oversized"
CODE_DECIDER_FAILED = "decider_failed"
CODE_TIMEOUT = "timeout"
CODE_UNAVAILABLE = "unavailable"

STRATEGY_VERSION = "transport-spike"

# json.dumps must use these separators everywhere. The payload hash attests the
# payload bytes exactly as they are stored, so the bytes hashed on their own
# must be byte-identical to the bytes embedded in the envelope. With the same
# separators and insertion-ordered keys, dumping the payload alone yields
# exactly the substring that dumping the envelope produces.
_SEPARATORS = (",", ":")


class Unavailable(Exception):
    """The decision engine could not be reached, or stopped answering.

    This is the condition the real adapter must treat as "submit no new
    orders" (docs/architecture.md). It is deliberately distinct from Rejected.
    """


class Rejected(Exception):
    """The engine is alive and refused one bar."""

    def __init__(self, code, message, causation_id=""):
        super().__init__("{}: {}".format(code, message))
        self.code = code
        self.message = message
        self.causation_id = causation_id


class OutOfOrder(Exception):
    """A reply arrived that does not answer the request just sent."""


def rfc3339(seconds=None):
    """Format a UTC timestamp the way Go's encoding/json parses time.Time."""
    if seconds is None:
        seconds = time.time()
    whole = int(seconds)
    micros = int(round((seconds - whole) * 1_000_000))
    if micros >= 1_000_000:  # rounding carried into the next second
        whole += 1
        micros = 0
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(whole)) + ".{:06d}Z".format(micros)


def bar_payload(universe, sequence):
    """Build the same bar-slice payload the Go harness builds.

    Prices are arithmetic, not random, so two runs send identical bytes and a
    latency difference is a property of the transport rather than of the data.
    """
    bars = []
    for i in range(universe):
        spread = ((sequence % 1000) * 7919 + i * 104729) % 18000
        base = 20 + spread / 100
        bars.append(
            {
                "symbol": "SPK{:04d}".format(i),
                "open": round(base, 2),
                "high": round(base * 1.012, 2),
                "low": round(base * 0.991, 2),
                "close": round(base * 1.004, 2),
                "volume": 100000 + (i * 13) % 900000,
            }
        )
    return {"as_of": "2026-09-09", "bars": bars}


def envelope(event_type, sequence, payload, source="lean-adapter", event_id=None):
    """Wrap a payload in the event envelope defined in internal/event."""
    payload_bytes = json.dumps(payload, separators=_SEPARATORS).encode("utf-8")
    stamp = rfc3339()
    return {
        "id": event_id or "bar-{}".format(sequence),
        "type": event_type,
        "schema_version": 1,
        "event_time": stamp,
        "recorded_at": stamp,
        "sequence": sequence,
        "correlation_id": "spike-run",
        "source": source,
        "strategy_version": STRATEGY_VERSION,
        "configuration_hash": "spike",
        "payload_hash": hashlib.sha256(payload_bytes).hexdigest(),
        "payload": payload,
    }


class Client:
    """A sequential request/response client over a Unix-domain socket.

    One exchange at a time, which is what a single-threaded QCAlgorithm needs.
    A connection is abandoned whenever an exchange does not complete: nothing
    on the wire pairs a request with a reply beyond ordering, so reusing a
    connection after a timeout would read the abandoned reply as the answer to
    the next bar.
    """

    def __init__(self, path, timeout=None, max_frame_bytes=DEFAULT_MAX_FRAME_BYTES):
        self.path = path
        self.max_frame_bytes = max_frame_bytes
        self._broken = False
        self._buffer = b""
        try:
            self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self._sock.settimeout(timeout)
            self._sock.connect(path)
        except OSError as err:
            raise Unavailable("connect {}: {}".format(path, err)) from err

    def close(self):
        try:
            self._sock.close()
        except OSError:
            pass

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def decide(self, bar):
        """Send one bar envelope and return the decision envelope."""
        if self._broken:
            raise Unavailable("connection abandoned by an earlier failure")
        try:
            return self._exchange(bar)
        except Rejected:
            # The engine answered, so the stream is still in step.
            raise
        except Exception:
            self._broken = True
            self.close()
            raise

    def _exchange(self, bar):
        frame = json.dumps(bar, separators=_SEPARATORS).encode("utf-8") + b"\n"
        if len(frame) > self.max_frame_bytes:
            raise ValueError(
                "request is {} bytes, limit is {}".format(len(frame), self.max_frame_bytes)
            )
        try:
            self._sock.sendall(frame)
            line = self._read_line()
        except socket.timeout as err:
            raise Unavailable("no reply within the deadline: {}".format(err)) from err
        except OSError as err:
            raise Unavailable("connection failed: {}".format(err)) from err

        reply = json.loads(line)
        error = reply.get("error")
        if error is not None:
            causation = error.get("causation_id", "")
            if causation and causation != bar["id"]:
                raise OutOfOrder("error names {}, sent {}".format(causation, bar["id"]))
            if error.get("code") == CODE_UNAVAILABLE:
                raise Unavailable(error.get("message", "engine is shutting down"))
            raise Rejected(error.get("code", ""), error.get("message", ""), causation)

        decision = reply.get("envelope")
        if decision is None:
            raise Unavailable("reply carries neither a decision nor an error")
        if decision.get("causation_id") != bar["id"]:
            raise OutOfOrder(
                "decision cites {}, sent {}".format(decision.get("causation_id"), bar["id"])
            )
        return decision

    def _read_line(self):
        while b"\n" not in self._buffer:
            chunk = self._sock.recv(65536)
            if not chunk:
                raise Unavailable("engine closed the connection mid-exchange")
            self._buffer += chunk
            if len(self._buffer) > self.max_frame_bytes:
                raise Unavailable("reply exceeds the maximum frame size")
        line, _, self._buffer = self._buffer.partition(b"\n")
        return line
