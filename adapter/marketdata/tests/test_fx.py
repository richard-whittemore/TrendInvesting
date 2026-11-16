"""ECB recording tests; rates and dates are synthetic, with no network."""
import datetime as dt
import io
import tempfile
import unittest
import zipfile
from pathlib import Path

from unittest import mock

from marketdata import fx, record, steps


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
                                      fetched_at='2026-03-04T23:00:00Z', url=fx.URL, body=rec['body']))
            self.assertEqual(fx.parse(rec['body']), fx.parse(CSV))
            self.assertEqual(steps.run_fx(DAY, d, get=get, now=NOW), path)
            self.assertEqual(calls, [fx.URL, fx.URL])
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


class ImmutableFixingTest(unittest.TestCase):
    def test_bootstrap_then_only_new_fixings_and_old_session_is_harmless(self):
        with tempfile.TemporaryDirectory() as d:
            bootstrap = steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            original = Path(bootstrap).read_bytes()
            later = CSV + '2026-03-05,5,10,20,\n'
            path = steps.run_fx(DAY, d, get=lambda url: (archive(later), url), now=NOW)
            self.assertEqual(Path(path).name, fx.KIND + '_2026-03-05.json')
            self.assertEqual(fx.parse(record.read(path)['body']), {dt.date(2026, 3, 5): {'USD': 5, 'GBP': 10, 'JPY': 20}})
            self.assertEqual(Path(bootstrap).read_bytes(), original)
            self.assertEqual(len(list(Path(d).iterdir())), 2)
            self.assertEqual(fx.load(d, DAY), fx.parse(CSV))
            self.assertEqual(fx.load(d, dt.date(2026, 3, 5)), fx.parse(later))

    def test_revised_fixing_refuses_before_any_new_recording(self):
        with tempfile.TemporaryDirectory() as d:
            path = steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            original = Path(path).read_bytes()
            revised = CSV.replace(',2,4,', ',3,4,') + '2026-03-05,5,10,20,\n'
            with self.assertRaisesRegex(steps.MissingData, 'ECB revised the 2026-03-04 fixing'):
                steps.run_fx(DAY, d, get=lambda url: (archive(revised), url), now=NOW)
            self.assertEqual(Path(path).read_bytes(), original)
            self.assertEqual(len(list(Path(d).iterdir())), 1)

    def test_union_includes_history_recorded_after_stream_and_rejects_conflicts(self):
        with tempfile.TemporaryDirectory() as d:
            rec = dict(kind=fx.KIND, source=fx.SOURCE, session='2026-03-06',
                       fetched_at='2026-03-06T23:00:00Z', url=fx.URL, body=CSV)
            record.write(d, rec)
            self.assertEqual(fx.load(d, DAY), fx.parse(CSV))
            rec['session'] = '2026-03-07'
            record.write(d, rec)
            self.assertEqual(fx.load(d, DAY), fx.parse(CSV))
            rec['session'] = '2026-03-08'
            rec['body'] = CSV.replace(',2,4,', ',3,4,')
            record.write(d, rec)
            with self.assertRaisesRegex(steps.MissingData, 'ECB revised the 2026-03-04 fixing'):
                fx.load(d, DAY)

    def test_concurrent_winner_is_compared_and_returned(self):
        for conflict in (False, True):
            with self.subTest(conflict=conflict), tempfile.TemporaryDirectory() as d:
                write = record.write
                def winner(directory, rec):
                    rec = dict(rec)
                    if conflict:
                        rec['body'] = rec['body'].replace('2.0', '9.0')
                    write(directory, rec)
                    raise record.AlreadyRecorded('winner')
                with mock.patch.object(record, 'write', side_effect=winner):
                    if conflict:
                        with self.assertRaisesRegex(steps.MissingData, 'ECB revised'):
                            steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
                    else:
                        path = steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
                        self.assertTrue(Path(path).exists())

    def test_download_is_chunked_and_bounded_by_time_and_bytes(self):
        for mode in ('success', 'deadline', 'bytes'):
            with self.subTest(mode=mode):
                response = mock.MagicMock()
                response.__enter__.return_value = response
                response.geturl.return_value = fx.URL
                response.read1.side_effect = [b'123', b'456', b'']
                response.read.side_effect = AssertionError('use a single bounded network read')
                ticks = [0, 0, 121] if mode == 'deadline' else [0] * 20
                with mock.patch.object(fx.time, 'monotonic', side_effect=ticks), mock.patch.object(fx, 'MAX_DOWNLOAD_BYTES', 5 if mode == 'bytes' else 20):
                    if mode == 'success':
                        self.assertEqual(fx.http_get(fx.URL, opener=lambda *a, **k: response), (b'123456', fx.URL))
                    else:
                        with self.assertRaisesRegex(fx.MarketDataError, 'deadline|byte limit'):
                            fx.http_get(fx.URL, opener=lambda *a, **k: response)
                self.assertTrue(all(c.args[0] <= 65536 for c in response.read1.call_args_list))

    def test_archive_declared_and_actual_size_are_bounded(self):
        data = archive()
        with mock.patch.object(fx, 'MAX_CSV_BYTES', 5):
            with mock.patch.object(zipfile.ZipFile, 'open', side_effect=AssertionError('must reject before extraction')):
                with self.assertRaisesRegex(steps.MissingData, 'ECB.*size limit'):
                    fx.csv_body(data)
            member = mock.MagicMock()
            member.file_size = 1
            contents = mock.MagicMock()
            contents.__enter__.return_value = contents
            contents.read.side_effect = [b'123', b'456', b'']
            with mock.patch.object(zipfile.ZipFile, 'getinfo', return_value=member), mock.patch.object(zipfile.ZipFile, 'open', return_value=contents):
                with self.assertRaisesRegex(steps.MissingData, 'ECB.*size limit'):
                    fx.csv_body(data)

    def test_unavailable_usd_is_preserved_and_noop_fetch_checks_provenance(self):
        with tempfile.TemporaryDirectory() as d:
            missing = 'Date,USD,GBP\n2026-03-04,N/A,N/A\n'
            path = steps.run_fx(DAY, d, get=lambda url: (archive(missing), url), now=NOW)
            self.assertEqual(fx.parse(record.read(path)['body']), {DAY: {}})
            with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                steps.run_fx(DAY, d, get=lambda url: (archive(missing), 'https://example.com'), now=NOW)
