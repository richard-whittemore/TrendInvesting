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
