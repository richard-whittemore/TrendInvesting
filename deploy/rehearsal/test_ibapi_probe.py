"""Tests for deploy/ibapi-probe.py, the owner's check that the Gateway's API port refuses a client.

A local fake server on 127.0.0.1 replies like the Gateway, closes, resets or
stays silent, and a closed port refuses; the probe must report EXPOSED (exit 1),
NOT-SERVED with a reason (exit 0) or ERROR (exit 2), and send nothing after
the opening handshake. Synthetic traffic only: no IBKR Gateway is needed.
"""
import importlib.util
import socket
import struct
import subprocess
import sys
import threading
import time
import unittest
from unittest import mock
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

    def test_close_without_reply_is_not_served(self):
        self.assertEqual(self.run_against("close")[0], "NOT-SERVED closed")

    def test_reset_is_not_served(self):
        self.assertEqual(self.run_against("reset")[0], "NOT-SERVED closed")

    def test_refused_connection_is_not_served(self):
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        self.assertEqual(probe.probe("127.0.0.1", port, 1.0), "NOT-SERVED refused")

    def test_silence_is_not_served(self):
        self.assertEqual(self.run_against("silent", timeout=0.3)[0], "NOT-SERVED silent")

    def test_unresolvable_host_is_an_error(self):
        self.assertEqual(probe.probe("no-such-host.invalid", 4002, 1.0), "ERROR lookup")

    def test_slow_resolver_is_bounded_by_the_timeout(self):
        release = threading.Event()

        def slow(*args, **kwargs):
            release.wait(10)
            return []

        start = time.monotonic()
        result = probe.probe("slow.example", 4002, 0.5, resolver=slow)
        elapsed = time.monotonic() - start
        release.set()
        self.assertEqual(result, "ERROR lookup")
        self.assertLess(elapsed, 3.0)

    def test_socket_creation_failure_tries_the_next_address(self):
        fake = Fake("reply")
        real = socket.socket
        calls = []

        def flaky(family=-1, *args, **kwargs):
            calls.append(family)
            if family == socket.AF_INET6:
                raise OSError("address family not supported")
            return real(family, *args, **kwargs)

        def resolver(host, port, type=0):
            return [(socket.AF_INET6, type, 6, "", ("::1", port, 0, 0)),
                    (socket.AF_INET, type, 6, "", ("127.0.0.1", port))]

        with mock.patch.object(socket, "socket", flaky):
            result = probe.probe("both.example", fake.port, 1.0, resolver=resolver)
        fake.finish()
        self.assertEqual(result, "EXPOSED")
        self.assertEqual(calls[0], socket.AF_INET6)

    def test_no_socket_at_all_is_an_error_not_exposed(self):
        def resolver(host, port, type=0):
            return [(socket.AF_INET, type, 6, "", ("127.0.0.1", port))]

        with mock.patch.object(socket, "socket", side_effect=OSError("too many open files")):
            self.assertEqual(probe.probe("x.example", 4002, 1.0, resolver=resolver),
                             "ERROR no usable address")

    def test_an_expired_deadline_stops_trying_further_addresses(self):
        attempts = []

        class Slow:
            def __init__(self, *a):
                pass

            def settimeout(self, t):
                pass

            def connect(self, addr):
                attempts.append(addr)
                time.sleep(0.3)
                raise socket.timeout("timed out")

            def close(self):
                pass

        def resolver(host, port, type=0):
            return [(socket.AF_INET, type, 6, "", ("192.0.2.%d" % i, port)) for i in range(1, 6)]

        with mock.patch.object(socket, "socket", Slow):
            result = probe.probe("many.example", 4002, 0.2, resolver=resolver)
        self.assertEqual(result, "NOT-SERVED silent")
        self.assertEqual(len(attempts), 1)

    def test_bad_arguments_are_usage_errors_not_exposed(self):
        for argv in (["127.0.0.1", "4002", "--timeout", "-1"],
                     ["127.0.0.1", "4002", "--timeout", "nan"],
                     ["127.0.0.1", "4002", "--timeout", "inf"],
                     ["127.0.0.1", "4002", "--timeout", "0"],
                     ["127.0.0.1", "4002", "--timeout", "1e20"],
                     ["127.0.0.1", "4002", "--timeout", "301"],
                     ["127.0.0.1", "70000"],
                     ["127.0.0.1", "0"],
                     ["127.0.0.1", "-5"]):
            done = subprocess.run([sys.executable, str(SCRIPT), *argv],
                                  capture_output=True, text=True, timeout=20)
            self.assertEqual(done.returncode, 2, argv)
            self.assertIn("usage", done.stderr, argv)

    def test_exit_codes(self):
        for behaviour, code in (("reply", 1), ("close", 0), ("silent", 0)):
            fake = Fake(behaviour)
            done = subprocess.run(
                [sys.executable, str(SCRIPT), "127.0.0.1", str(fake.port), "--timeout", "0.4"],
                capture_output=True, text=True, timeout=20)
            fake.finish()
            self.assertEqual(done.returncode, code, behaviour)
            self.assertIn({1: "EXPOSED", 0: "NOT-SERVED"}[code], done.stdout)

    def test_a_lookup_error_exits_2(self):
        done = subprocess.run([sys.executable, str(SCRIPT), "no-such-host.invalid", "4002", "--timeout", "2"],
                              capture_output=True, text=True, timeout=20)
        self.assertEqual(done.returncode, 2)
        self.assertIn("ERROR", done.stdout)

    def test_usage_error_exits_2(self):
        done = subprocess.run([sys.executable, str(SCRIPT)], capture_output=True, text=True, timeout=20)
        self.assertEqual(done.returncode, 2)


if __name__ == "__main__":
    unittest.main()
