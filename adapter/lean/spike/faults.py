"""Exercise the failure modes the ADR has to record, from the Python side.

    python3 faults.py --engine /path/to/transport-spike

Each case starts (or deliberately does not start) its own engine, so the run is
self-contained and repeatable. Every case prints PASS or FAIL with what was
actually observed, because the point of a spike is the observation, not the
assertion.
"""

import argparse
import os
import signal
import socket
import subprocess
import sys
import tempfile
import time

import client as spike

RESULTS = []


def record(name, expectation, observed, ok):
    RESULTS.append((name, expectation, observed, ok))
    print("{:<28} {:<4} expected {:<34} observed {}".format(
        name, "PASS" if ok else "FAIL", expectation, observed))


class Engine:
    """A transport-spike server process on its own socket."""

    def __init__(self, binary, extra_args=()):
        self.dir = tempfile.mkdtemp(prefix="fault-", dir="/tmp")
        self.socket_path = os.path.join(self.dir, "s.sock")
        self.proc = subprocess.Popen(
            [binary, "serve", "--socket", self.socket_path] + list(extra_args),
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        )
        self._await_socket()

    def _await_socket(self, deadline_s=10.0):
        end = time.time() + deadline_s
        while time.time() < end:
            if os.path.exists(self.socket_path):
                probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                try:
                    probe.connect(self.socket_path)
                    return
                except OSError:
                    pass
                finally:
                    probe.close()
            time.sleep(0.02)
        raise RuntimeError("engine did not start listening on {}".format(self.socket_path))

    def kill(self):
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGKILL)
            self.proc.wait()

    def stop(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            self.proc.wait()


def bar(sequence, universe=100):
    return spike.envelope("market.bar.completed", sequence,
                          spike.bar_payload(universe, sequence))


def case_engine_absent(_binary):
    """The Go side was never started."""
    path = os.path.join(tempfile.mkdtemp(prefix="absent-", dir="/tmp"), "s.sock")
    try:
        spike.Client(path)
    except spike.Unavailable as err:
        record("engine absent", "Unavailable at connect", type(err).__name__, True)
        return
    record("engine absent", "Unavailable at connect", "connected anyway", False)


def case_engine_killed_mid_run(binary):
    """The Go side is SIGKILLed between bars."""
    engine = Engine(binary)
    try:
        conn = spike.Client(engine.socket_path, timeout=10)
        conn.decide(bar(1))
        engine.kill()
        try:
            conn.decide(bar(2))
        except spike.Unavailable as err:
            record("engine killed mid-run", "Unavailable on next bar", str(err)[:40], True)
            return
        record("engine killed mid-run", "Unavailable on next bar", "succeeded anyway", False)
    finally:
        engine.kill()


def case_engine_restarted(binary):
    """The Go side comes back: an old connection stays dead, a new one works."""
    engine = Engine(binary)
    socket_path = engine.socket_path
    conn = spike.Client(socket_path, timeout=10)
    conn.decide(bar(1))
    engine.kill()
    try:
        conn.decide(bar(2))
    except spike.Unavailable:
        pass
    # The killed process left its socket file behind; a new engine must reclaim it.
    replacement = subprocess.Popen(
        [binary, "serve", "--socket", socket_path],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    try:
        deadline = time.time() + 10
        reconnected = None
        while time.time() < deadline and reconnected is None:
            try:
                reconnected = spike.Client(socket_path, timeout=10)
            except spike.Unavailable:
                time.sleep(0.05)
        if reconnected is None:
            record("engine restarted", "new connection succeeds",
                   "could not reconnect", False)
            return
        reconnected.decide(bar(3))
        reconnected.close()
        record("engine restarted", "new connection succeeds",
               "reclaimed the stale socket file", True)
    finally:
        replacement.terminate()
        replacement.wait()


def case_oversized_message(binary):
    """A bar larger than the engine's frame limit."""
    engine = Engine(binary, ["--max-frame", "65536"])
    try:
        conn = spike.Client(engine.socket_path, timeout=30)
        try:
            conn.decide(bar(1, universe=2000))
        except spike.Rejected as err:
            survived = "yes"
            try:
                conn.decide(bar(2, universe=10))
            except Exception as follow_on:  # noqa: BLE001 - reporting, not handling
                survived = "no ({})".format(type(follow_on).__name__)
            record("oversized message", "Rejected(oversized), session survives",
                   "{}, survives={}".format(err.code, survived), err.code == spike.CODE_OVERSIZED)
            return
        record("oversized message", "Rejected(oversized)", "accepted anyway", False)
    finally:
        engine.stop()


def case_slow_engine(binary):
    """The engine answers, but later than the adapter can wait."""
    engine = Engine(binary, ["--stall", "2s"])
    try:
        conn = spike.Client(engine.socket_path, timeout=0.25)
        try:
            conn.decide(bar(1))
        except spike.Unavailable as err:
            reused = "refused"
            try:
                conn.decide(bar(2))
            except spike.Unavailable:
                reused = "refused"
            else:
                reused = "reused (WRONG)"
            record("slow engine, client waits", "Unavailable, connection abandoned",
                   "{}; reuse {}".format(str(err)[:24], reused), reused == "refused")
            return
        record("slow engine, client waits", "Unavailable", "answered in time", False)
    finally:
        engine.stop()


def case_engine_decision_timeout(binary):
    """The engine's own decision timeout fires before the adapter's."""
    engine = Engine(binary, ["--stall", "5s", "--decision-timeout", "200ms"])
    try:
        conn = spike.Client(engine.socket_path, timeout=30)
        try:
            conn.decide(bar(1))
        except spike.Rejected as err:
            record("engine decision timeout", "Rejected(timeout)", err.code,
                   err.code == spike.CODE_TIMEOUT)
            return
        record("engine decision timeout", "Rejected(timeout)", "answered anyway", False)
    finally:
        engine.stop()


def case_engine_started_after_adapter(binary):
    """Startup ordering: the adapter is up first and must wait."""
    path = os.path.join(tempfile.mkdtemp(prefix="order-", dir="/tmp"), "s.sock")
    try:
        spike.Client(path)
        record("adapter starts first", "connect fails until engine binds",
               "connected with no engine", False)
        return
    except spike.Unavailable:
        pass
    proc = subprocess.Popen([binary, "serve", "--socket", path],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    try:
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                conn = spike.Client(path, timeout=10)
            except spike.Unavailable:
                time.sleep(0.05)
                continue
            conn.decide(bar(1))
            conn.close()
            record("adapter starts first", "connect fails until engine binds",
                   "retry loop connected once the engine bound", True)
            return
        record("adapter starts first", "connect fails until engine binds",
               "never connected", False)
    finally:
        proc.terminate()
        proc.wait()


CASES = [
    case_engine_absent,
    case_engine_started_after_adapter,
    case_engine_killed_mid_run,
    case_engine_restarted,
    case_oversized_message,
    case_slow_engine,
    case_engine_decision_timeout,
]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--engine", required=True, help="path to the transport-spike binary")
    args = parser.parse_args(argv)

    for case in CASES:
        try:
            case(args.engine)
        except Exception as err:  # noqa: BLE001 - a spike reports, it does not hide
            record(case.__name__, "no exception", "{}: {}".format(type(err).__name__, err), False)

    failed = [name for name, _, _, ok in RESULTS if not ok]
    print("\n{}/{} cases behaved as specified".format(len(RESULTS) - len(failed), len(RESULTS)))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
