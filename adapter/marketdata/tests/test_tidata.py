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
import unittest
from pathlib import Path
from unittest import mock

from marketdata import nyse_calendar, record, tidata

D = dt.date
UTC = dt.timezone.utc
REPO = Path(__file__).resolve().parents[3]
KEY = "synthetic-key-0123456789"
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
        self.calls, self.empty = [], set(empty)

    def grouped(self, day):
        self.calls.append(("grouped", day))
        if day in self.empty:
            return {"status": "OK", "resultsCount": 0}
        return {"status": "OK", "results": [spy(day), {"T": "OTHER", "o": 9, "h": 9, "l": 9, "c": 9, "v": 1, "t": 0}]}

    def splits(self, first, last):
        self.calls.append(("splits", first, last))
        return [{"results": []}]

    def all_dividends(self, first, last):
        self.calls.append(("dividends", first, last))
        return [{"results": []}]


def write_log(path, configuration_hash="hash-boost", strategy_version="boost/r1/test", inputs=1):
    """A durable log as the engine writes one: a header, then one committed
    group per input (here inputs only, no decisions)."""
    lines = [json.dumps({"chain_algorithm": CHAIN, "configuration_hash": configuration_hash, "log_version": 1,
                         "strategy_version": strategy_version}, sort_keys=True, separators=(",", ":"))]
    for n in range(1, inputs + 1):
        payload = json.dumps({"n": n}, separators=(",", ":"))
        envelope = {"id": "configuration:" + configuration_hash if n == 1 else "input-{}".format(n),
                    "type": "x", "envelope_version": 1, "schema_version": 1,
                    "event_time": "2026-03-13T21:00:00Z", "recorded_at": "2026-03-13T21:00:00Z", "sequence": n,
                    "source": "engine", "strategy_version": strategy_version,
                    "configuration_hash": configuration_hash,
                    "payload_hash": hashlib.sha256(payload.encode()).hexdigest(), "payload": json.loads(payload)}
        digest = "sha256:" + hashlib.sha256(str(n).encode()).hexdigest()
        lines.append(json.dumps({"record": {"sequence": n, "kind": "input", "envelope": envelope,
                                            "record_hash": digest}}, separators=(",", ":")))
        lines.append(json.dumps({"commit": {"sequence": n, "record_hash": digest}}, separators=(",", ":")))
    Path(path).write_text("\n".join(lines) + "\n")


class Job:
    """A temporary server layout: data folder, config, engine log, secrets."""

    def __init__(self, root, trend_days=5, warm_up_start=BASE, secrets="MASSIVE_API_KEY=" + KEY + "\n"):
        self.root = Path(root)
        self.data = self.root / "data"
        self.streams = self.data / "streams"
        self.config = self.root / "boost.config"
        cfg = json.loads((REPO / "deploy/etc/engines/boost.config").read_text())
        cfg["boost"].update(trend_days=trend_days, warm_up_start=warm_up_start.isoformat())
        self.config.write_text(json.dumps(cfg))
        self.log = self.root / (RUN + ".log")
        write_log(self.log)
        self.secrets = self.root / "secrets"
        if secrets is not None:
            self.secrets.write_text(secrets)
        self.client = FakeMassive()
        self.made = 0

    def factory(self, key):
        if key != KEY:
            raise AssertionError("the job built a client with the wrong key")
        self.made += 1
        return self.client

    def argv(self, *extra):
        return ["--data", str(self.data), "--streams", str(self.streams), "--config", str(self.config),
                "--engine-log", str(self.log), "--secrets-file", str(self.secrets)] + list(extra)

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
        when = re.search(r"^OnCalendar=Mon\.\.Fri (\d\d):(\d\d) America/New_York$", timer, re.M)
        self.assertIsNotNone(when, "the timer must fire Mon..Fri at a New York time")
        self.assertRegex(timer, r"(?m)^Persistent=true$")
        minutes = re.search(r"--settle-minutes (\d+)", service)
        self.assertIsNotNone(minutes, "ExecStart must pass --settle-minutes")
        settle = dt.timedelta(minutes=int(minutes.group(1)))
        hour, minute = int(when.group(1)), int(when.group(2))
        for day in nyse_calendar.sessions(D(2026, 1, 1), D(2027, 12, 31)):
            fire = dt.datetime(day.year, day.month, day.day, hour, minute, tzinfo=nyse_calendar.NEW_YORK)
            self.assertEqual(tidata.settled_session(fire, settle), day, day)


class SecretsTest(unittest.TestCase):
    def test_the_key_is_read_from_the_secrets_file(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, "s")
            p.write_text("# comment\nIBKR_ACCOUNT_BOOST=\nMASSIVE_API_KEY= {} \nNTFY_URL=\n".format(KEY))
            self.assertEqual(tidata.read_key(str(p)), KEY)

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
    def test_identity_and_next_sequence_come_from_the_durable_log(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            write_log(p, inputs=2)
            got = tidata.read_engine_log(str(p))
            self.assertEqual(got, tidata.EngineLog("hash-boost", "boost/r1/test", RUN, 3))

    def test_a_group_not_yet_committed_holds_no_input(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, RUN + ".log")
            write_log(p, inputs=2)
            lines = p.read_text().splitlines()
            p.write_text("\n".join(lines[:-1]) + "\n")   # the second input's commit is missing
            self.assertEqual(tidata.read_engine_log(str(p)).next_sequence, 2)

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
            self.assertEqual([e["sequence"] for e in stream], list(range(2, 2 + len(stream))))
            self.assertEqual({(e["configuration_hash"], e["strategy_version"], e["correlation_id"])
                              for e in stream}, {("hash-boost", "boost/r1/test", RUN)})
            self.assertEqual({e["payload"]["adjusted_base"] for e in bars}, {BASE.isoformat()})
            self.assertNotIn(KEY, out + err)
            for p in job.root.rglob("*"):
                if p.is_file() and p != job.secrets:
                    self.assertNotIn(KEY, p.read_text())

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

    def test_the_next_session_is_its_own_stream_continuing_the_sequence(self):
        with tempfile.TemporaryDirectory() as root:
            job = self.warmed(root)
            last = job.envelopes(WARM)[-1]["sequence"]
            status, out, err = job.main(after_settle(D(2026, 3, 16)))
            self.assertEqual(status, 0, err)
            self.assertEqual(out.strip(), str(job.stream_dir() / "boost-2026-03-16.jsonl"))
            self.assertEqual([c for c in job.client.calls if c[0] == "grouped"], [("grouped", D(2026, 3, 16))])
            stream = job.envelopes(D(2026, 3, 16))
            self.assertEqual([e["type"] for e in stream], ["market.bar.completed", "market.session.closed"])
            self.assertEqual([e["sequence"] for e in stream], [last + 1, last + 2])
            self.assertEqual(stream[0]["payload"]["raw"]["close"], spy(D(2026, 3, 16))["c"])

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
            write_log(job.log, inputs=3)   # the engine has taken inputs since; the file is unchanged
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
            write_log(job.log, configuration_hash="hash-other")
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
        for secrets in (None, "", "MASSIVE_API_KEY=\n", "NTFY_URL=x\n"):
            with tempfile.TemporaryDirectory() as root:
                job = Job(root, secrets=secrets)
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


class PackageTest(unittest.TestCase):
    """deploy/build-data.sh packs the job as one executable file."""

    def test_the_built_file_runs_the_job(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d, "ti-data")
            subprocess.run(["deploy/build-data.sh", str(out)], cwd=REPO, check=True, capture_output=True)
            self.assertTrue(os.access(out, os.X_OK))
            result = subprocess.run([sys.executable, str(out), "--help"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("--backfill-from", result.stdout)
            result = subprocess.run([sys.executable, str(out), "--data", d, "--streams", d, "--config", "x",
                                     "--engine-log", "x.log", "--secrets-file", str(Path(d, "none"))],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("ti-data refused", result.stderr)


if __name__ == "__main__":
    unittest.main()
