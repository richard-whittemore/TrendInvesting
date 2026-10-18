"""The adapter's side of the engine socket (ADR 0014; ADR 0025 point 2): one exchange at a time, fail closed."""
import hashlib
import json
import os
import socket
import sys
import tempfile
import threading
import unittest
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
adapter_dir = str(Path(__file__).resolve().parents[1])
for p in (tests_dir, adapter_dir):
    if p not in sys.path:
        sys.path.insert(0, p)

from engine_client import EngineClient, EngineRejected, connector
from relay import EngineUnavailable

INPUT = {"id": "ibkr:1", "type": "execution.fill", "envelope_version": 1, "schema_version": 5, "sequence": 4,
         "event_time": "2026-10-17T13:30:01Z", "recorded_at": "2026-10-17T13:30:02Z", "source": "ibkr",
         "strategy_version": "build-1", "configuration_hash": "c" * 64, "payload_hash": "0" * 64,
         "payload": {"fill_id": "x"}}


def reply_line(req, payload=b'{"decisions":[]}', **overrides):
    """A reply as cmd/engine writes one: the decision envelope, payload last, hashed over its exact bytes."""
    head = {"id": "decisions:" + req["id"], "type": "engine.decisions", "envelope_version": 1, "schema_version": 1,
            "causation_id": req["id"], "sequence": req["sequence"], "strategy_version": req["strategy_version"],
            "configuration_hash": req["configuration_hash"], "payload_hash": hashlib.sha256(payload).hexdigest()}
    head.update(overrides)
    text = json.dumps(head, separators=(",", ":"))
    return b'{"envelope":' + text[:-1].encode() + b',"payload":' + payload + b"}}\n"


class EngineClientTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.path = os.path.join(self.tmpdir.name, "e.sock")
        self.received = []

    def serve(self, *answers):
        """Serve one connection, answering each request with the next of answers (bytes, or a callable of it)."""
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(self.path)
        server.listen(1)

        def run():
            conn, _ = server.accept()
            buf = b""
            try:
                for answer in answers:
                    while b"\n" not in buf:
                        chunk = conn.recv(65536)
                        if not chunk:
                            return
                        buf += chunk
                    line, _, buf = buf.partition(b"\n")
                    req = json.loads(line)
                    self.received.append(req)
                    out = answer(req) if callable(answer) else answer
                    if out is None:
                        return
                    conn.sendall(out)
            finally:
                conn.close()
                server.close()
        thread = threading.Thread(target=run, daemon=True)
        thread.start()
        self.addCleanup(thread.join, 2)

    def test_one_exchange_returns_the_decision_envelope(self):
        self.serve(lambda req: reply_line(req, b'{"decisions":[{"type":"x","payload":{}}]}'))
        client = EngineClient(self.path, timeout=2)
        reply = client.decide(INPUT)
        self.assertEqual(reply["payload"], {"decisions": [{"type": "x", "payload": {}}]})
        self.assertEqual(self.received, [INPUT])
        client.close()

    def test_no_engine_listening_is_unavailable(self):
        with self.assertRaises(EngineUnavailable):
            EngineClient(self.path, timeout=1)
        with self.assertRaises(EngineUnavailable):
            connector(self.path, 1)()

    def test_a_refusal_is_the_engines_answer_and_stops_the_adapter(self):
        self.serve(lambda req: json.dumps({"error": {"code": "invalid", "message": "no", "causation_id": req["id"]}})
                   .encode() + b"\n")
        with self.assertRaises(EngineRejected) as caught:
            EngineClient(self.path, timeout=2).decide(INPUT)
        self.assertEqual(caught.exception.code, "invalid")
        self.assertNotIsInstance(caught.exception, EngineUnavailable)

    def test_an_engine_shutting_down_is_unavailable(self):
        self.serve(lambda req: json.dumps({"error": {"code": "unavailable", "message": "stopping"}}).encode() + b"\n")
        with self.assertRaises(EngineUnavailable):
            EngineClient(self.path, timeout=2).decide(INPUT)

    def test_a_reply_that_does_not_answer_this_input_is_unavailable_and_the_connection_is_abandoned(self):
        cases = {
            "another input's reply": dict(causation_id="ibkr:0"),
            "not decisions": dict(type="engine.other"),
            "another run": dict(configuration_hash="d" * 64),
            "another build": dict(strategy_version="build-2"),
            "a payload that is not its hash": dict(payload_hash="0" * 64),
        }
        for name, change in cases.items():
            with self.subTest(name):
                if os.path.exists(self.path):
                    os.unlink(self.path)
                self.serve(lambda req, change=change: reply_line(req, **change))
                client = EngineClient(self.path, timeout=2)
                with self.assertRaises(EngineUnavailable):
                    client.decide(INPUT)
                with self.assertRaises(EngineUnavailable):
                    client.decide(INPUT)

    def test_a_reply_that_is_not_json_or_never_comes_is_unavailable(self):
        for answer in (b"not json\n", b"[]\n", b'{"envelope":null}\n', None):
            with self.subTest(answer=answer):
                if os.path.exists(self.path):
                    os.unlink(self.path)
                self.serve(answer)
                with self.assertRaises(EngineUnavailable):
                    EngineClient(self.path, timeout=2).decide(INPUT)


if __name__ == "__main__":
    unittest.main()
