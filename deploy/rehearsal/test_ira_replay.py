"""Offline server replay contract, using the real stamped engine and synthetic month (ADR 0026)."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import unittest
from unittest import mock
import time

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'deploy/ira-replay.sh'
MONTH = ROOT / 'cmd/engine/testdata/momentum-month.jsonl'



def durable_records(path):
    """The records of a durable log, skipping its header and the commit line that follows each record."""
    return [json.loads(x)['record'] for x in path.read_text().splitlines()[1:] if '"record"' in x]

class IRAReplayTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        probe = Path(tempfile.mkdtemp(dir='/tmp')) / 'probe.sock'
        try:
            with socket.socket(socket.AF_UNIX) as conn:
                conn.bind(str(probe))
        except PermissionError:
            raise unittest.SkipTest('sandbox denies Unix socket bind under /tmp; run in rehearsal container')
        finally:
            if probe.exists():
                probe.unlink()
            probe.parent.rmdir()
        cls.temp = tempfile.TemporaryDirectory(dir='/tmp')
        cls.base = Path(cls.temp.name)
        cls.engine = os.environ.get('TI_DRILL_ENGINE', str(cls.base / 'ti-engine'))
        if 'TI_DRILL_ENGINE' not in os.environ:
            subprocess.run(['go', 'build', '-ldflags',
                            '-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=rehearsal',
                            '-o', cls.engine, './cmd/engine'], cwd=ROOT, check=True)

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    def setUp(self):
        work = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(work.cleanup)
        self.work = Path(work.name)
        self.config = self.work / 'trad-ira.config'
        self.config.write_bytes(Path(os.environ.get('TI_DRILL_CONFIG', str(ROOT / 'cmd/engine/testdata/momentum-dry-run.json'))).read_bytes())
        self.env = dict(os.environ, TMPDIR='/tmp', TI_ENGINE_BIN=self.engine,
                        TI_IRA_CONFIG=str(self.config), TI_IRA_LIVE_SOCKET=str(self.work / 'live.sock'))
        self.stream = self.work / 'recorded.jsonl'
        month = Path(os.environ.get('TI_DRILL_MONTH', str(MONTH)))
        log = self.work / 'source.log'
        log.write_text(json.dumps(dict(configuration_hash='synthetic', strategy_version='synthetic')))
        result = subprocess.run(['python3', str(Path(__file__).with_name('ira-stream.py')), str(log),
                                 str(self.work / 'streams'), '--month-fixture', str(month)],
                                env=dict(os.environ, TI_ADAPTER_ROOT=str(ROOT)) if 'TI_DRILL_MONTH' not in os.environ else os.environ,
                                capture_output=True, text=True, check=True)
        self.stream.write_bytes(Path(result.stdout.strip()).read_bytes())

    def run_replay(self, close='2025-01-31T21:00:00Z'):
        return subprocess.run(['bash', str(SCRIPT), 'trad-ira', close, str(self.stream)],
                              env=self.env, capture_output=True, text=True, timeout=60)

    def test_replay_matches_on_time_and_preserves_source(self):
        before = self.stream.read_bytes()
        result = self.run_replay()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('2025-01-31T21:00:00Z strategy.targets.decided decided', result.stdout)
        evidence = Path(result.stdout.split('Evidence: ')[1].splitlines()[0])
        self.assertEqual((evidence / 'recorded.jsonl').read_bytes(), before)
        self.assertEqual(self.stream.read_bytes(), before)
        records = durable_records(evidence / 'replay.log')
        released = [x['envelope']['payload'] for x in records if x['envelope']['type'] == 'risk.order.released']
        self.assertTrue(released)
        self.assertTrue(all(x['send'] is False for x in released))
        # An on-time engine starts at the month's first close on exactly the copied inputs.
        control = Path(tempfile.mkdtemp(dir='/tmp'))
        proc = subprocess.Popen([self.engine, '-mode', 'dry-run', '-config', str(self.config),
                                 '-socket', str(control / 'engine.sock'), '-log', str(control / 'run.log'),
                                 '-out', str(control / 'run.journal'), '-as-of', '2025-01-02T21:00:00Z'],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(300):
                if (control / 'engine.sock').exists() or proc.poll() is not None:
                    break
                time.sleep(0.05)
            feed_spec = importlib.util.spec_from_file_location(
                'feed_month', ROOT / 'deploy/feed-month.py')
            feed = importlib.util.module_from_spec(feed_spec)
            feed_spec.loader.exec_module(feed)
            feed.feed(str(control / 'engine.sock'), str(control / 'run.log'), str(self.config), str(self.stream))
        finally:
            if proc.poll() is None:
                proc.terminate()
            proc.wait(timeout=15)
        ontime = durable_records(control / 'run.log')
        decision_types = {'strategy.targets.decided', 'strategy.order.planned', 'risk.order.released'}
        def decisions(entries):
            return [x['envelope']['payload'] for x in entries if x['envelope']['type'] in decision_types]
        self.assertEqual(decisions(records), decisions(ontime))

    def test_withheld_is_printed_and_fails_gate(self):
        cfg = json.loads(self.config.read_text())
        cfg['momentum']['score_lookback'] = 252
        self.config.write_text(json.dumps(cfg))
        result = self.run_replay()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('strategy.targets.decided withheld', result.stdout)
        self.assertIn('Evidence: ', result.stdout)

    def test_decided_month_with_refused_orders_fails_gate(self):
        cfg = json.loads(self.config.read_text())
        cfg['limits']['max_order_fraction'] = 0.01
        self.config.write_text(json.dumps(cfg))
        result = self.run_replay()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('risk.order.refused', result.stdout)
        self.assertNotIn('Replay gate: PASS', result.stdout)

    def test_live_socket_refused_without_touching_source(self):
        with socket.socket(socket.AF_UNIX) as live:
            live.bind(self.env['TI_IRA_LIVE_SOCKET'])
            live.listen()
            result = self.run_replay()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('live socket', result.stderr)
        self.assertNotIn('Evidence:', result.stdout)

    def test_paper_config_refused(self):
        cfg = json.loads(self.config.read_text())
        cfg['mode'] = 'paper'
        self.config.write_text(json.dumps(cfg))
        result = self.run_replay()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('dry-run', result.stderr)

    def test_missing_close_refused(self):
        result = self.run_replay('2025-02-28T21:00:00Z')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('month-end close', result.stderr)


class ReplayValidationTest(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location('ira_replay', ROOT / 'deploy/ira-replay.py')
        self.replay = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.replay)
        work = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(work.cleanup)
        self.work = Path(work.name)

    def test_inaccessible_or_stale_live_path_fails_closed(self):
        stale = self.work / 'engine.sock'
        stale.touch()
        with self.assertRaisesRegex(ValueError, 'live socket'):
            self.replay.live_guard(stale)

    def test_calendar_must_prove_month_end_and_close(self):
        rows = [json.loads(x) for x in Path(os.environ.get('TI_DRILL_MONTH', str(MONTH))).read_text().splitlines()]
        self.replay.check_close(rows, '2025-01-31T21:00:00Z')
        for close in ('2025-01-30T21:00:00Z', '2025-01-31T20:00:00Z'):
            with self.assertRaisesRegex(ValueError, 'month-end close'):
                self.replay.check_close(rows, close)
        rows = [r for r in rows if r['type'] != 'market.session.calendar']
        with self.assertRaisesRegex(ValueError, 'month-end close'):
            self.replay.check_close(rows, '2025-01-31T21:00:00Z')

    def test_durable_log_extracts_inputs_never_decisions_or_approvals(self):
        path = self.work / 'log'
        external = dict(type='market.session.closed', payload={'period_end': '2025-01-31T21:00:00Z'})
        entries = [dict(log_version=1), dict(record=dict(kind='decision', envelope=external)),
                   dict(record=dict(kind='input', envelope=dict(type='operator.run.approved'))),
                   dict(record=dict(kind='input', envelope=external))]
        path.write_text('\n'.join(json.dumps(x) for x in entries))
        self.assertEqual(list(self.replay.inputs(path)), [external])

    def test_paper_config_refused_before_engine_can_start(self):
        config = self.work / 'config'
        config.write_text(json.dumps(dict(strategy_id='momentum', mode='paper')))
        with mock.patch.dict(os.environ, TI_IRA_CONFIG=str(config)), mock.patch.object(self.replay.subprocess, 'Popen') as start:
            with self.assertRaisesRegex(ValueError, 'dry-run'):
                self.replay.replay('trad-ira', '2025-01-31T21:00:00Z', 'unused')
            start.assert_not_called()

    def test_missing_close_keeps_copy_and_never_starts_engine(self):
        config, source = self.work / 'config', self.work / 'source'
        config.write_text(json.dumps(dict(strategy_id='momentum', mode='dry-run')))
        source.write_text('')
        out = io.StringIO()
        with mock.patch.dict(os.environ, TI_IRA_CONFIG=str(config)), mock.patch.object(self.replay.subprocess, 'Popen') as start:
            with contextlib.redirect_stdout(out), self.assertRaisesRegex(ValueError, 'month-end close'):
                self.replay.replay('trad-ira', '2025-01-31T21:00:00Z', source)
            start.assert_not_called()
        evidence = Path(out.getvalue().split('Evidence: ')[1].strip())
        self.assertEqual((evidence / 'recorded.jsonl').read_bytes(), b'')
