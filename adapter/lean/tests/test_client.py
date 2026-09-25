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

from client import Client, Unavailable, Rejected, OutOfOrder, CODE_UNAVAILABLE


class ClientTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.sock_path = os.path.join(self.tmpdir.name, "test.sock")

    def tearDown(self):
        self.tmpdir.cleanup()

    def _serve_one(self, handler):
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(self.sock_path)
        server.listen(1)

        def run():
            conn, _ = server.accept()
            try:
                handler(conn)
            finally:
                conn.close()
                server.close()

        thread = threading.Thread(target=run)
        thread.start()
        return thread

    def test_connect_fails_closed_when_absent(self):
        with self.assertRaises(Unavailable):
            Client(self.sock_path, timeout=1.0)

    def _reply_line(self, req, payload_bytes, payload_hash=None):
        """Build a reply the way cmd/engine does: payload_hash over the exact
        payload bytes placed on the wire."""
        if payload_hash is None:
            payload_hash = hashlib.sha256(payload_bytes).hexdigest()
        head = json.dumps({"id": "decisions:" + req["id"], "type": "engine.decisions",
                           "causation_id": req["id"], "sequence": req["sequence"],
                           "payload_hash": payload_hash}, separators=(",", ":"))
        return b'{"envelope":' + head[:-1].encode() + b',"payload":' + payload_bytes + b"}}\n"

    def _round_trip(self, payload_bytes, payload_hash=None):
        def handler(conn):
            req = json.loads(conn.recv(4096).decode())
            conn.sendall(self._reply_line(req, payload_bytes, payload_hash))
        th = self._serve_one(handler)
        try:
            with Client(self.sock_path, timeout=2.0) as client:
                return client.decide({"id": "bar-2", "sequence": 2})
        finally:
            th.join()

    def test_decide_round_trip_success(self):
        decision = self._round_trip(b'{"decisions":[]}')
        self.assertEqual(decision["causation_id"], "bar-2")

    def test_payload_hash_is_checked_over_the_bytes_received(self):
        """Go's encoder escapes '<' as \\u003c; a Python re-serialisation would
        write '<' and hash different bytes. The nested decision's own
        "payload" key must not be mistaken for the envelope's."""
        payload = b'{"decisions":[{"id":"d1","payload":{"note":"a\\u003cb","x":1e-07}}]}'
        self.assertNotEqual(json.dumps(json.loads(payload), separators=(",", ":")).encode(), payload)
        decision = self._round_trip(payload)
        self.assertEqual(decision["payload"]["decisions"][0]["payload"]["note"], "a<b")

    def test_payload_that_does_not_match_its_hash_is_refused(self):
        with self.assertRaises(Unavailable):
            self._round_trip(b'{"decisions":[]}', payload_hash=hashlib.sha256(b"other").hexdigest())

    def test_decide_rejected_preserves_connection_state(self):
        def handler(conn):
            conn.recv(4096)
            reply = {
                "error": {
                    "code": "non_contiguous_sequence",
                    "message": "sequence gap",
                    "causation_id": "bar-2",
                }
            }
            conn.sendall(json.dumps(reply).encode() + b"\n")

        th = self._serve_one(handler)
        with Client(self.sock_path, timeout=2.0) as client:
            with self.assertRaises(Rejected) as cm:
                client.decide({"id": "bar-2", "sequence": 2})
            self.assertEqual(cm.exception.code, "non_contiguous_sequence")
            self.assertEqual(cm.exception.causation_id, "bar-2")
            self.assertFalse(client._broken)
        th.join()

    def test_decide_unavailable_code_raises_unavailable(self):
        def handler(conn):
            conn.recv(4096)
            reply = {
                "error": {
                    "code": CODE_UNAVAILABLE,
                    "message": "engine shutting down",
                    "causation_id": "bar-2",
                }
            }
            conn.sendall(json.dumps(reply).encode() + b"\n")

        th = self._serve_one(handler)
        with Client(self.sock_path, timeout=2.0) as client:
            with self.assertRaises(Unavailable):
                client.decide({"id": "bar-2", "sequence": 2})
            self.assertTrue(client._broken)
        th.join()

    def test_decide_causation_mismatch_raises_out_of_order(self):
        def handler(conn):
            conn.recv(4096)
            reply = {
                "envelope": {
                    "id": "decisions:bar-wrong",
                    "type": "engine.decisions",
                    "causation_id": "bar-wrong",
                    "sequence": 2,
                    "payload": {"decisions": []},
                }
            }
            conn.sendall(json.dumps(reply).encode() + b"\n")

        th = self._serve_one(handler)
        with Client(self.sock_path, timeout=2.0) as client:
            with self.assertRaises(OutOfOrder):
                client.decide({"id": "bar-2", "sequence": 2})
            self.assertTrue(client._broken)
        th.join()

    def test_decide_broken_connection_abandoned(self):
        def handler(conn):
            conn.close()

        th = self._serve_one(handler)
        client = Client(self.sock_path, timeout=2.0)
        with self.assertRaises(Unavailable):
            client.decide({"id": "bar-2", "sequence": 2})
        self.assertTrue(client._broken)
        with self.assertRaises(Unavailable) as cm:
            client.decide({"id": "bar-3", "sequence": 3})
        self.assertIn("abandoned", str(cm.exception))
        client.close()
        th.join()

    def test_a_slow_engine_times_out_and_the_connection_is_abandoned(self):
        """Issue #31's 'timeout' case (ADR 0014's measured failure table:
        'Slow engine, adapter's deadline first' -> safe mode). The server
        accepts the connection but never answers; the client's own deadline
        fires first, raising Unavailable rather than blocking forever, and
        the connection is abandoned exactly like any other failed exchange:
        it is never reused for a later input."""
        released = threading.Event()

        def handler(conn):
            conn.recv(4096)
            released.wait(2.0)
            conn.close()

        th = self._serve_one(handler)
        client = Client(self.sock_path, timeout=0.1)
        try:
            with self.assertRaises(Unavailable) as cm:
                client.decide({"id": "bar-2", "sequence": 2})
            self.assertIn("deadline", str(cm.exception))
            self.assertTrue(client._broken)
            with self.assertRaises(Unavailable) as cm:
                client.decide({"id": "bar-3", "sequence": 3})
            self.assertIn("abandoned", str(cm.exception))
        finally:
            released.set()
            client.close()
            th.join()

    def test_undecodable_json_reply_fails_closed_without_reuse(self):
        """A corrupted reply (issue #31: 'corrupted payload') is refused with
        a clear, typed error, never a bare json.JSONDecodeError, and the
        connection is abandoned exactly as any other failed exchange is: the
        next call is refused too, never silently retried on the same
        connection (ADR 0014)."""
        def handler(conn):
            conn.recv(4096)
            conn.sendall(b"not json at all\n")

        th = self._serve_one(handler)
        client = Client(self.sock_path, timeout=2.0)
        with self.assertRaises(Unavailable) as cm:
            client.decide({"id": "bar-2", "sequence": 2})
        self.assertIn("not valid JSON", str(cm.exception))
        self.assertTrue(client._broken)
        with self.assertRaises(Unavailable) as cm:
            client.decide({"id": "bar-3", "sequence": 3})
        self.assertIn("abandoned", str(cm.exception))
        client.close()
        th.join()

    def _reply_shape(self, raw_json_line):
        """A single exchange whose reply is exactly raw_json_line: valid JSON,
        of whatever shape the caller wants to probe."""
        def handler(conn):
            conn.recv(4096)
            conn.sendall(raw_json_line + b"\n")

        th = self._serve_one(handler)
        client = Client(self.sock_path, timeout=2.0)
        try:
            with self.assertRaises(Unavailable) as cm:
                client.decide({"id": "bar-2", "sequence": 2})
            self.assertIn("must be a JSON object", str(cm.exception))
            self.assertTrue(client._broken)
            with self.assertRaises(Unavailable) as cm:
                client.decide({"id": "bar-3", "sequence": 3})
            self.assertIn("abandoned", str(cm.exception))
        finally:
            client.close()
            th.join()

    def test_a_reply_that_is_a_json_array_fails_closed_without_reuse(self):
        """Valid JSON of the wrong shape (issue #31 review) is exactly as
        unusable as no reply at all: every field access below assumes a JSON
        object, and this must fail closed with a clear, typed error rather
        than a bare AttributeError from the first .get call."""
        self._reply_shape(b"[]")

    def test_a_reply_that_is_json_null_fails_closed_without_reuse(self):
        self._reply_shape(b"null")

    def test_a_reply_that_is_a_json_string_fails_closed_without_reuse(self):
        self._reply_shape(b'"not an object"')

    def test_a_reply_whose_error_field_is_the_wrong_type_fails_closed(self):
        """The top-level reply is a well-formed object, but its "error" is a
        string rather than the object every other reply's error is; the
        shape check must catch this nested case too, not only the top level."""
        def handler(conn):
            conn.recv(4096)
            conn.sendall(b'{"error": "boom"}\n')

        th = self._serve_one(handler)
        client = Client(self.sock_path, timeout=2.0)
        with self.assertRaises(Unavailable) as cm:
            client.decide({"id": "bar-2", "sequence": 2})
        self.assertIn("error", str(cm.exception))
        self.assertIn("must be a JSON object", str(cm.exception))
        self.assertTrue(client._broken)
        client.close()
        th.join()

    def test_a_reply_whose_envelope_field_is_the_wrong_type_fails_closed(self):
        def handler(conn):
            conn.recv(4096)
            conn.sendall(b'{"envelope": "boom"}\n')

        th = self._serve_one(handler)
        client = Client(self.sock_path, timeout=2.0)
        with self.assertRaises(Unavailable) as cm:
            client.decide({"id": "bar-2", "sequence": 2})
        self.assertIn("envelope", str(cm.exception))
        self.assertIn("must be a JSON object", str(cm.exception))
        self.assertTrue(client._broken)
        client.close()
        th.join()

    def test_oversized_request_rejected_locally(self):
        client = Client.__new__(Client)
        client.max_frame_bytes = 100
        client._broken = False
        client._sock = None
        with self.assertRaises(ValueError):
            client.decide({"huge": "x" * 200})


if __name__ == "__main__":
    unittest.main()
