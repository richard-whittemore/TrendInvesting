"""The momentum job records immutable ECB fixings; synthetic fixtures only."""
import datetime as dt
import io
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from marketdata import fx, record, tidata
from marketdata.tests.test_encode import build, LAST, IDENTITY
from marketdata.tests.test_fx import archive
from marketdata.tests.test_fx_encode import set_currency, ranker
from marketdata.tests.test_tidata import after_settle
from marketdata.tests.test_tidata_momentum import MomentumJob, sector_pages


class FXJobTest(unittest.TestCase):
    def invoke(self, job, get):
        out, err = io.StringIO(), io.StringIO()
        with mock.patch('sys.stdout', out), mock.patch('sys.stderr', err):
            code = tidata.main(job.argv('--backfill-from', '2026-11-23'), now=lambda: after_settle(LAST),
                               client_factory=lambda key: job.client, ranker=job.ranker,
                               sectors_get=sector_pages, fx_get=get)
        return code, err.getvalue()

    def test_fetches_history_records_and_outputs_skip_diagnostics(self):
        with tempfile.TemporaryDirectory() as d:
            job = MomentumJob(d, ranker=lambda reqs: [dict(session=r['universe']['session'], decision_session=False,
                                                           window=5, members=[]) for r in reqs])
            build(job.data)
            set_currency(job.data, 'ZZZ')
            calls = []
            def get(url):
                calls.append(url)
                return archive('Date,USD,GBP\n2026-11-27,2,4\n'), url
            code, err = self.invoke(job, get)
            self.assertEqual(code, 0, err)
            for detail in ['excluded CCC', '2026-11-27', 'ZZZ', 'not covered', 'skipped']:
                self.assertIn(detail, err)
            self.assertEqual(calls, [fx.URL])
            path = job.data / (fx.KIND + '_2026-11-27.json')
            self.assertEqual(record.read(path)['source'], fx.SOURCE)
            code, err = self.invoke(job, get)
            self.assertEqual(code, 0, err)

    def test_bad_ecb_download_refuses_without_publishing(self):
        with tempfile.TemporaryDirectory() as d:
            job = MomentumJob(d)
            build(job.data)
            code, err = self.invoke(job, lambda url: (b'bad zip', url))
            self.assertEqual(code, 1)
            self.assertIn('ECB', err)
            self.assertFalse(job.path(LAST).exists())

    def test_fx_does_not_trigger_reencoding_even_in_legacy_manifest(self):
        from marketdata.tests.test_fx_encode import rates_record
        with tempfile.TemporaryDirectory() as d:
            path = rates_record(d)
            self.assertEqual(tidata._action_set(d), {})
            manifest = {'actions': {Path(path).name: 'old-digest'}}
            spans = [(LAST, LAST, 'published.jsonl')] * 20
            with mock.patch.object(tidata, '_verify_momentum') as verify:
                self.assertEqual(tidata._reencode_changed(manifest, spans, d, None, LAST, None, None, None, False), {})
            verify.assert_not_called()

    def test_sector_capture_precedes_ecb_failure(self):
        with tempfile.TemporaryDirectory() as d:
            job = MomentumJob(d)
            build(job.data)
            events = []
            def fail(url):
                events.append('fx')
                raise fx.MarketDataError('ECB outage')
            with mock.patch.object(tidata, '_record_sectors', side_effect=lambda *a: events.append('sectors')):
                code, err = self.invoke(job, fail)
            self.assertEqual(code, 1)
            self.assertEqual(events, ['sectors', 'fx'])
            self.assertFalse(job.path(LAST).exists())

    def test_preupgrade_file_missing_fx_names_fresh_run_remedy(self):
        with tempfile.TemporaryDirectory() as d:
            job = MomentumJob(d, ranker=ranker)
            build(job.data)
            set_currency(job.data, 'EUR')
            with mock.patch.object(fx, 'usd', side_effect=lambda cash, currency, day, rates: cash):
                code, err = self.invoke(job, lambda url: (archive(), url))
            self.assertEqual(code, 0, err)
            for path in job.data.glob(fx.KIND + '_*.json'):
                path.unlink()
            with self.assertRaisesRegex(tidata.Refused, 'fresh run.*Upgrading to dividend currency conversion'):
                tidata._verify_momentum([(LAST, LAST, str(job.path(LAST)))], str(job.data),
                                        mock.Mock(required=['SPY', 'IEF']),
                                        dt.date(2026, 11, 23), IDENTITY, 'engine.log', job.ranker)
