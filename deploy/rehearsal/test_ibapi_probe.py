"""Tests for deploy/ibapi-probe.py, the owner's check that the Gateway's API port refuses a client.

A local fake server on 127.0.0.1 replies like the Gateway, closes, resets or
stays silent, and a closed port refuses; the probe must report EXPOSED,
REJECTED or INCONCLUSIVE with exit codes 1, 0 and 2, and send nothing after
the opening handshake. Synthetic traffic only: no IBKR Gateway is needed.
"""
import importlib.util
import socket
import struct
import subprocess
import sys
import threading
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "ibapi-probe.py"
spec = importlib.util.spec_from_file_location("ibapi_probe", SCRIPT)
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class Fake:
    """One-shot server; behaviour is 'reply', 'close', 'reset' or 'silent'."""

    def __init__(self, behaviour):
        self.behaviour = behaviour
        self.received = b""
        self.after = b""
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(1)
        self.port = self.sock.getsockname()[1]
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def serve(self):
        conn, _ = self.sock.accept()
        conn.settimeout(3)
        try:
            want = 4 + 4 + len(probe.VERSIONS)
            while len(self.received) < want:
                chunk = conn.recv(want - len(self.received))
                if not chunk:
                    break
                self.received += chunk
            if self.behaviour == "reply":
                body = b"187\x0020260101 00:00:00 EST\x00"
                conn.sendall(struct.pack(">I", len(body)) + body)
            elif self.behaviour == "reset":
                conn.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                conn.close()
                return
            elif self.behaviour == "close":
                conn.close()
                return
            try:
                self.after = conn.recv(4096)  # what the probe sends next, if anything
            except OSError:
                pass
        finally:
            conn.close()

    def finish(self):
        self.thread.join(5)
        self.sock.close()


class ProbeTest(unittest.TestCase):
    def run_against(self, behaviour, timeout=1.0):
        fake = Fake(behaviour)
        result = probe.probe("127.0.0.1", fake.port, timeout)
        fake.finish()
        return result, fake

    def test_server_version_reply_is_exposed(self):
        result, fake = self.run_against("reply")
        self.assertEqual(result, "EXPOSED")
        self.assertEqual(fake.received, b"API\x00" + struct.pack(">I", len(probe.VERSIONS)) + probe.VERSIONS)
        self.assertEqual(fake.after, b"", "nothing may follow the handshake")

    def test_close_without_reply_is_rejected(self):
        self.assertEqual(self.run_against("close")[0], "REJECTED")

    def test_reset_is_rejected(self):
        self.assertEqual(self.run_against("reset")[0], "REJECTED")

    def test_refused_connection_is_rejected(self):
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        self.assertEqual(probe.probe("127.0.0.1", port, 1.0), "REJECTED")

    def test_silence_is_inconclusive(self):
        self.assertEqual(self.run_against("silent", timeout=0.3)[0], "INCONCLUSIVE")

    def test_unresolvable_host_is_inconclusive(self):
        self.assertEqual(probe.probe("no-such-host.invalid", 4002, 1.0), "INCONCLUSIVE")

    def test_exit_codes(self):
        for behaviour, code in (("reply", 1), ("close", 0), ("silent", 2)):
            fake = Fake(behaviour)
            done = subprocess.run(
                [sys.executable, str(SCRIPT), "127.0.0.1", str(fake.port), "--timeout", "0.4"],
                capture_output=True, text=True, timeout=20)
            fake.finish()
            self.assertEqual(done.returncode, code, behaviour)
            self.assertIn({1: "EXPOSED", 0: "REJECTED", 2: "INCONCLUSIVE"}[code], done.stdout)

    def test_usage_error_exits_2(self):
        done = subprocess.run([sys.executable, str(SCRIPT)], capture_output=True, text=True, timeout=20)
        self.assertEqual(done.returncode, 2)


if __name__ == "__main__":
    unittest.main()
