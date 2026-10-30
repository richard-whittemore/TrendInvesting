"""Run the missed-rebalance runbook against a fake venue, never IBKR (ADR 0026)."""
import os
from pathlib import Path
import subprocess
import unittest

HERE = Path(__file__).resolve().parent


class IRADrillTest(unittest.TestCase):
    def test_restart_hold_clearance_and_next_month_equivalence(self):
        # Distinct operator/engine users are a security invariant, not mocked away (ADR 0026).
        if os.geteuid() != 0 or not Path('/dist/ti-engine').exists():
            self.skipTest('requires the provisioned rehearsal container and its separate users')
        result = subprocess.run(['python3', str(HERE / 'ira-drills.py')],
                                capture_output=True, text=True, timeout=90, env=dict(os.environ, TMPDIR='/tmp'))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        for line in ('STOPPED: sells working; buys not released',
                     'HELD: rebalance-in-progress; no orders released',
                     'RECONNECTED: old sell cancellations reported and refused as never released by this run',
                     'CLEARED: operator.rebalance.cleared recorded',
                     'RECOVERED: next month equals on-time targets and orders; no old buys',
                     'FAKE ONLY: no adapter or broker connection'):
            self.assertIn(line, result.stdout)


class SyntheticMonthTest(unittest.TestCase):
    def test_following_month_keeps_base_and_never_recloses_january(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location('ira_drills', HERE / 'ira-drills.py')
        drills = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(drills)
        fixture = Path(os.environ.get('TI_DRILL_MONTH', str(HERE.parents[1] / 'cmd/engine/testdata/momentum-month.jsonl')))
        rows = drills.month(fixture, 28)
        self.assertTrue(any(r['event_time'].startswith('2025-02-28') for r in rows))
        for row in rows:
            if row['type'] == 'market.session.calendar':
                continue
            self.assertGreaterEqual(row['event_time'][:10], '2025-02-01')
            if row['type'] == 'market.bar.completed':
                self.assertEqual(row['payload']['adjusted_base'], '2025-01-02')


class DrillFlowTest(unittest.TestCase):
    """Exercise both execution phases and restart reports (ADR 0026 points 2/5)."""

    def run_drill(self, change=None):
        import importlib.util
        import json
        import tempfile
        from types import SimpleNamespace
        from unittest.mock import patch
        spec = importlib.util.spec_from_file_location('drills_flow', HERE / 'ira-drills.py')
        drills = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(drills)
        instances, actions = [], []
        tag = 'targets-decided:' + drills.ACCOUNT + ':'

        class Venue:
            def __init__(self, folder, engine, config, asof, approval):
                self.role = ('first', 'restart', 'control')[len(instances)]
                instances.append(self)
                self.folder = folder
                folder.mkdir(exist_ok=True)
                self.inbox = folder / 'operator'
                self.inbox.mkdir(exist_ok=True)
                self.log = folder / (self.role + '.log')
                self.log.write_text('')
                self.decisions = []

            def order(self, kind, month, side, quantity=10):
                return {'type': kind, 'payload': {'tag': tag + month + 'T21:00:00Z',
                        'client_order_id': self.role + '-' + side, 'account_id': drills.ACCOUNT,
                        'instrument_id': 'SHY' if side == 'sell' else 'R00',
                        'side': side, 'quantity': quantity, 'limit_price': 50, 'send': True}}

            def feed(self, rows):
                feb = any(r['event_time'].startswith('2025-02-28') for r in rows)
                month = '2025-02-28' if feb else '2025-01-31'
                self.decisions.append({'type': 'strategy.targets.decided', 'payload': {
                    'outcome': 'decided' if feb or self.role == 'first' else 'withheld',
                    'withheld_reason': 'rebalance-in-progress', 'targets': [{'instrument_id': 'R00'}]}})
                if self.role == 'first' or feb:
                    for kind in ('strategy.order.planned', 'risk.order.released'):
                        self.decisions.append(self.order(kind, month, 'sell'))
                if feb and self.role == 'restart':
                    self.log.write_text(json.dumps({'record': {'envelope': {'type': 'operator.rebalance.cleared'}}}))

            def cancel_reports(self, orders, at, expected_refusal=None):
                actions.append(('old-cancels', [o['side'] for o in orders], expected_refusal))

            def cancel_all(self, at):
                actions.append(('cancel-all', self.role))
                for kind in ('strategy.order.planned', 'risk.order.released'):
                    row = self.order(kind, '2025-02-28', 'buy')
                    if self.role == 'restart' and change:
                        row = change(row)
                    if row:
                        self.decisions.append(row)

            def stop(self):
                pass

        def operate(binary, user, *args):
            if args[0] == 'rebalance-cleared':
                actions.append(('clearance',))
                return ''
            if '-log' in args and str(args[args.index('-log') + 1]).endswith('first.log'):
                return 'ends mid-rebalance sells-working\n'
            return 'no rebalance in progress\n'

        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            base = Path(temporary) / 'evidence'
            base.mkdir()
            patches = [patch.object(drills.os, 'geteuid', return_value=0),
                       patch.object(drills.os, 'chown'),
                       patch.object(drills.pwd, 'getpwnam', return_value=SimpleNamespace(pw_uid=0, pw_gid=0)),
                       patch.object(drills.tempfile, 'mkdtemp', return_value=str(base)),
                       patch.object(drills, 'FakeVenue', Venue), patch.object(drills, 'operate', operate)]
            from contextlib import ExitStack, redirect_stdout
            from io import StringIO
            with ExitStack() as stack, redirect_stdout(StringIO()):
                for p in patches:
                    stack.enter_context(p)
                drills.drill()
        return actions

    def test_reconnect_cancels_precede_clearance_and_both_phases_finish(self):
        actions = self.run_drill()
        self.assertEqual(actions, [
            ('old-cancels', ['sell'], 'which this run never released'),
            ('clearance',), ('cancel-all', 'restart'), ('cancel-all', 'control')])

    def test_changed_missing_or_refused_recovered_buy_cannot_pass(self):
        def change_quantity(row):
            row['payload']['quantity'] += 1
            return row

        def refuse(row):
            if row['type'] == 'risk.order.released':
                row['type'] = 'risk.order.refused'
                row['payload']['reason'] = 'insufficient-cash'
            return row

        for name, change in [('quantity', change_quantity), ('missing', lambda row: None), ('refused', refuse)]:
            with self.subTest(name=name), self.assertRaisesRegex(RuntimeError, 'orders differ|both.*phases|refused'):
                self.run_drill(change)


class ReconnectedReportsTest(unittest.TestCase):
    def test_old_cancel_is_sent_and_its_expected_refusal_advances_sequence(self):
        import importlib.util
        import json
        from io import StringIO
        from unittest.mock import Mock
        spec = importlib.util.spec_from_file_location('drills_reports', HERE / 'ira-drills.py')
        drills = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(drills)
        venue = drills.FakeVenue.__new__(drills.FakeVenue)
        venue.sequence = 17
        venue.header = {'configuration_hash': 'synthetic', 'strategy_version': 'synthetic'}
        venue.conn = Mock()
        venue.decisions = []
        order = {'account_id': 'DUSYNTHETIC', 'client_order_id': 'january-sell', 'instrument_id': 'SHY',
                 'side': 'sell', 'quantity': 10, 'limit_price': 80}
        error = {'error': {'code': 'decider_failed', 'causation_id': 'cancel:january-sell',
                          'message': 'names order january-sell, which this run never released'}}
        venue.replies = StringIO(json.dumps(error) + '\n')
        venue.cancel_reports([order], '2025-01-31T22:00:30Z', expected_refusal='which this run never released')
        sent = json.loads(venue.conn.sendall.call_args.args[0])
        self.assertEqual(sent['type'], 'execution.order.lifecycle')
        self.assertEqual(sent['payload']['status'], 'canceled')
        self.assertEqual(sent['payload']['client_order_id'], 'january-sell')
        self.assertEqual(venue.sequence, 18)
        self.assertEqual(venue.decisions, [])

        for changed in ({'code': 'invalid_envelope'}, {'message': 'unrelated failure'},
                        {'causation_id': 'some-other-input'}):
            with self.subTest(changed=changed):
                venue.replies = StringIO(json.dumps({'error': dict(error['error'], **changed)}) + '\n')
                with self.assertRaisesRegex(RuntimeError, 'expected.*refusal'):
                    venue.cancel_reports([order], '2025-01-31T22:00:30Z',
                                         expected_refusal='which this run never released')
