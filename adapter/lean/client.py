"""Sequential Unix-socket transport reused from the measured ADR 0014 spike."""
import hashlib
import json
import socket

DEFAULT_MAX_FRAME_BYTES = 1 << 20
CODE_UNAVAILABLE = "unavailable"
_SEPARATORS = (",", ":")
_PAYLOAD_KEY = b'"payload":'


def raw_payload(line):
    """Return the exact bytes of the decision envelope's own payload.

    event.HashPayload hashes the payload's raw bytes, so the check must see
    those bytes, not a re-serialisation: Python's JSON encoder does not
    reproduce Go's float formatting or HTML escaping. The first unescaped
    '"payload":' in the reply is the envelope's own key — "payload_hash"
    does not match it, a quote inside a string value is always escaped, and
    every nested decision's payload lies inside this one, after it.
    """
    at = line.find(_PAYLOAD_KEY)
    if at < 0:
        raise Unavailable("decision envelope carries no payload")
    text = line[at + len(_PAYLOAD_KEY):].decode("utf-8").lstrip(" \t\r\n")
    _, end = json.JSONDecoder().raw_decode(text)
    return text[:end].encode("utf-8")

class Unavailable(Exception):
    """The engine cannot safely answer this stream."""

class Rejected(Exception):
    """The engine rejected an input; the adapter must stop the run."""
    def __init__(self, code, message, causation_id=""):
        super().__init__("{}: {}".format(code, message))
        self.code = code
        self.causation_id = causation_id

class OutOfOrder(Exception):
    """The response does not name the input just sent."""

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
            self.close()
            raise Unavailable("connect {}: {}".format(path, err)) from err

    def close(self):
        try:
            sock = getattr(self, "_sock", None)
            if sock is not None:
                sock.close()
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
        frame = json.dumps(bar, separators=_SEPARATORS, allow_nan=False).encode("utf-8") + b"\n"
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
            self.close()
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
        # The envelope's own integrity field (event.Envelope.Validate's
        # PayloadHash rule), checked before any decision inside it is used.
        if hashlib.sha256(raw_payload(line)).hexdigest() != decision.get("payload_hash"):
            raise Unavailable("decision payload does not match its payload_hash")
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
