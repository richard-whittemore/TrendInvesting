"""The momentum job records its ECB input once; synthetic fixtures only."""
import io
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from marketdata import fx, record, tidata
from marketdata.tests.test_encode import build, LAST
from marketdata.tests.test_fx import archive
from marketdata.tests.test_fx_encode import set_currency
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

    def test_fetches_once_records_and_outputs_skip_diagnostics(self):
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
            path = job.data / (fx.KIND + '_' + str(LAST) + '.json')
            self.assertEqual(record.read(path)['source'], fx.SOURCE)
            code, err = self.invoke(job, lambda url: self.fail('second firing fetched ECB'))
            self.assertEqual(code, 0, err)

    def test_bad_ecb_download_refuses_without_publishing(self):
        with tempfile.TemporaryDirectory() as d:
            job = MomentumJob(d)
            build(job.data)
            code, err = self.invoke(job, lambda url: (b'bad zip', url))
            self.assertEqual(code, 1)
            self.assertIn('ECB', err)
            self.assertFalse(job.path(LAST).exists())

    def test_revised_recorded_rates_trigger_published_stream_verification(self):
        with tempfile.TemporaryDirectory() as d:
            from marketdata.tests.test_fx_encode import rates_record
            path = rates_record(d)
            name = Path(path).name
            manifest = {'actions': tidata._action_set(d)}
            rec = record.read(path)
            Path(path).unlink()
            rec['body'] = rec['body'].replace(',2,4', ',3,4')
            record.write(d, rec)
            span = (LAST, LAST, 'published.jsonl')
            with mock.patch.object(tidata, '_verify_momentum') as verify:
                now = tidata._reencode_changed(manifest, [span], d, None, LAST, None, None, None, False)
            self.assertEqual(verify.call_count, 1)
            self.assertEqual(verify.call_args.args[0], [span])
            self.assertNotEqual(manifest['actions'][name], now[name])
