"""Tests for deploy/feed-month.py, the stand-in adapter that replays a synthetic month into an engine in dry-run.

Standard-library Python only; the engine is played by a stand-in socket server that answers each input with an
engine.decisions frame, as cmd/engine's wire contract does (cmd/engine/engine.go).
"""
import hashlib
import contextlib
import io
import importlib.util
import json
import os
from pathlib import Path
import socket
import tempfile
import threading
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("feed_month", os.path.join(HERE, "..", "feed-month.py"))
feed_month = importlib.util.module_from_spec(spec)
spec.loader.exec_module(feed_month)

stream_spec = importlib.util.spec_from_file_location("ira_stream", os.path.join(HERE, "ira-stream.py"))
ira_stream = importlib.util.module_from_spec(stream_spec)
stream_spec.loader.exec_module(ira_stream)

MONTH = os.path.join(HERE, "..", "..", "cmd", "engine", "testdata", "momentum-month.jsonl")


class MonthStreamTest(unittest.TestCase):
    def test_serialized_payload_bytes_match_their_hash(self):
        # ADR 0016 hashes the payload bytes consumed by the engine, including JSON spacing.
        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            root = Path(directory)
            log = root / "synthetic.log"
            log.write_text(json.dumps({"configuration_hash": "sha256:cfg", "strategy_version": "momentum/1"}) + "\n")
            with contextlib.redirect_stdout(io.StringIO()):
                ira_stream.write_month(str(log), str(root / "streams"), MONTH)
            for raw in (root / "streams/synthetic/synthetic-month.jsonl").read_text().splitlines():
                envelope = json.loads(raw)
                payload_start = raw.index('"payload":') + len('"payload":')
                payload_text = raw[payload_start:].lstrip()
                _, end = json.JSONDecoder().raw_decode(payload_text)
                self.assertEqual(envelope["payload_hash"], hashlib.sha256(payload_text[:end].encode()).hexdigest())


    def test_failed_or_interrupted_write_never_publishes_partial_month_and_can_retry(self):
        # AGENTS.md preserves evidence; the stream file contract publishes complete files only.
        for interrupt in (False, True):
            with self.subTest(interrupt=interrupt), tempfile.TemporaryDirectory(dir="/tmp") as directory:
                root = Path(directory)
                log, month = root / "synthetic.log", root / "month.jsonl"
                log.write_text(json.dumps({"configuration_hash": "sha256:cfg", "strategy_version": "momentum/1"}) + "\n")
                row = {"id": "synthetic", "type": "market.session.closed", "event_time": "2025-01-31T21:00:00Z",
                       "payload": {"period_end": "2025-01-31T21:00:00Z"}}
                valid = json.dumps(row) + "\n"
                month.write_text(valid + (valid if interrupt else "malformed\n"))
                target = root / "streams/synthetic/synthetic-month.jsonl"
                loads = json.loads
                calls = 0

                def interrupted_loads(raw):
                    nonlocal calls
                    calls += 1
                    if calls == 3:
                        raise KeyboardInterrupt()
                    return loads(raw)

                with contextlib.redirect_stdout(io.StringIO()):
                    with mock.patch.object(ira_stream.json, "loads", side_effect=interrupted_loads if interrupt else loads):
                        with self.assertRaises(KeyboardInterrupt if interrupt else json.JSONDecodeError):
                            ira_stream.write_month(str(log), str(root / "streams"), str(month))
                    self.assertFalse(target.exists(), "partial month must not be published")
                    month.write_text(valid + valid)
                    ira_stream.write_month(str(log), str(root / "streams"), str(month))
                    complete = target.read_bytes()
                    self.assertEqual(len(complete.splitlines()), 2)
                    with self.assertRaises(FileExistsError):
                        ira_stream.write_month(str(log), str(root / "streams"), str(month))
                    self.assertEqual(target.read_bytes(), complete, "published evidence must not be replaced")


class StandIn(threading.Thread):
    """An engine stand-in: records each envelope and answers it with one decision on a session close."""

    def __init__(self, path):
        super().__init__(daemon=True)
        self.server = socket.socket(socket.AF_UNIX)
        self.server.bind(path)
        self.server.listen(1)
        self.received = []

    def run(self):
        conn, _ = self.server.accept()
        with conn, conn.makefile("rwb") as stream:
            for line in stream:
                envelope = json.loads(line)
                self.received.append(envelope)
                decisions = []
                if envelope["type"] == "market.session.closed":
                    decisions = [{"type": "strategy.targets.decided", "id": "d", "payload": {
                        "outcome": "withheld", "withheld_reason": "histories-missing", "session_period_end": envelope["event_time"]}}]
                reply = {"envelope": {"type": "engine.decisions", "sequence": envelope["sequence"],
                                      "payload": {"decisions": decisions}}}
                stream.write((json.dumps(reply) + "\n").encode())
                stream.flush()


class FeedMonthTest(unittest.TestCase):
    def setUp(self):
        # The review gate names its run's own socket folder (docs/development.md, "Ending processes").
        self.dir = tempfile.mkdtemp(dir=os.environ.get("TI_TEST_SOCKET_DIR") or "/tmp")
        self.socket = os.path.join(self.dir, "engine.sock")
        self.write_run("dry-run")

    def write_run(self, mode, logged_mode=None, logged_account="DUTEST0001"):
        """The engine's configuration file and its durable log: the header, then the configuration input."""
        self.config = os.path.join(self.dir, "trad-ira.config")
        with open(self.config, "w") as f:
            json.dump({"strategy_id": "momentum", "account_id": "DUTEST0001", "mode": mode}, f)
        self.log = os.path.join(self.dir, "run.log")
        configuration = {"strategy_id": "momentum", "account_id": logged_account, "mode": logged_mode or mode}
        with open(self.log, "w") as f:
            f.write(json.dumps({"log_version": 1, "configuration_hash": "sha256:cfg", "strategy_version": "momentum/1.26.0+x"}) + "\n")
            f.write(json.dumps({"record": {"sequence": 1, "kind": "input", "envelope": {
                "id": "configuration:sha256:cfg", "type": "strategy.target-weight.configuration", "payload": configuration}}}) + "\n")

    def tearDown(self):
        for name in os.listdir(self.dir):
            os.remove(os.path.join(self.dir, name))
        os.rmdir(self.dir)

    def test_each_input_is_stamped_with_the_runs_identity_and_the_configured_account(self):
        engine = StandIn(self.socket)
        engine.start()
        lines = feed_month.feed(self.socket, self.log, self.config, MONTH, first_sequence=2)
        engine.join(5)
        with open(MONTH) as f:
            month = [json.loads(line) for line in f]
        self.assertEqual(len(engine.received), len(month))
        for i, (sent, line) in enumerate(zip(engine.received, month)):
            self.assertEqual(sent["sequence"], 2 + i)
            self.assertEqual((sent["id"], sent["type"], sent["schema_version"]), (line["id"], line["type"], line["schema_version"]))
            self.assertEqual((sent["configuration_hash"], sent["strategy_version"]), ("sha256:cfg", "momentum/1.26.0+x"))
            raw = json.dumps(sent["payload"], separators=(",", ":"))
            self.assertEqual(sent["payload_hash"], hashlib.sha256(raw.encode()).hexdigest())
            self.assertNotIn("trad-ira", raw, "the fixture's placeholder account is replaced")
        snapshots = [s for s in engine.received if s["type"] == "account.snapshot"]
        self.assertTrue(snapshots and all(s["payload"]["account_id"] == "DUTEST0001" for s in snapshots))
        self.assertIn("withheld histories-missing", "\n".join(lines))
        self.assertTrue(all("DUTEST0001" not in line for line in lines), "the account is never printed")

    def test_only_a_dry_run_is_fed(self):
        # Invented prices and cash recorded in a paper or live run would be its account's facts (AGENTS.md rule 5),
        # so the feed refuses before it connects: the configuration and the log's own configuration must both say
        # dry-run and name the same account.
        for mode, logged, account, want in (("paper", None, "DUTEST0001", "dry-run"), ("live", None, "DUTEST0001", "dry-run"),
                                            ("dry-run", "paper", "DUTEST0001", "dry-run"), (None, None, "DUTEST0001", "dry-run"),
                                            ("dry-run", None, "DUOTHER0001", "not the run")):
            with self.subTest(mode=mode, logged=logged, account=account):
                self.write_run(mode, logged, account)
                with self.assertRaises(SystemExit) as stop:
                    feed_month.feed(self.socket, self.log, self.config, MONTH, first_sequence=2)
                self.assertIn(want, str(stop.exception))
                self.assertNotIn("DUTEST0001", str(stop.exception))

    def test_a_refused_input_stops_the_feed_naming_it(self):
        server = socket.socket(socket.AF_UNIX)
        server.bind(self.socket)
        server.listen(1)

        def refuse():
            conn, _ = server.accept()
            with conn, conn.makefile("rwb") as stream:
                stream.readline()
                stream.write(b'{"error":{"message":"refused"}}\n')
                stream.flush()
        threading.Thread(target=refuse, daemon=True).start()
        with self.assertRaises(SystemExit) as stop:
            feed_month.feed(self.socket, self.log, self.config, MONTH, first_sequence=2)
        self.assertIn("calendar", str(stop.exception))
        server.close()


if __name__ == "__main__":
    unittest.main()
