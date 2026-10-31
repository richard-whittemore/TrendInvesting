"""Synthetic interruption cases for docs/manual/ibkr-adapter.md's wait invariant."""
import itertools
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from datetime import datetime, timedelta, timezone

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_connection import Harness
import connection

DEFAULT = {"windows": [
    {"days": ["daily"], "start": "23:40", "end": "01:00", "zone": "America/New_York"},
    {"days": ["Sat"], "start": "00:00", "end": "12:00", "zone": "America/New_York"},
]}


def utc(text):
    return datetime.fromisoformat(text).replace(tzinfo=timezone.utc)


class ScheduleTests(unittest.TestCase):
    def test_boundaries_overlap_weekday_and_dst_night(self):
        from maintenance import MaintenanceSchedule
        schedule = MaintenanceSchedule.from_mapping(DEFAULT)
        cases = [
            ("2026-10-30T03:39:59", None),
            ("2026-10-30T03:40:00", "2026-10-30T05:00:00"),
            ("2026-10-30T05:00:00", None),
            ("2026-10-31T03:50:00", "2026-10-31T16:00:00"),
            ("2026-10-31T09:44:00", "2026-10-31T16:00:00"),
            ("2026-10-31T16:00:00", None),
            ("2026-11-01T03:40:00", "2026-11-01T06:00:00"),
            ("2026-11-01T05:30:00", "2026-11-01T06:00:00"),
            ("2026-11-01T06:00:00", None),
        ]
        for now, end in cases:
            with self.subTest(now=now):
                self.assertEqual(schedule.window_end(utc(now)), utc(end) if end else None)

    def test_missing_empty_and_malformed_fail_closed(self):
        from maintenance import MaintenanceSchedule
        from config import ConfigError
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "schedule.json"
            self.assertIsNone(MaintenanceSchedule.load(path).window_end(utc("2026-10-31T09:00:00")))
            for content in ("{", "[]", '{"windows":null}', '{"windows":[],"typo":1}'):
                path.write_text(content)
                with self.subTest(content=content), self.assertRaises(ConfigError):
                    MaintenanceSchedule.load(path)
            for key, value in [("days", []), ("days", ["Saturday"]), ("days", "daily"),
                               ("start", "24:00"), ("end", "1:00"), ("zone", "bad/zone"),
                               ("start", "01:00"), ("typo", 1)]:
                window = dict(DEFAULT["windows"][0], **{key: value})
                path.write_text(json.dumps({"windows": [window]}))
                with self.subTest(key=key, value=value), self.assertRaises(ConfigError):
                    MaintenanceSchedule.load(path)
            path.write_text('{"windows": []}')
            self.assertIsNone(MaintenanceSchedule.load(path).window_end(utc("2026-10-31T09:00:00")))

    def test_duplicate_json_keys_are_refused_at_every_level(self):
        from maintenance import MaintenanceSchedule
        from config import ConfigError
        contents = [
            '{"windows": [], "windows": []}',
            '{"windows": [{"days": ["daily"], "start": "23:40", '
            '"end": "01:00", "end": "02:00", "zone": "America/New_York"}]}',
        ]
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "schedule.json"
            for content in contents:
                path.write_text(content)
                with self.subTest(content=content), self.assertRaisesRegex(ConfigError, "duplicate"):
                    MaintenanceSchedule.load(path)

    def test_weekly_gap_must_survive_spring_forward(self):
        from maintenance import MaintenanceSchedule, DAYS
        from config import ConfigError
        windows = [
            {"days": list(DAYS[:6]), "start": "00:00", "end": "12:00", "zone": "America/New_York"},
            {"days": list(DAYS[:6]), "start": "12:00", "end": "00:00", "zone": "America/New_York"},
            {"days": ["Sun"], "start": "00:00", "end": "02:00", "zone": "America/New_York"},
            {"days": ["Sun"], "start": "03:00", "end": "00:00", "zone": "America/New_York"},
        ]
        with self.assertRaisesRegex(ConfigError, "weekly gap"):
            MaintenanceSchedule.from_mapping({"windows": windows})


class InterruptionTests(unittest.TestCase):
    def harness(self, inside):
        from maintenance import MaintenanceSchedule
        h = Harness(restore_timeout=300, max_connect_attempts=2)
        start = utc("2026-10-31T09:44:00" if inside else "2026-10-30T09:44:00")
        origin = h.clock.now
        h.conn._wall_clock = lambda: start + timedelta(seconds=h.clock.now - origin)
        h.conn.maintenance = MaintenanceSchedule.from_mapping(DEFAULT)
        h.conn.stop_event = threading.Event()
        # Keep deterministic sleeps while making cancellation observable.
        h.conn.stop_event.wait = lambda seconds: (h.conn._sleep(seconds), False)[1]
        h.fake.monotonic = h.clock
        h.conn.connect()
        h.conn.mark_reconciled()
        return h

    def test_timely_queued_restoration_or_close_precedes_expiry(self):
        for callback in ("restore", "close"):
            for consume in ("pump", "maintain"):
                with self.subTest(callback=callback, consume=consume):
                    h = self.harness(True)
                    h.fake.hold_connectivity()
                    h.conn.pump(0)
                    h.clock.now = h.conn._maintenance_deadline - 1
                    # Unrelated callbacks ahead of restoration must also survive.
                    h.fake.callbacks.error(-1, 2104, "synthetic notice")
                    if callback == "restore":
                        h.fake.restore_connectivity()
                    else:
                        h.fake.connected = False
                        h.fake.callbacks.connection_closed()
                    h.clock.now += 2
                    if consume == "pump":
                        h.conn.pump(0)
                        h.conn.pump(0)
                    h.conn.maintain()
                    self.assertTrue(h.conn.connected)
                    self.assertTrue(h.conn.needs_reconciliation)
                    self.assertEqual(len(h.fake.called("connect")), 1 if callback == "restore" else 2)

    def test_loss_just_before_window_end_gets_five_minutes(self):
        h = self.harness(True)
        h.clock.now += (utc("2026-10-31T15:59:30") - h.conn.now_utc()).total_seconds()
        h.fake.hold_connectivity()
        h.conn.pump(0)
        lost = h.clock.now
        for elapsed in (30, 299):
            h.clock.now = lost + elapsed
            h.conn.maintain()
            self.assertTrue(h.fake.connected)
        h.clock.now = lost + 300
        with self.assertRaises(connection.MaintenanceExpired):
            h.conn.maintain()

    def test_wall_clock_rollback_does_not_extend_wait(self):
        h = self.harness(True)
        h.fake.hold_connectivity()
        h.conn.pump(0)
        deadline = h.conn._maintenance_deadline
        h.conn._wall_clock = lambda: utc("2026-10-30T09:44:00")
        h.clock.now = deadline - 1
        h.conn.maintain()
        self.assertTrue(h.fake.connected)
        h.clock.now = deadline
        with self.assertRaises(connection.MaintenanceExpired):
            h.conn.maintain()

    def test_fall_back_wait_names_the_later_hour(self):
        h = self.harness(True)
        h.conn._wall_clock = lambda: utc("2026-11-01T04:10:00")
        with self.assertLogs("ibkr", level="INFO") as logs:
            h.fake.hold_connectivity()
            h.conn.pump(0)
        self.assertIn("01:00 EST, the later one", "\n".join(logs.output))

    def test_stale_id_released_at_25_seconds_gets_final_try_at_30(self):
        h = Harness(backoff_initial=1, backoff_max=60)
        h.fake.monotonic = h.clock
        h.conn.connect()
        h.fake.stale_client_seconds = 25
        start = h.clock.now
        attempted = []
        h.fake.connect_hook = lambda: attempted.append(h.clock.now - start)
        h.conn.drop("synthetic-own-close")
        h.conn.maintain()
        self.assertTrue(h.conn.connected)
        self.assertEqual(attempted, [0, 1, 3, 7, 15, 30])

    def test_handshake_owns_id_before_volunteered_read_fails(self):
        h = Harness()
        h.fake.monotonic = h.clock
        h.fake.stale_client_seconds = 3
        connect = h.fake.connect
        def interrupted(*args):
            connect(*args)
            h.fake.callbacks.error(-1, 1100, "synthetic loss during volunteered read")
            h.fake.connect = connect
        h.fake.connect = interrupted
        h.conn.connect_with_backoff()
        self.assertTrue(h.conn.connected)
        self.assertEqual(len(h.fake.called("connect")), 3)
        self.assertEqual(h.sleeps, [1, 2])

    def test_loss_matrix(self):
        for inside, restored, stale, code in itertools.product((False, True), repeat=4):
            # Scheduled expiry never reconnects, so stale-ID grace cannot apply.
            if inside and not restored and stale:
                continue
            with self.subTest(inside=inside, restored=restored, stale=stale, code=code):
                h = self.harness(inside)
                h.fake.hold_connectivity(code=2110 if code else 1100)
                h.conn.pump(0)
                h.fake.stale_client_seconds = 3 if stale else 0
                if restored:
                    if stale:
                        h.conn.drop("untagged-read-interrupted")
                    h.fake.restore_connectivity()
                    h.conn.pump(0)
                    h.conn.maintain()
                    self.assertTrue(h.conn.connected)
                    self.assertTrue(h.conn.needs_reconciliation)
                    self.assertEqual(len(h.fake.called("connect")), 1 if not stale else 4)
                elif inside:
                    h.clock.now = h.conn._maintenance_deadline - 1
                    h.conn.maintain()
                    self.assertEqual(len(h.fake.called("connect")), 1)
                    h.clock.now += 1
                    with self.assertRaisesRegex(connection.MaintenanceExpired, "Gateway restart is the remedy"):
                        h.conn.maintain()
                    self.assertFalse(h.fake.connected)
                    self.assertEqual(len(h.fake.called("connect")), 1)
                else:
                    h.clock.now += 299
                    h.conn.maintain()
                    self.assertEqual(len(h.fake.called("connect")), 1)
                    h.clock.now += 1
                    h.conn.maintain()
                    self.assertTrue(h.conn.connected)
                    self.assertTrue(h.conn.needs_reconciliation)
                    self.assertEqual(len(h.fake.called("connect")), 2 if not stale else 4)
                if stale:
                    self.assertGreater(h.fake._stale_until, 0, "the stale flag must be consumed by own-close")
                    self.assertEqual(h.sleeps, [1, 2], "326 must exercise its backoff")
                self.assertEqual({c[3] for c in h.fake.called("connect")}, {11})

    def test_stop_matrix_interrupts_an_entered_wait(self):
        cases = [
            (True, False, "restoration"), (False, False, "restoration"),
            (True, True, "stale"), (False, True, "stale"),
            (True, True, "backoff"), (False, True, "backoff"),
            (False, False, "stale"), (False, False, "backoff"),
        ]
        for inside, restored, kind in cases:
            for code in (1100, 2110):
                with self.subTest(inside=inside, restored=restored, kind=kind, code=code):
                    h = self.harness(inside)
                    h.fake.hold_connectivity(code=code)
                    h.conn.pump(0)
                    if kind != "restoration":
                        h.fake.stale_client_seconds = 60 if kind == "stale" else 0
                        if restored:
                            h.conn.drop("synthetic-deferred-close")
                            h.fake.restore_connectivity()
                            h.conn.pump(0)
                        else:
                            h.clock.now += 300
                        if kind == "backoff":
                            h.fake.connect_behaviours = ["refused"]
                    entered = threading.Event()
                    waits = []
                    h.conn.stop_event = threading.Event()
                    wait = h.conn.stop_event.wait
                    get = h.conn.inbox.get
                    def wait_inside(seconds):
                        waits.append(kind)
                        entered.set()
                        return wait(seconds)
                    def get_inside(seconds):
                        if seconds > 0:
                            waits.append("restoration")
                            entered.set()
                        return get(seconds)
                    h.conn.stop_event.wait = wait_inside
                    h.conn.inbox.get = get_inside
                    def cancel():
                        if entered.wait(.5):
                            time.sleep(.01)
                            h.conn.stop_event.set()
                    stopper = threading.Thread(target=cancel)
                    stopper.start()
                    started = time.monotonic()
                    try:
                        with self.assertRaises(connection.StopRequested):
                            h.conn.maintain()
                        self.assertLess(time.monotonic() - started, 1)
                        self.assertEqual(waits, [kind])
                        if kind == "stale":
                            self.assertGreater(h.fake._stale_until, h.clock.now)
                            self.assertEqual(len(h.fake.called("connect")), 2)
                    finally:
                        stopper.join(1)

    def test_restoration_after_the_window_end_cannot_revive_the_session(self):
        h = self.harness(True)
        h.fake.hold_connectivity()
        h.conn.pump(0)
        h.clock.now += 8 * 3600
        h.fake.restore_connectivity()
        with self.assertRaisesRegex(connection.MaintenanceExpired, "Gateway restart"):
            h.conn.pump(0)
        self.assertFalse(h.fake.connected)

    def test_timed_fake_restoration_keeps_the_session(self):
        h = self.harness(True)
        timer = h.fake.hold_connectivity(seconds=.02)
        try:
            h.conn.pump(0)
            h.conn.pump(.5)
            self.assertTrue(h.conn.connected)
            self.assertTrue(h.conn.needs_reconciliation)
            self.assertEqual(len(h.fake.called("connect")), 1)
        finally:
            timer.cancel()
            timer.join()

    def test_wait_journal_once_then_hourly_and_repeated_loss_does_not_extend(self):
        h = self.harness(True)
        with self.assertLogs("ibkr", level="INFO") as logs:
            h.fake.hold_connectivity()
            h.conn.pump(0)
            for seconds in (1, 3598, 1, 1):
                h.clock.now += seconds
                h.fake.hold_connectivity()
                h.conn.pump(0)
                h.conn.maintain()
        waits = [s for s in logs.output if "scheduled window ending 12:00 EDT; waiting" in s]
        self.assertEqual(len(waits), 2)
        self.assertEqual(len([s for s in logs.output if "state=connectivity-lost" in s]), 1)

    def test_stale_id_grace_is_fixed_and_foreign_id_at_start_is_immediate(self):
        h = self.harness(False)
        h.fake.stale_client_seconds = 60
        start = h.clock.now
        attempted = []
        h.fake.connect_hook = lambda: attempted.append(h.clock.now - start)
        h.conn.drop("synthetic-own-close")
        with self.assertRaises(connection.ClientIdInUse):
            h.conn.maintain()
        self.assertEqual(h.clock.now - start, 30)
        self.assertEqual(attempted[-1], 30)
        self.assertGreater(len(h.fake.called("connect")), h.conn.config.max_connect_attempts)
        h = Harness()
        h.fake.connect_behaviours = ["client-id-in-use"]
        with self.assertRaises(connection.ClientIdInUse):
            h.conn.connect_with_backoff()
        self.assertEqual(h.sleeps, [])

    def test_sigterm_waits(self):
        # A separate process receives a real SIGTERM in the actual CLI signal handler.
        for kind in ("inside", "outside", "backoff", "stale", "reconcile"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as folder:
                ready = Path(folder) / "ready"
                log = Path(folder) / "journal"
                with log.open("w") as output:
                    proc = subprocess.Popen([sys.executable, __file__, kind, str(ready)], stdout=output, stderr=output,
                                            env=dict(os.environ, TMPDIR="/tmp"))
                    try:
                        deadline = time.monotonic() + 8
                        while not ready.exists() and proc.poll() is None and time.monotonic() < deadline:
                            time.sleep(.01)
                        self.assertTrue(ready.exists(), log.read_text())
                        time.sleep(.05)
                        started = time.monotonic()
                        proc.send_signal(signal.SIGTERM)
                        proc.wait(timeout=1)
                        self.assertLess(time.monotonic() - started, 1)
                        self.assertEqual(proc.returncode, 0, log.read_text())
                        self.assertIn("ibkr adapter stopped on request", log.read_text())
                    finally:
                        if proc.poll() is None:
                            proc.kill()
                            proc.wait()


def signal_child(kind, ready):
    from test_cli import CommandTests
    import cli
    from unittest.mock import patch
    t = CommandTests()
    t.setUp()
    original = cli.AdapterLoop.reconcile
    def reconcile(loop):
        if kind == "reconcile":
            # The reconcile retry uses Connection.pause, with callbacks still applied.
            Path(ready).touch()
            loop.conn.pause(30)
        else:
            original(loop)
    if kind in ("inside", "outside"):
        def positions():
            t.fake.hold_connectivity()
            Path(ready).touch()
        t.fake.req_positions = positions
    if kind in ("backoff", "stale"):
        original_connect = t.fake.connect
        def connect(*args):
            if kind == "backoff":
                t.fake.connect_behaviours = ["refused"]
                Path(ready).touch()
            original_connect(*args)
        t.fake.connect = connect
        if kind == "stale":
            def reconcile(loop):
                t.fake.stale_client_seconds = 60
                loop.conn.drop("synthetic-own-close")
                Path(ready).touch()
                loop.conn.maintain()
    schedule = t.dir / "maintenance.json"
    schedule.write_text(json.dumps(DEFAULT if kind == "inside" else {"windows": []}))
    try:
        with patch.object(cli.AdapterLoop, "reconcile", reconcile):
            return cli.main(t.argv("--maintenance", str(schedule)), api_factory=t.api,
                            engine_connect=lambda *a: t.engine, err=sys.stderr,
                            now=lambda: utc("2026-10-31T09:44:00"),
                            ibapi_version=lambda: (10, 51, 1), protobuf_version=lambda: "5.29.5")
    finally:
        t.doCleanups()


if __name__ == "__main__":
    if len(sys.argv) == 3:
        sys.exit(signal_child(*sys.argv[1:]))
    unittest.main()
