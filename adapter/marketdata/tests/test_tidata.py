"""Tests for ti-data, the daily data job: synthetic Massive answers and a
synthetic engine durable log in, the Boost engine's stream file out.
Synthetic data only; no network."""
import datetime as dt
import hashlib
import io
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from marketdata import nyse_calendar, record, tidata

D = dt.date
UTC = dt.timezone.utc
REPO = Path(__file__).resolve().parents[3]
# A made-up value standing in for the Massive key: synthetic test data, not a
# credential (the repository is public and holds none).
SYNTHETIC_VALUE = "synthetic-value-0123456789"
BASE = D(2026, 3, 2)                 # a Monday: the configured warm-up start
WARM = D(2026, 3, 13)                # the warm-up's last session (10 sessions)
RUN = "run-2026-03-13"
CHAIN = ("seed=sha256(canonical_log_header_bytes); "
         "record=sha256(previous_record_hash||kind||canonical_envelope_bytes)")


def after_settle(day, minutes=65):
    return nyse_calendar.session_close(day).astimezone(UTC) + dt.timedelta(minutes=minutes)


def spy(day):
    c = 400.0 + (day - BASE).days
    return {"T": "SPY", "o": c, "h": c + 1, "l": c - 1, "c": c, "v": 1000, "t": 0}


class FakeMassive:
    """Massive's three endpoints the job uses, answered from synthetic rows."""

    def __init__(self, empty=()):
        self.calls, self.empty, self.dividends, self.splits_rows = [], set(empty), [], []

    def grouped(self, day):
        self.calls.append(("grouped", day))
        if day in self.empty:
            return {"status": "OK", "resultsCount": 0}
        return {"status": "OK", "results": [spy(day), {"T": "OTHER", "o": 9, "h": 9, "l": 9, "c": 9, "v": 1, "t": 0}]}

    def splits(self, first, last):
        self.calls.append(("splits", first, last))
        return [{"results": list(self.splits_rows)}]

    def all_dividends(self, first, last):
        self.calls.append(("dividends", first, last))
        return [{"results": list(self.dividends)}]


def shipped_config():
    return json.loads((REPO / "deploy/etc/engines/boost.config").read_text())


def _digest(text):
    return "sha256:" + hashlib.sha256(text.encode()).hexdigest()


def write_log(path, configuration_hash="hash-boost", strategy_version="boost/r1/test", inputs=1, config=None):
    """A durable log in the engine's shape (testdata/boost-fixture.log is one
    the engine wrote): a header, then one committed group per input, the
    first the configuration input carrying ``config`` (default the shipped
    Boost configuration), the others inputs only, no decisions."""
    lines = [json.dumps({"chain_algorithm": CHAIN, "configuration_hash": configuration_hash, "log_version": 1,
                         "strategy_version": strategy_version}, sort_keys=True, separators=(",", ":"))]
    for n in range(1, inputs + 1):
        if n == 1:
            ident, kind, payload = "configuration:" + configuration_hash, tidata.CONFIGURATION_TYPE, config or shipped_config()
        else:
            ident, kind, payload = "input-{}".format(n), "x", {"n": n}
        raw = json.dumps(payload, separators=(",", ":"))
        envelope = {"id": ident, "type": kind, "envelope_version": 1, "schema_version": 1,
                    "event_time": "2026-03-13T21:00:00Z", "recorded_at": "2026-03-13T21:00:00Z", "sequence": n,
                    "source": "engine", "strategy_version": strategy_version,
                    "configuration_hash": configuration_hash,
                    "payload_hash": hashlib.sha256(raw.encode()).hexdigest(), "payload": payload}
        digest = _digest(str(n))
        lines.append(json.dumps({"record": {"sequence": n, "kind": "input", "envelope": envelope,
                                            "record_hash": digest}}, separators=(",", ":")))
        lines.append(json.dumps({"commit": {"sequence": n, "record_hash": digest}}, separators=(",", ":")))
    Path(path).write_text("\n".join(lines) + "\n")


class Job:
    """A temporary server layout: data folder, config, engine log, secrets."""

    def __init__(self, root, trend_days=5, warm_up_start=BASE, env_text="MASSIVE_API_KEY=" + SYNTHETIC_VALUE + "\n"):
        self.root = Path(root)
        self.data = self.root / "data"
        self.streams = self.data / "streams"
        self.config = self.root / "boost.config"
        self.cfg = shipped_config()
        self.cfg["boost"].update(trend_days=trend_days, warm_up_start=warm_up_start.isoformat())
        self.config.write_text(json.dumps(self.cfg))
        self.log = self.root / (RUN + ".log")
        self.write_log()
        self.env_file = self.root / "env"
        if env_text is not None:
            self.env_file.write_text(env_text)
        self.client = FakeMassive()
        self.made = 0

    def write_log(self, **kw):
        write_log(self.log, config=self.cfg, **kw)

    def factory(self, value):
        if value != SYNTHETIC_VALUE:
            raise AssertionError("the job built a client with the wrong key")
        self.made += 1
        return self.client

    def argv(self, *extra):
        return ["--data", str(self.data), "--streams", str(self.streams), "--config", str(self.config),
                "--engine-log", str(self.log), "--secrets-file", str(self.env_file)] + list(extra)

    def main(self, now, *extra, factory=None):
        out, err = io.StringIO(), io.StringIO()
        with mock.patch("sys.stdout", out), mock.patch("sys.stderr", err):
            status = tidata.main(self.argv(*extra), now=lambda: now, client_factory=factory or self.factory)
        return status, out.getvalue(), err.getvalue()

    def stream_dir(self):
        return self.streams / "boost" / RUN

    def files(self):
        return sorted(str(p.relative_to(self.root)) for p in self.root.rglob("*") if p.is_file())

    def envelopes(self, day):
        return [json.loads(line) for line in (self.stream_dir() / "boost-{}.jsonl".format(day)).read_text().splitlines()]


def go_check(path):
    with open(path) as f:
        return subprocess.run(["go", "run", "./adapter/marketdata/tests/testdata/envelope_contract.go"],
                              cwd=REPO, stdin=f, text=True, capture_output=True)


class SettleTest(unittest.TestCase):
    """The latest settled session: its close plus the settle minutes have passed."""

    def test_a_session_settles_at_its_close_plus_the_settle_minutes(self):
        day, settle = D(2026, 3, 13), dt.timedelta(minutes=60)
        self.assertEqual(tidata.settled_session(after_settle(day, 59), settle), D(2026, 3, 12))
        self.assertEqual(tidata.settled_session(after_settle(day, 60), settle), day)

    def test_the_settle_minutes_are_configurable(self):
        day = D(2026, 3, 13)
        self.assertEqual(tidata.settled_session(after_settle(day, 89), dt.timedelta(minutes=90)), D(2026, 3, 12))
        self.assertEqual(tidata.settled_session(after_settle(day, 90), dt.timedelta(minutes=90)), day)

    def test_a_weekend_or_a_holiday_reads_the_session_before_it(self):
        settle = dt.timedelta(minutes=60)
        sunday = dt.datetime(2026, 3, 15, 18, 0, tzinfo=UTC)
        self.assertEqual(tidata.settled_session(sunday, settle), D(2026, 3, 13))
        thanksgiving = dt.datetime(2026, 11, 26, 23, 0, tzinfo=UTC)
        self.assertEqual(tidata.settled_session(thanksgiving, settle), D(2026, 11, 25))

    def test_an_early_close_settles_an_hour_after_one_pm(self):
        day = D(2026, 11, 27)   # the day after Thanksgiving closes at 1 p.m.
        self.assertEqual(tidata.settled_session(dt.datetime(2026, 11, 27, 18, 59, tzinfo=UTC),
                                                dt.timedelta(minutes=60)), D(2026, 11, 25))
        self.assertEqual(tidata.settled_session(dt.datetime(2026, 11, 27, 19, 0, tzinfo=UTC),
                                                dt.timedelta(minutes=60)), day)


class TimerAgreementTest(unittest.TestCase):
    """The timer fires after every weekday session has settled, by the unit's own --settle-minutes."""

    def test_the_timer_fires_after_each_session_settles(self):
        timer = (REPO / "deploy/systemd/ti-data.timer").read_text()
        service = (REPO / "deploy/systemd/ti-data.service").read_text()
        # systemd's calendar form: a list of hours, one minute (17,18,20:05).
        when = re.search(r"^OnCalendar=Mon\.\.Fri ([0-9,]+):(\d\d) America/New_York$", timer, re.M)
        self.assertIsNotNone(when, "the timer must fire Mon..Fri at New York times")
        self.assertRegex(timer, r"(?m)^Persistent=true$")
        minutes = re.search(r"--settle-minutes (\d+)", service)
        self.assertIsNotNone(minutes, "ExecStart must pass --settle-minutes")
        settle = dt.timedelta(minutes=int(minutes.group(1)))
        times = [(int(h), int(when.group(2))) for h in when.group(1).split(",")]
        for hour, minute in times:
            for day in nyse_calendar.sessions(D(2026, 1, 1), D(2027, 12, 31)):
                fire = dt.datetime(day.year, day.month, day.day, hour, minute, tzinfo=nyse_calendar.NEW_YORK)
                self.assertEqual(tidata.settled_session(fire, settle), day, (day, hour, minute))

    def test_the_timer_fires_three_times_an_evening(self):
        # A firing that fails (a Massive outage, a 429 streak) is retried the
        # same evening; a later firing after a success finds the file and
        # fetches nothing.
        timer = (REPO / "deploy/systemd/ti-data.timer").read_text()
        self.assertRegex(timer, r"(?m)^OnCalendar=Mon\.\.Fri 17,18,20:05 America/New_York$")


class UnitTest(unittest.TestCase):
    """ti-data.service fails loudly, never skips silently, and never hangs."""

    def setUp(self):
        self.service = (REPO / "deploy/systemd/ti-data.service").read_text()

    def test_a_hung_run_times_out_as_a_failed_unit(self):
        self.assertRegex(self.service, r"(?m)^TimeoutStartSec=1h$")

    def test_nothing_skips_the_unit(self):
        self.assertNotRegex(self.service, r"(?m)^Condition")

    def test_a_missing_run_env_fails_the_unit_with_a_message(self):
        self.assertRegex(self.service, r"(?m)^EnvironmentFile=-/var/lib/trend-investing/engines/boost/run\.env$")
        pre = re.search(r"(?m)^ExecStartPre=(.*)$", self.service)
        self.assertIsNotNone(pre, "the unit checks run.env itself")
        with tempfile.TemporaryDirectory() as d:
            command = pre.group(1).replace("/var/lib/trend-investing/engines/boost/run.env", str(Path(d, "run.env")))
            argv = command.split(" ", 2)
            self.assertEqual(argv[:2], ["/bin/sh", "-c"])
            script = argv[2]
            self.assertTrue(script.startswith("'") and script.endswith("'"), script)
            result = subprocess.run(["/bin/sh", "-c", script[1:-1]], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("run.env", result.stderr)
            self.assertIn("start the Boost engine", result.stderr)
            Path(d, "run.env").write_text("TI_RUN=run-1\n")
            self.assertEqual(subprocess.run(["/bin/sh", "-c", script[1:-1]]).returncode, 0)

    def test_the_unit_holds_no_ib_gateway_client_id(self):
        self.assertNotIn("TI_ADAPTER_CLIENT_ID", self.service)


class SecretsTest(unittest.TestCase):
    def test_the_key_is_read_from_the_secrets_file(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, "s")
            p.write_text("# comment\nIBKR_ACCOUNT_BOOST=\nMASSIVE_API_KEY= {} \nNTFY_URL=\n".format(SYNTHETIC_VALUE))
            self.assertEqual(tidata.read_key(str(p)), SYNTHETIC_VALUE)

    def test_a_missing_or_empty_key_is_refused_without_naming_a_value(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, "s")
            for text in ("NTFY_URL=\n", "MASSIVE_API_KEY=\n", "MASSIVE_API_KEY=   \n"):
                p.write_text(text)
                with self.assertRaises(tidata.Refused):
                    tidata.read_key(str(p))
            with self.assertRaises(tidata.Refused):
                tidata.read_key(str(Path(d, "absent")))
            with self.assertRaises(tidata.Refused):
                tidata.read_key(None)


class EngineLogTest(unittest.TestCase):
    def test_identity_and_configuration_come_from_the_durable_log(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            write_log(p, inputs=2)
            got = tidata.read_engine_log(str(p))
            boost = tidata.read_config(str(REPO / "deploy/etc/engines/boost.config"))
            self.assertEqual(got, tidata.EngineLog("hash-boost", "boost/r1/test", RUN, boost))

    def test_a_log_the_engine_wrote_gives_its_identity(self):
        # testdata/boost-fixture.log was written by cmd/engine's own LogWriter
        # from the shipped Boost configuration (data_job_log_fixture_test.go).
        path = REPO / "adapter/marketdata/tests/testdata/boost-fixture.log"
        lines = path.read_text().splitlines()
        header, envelope = json.loads(lines[0]), json.loads(lines[1])["record"]["envelope"]
        # Later groups too: an input followed by its decision, then an input,
        # numbered in one series and committed as the engine commits them.
        records = [json.loads(line)["record"] for line in lines[1:] if line.startswith('{"record"')]
        self.assertEqual([r["kind"] for r in records], ["input", "input", "decision", "input"])
        got = tidata.read_engine_log(str(path))
        self.assertEqual(got.configuration_hash, header["configuration_hash"])
        self.assertEqual(got.configuration_hash, envelope["configuration_hash"])
        self.assertEqual(got.strategy_version, header["strategy_version"])
        self.assertTrue(got.strategy_version.startswith("boost/"), got.strategy_version)
        self.assertEqual(got.run_id, "boost-fixture")
        self.assertEqual(got.boost, tidata.read_config(str(REPO / "deploy/etc/engines/boost.config")))

    def test_a_log_whose_configuration_input_is_not_committed_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            write_log(p)
            lines = p.read_text().splitlines()
            p.write_text("\n".join(lines[:-1]) + "\n")   # the configuration's commit is missing
            with self.assertRaises(tidata.Refused):
                tidata.read_engine_log(str(p))

    def test_a_first_input_that_is_not_the_runs_configuration_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            for old, new in (('"type":"strategy.target-weight.configuration"', '"type":"x"'),
                             ('"id":"configuration:hash-boost"', '"id":"configuration:hash-other"'),
                             ('"strategy_id":"boost"', '"strategy_id":"momentum"')):
                write_log(p)
                p.write_text(p.read_text().replace(old, new, 1))
                with self.assertRaises(tidata.Refused, msg=new):
                    tidata.read_engine_log(str(p))

    def test_a_damaged_commit_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            for damage in (lambda c: c.pop("record_hash"), lambda c: c.update(record_hash=_digest("other")),
                           lambda c: c.update(extra=1)):
                write_log(p)
                lines = p.read_text().splitlines()
                commit = json.loads(lines[2])
                damage(commit["commit"])
                lines[2] = json.dumps(commit, separators=(",", ":"))
                p.write_text("\n".join(lines) + "\n")
                with self.assertRaises(tidata.Refused):
                    tidata.read_engine_log(str(p))

    def test_a_record_not_as_the_engine_writes_one_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            for damage in (lambda r: r.pop("record_hash"), lambda r: r.update(record_hash="sha256:short"),
                           lambda r: r.update(extra=1)):
                write_log(p)
                lines = p.read_text().splitlines()
                rec = json.loads(lines[1])
                damage(rec["record"])
                lines[1] = json.dumps(rec, separators=(",", ":"))
                p.write_text("\n".join(lines) + "\n")
                with self.assertRaises(tidata.Refused):
                    tidata.read_engine_log(str(p))

    def test_a_log_with_no_run_name_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, ".log")
            write_log(p)
            with self.assertRaises(tidata.Refused):
                tidata.read_engine_log(str(p))

    def test_an_unreadable_log_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            for text in ("", '{"journal_version":1}\n', "not json\n"):
                p.write_text(text)
                with self.assertRaises(tidata.Refused):
                    tidata.read_engine_log(str(p))
            write_log(p)
            lines = p.read_text().splitlines()
            p.write_text("\n".join([lines[0], lines[2]]) + "\n")   # a commit with no record
            with self.assertRaises(tidata.Refused):
                tidata.read_engine_log(str(p))
            with self.assertRaises(tidata.Refused):
                tidata.read_engine_log(str(Path(d, "absent.log")))
            q = Path(d, "run.journal")
            write_log(q)
            with self.assertRaises(tidata.Refused):   # the run is named by a .log file
                tidata.read_engine_log(str(q))
            write_log(p)
            p.write_text(p.read_text().replace('"log_version":1', '"log_version":2'))
            with self.assertRaises(tidata.Refused):
                tidata.read_engine_log(str(p))


class WarmUpTest(unittest.TestCase):
    def test_the_warm_up_records_the_history_and_writes_the_first_stream(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            status, out, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 0, err)
            path = job.stream_dir() / "boost-{}.jsonl".format(WARM)
            self.assertEqual(out.strip(), str(path))
            sessions = nyse_calendar.sessions(BASE, WARM)
            self.assertEqual([c for c in job.client.calls if c[0] == "grouped"],
                             [("grouped", d) for d in reversed(sessions)])
            self.assertIn(("splits", BASE + dt.timedelta(days=1), WARM), job.client.calls)
            stream = job.envelopes(WARM)
            bars = [e for e in stream if e["type"] == "market.bar.completed"]
            closes = [e for e in stream if e["type"] == "market.session.closed"]
            self.assertEqual([e["payload"]["instrument_id"] for e in bars], ["SPY"] * len(sessions))
            self.assertEqual(len(closes), len(sessions))
            self.assertEqual({e["type"] for e in stream}, {"market.bar.completed", "market.session.closed"})
            # A stream file numbers its envelopes from 1: the position in the file, not the engine's
            # sequence, which the deliverer assigns at send time.
            self.assertEqual([e["sequence"] for e in stream], list(range(1, 1 + len(stream))))
            self.assertEqual({(e["configuration_hash"], e["strategy_version"], e["correlation_id"])
                              for e in stream}, {("hash-boost", "boost/r1/test", RUN)})
            self.assertEqual({e["payload"]["adjusted_base"] for e in bars}, {BASE.isoformat()})
            self.assertNotIn(SYNTHETIC_VALUE, out + err)
            for p in job.root.rglob("*"):
                if p.is_file() and p != job.env_file:
                    self.assertNotIn(SYNTHETIC_VALUE, p.read_text())

    def test_the_stream_satisfies_the_go_validators(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            self.assertEqual(job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())[0], 0)
            job.main(after_settle(D(2026, 3, 16)))
            for day in (WARM, D(2026, 3, 16)):
                result = go_check(job.stream_dir() / "boost-{}.jsonl".format(day))
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("envelopes valid", result.stdout)

    def test_the_warm_up_must_start_at_the_configured_warm_up_start(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            status, _, err = job.main(after_settle(WARM), "--backfill-from", "2026-03-03")
            self.assertEqual(status, 1)
            self.assertIn("warm_up_start", err)
            self.assertEqual(job.made, 0)
            self.assertFalse(job.data.exists())

    def test_a_history_too_short_for_the_indicators_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root, trend_days=200)
            status, _, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 1)
            self.assertIn("200", err)
            self.assertFalse(job.stream_dir().exists())

    def test_a_trading_day_massive_returns_empty_writes_no_stream(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            job.client.empty = {D(2026, 3, 5)}
            status, _, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 1)
            self.assertIn("2026-03-05", err)
            self.assertFalse(job.stream_dir().exists())

    def test_the_daily_job_refuses_until_the_warm_up_has_run(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            status, _, err = job.main(after_settle(WARM))
            self.assertEqual(status, 1)
            self.assertIn("--backfill-from {}".format(BASE), err)
            self.assertEqual(job.made, 0)
            self.assertFalse(job.data.exists())


class DailyTest(unittest.TestCase):
    def warmed(self, root):
        job = Job(root)
        status, _, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())
        self.assertEqual(status, 0, err)
        job.client.calls.clear()
        return job

    def test_the_next_session_is_its_own_stream_numbered_from_one(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            status, out, err = job.main(after_settle(D(2026, 3, 16)))
            self.assertEqual(status, 0, err)
            self.assertEqual(out.strip(), str(job.stream_dir() / "boost-2026-03-16.jsonl"))
            self.assertEqual([c for c in job.client.calls if c[0] == "grouped"], [("grouped", D(2026, 3, 16))])
            stream = job.envelopes(D(2026, 3, 16))
            self.assertEqual([e["type"] for e in stream], ["market.bar.completed", "market.session.closed"])
            self.assertEqual([e["sequence"] for e in stream], [1, 2])
            self.assertEqual(stream[0]["payload"]["raw"]["close"], spy(D(2026, 3, 16))["c"])

    def test_inputs_the_engine_commits_between_two_files_do_not_change_the_next(self):
        # The engine's sequence is global across sources (the Run approval,
        # fills, snapshots): a file never claims one, so another source's
        # input committed between two files cannot collide with it.
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as other:
            job = self.warmed(root)
            alone = self.warmed(other)
            job.write_log(inputs=7)
            self.assertEqual(job.main(after_settle(D(2026, 3, 16)))[0], 0)
            self.assertEqual(alone.main(after_settle(D(2026, 3, 16)))[0], 0)
            self.assertEqual((job.stream_dir() / "boost-2026-03-16.jsonl").read_bytes(),
                             (alone.stream_dir() / "boost-2026-03-16.jsonl").read_bytes())

    def test_missed_sessions_are_caught_up_in_one_stream(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            status, _, err = job.main(after_settle(D(2026, 3, 18)))
            self.assertEqual(status, 0, err)
            stream = job.envelopes(D(2026, 3, 18))
            self.assertEqual([e["id"] for e in stream if e["type"] == "market.session.closed"],
                             ["marketdata:session-closed:2026-03-{}".format(d) for d in (16, 17, 18)])

    def test_a_second_run_writes_the_same_bytes_and_records_nothing(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            now = after_settle(D(2026, 3, 16))
            self.assertEqual(job.main(now)[0], 0)
            path = job.stream_dir() / "boost-2026-03-16.jsonl"
            before, files = path.read_bytes(), job.files()
            refuse = mock.Mock(side_effect=AssertionError("a second run fetched"))
            status, out, err = job.main(now, factory=refuse)
            self.assertEqual(status, 0, err)
            self.assertEqual(out.strip(), str(path))
            self.assertEqual(path.read_bytes(), before)
            self.assertEqual(job.files(), files)
            # A holiday or weekend run finds the last session already written.
            status, _, err = job.main(dt.datetime(2026, 3, 21, 21, 0, tzinfo=UTC),
                                      "--session", "2026-03-16", factory=refuse)
            self.assertEqual(status, 0, err)

    def test_the_warm_up_run_twice_is_also_idempotent(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            path = job.stream_dir() / "boost-{}.jsonl".format(WARM)
            before = path.read_bytes()
            job.write_log(inputs=3)   # the engine has taken inputs since; the file is unchanged
            refuse = mock.Mock(side_effect=AssertionError("fetched"))
            status, _, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat(), factory=refuse)
            self.assertEqual(status, 0, err)
            self.assertEqual(path.read_bytes(), before)

    def test_a_stream_that_differs_from_its_recordings_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            now = after_settle(D(2026, 3, 16))
            job.main(now)
            path = job.stream_dir() / "boost-2026-03-16.jsonl"
            path.write_text(path.read_text().replace('"source":"marketdata"', '"source":"other"', 1))
            status, _, err = job.main(now)
            self.assertEqual(status, 1)
            self.assertIn(str(path), err)

    def test_a_new_engine_identity_against_an_old_stream_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            job.write_log(configuration_hash="hash-other")
            status, _, err = job.main(after_settle(WARM))
            self.assertEqual(status, 1)
            self.assertIn("differs", err)

    def test_an_unsettled_session_is_refused_and_nothing_is_written(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            files = job.files()
            status, _, err = job.main(after_settle(D(2026, 3, 16), 59), "--session", "2026-03-16")
            self.assertEqual(status, 1)
            self.assertIn("not recordable before", err)
            self.assertEqual(job.made, 1)   # the warm-up's
            self.assertEqual(job.files(), files)

    def test_a_missing_key_is_refused_and_nothing_is_written(self):
        for env_text in (None, "", "MASSIVE_API_KEY=\n", "NTFY_URL=x\n"):
            with tempfile.TemporaryDirectory() as root:
                job = Job(root, env_text=env_text)
                status, out, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())
                self.assertEqual(status, 1)
                self.assertIn("MASSIVE_API_KEY", err)
                self.assertEqual(job.made, 0)
                self.assertFalse(job.data.exists())

    def test_a_missing_recording_in_the_history_writes_no_stream(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            os.remove(job.data / "grouped_2026-03-05.json")
            status, _, err = job.main(after_settle(D(2026, 3, 16)))
            self.assertEqual(status, 1)
            self.assertIn("2026-03-05", err)
            self.assertFalse((job.stream_dir() / "boost-2026-03-16.jsonl").exists())
            self.assertEqual(sorted(os.listdir(job.stream_dir())), ["boost-{}.jsonl".format(WARM)])

    def test_a_missing_engine_log_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            os.remove(job.log)
            status, _, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 1)
            self.assertIn(str(job.log), err)
            self.assertFalse(job.data.exists())

    def test_the_action_windows_keep_covering_the_span(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            self.assertEqual(job.main(after_settle(D(2026, 3, 16)))[0], 0)
            splits = [c for c in job.client.calls if c[0] == "splits"]
            self.assertEqual(len(splits), 1)
            first, last = splits[0][1], splits[0][2]
            self.assertEqual(last, D(2026, 3, 16))
            self.assertLessEqual(first, WARM + dt.timedelta(days=1))
            self.assertGreater(first, BASE)


class ReverifyTest(unittest.TestCase):
    """Before a new file is written, every file already written for the run
    must still re-encode byte for byte; otherwise nothing is written."""

    def daily(self, root):
        job = Job(root)
        self.assertEqual(job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())[0], 0)
        self.assertEqual(job.main(after_settle(D(2026, 3, 16)))[0], 0)
        job.client.calls.clear()
        return job

    def assert_refused_naming(self, job, now, path):
        files = sorted(os.listdir(job.stream_dir()))
        status, out, err = job.main(now)
        self.assertEqual(status, 1, out)
        self.assertIn(str(path), err)
        self.assertIn("a person", err)
        self.assertEqual(sorted(os.listdir(job.stream_dir())), files)

    def test_a_dividend_recorded_late_inside_a_written_file_stops_the_job(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            job.client.dividends = [{"id": "DIV1", "ticker": "SPY", "ex_dividend_date": "2026-03-10",
                                     "cash_amount": 1.0}]
            self.assert_refused_naming(job, after_settle(D(2026, 3, 17)), job.stream_dir() / "boost-{}.jsonl".format(WARM))
            self.assertIn(("dividends", D(2026, 3, 3), D(2026, 3, 17)), job.client.calls)

    def test_an_ordinary_dividend_after_a_delivered_file_leaves_every_earlier_file_unchanged(self):
        # Forward adjustment: an action changes only the sessions on and
        # after its ex-date, so the next quarterly ex-date never stops the job.
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            before = {p.name: p.read_bytes() for p in job.stream_dir().iterdir()}
            job.client.dividends = [{"id": "DIV1", "ticker": "SPY", "ex_dividend_date": "2026-03-17",
                                     "cash_amount": 1.0}]
            status, _, err = job.main(after_settle(D(2026, 3, 17)))
            self.assertEqual(status, 0, err)
            self.assertEqual({n: (job.stream_dir() / n).read_bytes() for n in before}, before)
            bar = job.envelopes(D(2026, 3, 17))[0]["payload"]
            prior = spy(D(2026, 3, 16))["c"]
            self.assertAlmostEqual(bar["adjusted"]["close"], bar["raw"]["close"] * prior / (prior - 1.0))
            self.assertEqual(bar["split_adjusted"]["close"], bar["raw"]["close"])
            status, _, err = job.main(after_settle(D(2026, 3, 18)))   # and the run after it
            self.assertEqual(status, 0, err)

    def test_an_ordinary_split_after_a_delivered_file_leaves_every_earlier_file_unchanged(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            before = {p.name: p.read_bytes() for p in job.stream_dir().iterdir()}
            job.client.splits_rows = [{"id": "SPL1", "ticker": "SPY", "execution_date": "2026-03-17",
                                       "split_from": 1, "split_to": 2}]
            status, _, err = job.main(after_settle(D(2026, 3, 17)))
            self.assertEqual(status, 0, err)
            self.assertEqual({n: (job.stream_dir() / n).read_bytes() for n in before}, before)
            bar = job.envelopes(D(2026, 3, 17))[0]["payload"]
            self.assertEqual(bar["split_adjusted"]["close"], bar["raw"]["close"] * 2)

    def test_a_revised_dividend_of_another_ticker_does_not_stop_the_boost_job(self):
        # The bulk action windows hold every ticker and overlap day to day,
        # so a vendor revision of a figure the Boost stream never reads is
        # in the data folder twice; only the encoded instrument's actions
        # are checked for conflicts.
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            row = {"id": "DIVO", "ticker": "OTHER", "ex_dividend_date": "2026-03-10", "cash_amount": 1.0}
            job.client.dividends = [row]
            self.assertEqual(job.main(after_settle(D(2026, 3, 17)))[0], 0)
            job.client.dividends = [dict(row, cash_amount=1.5)]
            status, _, err = job.main(after_settle(D(2026, 3, 18)))
            self.assertEqual(status, 0, err)
            self.assertTrue((job.stream_dir() / "boost-2026-03-18.jsonl").exists())

    def test_a_revised_dividend_of_the_encoded_instrument_still_stops_the_job(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            row = {"id": "DIVS", "ticker": "SPY", "ex_dividend_date": "2026-03-17", "cash_amount": 1.0}
            job.client.dividends = [row]
            self.assertEqual(job.main(after_settle(D(2026, 3, 17)))[0], 0)
            job.client.dividends = [dict(row, cash_amount=1.5)]
            status, _, err = job.main(after_settle(D(2026, 3, 18)))
            self.assertEqual(status, 1)
            self.assertIn("dividends DIVS differs between recordings", err)
            self.assertIn("a person", err)
            self.assertFalse((job.stream_dir() / "boost-2026-03-18.jsonl").exists())

    def test_the_written_files_are_encoded_again_once_per_run(self):
        # One encoding of the base through the newest file's last session,
        # each file compared with its slice, so a run's work grows with the
        # sessions, not with files times sessions.
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            for day in (17, 18, 19):
                self.assertEqual(job.main(after_settle(D(2026, 3, day)))[0], 0)
            with mock.patch.object(tidata.encode, "encode", wraps=tidata.encode.encode) as enc:
                self.assertEqual(job.main(after_settle(D(2026, 3, 20)))[0], 0)
                self.assertEqual([(c.args[1], c.args[2]) for c in enc.call_args_list],
                                 [(BASE, D(2026, 3, 19)), (D(2026, 3, 20), D(2026, 3, 20))])
                enc.reset_mock()
                self.assertEqual(job.main(after_settle(D(2026, 3, 20)))[0], 0)   # the idempotent path
                self.assertEqual(enc.call_count, 1)

    def test_an_earlier_file_changed_on_disk_stops_the_job(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            path = job.stream_dir() / "boost-{}.jsonl".format(WARM)
            path.write_text(path.read_text().replace('"source":"marketdata"', '"source":"other"', 1))
            self.assert_refused_naming(job, after_settle(D(2026, 3, 17)), path)

    def test_a_removed_file_between_two_others_stops_the_job(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            self.assertEqual(job.main(after_settle(D(2026, 3, 17)))[0], 0)
            os.remove(job.stream_dir() / "boost-2026-03-16.jsonl")
            self.assert_refused_naming(job, after_settle(D(2026, 3, 18)), job.stream_dir() / "boost-2026-03-17.jsonl")

    def test_an_engine_log_header_changed_after_delivery_stops_the_job(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.daily(root)
            job.write_log(strategy_version="boost/r2/test")
            self.assert_refused_naming(job, after_settle(D(2026, 3, 17)), job.stream_dir() / "boost-{}.jsonl".format(WARM))

    def test_a_configuration_the_engine_did_not_log_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            edited = json.loads(job.config.read_text())
            edited["boost"]["warm_up_start"] = "2026-03-03"
            job.config.write_text(json.dumps(edited))
            status, _, err = job.main(after_settle(WARM), "--backfill-from", "2026-03-03")
            self.assertEqual(status, 1)
            self.assertIn("the engine's durable log", err)
            self.assertEqual(job.made, 0)
            self.assertFalse(job.data.exists())


class KeyScrubTest(unittest.TestCase):
    def test_an_error_that_carries_the_key_never_prints_it(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)

            class Leaky(FakeMassive):
                def grouped(self, day):
                    raise RuntimeError("401 for token " + SYNTHETIC_VALUE + " at " + SYNTHETIC_VALUE[:4])

            def factory(value):
                job.made += 1
                return Leaky()

            status, out, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat(), factory=factory)
            self.assertEqual(status, 1)
            self.assertNotIn(SYNTHETIC_VALUE, out + err)
            self.assertIn("401 for token [MASSIVE_API_KEY] at", err)

    def test_a_factory_that_fails_with_the_key_never_prints_it(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)

            def factory(value):
                raise ValueError("bad key {!r}".format(value))

            status, out, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat(), factory=factory)
            self.assertEqual(status, 1)
            self.assertNotIn(SYNTHETIC_VALUE, out + err)
            self.assertIn("[MASSIVE_API_KEY]", err)


class RetryAndInputTest(unittest.TestCase):
    def test_a_retry_after_a_refusal_reuses_the_recordings(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            self.assertEqual(job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())[0], 0)
            gone = job.data / "grouped_2026-03-05.json"
            kept = gone.read_bytes()
            gone.unlink()
            now = after_settle(D(2026, 3, 16))
            self.assertEqual(job.main(now)[0], 1)
            gone.write_bytes(kept)
            job.client.calls.clear()
            status, _, err = job.main(now)
            self.assertEqual(status, 0, err)
            self.assertEqual(job.client.calls, [])   # the day and its action window were recorded by the first try

    def test_action_windows_with_a_hole_reach_only_to_the_hole(self):
        with tempfile.TemporaryDirectory() as d:
            for kind in ("splits", "dividends"):
                for a, b in (("2026-03-03", "2026-03-06"), ("2026-03-10", "2026-03-13")):
                    record.write(d, {"kind": kind, "source": "massive", "session": b, "from": a,
                                     "fetched_at": "2026-03-13T22:00:00Z", "rows": []})
            self.assertEqual(tidata._actions_reach(d, D(2026, 3, 3)), D(2026, 3, 6))

    def test_a_configuration_that_is_not_boost_or_unreadable_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            cfg = json.loads(job.config.read_text())
            for change in (lambda c: c.update(strategy_id="momentum"), lambda c: c["boost"].pop("trend_days"),
                           lambda c: c["boost"].update(warm_up_start="2026-03-01")):   # a Sunday
                spoilt = json.loads(json.dumps(cfg))
                change(spoilt)
                job.config.write_text(json.dumps(spoilt))
                status, _, err = job.main(after_settle(WARM), "--backfill-from", BASE.isoformat())
                self.assertEqual(status, 1)
                self.assertIn(str(job.config), err)
            self.assertFalse(job.data.exists())

    def test_a_configuration_file_missing_or_not_json_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(tidata.Refused):
                tidata.read_config(str(Path(d, "absent.config")))
            Path(d, "bad.config").write_text("{not json")
            with self.assertRaises(tidata.Refused):
                tidata.read_config(str(Path(d, "bad.config")))

    def test_a_session_before_the_warm_up_start_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            job = Job(root)
            status, _, err = job.main(after_settle(WARM), "--session", "2026-02-27")
            self.assertEqual(status, 1)
            self.assertIn("precedes the warm-up start", err)

    def test_log_lines_out_of_shape_are_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            write_log(p, inputs=2)
            header, rec1, com1, rec2, com2 = p.read_text().splitlines()
            for lines in ([header, rec1, com1, rec1, com1],                      # a sequence repeated
                          [header, rec1, rec1.replace('"sequence":1,"kind":"input"', '"sequence":2,"kind":"input"')],
                          [header, '{"other":{}}'], [header, '{"record":{},"commit":{}}']):
                p.write_text("\n".join(lines) + "\n")
                with self.assertRaises(tidata.Refused):
                    tidata.read_engine_log(str(p))

    def test_the_streams_default_under_the_data_folder_and_bad_flags_are_refused(self):
        args = tidata.parse(["--data", "/d", "--config", "c", "--engine-log", "l.log"])
        self.assertEqual(args.streams, os.path.join("/d", "streams"))
        with mock.patch.dict(os.environ, {"TI_DATA_DIR": "", "CREDENTIALS_DIRECTORY": "/run/creds"}):
            with mock.patch("sys.stderr", io.StringIO()):
                for argv in (["--config", "c", "--engine-log", "l.log"],
                             ["--data", "/d", "--config", "c", "--engine-log", "l.log", "--settle-minutes", "-1"]):
                    with self.assertRaises(SystemExit):
                        tidata.parse(argv)
            self.assertEqual(tidata.parse(["--data", "/d", "--config", "c", "--engine-log", "l.log"]).secrets_file,
                             "/run/creds/secrets")


def git(cwd, *args):
    return subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
                           *args], cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()


class PackageTest(unittest.TestCase):
    """deploy/build-data.sh packs the job as one executable file, from a clean
    tree only, byte for byte the same for one commit, stamped with it."""

    def checkout(self, d):
        """A clean git checkout of what build-data.sh reads, so the test does
        not depend on the state of the working tree it runs in."""
        root = Path(d, "repo")
        for rel in ["deploy/build-data.sh", "scripts/build-stamp.sh"] + [
                str(p.relative_to(REPO)) for p in (REPO / "adapter/marketdata").glob("*.py")]:
            (root / rel).parent.mkdir(parents=True, exist_ok=True)
            (root / rel).write_bytes((REPO / rel).read_bytes())
            (root / rel).chmod((REPO / rel).stat().st_mode)
        git(root, "init", "-q")
        git(root, "add", "-A")
        git(root, "commit", "-q", "-m", "fixture")
        return root

    def build(self, root, out, **env):
        return subprocess.run([str(root / "deploy/build-data.sh"), str(out)], cwd=root, capture_output=True, text=True,
                              env=dict(os.environ, **env))

    def test_the_built_file_runs_the_job(self):
        with tempfile.TemporaryDirectory() as d:
            root = self.checkout(d)
            out = Path(d, "ti-data")
            result = self.build(root, out)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(os.access(out, os.X_OK))
            result = subprocess.run([sys.executable, str(out), "--help"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("--backfill-from", result.stdout)
            result = subprocess.run([sys.executable, str(out), "--data", d, "--streams", d, "--config", "x",
                                     "--engine-log", "x.log", "--secrets-file", str(Path(d, "none"))],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("ti-data refused", result.stderr)

    def test_the_built_file_carries_the_commit_it_was_built_from(self):
        with tempfile.TemporaryDirectory() as d:
            root = self.checkout(d)
            out = Path(d, "ti-data")
            self.assertEqual(self.build(root, out).returncode, 0)
            stamp = git(root, "describe", "--always", "--dirty")
            result = subprocess.run([sys.executable, str(out), "--version"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), "ti-data " + stamp)
            result = subprocess.run([sys.executable, str(out), "--data", d, "--config", "x", "--engine-log", "x.log",
                                     "--secrets-file", str(Path(d, "none"))], capture_output=True, text=True)
            self.assertIn("ti-data " + stamp, result.stderr)

    def test_two_builds_of_one_commit_are_byte_for_byte_the_same(self):
        with tempfile.TemporaryDirectory() as d:
            root = self.checkout(d)
            first, second = Path(d, "a"), Path(d, "b")
            self.assertEqual(self.build(root, first).returncode, 0)
            os.utime(root / "adapter/marketdata/tidata.py", (1, 1))   # a later checkout's mtimes
            time.sleep(2.1)   # past the zip format's two-second time resolution
            self.assertEqual(self.build(root, second).returncode, 0)
            self.assertEqual(first.read_bytes(), second.read_bytes())

    def test_a_dirty_tree_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root = self.checkout(d)
            out = Path(d, "ti-data")
            with open(root / "adapter/marketdata/tidata.py", "a") as f:
                f.write("# an uncommitted edit\n")
            result = self.build(root, out)
            self.assertEqual(result.returncode, 1)
            self.assertIn("dirty", result.stderr)
            self.assertFalse(out.exists())
            git(root, "checkout", "--", ".")
            Path(root, "adapter/marketdata/untracked.py").write_text("")
            self.assertEqual(self.build(root, out).returncode, 1)
            self.assertFalse(out.exists())


if __name__ == "__main__":
    unittest.main()
