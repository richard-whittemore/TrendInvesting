"""Offline server replay contract, using the real stamped engine and synthetic month (ADR 0026)."""
import contextlib
import importlib.util
import io
import hashlib
import copy
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
        # ADR 0027 isolates rehearsals; the container stand-in declares this test instance stopped.
        command = self.work / 'systemctl'
        command.write_text('#!/bin/sh\nprintf "inactive\\n"\n')
        command.chmod(0o700)
        self.env['PATH'] = str(self.work) + os.pathsep + os.environ['PATH']
        self.record_month()

    def record_month(self):
        """Record the adapter and owner input shapes under the actual configuration (ADR 0026)."""
        folder = Path(tempfile.mkdtemp(dir=self.work))
        self.stream = folder / 'source.log'
        cfg = json.loads(self.config.read_text())
        approval = folder / 'approval.json'
        approval.write_text(json.dumps(dict(account_id=cfg['account_id'], previous_log='',
            previous_log_final_hash='', incident_id='', approved_by='synthetic-owner',
            approved_at='2025-01-02T13:00:00Z', boost=None,
            momentum=dict(rebalance_in_progress=None), incident_in_force=None,
            starting_book=dict(positions=[], total_cash=10000, currency='USD'))))
        rows = self.month_rows(Path(os.environ.get('TI_DRILL_MONTH', str(MONTH))), cfg)
        feed_path = folder / 'inputs.jsonl'
        feed_path.write_text(''.join(json.dumps(row) + '\n' for row in rows))
        output_path = folder / 'engine-output.txt'
        with output_path.open('w') as output:
            proc = subprocess.Popen([self.engine, '-mode', 'dry-run', '-config', str(self.config),
                '-approval', str(approval), '-socket', str(folder / 'engine.sock'), '-log', str(self.stream),
                '-out', str(folder / 'source.journal'), '-as-of', '2025-01-02T21:00:00Z'],
                stdout=output, stderr=output)
            try:
                for _ in range(300):
                    if (folder / 'engine.sock').exists() or proc.poll() is not None:
                        break
                    time.sleep(0.05)
                self.assertTrue((folder / 'engine.sock').exists(), output_path.read_text())
                feed_spec = importlib.util.spec_from_file_location('feed_month', ROOT / 'deploy/feed-month.py')
                feed = importlib.util.module_from_spec(feed_spec)
                feed_spec.loader.exec_module(feed)
                feed.feed(str(folder / 'engine.sock'), str(self.stream), str(self.config), str(feed_path), first_sequence=3)
            finally:
                if proc.poll() is None:
                    proc.terminate()
                self.assertEqual(proc.wait(timeout=15), 0, output_path.read_text())

    @staticmethod
    def month_rows(month, cfg):
        """Synthetic producer inputs, including the post-close delivery order in ADR 0026."""
        rows = []
        for raw in month.read_text().splitlines():
            row = json.loads(raw)
            if row['type'] == 'account.snapshot':
                p = row['payload']
                at = row['event_time']
                if at[:10] >= '2025-01-03':
                    for key in ('net_liquidation', 'total_cash', 'settled_cash'):
                        p[key] += 100
                if at[:10] == '2025-01-03':
                    transfer = at[:10] + 'T05:00:00Z'
                    rows.append(dict(type='account.cash-movement', schema_version=2, id='ibkr-flex:synthetic-deposit',
                        event_time=transfer, payload=dict(as_of=transfer, amount=100, equity_before=10000,
                                                         currency='USD', transaction_id='synthetic-deposit')))
                    rows.append(dict(type='account.flex-status', schema_version=1, id='synthetic-flex-read',
                        event_time=at, payload=dict(account_id=cfg['account_id'], state='read', checked_at=at,
                                                   covered_to='20250103')))
                # ADR 0026 preserves the broker's after-close reading before its close-stamped account snapshot.
                read_at = at[:10] + 'T21:01:00Z'
                rows.append(dict(type='account.broker.snapshot', schema_version=2, id='synthetic-broker:' + at[:10],
                    event_time=read_at, payload=dict(account_id=cfg['account_id'], as_of=at,
                        read_at=read_at, as_of_close=True, market_open=False, unreadable=[],
                        net_liquidation=p['net_liquidation'], read_net_liquidation=p['net_liquidation'],
                        excess_liquidity=p['net_liquidation'], positions=[], open_orders=[])))
            rows.append(row)
        rows.append(dict(type='operator.alert.acknowledged', schema_version=1, id='synthetic-owner-ack',
            event_time='2025-01-31T21:02:00Z', payload=dict(account_id=cfg['account_id'],
                alert_key='synthetic-page', acknowledged_by='synthetic-owner',
                acknowledged_at='2025-01-31T21:02:00Z', detail='synthetic rehearsal acknowledgement')))
        return rows

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
        def raw_input_payloads(path):
            payloads = []
            for line in path.read_text().splitlines():
                record = json.loads(line).get('record', {})
                if record.get('kind') != 'input':
                    continue
                raw = line[line.index('"payload":') + len('"payload":'):].lstrip()
                _, end = json.JSONDecoder().raw_decode(raw)
                payloads.append(raw[:end])
            return payloads
        self.assertEqual(raw_input_payloads(self.stream), raw_input_payloads(evidence / 'run/replay.log'))
        records = durable_records(evidence / 'run/replay.log')
        released = [x['envelope']['payload'] for x in records if x['envelope']['type'] == 'risk.order.released']
        self.assertTrue(released)
        self.assertTrue(all(x['send'] is False for x in released))
        recorded = durable_records(self.stream)
        def inputs(entries):
            return [x['envelope'] for x in entries if x['kind'] == 'input']
        original, replayed = inputs(recorded), inputs(records)
        self.assertEqual([r['type'] for r in original], [r['type'] for r in replayed])
        self.assertTrue({'operator.run.approved', 'account.broker.snapshot', 'account.cash-movement',
                         'account.flex-status', 'operator.alert.acknowledged'} <= {r['type'] for r in original})
        for before, after in zip(original, replayed):
            self.assertEqual(before['payload'], after['payload'])
            self.assertEqual(before['payload_hash'], after['payload_hash'])
            identity = {'sequence', 'strategy_version', 'configuration_hash', 'payload_hash'}
            self.assertEqual({k: v for k, v in before.items() if k not in identity},
                             {k: v for k, v in after.items() if k not in identity})
        spec = importlib.util.spec_from_file_location('ira_replay', ROOT / 'deploy/ira-replay.py')
        replay = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(replay)
        replay.compare_month([r['envelope'] for r in recorded if r['kind'] == 'decision'],
                             [r['envelope'] for r in records if r['kind'] == 'decision'], '2025-01-31T21:00:00Z')
        self.assertIn('Replay gate: PASS', result.stdout)

    def test_changed_installed_configuration_is_refused(self):
        cfg = json.loads(self.config.read_text())
        cfg['limits']['max_order_fraction'] = 0.5
        self.config.write_text(json.dumps(cfg))
        result = self.run_replay()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('configuration', result.stderr)
        self.assertNotIn('Replay gate: PASS', result.stdout)

    def test_withheld_is_printed_and_fails_gate(self):
        cfg = json.loads(self.config.read_text())
        cfg['momentum']['score_lookback'] = 252
        self.config.write_text(json.dumps(cfg))
        self.record_month()
        result = self.run_replay()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('strategy.targets.decided withheld', result.stdout)
        self.assertIn('Evidence: ', result.stdout)

    def test_decided_month_with_refused_orders_fails_gate(self):
        cfg = json.loads(self.config.read_text())
        cfg['limits']['max_order_fraction'] = 0.01
        self.config.write_text(json.dumps(cfg))
        self.record_month()
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


class RecordedMonthFixtureTest(unittest.TestCase):
    def test_recording_fixture_has_consistent_adapter_and_owner_inputs(self):
        # ADR 0026's account-snapshot amendment keeps read time and close time distinct.
        cfg = json.loads(Path(os.environ.get('TI_DRILL_CONFIG', str(ROOT / 'cmd/engine/testdata/momentum-dry-run.json'))).read_text())
        rows = IRAReplayTest.month_rows(Path(os.environ.get('TI_DRILL_MONTH', str(MONTH))), cfg)
        brokers = [i for i, row in enumerate(rows) if row['type'] == 'account.broker.snapshot']
        self.assertGreater(len(brokers), 1)
        for i in brokers:
            broker, snapshot = rows[i], rows[i + 1]
            self.assertEqual(snapshot['type'], 'account.snapshot')
            self.assertEqual(broker['schema_version'], 2)
            self.assertEqual(broker['payload']['as_of'], snapshot['payload']['as_of'])
            self.assertEqual(broker['payload']['read_at'], broker['event_time'])
            self.assertGreater(broker['event_time'], snapshot['event_time'])
            self.assertTrue(broker['payload']['as_of_close'])
            self.assertEqual(broker['payload']['net_liquidation'], snapshot['payload']['net_liquidation'])
            self.assertEqual(broker['payload']['positions'], [])
            self.assertEqual(broker['payload']['open_orders'], [])
        movement = next(r for r in rows if r['type'] == 'account.cash-movement')
        self.assertEqual(movement['schema_version'], 2)
        self.assertEqual(movement['id'], 'ibkr-flex:' + movement['payload']['transaction_id'])
        snapshots = [r for r in rows if r['type'] == 'account.snapshot']
        self.assertEqual(snapshots[1]['payload']['total_cash'] - snapshots[0]['payload']['total_cash'],
                         movement['payload']['amount'])
        status = next(r for r in rows if r['type'] == 'account.flex-status')
        time.strptime(status['payload']['covered_to'], '%Y%m%d')
        self.assertEqual(status['payload']['state'], 'read')
        self.assertEqual(rows[-1]['type'], 'operator.alert.acknowledged')
        self.assertGreater(rows[-1]['event_time'], '2025-01-31T21:00:00Z')


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

    def test_paper_config_refused_before_engine_can_start(self):
        config = self.work / 'config'
        config.write_text(json.dumps(dict(strategy_id='momentum', mode='paper')))
        with mock.patch.dict(os.environ, TI_IRA_CONFIG=str(config)), mock.patch.object(self.replay, 'service_guard'), \
                mock.patch.object(self.replay.subprocess, 'Popen') as start:
            with self.assertRaisesRegex(ValueError, 'dry-run'):
                self.replay.replay('trad-ira', '2025-01-31T21:00:00Z', 'unused')
            start.assert_not_called()

    def test_missing_close_keeps_copy_and_never_starts_engine(self):
        config, source = self.work / 'config', self.work / 'source'
        config.write_text(json.dumps(dict(strategy_id='momentum', mode='dry-run')))
        source.write_text('')
        out = io.StringIO()
        with mock.patch.dict(os.environ, TI_IRA_CONFIG=str(config)), mock.patch.object(self.replay, 'service_guard'), \
                mock.patch.object(self.replay.subprocess, 'Popen') as start:
            with contextlib.redirect_stdout(out), self.assertRaisesRegex(ValueError, 'durable log'):
                self.replay.replay('trad-ira', '2025-01-31T21:00:00Z', source)
            start.assert_not_called()
        evidence = Path(out.getvalue().split('Evidence: ')[1].strip())
        self.assertEqual((evidence / 'recorded.jsonl').read_bytes(), b'')


class RecordedReplayTest(ReplayValidationTest):
    def setUp(self):
        super().setUp()
        self.log = self.replay.log_reader()
        self.header = dict(log_version=1, chain_algorithm=self.log.LOG_CHAIN_ALGORITHM,
                           configuration_hash='sha256:' + '1' * 64, strategy_version='synthetic')
        self.path = self.work / 'recorded.log'

    def envelope(self, kind, payload, at='2025-01-31T21:00:00Z'):
        return dict(id='synthetic:' + kind, type=kind, envelope_version=1, schema_version=1,
                    event_time=at, recorded_at=at, sequence=1, source='synthetic',
                    strategy_version=self.header['strategy_version'], configuration_hash=self.header['configuration_hash'],
                    payload_hash=hashlib.sha256(json.dumps(payload, separators=(',', ':')).encode()).hexdigest(), payload=payload)

    def write_log(self, groups, tail=False):
        chain = self.log.chain_seed(self.header)
        lines = [json.dumps(self.header)]
        sequence = 0
        for group in groups:
            for kind, env in group:
                sequence += 1
                env = dict(env, sequence=sequence)
                raw = json.dumps(env['payload'], separators=(',', ':'))
                chain = self.log.chain_next(chain, kind, env, raw.encode())
                record = dict(sequence=sequence, kind=kind, envelope=env, record_hash='sha256:' + chain.hex())
                lines.append(json.dumps(dict(record=record), separators=(',', ':')))
            lines.append(json.dumps(dict(commit=dict(sequence=sequence, record_hash='sha256:' + chain.hex()))))
        if tail:
            lines.pop()
        self.path.write_text('\n'.join(lines) + '\n')

    def test_committed_inputs_keep_approval_ack_flex_and_after_close_broker_in_delivery_order(self):
        types = ['strategy.target-weight.configuration', 'operator.run.approved', 'account.flex.status',
                 'operator.alert.acknowledged', 'account.broker.snapshot', 'account.snapshot', 'market.session.closed']
        rows = [self.envelope(t, {'value': i}, '2025-01-31T21:01:00Z' if t == 'account.broker.snapshot'
                              else '2025-01-31T21:00:00Z') for i, t in enumerate(types)]
        self.write_log([[('input', r)] for r in rows])
        self.assertEqual([r['type'] for r in self.replay.inputs(self.path)], types)

    def test_uncommitted_complete_input_is_not_authoritative(self):
        self.write_log([[('input', self.envelope('market.session.closed', {}))]], tail=True)
        self.assertEqual(list(self.replay.inputs(self.path)), [])

    def test_tampered_payload_record_hash_and_commit_are_rejected(self):
        for field in ('payload', 'record_hash', 'commit'):
            with self.subTest(field=field):
                self.write_log([[('input', self.envelope('market.session.closed', {'value': 1}))]])
                text = self.path.read_text()
                if field == 'payload':
                    text = text.replace('"value":1', '"value":2')
                elif field == 'record_hash':
                    text = text.replace('"record_hash":"sha256:', '"record_hash":"sha256:0', 1)
                else:
                    lines = text.splitlines()
                    commit = json.loads(lines[-1]); commit['commit']['record_hash'] = 'sha256:' + '0' * 64
                    lines[-1] = json.dumps(commit); text = '\n'.join(lines) + '\n'
                self.path.write_text(text)
                with self.assertRaisesRegex(ValueError, 'durable log|chain|hash|commit'):
                    list(self.replay.inputs(self.path))

    def test_chain_rejects_changed_payload_even_when_payload_hash_repaired(self):
        self.write_log([[('input', self.envelope('market.session.closed', {'value': 1}))]])
        text = self.path.read_text().replace('"value":1', '"value":2')
        text = text.replace(hashlib.sha256(b'{"value":1}').hexdigest(), hashlib.sha256(b'{"value":2}').hexdigest())
        self.path.write_text(text)
        with self.assertRaisesRegex(ValueError, 'chain'):
            list(self.replay.inputs(self.path))

    def test_rewrite_preserves_all_fields_except_run_identity_and_exact_payload_bytes(self):
        row = self.envelope('operator.alert.acknowledged', {'detail': 'a b'})
        row.update(correlation_id='original', causation_id='cause')
        raw = '{"detail": "a b"}'
        row['payload_hash'] = hashlib.sha256(raw.encode()).hexdigest()
        encoded = self.replay.rewrite(row, raw, 19, self.header)
        self.assertIn(raw, encoded)
        got = json.loads(encoded)
        self.assertEqual(got, dict(row, sequence=19))

    def test_decision_plan_and_release_first_difference_is_reported(self):
        close = '2025-01-31T21:00:00Z'
        recorded = [dict(id='decision', type='strategy.targets.decided', payload=dict(session_period_end=close,
                      outcome='decided', targets=[dict(instrument_id='SYNTH', weight=0.5)])),
                    dict(type='strategy.order.planned', payload=dict(tag='decision', client_order_id='old', quantity=10)),
                    dict(type='risk.order.released', payload=dict(tag='decision', client_order_id='old', quantity=10, send=False))]
        changed = copy.deepcopy(recorded)
        changed[1]['payload']['client_order_id'] = 'new'
        self.replay.compare_month(recorded, changed, close)
        for index, key, value in ((0, 'outcome', 'withheld'), (1, 'quantity', 11), (2, 'send', True)):
            with self.subTest(index=index):
                changed = copy.deepcopy(recorded); changed[index]['payload'][key] = value
                with self.assertRaisesRegex(ValueError, key):
                    self.replay.compare_month(recorded, changed, close)
        with self.assertRaisesRegex(ValueError, 'length'):
            self.replay.compare_month(recorded, recorded[:-1], close)

    def test_systemd_starting_active_and_unknown_fail_closed(self):
        for state in ('activating', 'active', 'deactivating', 'unknown'):
            with self.subTest(state=state), mock.patch.object(self.replay.shutil, 'which', return_value='/bin/systemctl'), \
                    mock.patch.object(self.replay.subprocess, 'run', return_value=mock.Mock(returncode=0, stdout=state)):
                with self.assertRaisesRegex(ValueError, 'unit'):
                    self.replay.service_guard('trad-ira')

    def test_diagnostics_mask_identifiers_in_input_id_and_frame(self):
        text = self.replay.redact('input:DUSYNTHETIC:bad account_id=trad-ira U1234567', 'trad-ira')
        self.assertNotIn('DUSYNTHETIC', text)
        self.assertNotIn('trad-ira', text)
        self.assertNotIn('U1234567', text)

    def test_changed_configuration_or_strategy_identity_is_rejected(self):
        for field in ('configuration_hash', 'strategy_version'):
            with self.subTest(field=field):
                changed = dict(self.header, **{field: 'different'})
                with self.assertRaisesRegex(ValueError, field):
                    self.replay.check_identity(self.header, changed)

    def test_shutdown_failure_never_announces_pass(self):
        for failure in (1, subprocess.TimeoutExpired('engine', 15)):
            process = mock.Mock()
            process.poll.return_value = None
            process.wait.side_effect = [failure, 0] if isinstance(failure, Exception) else None
            process.wait.return_value = failure
            out = io.StringIO()
            with contextlib.redirect_stdout(out), self.assertRaisesRegex(ValueError, 'stop|exited'):
                self.replay.finish(process)
            self.assertNotIn('PASS', out.getvalue())

    def test_replay_retains_every_input_and_approval_and_waits_before_pass(self):
        self.exercise_replay()

    def test_replay_shutdown_timeout_and_error_never_print_pass(self):
        for failure in (1, subprocess.TimeoutExpired('engine', 15)):
            with self.subTest(failure=failure):
                self.exercise_replay(failure)

    def exercise_replay(self, failure=None):
        cfg = dict(strategy_id='momentum', mode='dry-run', account_id='synthetic-ira')
        close = '2025-01-31T21:00:00Z'
        start = '2025-01-02T21:00:00Z'
        approval = dict(previous_log='', momentum=dict(rebalance_in_progress=dict(phase='sells-working')))
        rows = [self.envelope('strategy.target-weight.configuration', cfg, start),
                self.envelope('operator.run.approved', approval, start),
                self.envelope('market.session.calendar', dict(sessions=['2025-01-31', '2025-02-03'])),
                self.envelope('account.broker.snapshot', dict(account_id='synthetic-ira'), '2025-01-31T21:01:00Z'),
                self.envelope('account.snapshot', dict(account_id='synthetic-ira')),
                self.envelope('market.session.closed', dict(period_end=close)),
                self.envelope('operator.alert.acknowledged', dict(detail='seen'), '2025-02-01T12:00:00Z')]
        decision = self.envelope('strategy.targets.decided', dict(session_period_end=close, outcome='decided'))
        self.write_log([[('input', row)] + ([('decision', decision)] if i == 5 else []) for i, row in enumerate(rows)])
        source_bytes = self.path.read_bytes()
        config = self.work / 'config.json'; config.write_text(json.dumps(cfg))
        output = io.StringIO()
        process = mock.Mock(); process.poll.return_value = None; process.wait.return_value = 0
        sent = []
        replies = io.StringIO(''.join(json.dumps(dict(envelope=dict(type='engine.decisions', sequence=i + 3))) + '\n'
                                    for i in range(len(rows) - 2)))
        conn = mock.MagicMock(); conn.__enter__.return_value = conn
        conn.makefile.return_value = replies
        conn.sendall.side_effect = lambda raw: sent.append(json.loads(raw))
        def launch(command, **kwargs):
            self.assertEqual(command[command.index('-as-of') + 1], start)
            self.assertEqual(json.loads(Path(command[command.index('-approval') + 1]).read_text()), approval)
            Path(command[command.index('-socket') + 1]).touch()
            self.write_log([[('input', row)] for row in rows[:2]])
            Path(command[command.index('-log') + 1]).write_bytes(self.path.read_bytes())
            self.path.write_bytes(source_bytes)
            waits = []
            def wait(timeout=15):
                self.assertNotIn('Replay gate: PASS', output.getvalue())
                waits.append(timeout)
                if len(waits) == 1 and isinstance(failure, Exception):
                    raise failure
                if failure and not isinstance(failure, Exception):
                    return failure
                Path(command[command.index('-log') + 1]).write_bytes(source_bytes)
                return 0
            process.wait.side_effect = wait
            return process
        with mock.patch.dict(os.environ, TI_IRA_CONFIG=str(config)), mock.patch.object(self.replay, 'service_guard'), \
                mock.patch.object(self.replay.subprocess, 'Popen', side_effect=launch), \
                mock.patch.object(self.replay.socket, 'socket', return_value=conn), contextlib.redirect_stdout(output), \
                (self.assertRaisesRegex(ValueError, 'stop|exited') if failure else contextlib.nullcontext()):
            self.replay.replay('trad-ira', close, self.path)
        self.assertEqual([r['type'] for r in sent], [r['type'] for r in rows[2:]])
        for old, new in zip(rows[2:], sent):
            for key in old.keys() - {'sequence', 'strategy_version', 'configuration_hash', 'payload_hash'}:
                self.assertEqual(new[key], old[key])
        if failure:
            self.assertNotIn('Replay gate: PASS', output.getvalue())
        else:
            self.assertIn('Replay gate: PASS', output.getvalue())

    def test_plan_reference_normalizes_only_its_client_order_identity(self):
        close = '2025-01-31T21:00:00Z'
        original = [dict(id='decision', type='strategy.targets.decided', payload=dict(session_period_end=close)),
                    dict(type='risk.order.released', payload=dict(tag='decision', client_order_id='old',
                                                                 plan_id='order-planned:old', quantity=10))]
        replayed = copy.deepcopy(original)
        replayed[1]['payload'].update(client_order_id='new', plan_id='order-planned:new')
        self.replay.compare_month(original, replayed, close)
        replayed[1]['payload']['plan_id'] = 'wrong-plan'
        with self.assertRaisesRegex(ValueError, 'plan_id'):
            self.replay.compare_month(original, replayed, close)

    def test_approval_copies_history_before_configuration_only_predecessor(self):
        self.write_log([[('input', self.envelope('strategy.target-weight.configuration', {}))]])
        previous = self.work / 'previous.log'
        previous.write_bytes(self.path.read_bytes())
        previous_hash = json.loads(previous.read_text().splitlines()[-1])['commit']['record_hash']
        older = self.work / 'older.log'; older.write_bytes(previous.read_bytes())
        os.utime(older, (1, 1)); os.utime(previous, (2, 2))
        self.path.write_text('recorded source is supplied separately')
        approval = dict(previous_log=previous.name, previous_log_final_hash=previous_hash)
        rows = [(self.envelope('strategy.target-weight.configuration', {}), '{}'),
                (self.envelope('operator.run.approved', approval), json.dumps(approval))]
        target = self.work / 'run'; target.mkdir()
        self.replay.approval_history(self.path, rows, target)
        self.assertEqual((target / older.name).read_bytes(), older.read_bytes())
        self.assertEqual((target / previous.name).read_bytes(), previous.read_bytes())
        self.assertLess((target / older.name).stat().st_mtime, (target / previous.name).stat().st_mtime)

    def test_diagnostics_mask_embedded_and_structured_accounts(self):
        text = self.replay.redact('prefixDUSYNTHETIC and {"account_id": "synthetic-other"}')
        self.assertNotIn('DUSYNTHETIC', text)
        self.assertNotIn('synthetic-other', text)

    def test_comparison_preserves_non_identity_envelope_fields(self):
        close = '2025-01-31T21:00:00Z'
        original = [self.envelope('strategy.targets.decided', dict(session_period_end=close, outcome='decided'))]
        for field, value in (('schema_version', 7), ('source', 'other'), ('recorded_at', '2025-02-01T21:00:00Z')):
            replayed = copy.deepcopy(original)
            replayed[0][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, field):
                self.replay.compare_month(original, replayed, close)
