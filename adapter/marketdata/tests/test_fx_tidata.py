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
            path = job.data / (fx.KIND + '_bootstrap.json')
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
            job = MomentumJob(d, ranker=ranker)
            build(job.data)
            events = []
            def fail(url):
                events.append('fx')
                raise fx.MarketDataError('ECB outage')
            with mock.patch.object(tidata, '_record_sectors', side_effect=lambda *a: events.append('sectors')):
                code, err = self.invoke(job, fail)
            self.assertEqual(code, 0, err)
            self.assertEqual(events, ['sectors', 'fx'])
            self.assertIn('ECB outage', err)
            self.assertTrue(job.path(LAST).exists())

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


    def test_fetch_outage_uses_existing_coverage_and_preserves_bytes(self):
        from marketdata.tests.test_fx_encode import rates_record
        for currency, covered in [('USD', False), ('GBP', True), ('GBP', False)]:
            with self.subTest(currency=currency, covered=covered), tempfile.TemporaryDirectory() as d:
                job = MomentumJob(d, ranker=ranker)
                build(job.data)
                set_currency(job.data, currency)
                if covered:
                    rates_record(job.data)
                elif currency == 'GBP':
                    record.write(job.data, dict(kind=fx.KIND, source=fx.SOURCE, session='2026-11-25',
                                               fetched_at='2026-11-25T23:00:00Z', url=fx.URL,
                                               body='Date,USD,GBP\n2026-11-25,2,4\n'))
                def fail(url):
                    raise fx.MarketDataError('ECB network outage')
                code, err = self.invoke(job, fail)
                self.assertIn('ECB network outage', err)
                if currency == 'GBP' and not covered:
                    self.assertEqual(code, 1, err)
                    self.assertFalse(job.path(LAST).exists())
                else:
                    self.assertEqual(code, 0, err)
                    published = job.path(LAST).read_bytes()
                    code, err = self.invoke(job, fail)
                    self.assertEqual(code, 0, err)
                    self.assertEqual(job.path(LAST).read_bytes(), published)
                    if covered:
                        code, err = self.invoke(job, lambda url: (archive('Date,USD,GBP\n2026-11-26,2,4\n'), url))
                        self.assertEqual(code, 0, err)
                        self.assertEqual(job.path(LAST).read_bytes(), published)

    def test_download_size_limit_warns_and_usd_job_publishes(self):
        for target, limit in [('MAX_DOWNLOAD_BYTES', 1), ('MAX_CSV_BYTES', 1)]:
            with self.subTest(target=target), tempfile.TemporaryDirectory() as d:
                job = MomentumJob(d, ranker=ranker)
                build(job.data)
                if target == 'MAX_DOWNLOAD_BYTES':
                    def get(url):
                        raise fx.MarketDataError('ECB download exceeded byte limit')
                else:
                    def get(url):
                        return archive(), url
                with mock.patch.object(fx, target, limit):
                    code, err = self.invoke(job, get)
                self.assertEqual(code, 0, err)
                self.assertIn('ECB', err)
                self.assertTrue(job.path(LAST).exists())


    def test_malformed_revised_and_withdrawn_histories_still_refuse(self):
        from marketdata.tests.test_fx_encode import rates_record
        for text in ('bad', 'Date,USD,GBP\n2026-11-26,9,4\n',
                     'Date,USD,GBP\n2026-11-27,2,4\n'):
            with self.subTest(text=text), tempfile.TemporaryDirectory() as d:
                job = MomentumJob(d, ranker=ranker)
                build(job.data)
                rates_record(job.data)
                before = {p.name: p.read_bytes() for p in job.data.glob(fx.KIND + '_*.json')}
                code, err = self.invoke(job, lambda url: (archive(text), url))
                self.assertEqual(code, 1, err)
                self.assertIn('ECB', err)
                self.assertFalse(job.path(LAST).exists())
                self.assertEqual({p.name: p.read_bytes() for p in job.data.glob(fx.KIND + '_*.json')}, before)
