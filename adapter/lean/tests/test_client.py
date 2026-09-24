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

    def test_decide_round_trip_success(self):
        def handler(conn):
            line = conn.recv(4096).decode()
            req = json.loads(line)
            reply = {
                "envelope": {
                    "id": "decisions:" + req["id"],
                    "type": "engine.decisions",
                    "causation_id": req["id"],
                    "sequence": req["sequence"],
                    "payload": {"decisions": []},
                }
            }
            conn.sendall(json.dumps(reply).encode() + b"\n")

        th = self._serve_one(handler)
        with Client(self.sock_path, timeout=2.0) as client:
            bar = {"id": "bar-2", "sequence": 2}
            decision = client.decide(bar)
            self.assertEqual(decision["causation_id"], "bar-2")
        th.join()

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

    def test_oversized_request_rejected_locally(self):
        client = Client.__new__(Client)
        client.max_frame_bytes = 100
        client._broken = False
        client._sock = None
        with self.assertRaises(ValueError):
            client.decide({"huge": "x" * 200})


if __name__ == "__main__":
    unittest.main()
