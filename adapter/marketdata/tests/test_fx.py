"""ECB recording tests; rates and dates are synthetic, with no network."""
import datetime as dt
import io
import tempfile
import unittest
import zipfile
from pathlib import Path

from marketdata import record, steps


CSV = 'Date,USD,GBP,JPY,\n2026-03-04,2,4,N/A,\n2026-03-02,3,6,12,\n'
DAY = dt.date(2026, 3, 4)
NOW = lambda: dt.datetime(2026, 3, 4, 23, tzinfo=dt.timezone.utc)


def archive(text=CSV, name='eurofxref-hist.csv'):
    out = io.BytesIO()
    with zipfile.ZipFile(out, 'w') as z:
        z.writestr(name, text)
    return out.getvalue()


class FXRecordingTest(unittest.TestCase):
    def test_records_csv_and_provenance_once_and_reads_recording(self):
        from marketdata import fx
        calls = []
        def get(url):
            calls.append(url)
            return archive(), url
        with tempfile.TemporaryDirectory() as d:
            path = steps.run_fx(DAY, d, get=get, now=NOW)
            rec = record.read(path)
            self.assertEqual(rec, dict(kind=fx.KIND, source=fx.SOURCE, session=str(DAY),
                                      fetched_at='2026-03-04T23:00:00Z', url=fx.URL, body=CSV))
            self.assertEqual(steps.run_fx(DAY, d, get=get, now=NOW), path)
            self.assertEqual(calls, [fx.URL])
            self.assertEqual(fx.load(d, DAY)[DAY], {'USD': 2.0, 'GBP': 4.0})

    def test_missing_and_malformed_recordings_refuse_cleanly(self):
        from marketdata import fx
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                fx.load(d, DAY)
            record.write(d, dict(kind=fx.KIND, source=fx.SOURCE, session=str(DAY),
                                 fetched_at='2026-03-04T23:00:00Z', url=fx.URL, body='not a rates file'))
            with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                fx.load(d, DAY)
            with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                steps.run_fx(DAY, d, get=lambda url: self.fail('must read existing'), now=NOW)

    def test_malformed_download_writes_nothing(self):
        from marketdata import fx
        for body in (b'not zip', archive('bad'), archive(name='wrong.csv'),
                     archive(CSV.replace(',2,', ',0,')), archive(CSV.replace(',2,', ',nan,')),
                     archive(CSV.replace('2026-03-04', 'not-date')), archive(CSV + CSV.splitlines()[1] + '\n')):
            with self.subTest(body=body[:10]), tempfile.TemporaryDirectory() as d:
                with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                    steps.run_fx(DAY, d, get=lambda url: (body, url), now=NOW)
                self.assertEqual(list(Path(d).iterdir()), [])

    def test_currency_rates_and_five_day_window(self):
        from marketdata import fx
        rates = fx.parse(CSV)
        self.assertEqual(fx.usd(2, 'EUR', DAY, rates), 4)
        self.assertEqual(fx.usd(2, 'GBP', DAY, rates), 1)
        self.assertEqual(fx.usd(2, 'USD', DAY, None), 2)
        self.assertEqual(fx.usd(2, 'GBP', dt.date(2026, 3, 9), rates), 1)
        for currency, date, reason in [('GBP', dt.date(2026, 3, 10), '5'),
                                       ('ZZZ', DAY, 'not covered'),
                                       (None, DAY, 'currency')]:
            with self.subTest(currency=currency), self.assertRaisesRegex(fx.Unavailable, reason):
                fx.usd(2, currency, date, rates)

    def test_recording_session_must_match_its_filename(self):
        from marketdata import fx
        with tempfile.TemporaryDirectory() as d:
            path = steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            rec = record.read(path)
            Path(path).unlink()
            rec['session'] = '2026-03-05'
            other = record.write(d, rec)
            Path(other).rename(path)
            with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                fx.load(d, DAY)
            with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                steps.run_fx(DAY, d, now=NOW)

    def test_fx_cli_needs_no_massive_key_and_records_the_history(self):
        from unittest import mock
        from marketdata import __main__ as cli, fx
        with tempfile.TemporaryDirectory() as d:
            with mock.patch.object(fx, 'http_get', return_value=(archive(), fx.URL)), \
                    mock.patch('sys.stdout', io.StringIO()), \
                    mock.patch('marketdata.massive.load_key', side_effect=AssertionError('ECB needs no key')):
                status = cli.main(['fx', '--out', d, '--asof', str(DAY)])
            self.assertEqual(status, 0)
            self.assertEqual(fx.load(d, DAY)[DAY]['USD'], 2)

    def test_na_uses_the_latest_usable_fixing_in_the_window(self):
        from marketdata import fx
        rates = fx.parse(CSV)
        self.assertEqual(fx.usd(2, 'JPY', DAY, rates), 0.5)
        with self.assertRaisesRegex(fx.Unavailable, '5'):
            fx.usd(2, 'JPY', dt.date(2026, 3, 9), rates)

    def test_malformed_currency_is_unavailable(self):
        from marketdata import fx
        for currency in [[], ['GBP'], {}, {'GBP': 1}, 1, '', 'usd']:
            with self.subTest(currency=currency), self.assertRaises(fx.Unavailable):
                fx.usd(1, currency, DAY, fx.parse(CSV))
