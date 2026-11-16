"""ECB recording tests; rates and dates are synthetic, with no network."""
import datetime as dt
import io
import lzma
import tempfile
import unittest
import zipfile
from pathlib import Path

from unittest import mock

from marketdata import fx, record, steps


def synthetic_history(text):
    """Fill a synthetic archive's earlier TARGET days with constant rates."""
    rates = fx.parse(text)
    columns = text.splitlines()[0].split(',')
    currencies = [c for c in columns[1:] if c]
    day = dt.date(2024, 1, 1)
    newest = max(rates)
    while day <= newest:
        if not fx.target_closed(day) and day not in rates:
            cells = [day.isoformat()] + [dict(USD='2', GBP='4').get(c, 'N/A') for c in currencies]
            if columns[-1] == '':
                cells.append('')
            text += ','.join(cells) + '\n'
        day += dt.timedelta(days=1)
    return text


CSV = synthetic_history('Date,USD,GBP,JPY,\n2026-03-04,2,4,N/A,\n2026-03-03,3,6,N/A,\n2026-03-02,3,6,12,\n')
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
                                      fetched_at='2026-03-04T23:00:00Z', url=fx.URL, body=rec['body'], covered_before=None))
            self.assertEqual(fx.parse(rec['body']), fx.parse(CSV))
            with self.assertRaisesRegex(fx.MarketDataError, 'system clock is behind the latest ECB recording'):
                steps.run_fx(DAY, d, get=get, now=NOW)
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
                with self.assertRaisesRegex(fx.MarketDataError, 'ECB') as caught:
                    steps.run_fx(DAY, d, get=lambda url: (body, url), now=NOW)
                self.assertNotIsInstance(caught.exception, steps.MissingData)
                self.assertEqual(list(Path(d).glob('*.json')), [])

    def test_currency_rates_and_five_day_window(self):
        from marketdata import fx
        rates = fx.parse(CSV)
        self.assertEqual(fx.usd(2, 'EUR', DAY, rates), 6)
        self.assertEqual(fx.usd(2, 'GBP', DAY, rates), 1)
        self.assertEqual(fx.usd(2, 'USD', DAY, None), 2)
        rates[dt.date(2026, 3, 9)] = {}
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
                    mock.patch.object(steps, '_stamp', return_value='2026-03-04T23:00:00Z'), \
                    mock.patch('sys.stdout', io.StringIO()), \
                    mock.patch('marketdata.massive.load_key', side_effect=AssertionError('ECB needs no key')):
                status = cli.main(['fx', '--out', d, '--asof', str(DAY)])
            self.assertEqual(status, 0)
            self.assertEqual(fx.load(d, DAY)[DAY]['USD'], 2)

    def test_na_uses_the_latest_usable_fixing_in_the_window(self):
        from marketdata import fx
        rates = fx.parse(CSV)
        self.assertEqual(fx.usd(2, 'JPY', DAY, rates), 0.5)
        rates[dt.date(2026, 3, 8)] = {}
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
            path = steps.run_fx(DAY, d, get=lambda url: (archive(later), url),
                                now=lambda: dt.datetime(2026, 3, 5, 23, tzinfo=dt.timezone.utc))
            self.assertEqual(Path(path).name, fx.KIND + '_2026-03-05_20260305T230000Z.json')
            self.assertEqual(fx.parse(record.read(path)['body']), {dt.date(2026, 3, 5): {'USD': 5, 'GBP': 10, 'JPY': 20}})
            self.assertEqual(Path(bootstrap).read_bytes(), original)
            self.assertEqual(len(list(Path(d).glob('*.json'))), 2)
            self.assertEqual(fx.load(d, DAY), fx.parse(CSV))
            self.assertEqual(fx.load(d, dt.date(2026, 3, 5)), fx.parse(later))

    def test_revised_fixing_refuses_before_any_new_recording(self):
        with tempfile.TemporaryDirectory() as d:
            path = steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            original = Path(path).read_bytes()
            revised = CSV.replace(',2,4,', ',3,4,') + '2026-03-05,5,10,20,\n'
            with self.assertRaisesRegex(steps.MissingData, 'ECB revised the 2026-03-04 fixing'):
                steps.run_fx(DAY, d, get=lambda url: (archive(revised), url),
                             now=lambda: dt.datetime(2026, 3, 5, 23, tzinfo=dt.timezone.utc))
            self.assertEqual(Path(path).read_bytes(), original)
            self.assertEqual(len(list(Path(d).glob('*.json'))), 1)

    def test_union_includes_history_recorded_after_stream_and_rejects_conflicts(self):
        with tempfile.TemporaryDirectory() as d:
            rec = dict(kind=fx.KIND, source=fx.SOURCE, session='2026-03-06',
                       fetched_at='2026-03-06T23:00:00Z', url=fx.URL, body=CSV + '2026-03-05,2,4,8,\n2026-03-06,2,4,8,\n', covered_before=None)
            record.write(d, rec)
            self.assertEqual(fx.load(d, DAY), fx.parse(CSV))
            rec['session'] = '2026-03-07'
            rec['fetched_at'] = '2026-03-07T23:00:00Z'
            rec['covered_before'] = '2026-03-06'
            record.write(d, rec)
            self.assertEqual(fx.load(d, DAY), fx.parse(CSV))
            rec['session'] = '2026-03-08'
            rec['fetched_at'] = '2026-03-08T23:00:00Z'
            rec['covered_before'] = '2026-03-06'
            rec['body'] = rec['body'].replace(',2,4,', ',3,4,')
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
                with self.assertRaisesRegex(fx.MarketDataError, 'ECB.*size limit'):
                    fx.csv_body(data)
            member = mock.MagicMock()
            member.file_size = 1
            contents = mock.MagicMock()
            contents.__enter__.return_value = contents
            contents.read.side_effect = [b'123', b'456', b'']
            with mock.patch.object(zipfile.ZipFile, 'getinfo', return_value=member), mock.patch.object(zipfile.ZipFile, 'open', return_value=contents):
                with self.assertRaisesRegex(fx.MarketDataError, 'ECB.*size limit'):
                    fx.csv_body(data)

    def test_unavailable_usd_is_preserved_and_noop_fetch_checks_provenance(self):
        with tempfile.TemporaryDirectory() as d:
            missing = synthetic_history('Date,USD,GBP\n2023-12-29,N/A,N/A\n2026-03-04,2,4\n')
            path = steps.run_fx(DAY, d, get=lambda url: (archive(missing), url), now=NOW)
            self.assertEqual(fx.parse(record.read(path)['body'])[dt.date(2023, 12, 29)], {})
            with self.assertRaisesRegex(fx.MarketDataError, 'ECB'):
                steps.run_fx(DAY, d, get=lambda url: (archive(missing), 'https://example.com'),
                             now=lambda: NOW() + dt.timedelta(seconds=1))


class PriorFixingTest(unittest.TestCase):
    def test_conversion_window_must_start_at_or_after_continuity_floor(self):
        rates = fx.parse('Date,USD,GBP\n2023-12-28,2,4\n2024-01-01,2,4\n2024-01-05,2,4\n')
        for day in (dt.date(2023, 12, 29), dt.date(2024, 1, 5)):
            with self.subTest(day=day), self.assertRaisesRegex(
                    steps.MissingData, 'dividend conversion before 2024-01-01 is not supported:'):
                fx.usd(2, 'GBP', day, rates)
        self.assertEqual(fx.usd(2, 'GBP', dt.date(2024, 1, 6), rates), 1)

    def test_prior_day_coverage_is_required_even_for_uncovered_currency(self):
        rates = {DAY - dt.timedelta(days=2): {'USD': 2, 'GBP': 4}}
        for currency in ('GBP', 'ZZZ'):
            with self.subTest(currency=currency), self.assertRaisesRegex(steps.MissingData, 'coverage'):
                fx.usd(2, currency, DAY, rates)

    def test_holiday_fallback_uses_only_days_two_through_five(self):
        for lag in range(2, 6):
            with self.subTest(lag=lag):
                rates = {DAY: {'USD': 99, 'GBP': 1},
                         DAY - dt.timedelta(days=1): {},
                         DAY - dt.timedelta(days=lag): {'USD': lag, 'GBP': 2}}
                self.assertEqual(fx.usd(2, 'GBP', DAY, rates), lag)
        rates = {DAY: {'USD': 99, 'GBP': 1}, DAY - dt.timedelta(days=1): {},
                 DAY - dt.timedelta(days=6): {'USD': 2, 'GBP': 4}}
        with self.assertRaises(fx.Unavailable):
            fx.usd(2, 'GBP', DAY, rates)

    def test_future_fixing_refused_on_write(self):
        future = CSV + '2026-03-05,5,10,20,\n'
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(fx.MarketDataError, 'fetch') as caught:
                steps.run_fx(DAY, d, get=lambda url: (archive(future), url), now=NOW)
            self.assertNotIsInstance(caught.exception, steps.MissingData)
            self.assertEqual(list(Path(d).glob('*.json')), [])

    def test_future_fixing_refused_on_read(self):
        future = CSV + '2026-03-05,5,10,20,\n'
        rec = dict(kind=fx.KIND, source=fx.SOURCE, session=str(DAY),
                   fetched_at='2026-03-04T23:00:00Z', url=fx.URL, body=future)
        with self.assertRaisesRegex(steps.MissingData, 'fetch'):
            fx.checked(rec)
        with tempfile.TemporaryDirectory() as d:
            record.write(d, rec)
            with self.assertRaisesRegex(steps.MissingData, 'fetch'):
                fx.load(d)

    def test_new_fixing_cannot_fill_a_completed_historical_window(self):
        with tempfile.TemporaryDirectory() as d:
            path = steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            before = Path(path).read_bytes()
            backfilled = CSV + '2026-03-01,9,10,20,\n'
            with self.assertRaisesRegex(steps.MissingData, 'treated as complete'):
                steps.run_fx(DAY, d, get=lambda url: (archive(backfilled), url),
                             now=lambda: NOW() + dt.timedelta(seconds=1))
            self.assertEqual(Path(path).read_bytes(), before)
            self.assertEqual(len(list(Path(d).glob('*.json'))), 1)


class CoverageAndMergeTest(unittest.TestCase):
    def test_holiday_coverage_uses_utc_fetch_date(self):
        for ex, fixing, fetched, complete in [
                ('2026-04-06', '2026-04-02', '2026-04-07', True),
                ('2026-04-06', '2026-04-02', '2026-04-06', True),
                ('2026-04-06', '2026-04-02', '2026-04-05', False),
                ('2025-12-26', '2025-12-24', '2025-12-29', True)]:
            with self.subTest(fetched=fetched), tempfile.TemporaryDirectory() as d:
                steps.run_fx(dt.date.fromisoformat(fixing), d,
                             get=lambda url: (archive(synthetic_history('Date,USD,GBP\n{},2,4\n'.format(fixing))), url),
                             now=lambda: dt.datetime.fromisoformat(fetched + 'T08:00:00+00:00'))
                rates = fx.load(d)
                if complete:
                    self.assertEqual(fx.usd(2, 'GBP', dt.date.fromisoformat(ex), rates), 1)
                else:
                    with self.assertRaisesRegex(steps.MissingData, 'coverage'):
                        fx.usd(2, 'GBP', dt.date.fromisoformat(ex), rates)

    def test_withdrawn_fixing_refuses_without_writing(self):
        with tempfile.TemporaryDirectory() as d:
            steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            before = {p.name: p.read_bytes() for p in Path(d).glob('*.json')}
            withdrawn = CSV.replace('2026-03-04,2,4,N/A,\n', '') + '2026-03-05,5,10,20,\n'
            with self.assertRaisesRegex(steps.MissingData, 'ECB history no longer contains the 2026-03-04 fixing'):
                steps.run_fx(DAY, d, get=lambda url: (archive(withdrawn), url),
                             now=lambda: dt.datetime(2026, 3, 6, 8, tzinfo=dt.timezone.utc))
            self.assertEqual({p.name: p.read_bytes() for p in Path(d).glob('*.json')}, before)

    def test_same_second_fetch_fails_without_changing_recordings(self):
        for mode in ('same', 'extension', 'revision', 'withdrawal', 'backdating'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as d:
                winner = CSV + '2026-03-05,5,10,20,\n'
                loser = winner
                if mode == 'extension':
                    loser += '2026-03-06,6,12,24,\n'
                if mode == 'revision':
                    loser = loser.replace(',2,4,', ',9,4,')
                if mode == 'withdrawal':
                    loser = loser.replace('2026-03-04,2,4,N/A,\n', '')
                if mode == 'backdating':
                    loser += '2026-03-01,9,18,36,\n'

                def run(text):
                    return steps.run_fx(DAY, d, get=lambda url: (archive(text), url),
                                        now=lambda: dt.datetime(2026, 3, 6, 8, tzinfo=dt.timezone.utc))

                run(winner)
                with self.assertRaisesRegex(fx.MarketDataError, 'system clock is behind the latest ECB recording'):
                    run(loser)
                self.assertTrue((Path(d) / (fx.KIND + '_2026-03-06_20260306T080000Z.json')).exists())
                expected = fx.parse(winner)
                self.assertEqual(fx.load(d), expected)
                self.assertEqual(len(list(Path(d).glob('*.json'))), 1)

    def test_post_write_union_rechecks_withdrawal(self):
        with tempfile.TemporaryDirectory() as d:
            steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            write = record.write
            def concurrent_write(directory, rec):
                path = write(directory, rec)
                extra = dict(rec, session='2026-03-01', body='Date,USD,GBP\n2026-03-01,9,18\n')
                write(directory, extra)
                return path
            incoming = CSV + '2026-03-05,5,10,20,\n'
            with mock.patch.object(record, 'write', side_effect=concurrent_write):
                with self.assertRaisesRegex(steps.MissingData, 'treated as complete'):
                    steps.run_fx(DAY, d, get=lambda url: (archive(incoming), url),
                                 now=lambda: dt.datetime(2026, 3, 6, 8, tzinfo=dt.timezone.utc))

    def test_post_write_union_rechecks_backdating(self):
        with tempfile.TemporaryDirectory() as d:
            steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            write = record.write
            incoming = CSV + '2026-03-05,5,10,20,\n2026-03-06,6,12,24,\n'
            def concurrent_write(directory, rec):
                write(directory, dict(rec, session='2026-03-06', fetched_at='2026-03-06T23:00:01Z',
                                      body='Date,USD,GBP,JPY\n2026-03-06,6,12,24\n'))
                return write(directory, rec)
            with mock.patch.object(record, 'write', side_effect=concurrent_write):
                with self.assertRaisesRegex(steps.MissingData, 'chain is broken'):
                    steps.run_fx(DAY, d, get=lambda url: (archive(incoming), url),
                                 now=lambda: dt.datetime(2026, 3, 6, 23, tzinfo=dt.timezone.utc))


class FetchRecordingTest(unittest.TestCase):
    def fetch(self, directory, text, stamp):
        return steps.run_fx(dt.date(2026, 4, 2), directory,
                            get=lambda url: (archive(text), url),
                            now=lambda: dt.datetime.fromisoformat(stamp).replace(tzinfo=dt.timezone.utc))

    def test_empty_fetch_persists_coverage_and_backdating_refuses(self):
        text = synthetic_history('Date,USD,GBP\n2026-04-02,2,4\n')
        with tempfile.TemporaryDirectory() as d:
            self.fetch(d, text, '2026-04-02T23:00:00')
            path = self.fetch(d, text, '2026-04-07T08:00:00')
            self.assertEqual(len(list(Path(d).glob('*.json'))), 2)
            self.assertEqual(record.read(path)['session'], '2026-04-07')
            self.assertEqual(fx.checked(record.read(path)), {})
            self.assertEqual(fx.load(d).covered_through, dt.date(2026, 4, 6))
            before = {p.name: p.read_bytes() for p in Path(d).glob('*.json')}
            with self.assertRaisesRegex(steps.MissingData,
                    'ECB published a 2026-04-03 fixing after 2026-04-06 was treated as complete'):
                self.fetch(d, text + '2026-04-03,9,4\n2026-04-07,2,4\n', '2026-04-08T08:00:00')
            self.assertEqual({p.name: p.read_bytes() for p in Path(d).glob('*.json')}, before)

    def test_crash_at_atomic_install_exposes_whole_fetch_or_none(self):
        initial = synthetic_history('Date,USD,GBP\n2026-04-02,2,4\n')
        full = initial + '2026-04-06,6,4\n2026-04-07,8,4\n'
        for installed in (False, True):
            with self.subTest(installed=installed), tempfile.TemporaryDirectory() as d:
                self.fetch(d, initial, '2026-04-02T23:00:00')
                link = record.os.link
                def crash(src, dst):
                    if installed:
                        link(src, dst)
                    raise OSError('synthetic crash')
                with mock.patch.object(record.os, 'link', side_effect=crash):
                    with self.assertRaisesRegex(OSError, 'synthetic crash'):
                        self.fetch(d, full, '2026-04-08T08:00:00')
                rates = fx.load(d)
                self.assertEqual(rates, fx.parse(full if installed else initial))
                self.assertEqual(rates.covered_through, dt.date(2026, 4, 7 if installed else 2))
                self.assertFalse(list(Path(d).glob('*.part')))
                self.assertEqual(len(list(Path(d).glob('*.json'))), 2 if installed else 1)

    def test_revision_and_withdrawal_give_operator_remedy(self):
        for text in (CSV.replace(',2,4,', ',9,4,'), 'Date,USD,GBP,JPY\n2026-03-04,2,4,N/A\n'):
            with self.subTest(text=text), tempfile.TemporaryDirectory() as d:
                steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
                with self.assertRaises(steps.MissingData) as caught:
                    steps.run_fx(DAY, d, get=lambda url: (archive(text), url),
                                 now=lambda: NOW() + dt.timedelta(seconds=1))
                self.assertEqual(len(list(Path(d).glob('*.json'))), 1)
                for phrase in ('ECB site', 'quarantine', 'bootstrap', 'fresh run'):
                    self.assertIn(phrase, str(caught.exception))


class FetchIntegrityTest(unittest.TestCase):
    def test_western_easter_and_target_closures(self):
        for year, easter in [(2025, '2025-04-20'), (2026, '2026-04-05'), (2027, '2027-03-28')]:
            day = dt.date.fromisoformat(easter)
            self.assertEqual(fx.easter(year), day)
            for closed in [day - dt.timedelta(days=2), day + dt.timedelta(days=1),
                           dt.date(year, 1, 1), dt.date(year, 5, 1),
                           dt.date(year, 12, 25), dt.date(year, 12, 26)]:
                self.assertTrue(fx.target_closed(closed))
            self.assertFalse(fx.target_closed(day + dt.timedelta(days=2)))

    def test_load_refuses_backdated_fixing_after_predecessor_coverage(self):
        initial = synthetic_history('Date,USD\n2026-04-02,2\n')
        with tempfile.TemporaryDirectory() as d:
            record.write(d, dict(kind=fx.KIND, source=fx.SOURCE, session='2026-04-06',
                                 fetched_at='2026-04-06T08:00:00Z', url=fx.URL,
                                 body=initial, covered_before=None))
            path = record.write(d, dict(kind=fx.KIND, source=fx.SOURCE, session='2026-04-07',
                                        fetched_at='2026-04-07T08:00:00Z', url=fx.URL,
                                        body='Date,USD\n2026-04-03,2\n', covered_before='2026-04-05'))
            with self.assertRaises(steps.MissingData) as caught:
                fx.load(d)
            self.assertIn(Path(path).name, str(caught.exception))
            self.assertIn('treated as complete', str(caught.exception))

    def test_load_refuses_sparse_or_stale_bootstrap(self):
        sparse = 'Date,USD\n2026-11-20,2\n'
        for text, reason in [(sparse, 'incomplete'), (synthetic_history(sparse), 'stale')]:
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as d:
                path = record.write(d, dict(kind=fx.KIND, source=fx.SOURCE, session='bootstrap',
                                            fetched_at='2026-11-24T08:00:00Z', url=fx.URL,
                                            body=text, covered_before=None))
                with self.assertRaises(steps.MissingData) as caught:
                    fx.load(d)
                self.assertIn(Path(path).name, str(caught.exception))
                self.assertIn(reason, str(caught.exception))

    def test_stale_archive_writes_nothing_and_cannot_extend_coverage(self):
        with tempfile.TemporaryDirectory() as d:
            steps.run_fx(DAY, d, get=lambda url: (archive(), url), now=NOW)
            before = {p.name: p.read_bytes() for p in Path(d).glob('*.json')}
            with self.assertRaisesRegex(fx.MarketDataError, 'stale'):
                steps.run_fx(DAY, d, get=lambda url: (archive(), url),
                             now=lambda: dt.datetime(2026, 3, 6, 8, tzinfo=dt.timezone.utc))
            self.assertEqual({p.name: p.read_bytes() for p in Path(d).glob('*.json')}, before)
            self.assertEqual(fx.load(d).covered_through, DAY)

    def test_target_holiday_fetch_is_complete(self):
        for fetched, fixing in [('2026-04-06', '2026-04-02'), ('2026-12-26', '2026-12-24')]:
            with self.subTest(fetched=fetched), tempfile.TemporaryDirectory() as d:
                steps.run_fx(dt.date.fromisoformat(fixing), d,
                             get=lambda url: (archive(synthetic_history('Date,USD\n{},2\n'.format(fixing))), url),
                             now=lambda: dt.datetime.fromisoformat(fetched + 'T08:00:00+00:00'))
                self.assertEqual(fx.load(d).covered_through, dt.date.fromisoformat(fetched) - dt.timedelta(days=1))

    def test_deleted_bootstrap_or_middle_recording_breaks_chain(self):
        for removed in (0, 1):
            with self.subTest(removed=removed), tempfile.TemporaryDirectory() as d:
                paths = []
                text = CSV
                for day in (4, 5, 6):
                    if day > 4:
                        text += '2026-03-{:02d},2,4,8\n'.format(day)
                    paths.append(steps.run_fx(DAY, d, get=lambda url: (archive(text), url),
                                             now=lambda: dt.datetime(2026, 3, day, 23, tzinfo=dt.timezone.utc)))
                self.assertIsNone(record.read(paths[0])['covered_before'])
                self.assertEqual(record.read(paths[1])['covered_before'], str(DAY))
                Path(paths[removed]).unlink()
                with self.assertRaisesRegex(steps.MissingData, 'ECB recording chain is broken at ' + Path(paths[removed + 1]).name):
                    fx.load(d)

    def test_corrupt_deflate_is_a_download_error(self):
        out = io.BytesIO()
        with zipfile.ZipFile(out, 'w', compression=zipfile.ZIP_DEFLATED) as zipped:
            zipped.writestr('eurofxref-hist.csv', CSV)
        data = bytearray(out.getvalue())
        offset = 30 + len('eurofxref-hist.csv')
        data[offset] = 0xff
        with self.assertRaisesRegex(fx.MarketDataError, 'malformed ECB archive'):
            fx.csv_body(data)

    def test_other_decompression_errors_are_download_errors(self):
        for error in (lzma.LZMAError, EOFError, OSError):
            with self.subTest(error=error):
                with mock.patch.object(zipfile.ZipExtFile, 'read', side_effect=error('corrupt compressed stream')):
                    with self.assertRaisesRegex(fx.MarketDataError, 'malformed ECB archive'):
                        fx.csv_body(archive())

    def test_backdating_includes_remedy(self):
        recorded = fx.Rates(fx.parse(CSV), DAY)
        with self.assertRaises(steps.MissingData) as caught:
            fx.check_history(recorded, fx.parse(CSV + '2026-03-01,2,4,8\n'))
        self.assertIn(fx.REMEDY, str(caught.exception))


class FetchSafetyTest(unittest.TestCase):
    def fetch(self, directory, text, day=5):
        return steps.run_fx(dt.date(2024, 1, 2), directory,
                            get=lambda url: (archive(text), url),
                            now=lambda: dt.datetime(2024, 1, day, 23, tzinfo=dt.timezone.utc))

    def test_bootstrap_gap_or_missing_usd_writes_nothing(self):
        for row in ('', '2024-01-03,N/A,4\n'):
            with self.subTest(row=row), tempfile.TemporaryDirectory() as d:
                text = 'Date,USD,GBP\n2024-01-02,2,4\n' + row + '2024-01-04,2,4\n2024-01-05,2,4\n'
                with self.assertRaisesRegex(fx.MarketDataError, 'incomplete ECB archive') as caught:
                    self.fetch(d, text)
                self.assertNotIsInstance(caught.exception, steps.MissingData)
                self.assertEqual(list(Path(d).glob('*.json')), [])

    def test_only_pre_2024_gaps_are_accepted(self):
        text = 'Date,USD,GBP\n2023-12-27,2,4\n2024-01-02,2,4\n'
        with tempfile.TemporaryDirectory() as d:
            self.fetch(d, text, day=2)
            self.assertEqual(fx.load(d), fx.parse(text))

    def test_later_gap_or_missing_usd_preserves_recordings(self):
        initial = 'Date,USD,GBP\n2024-01-02,2,4\n'
        for row in ('', '2024-01-03,N/A,4\n'):
            with self.subTest(row=row), tempfile.TemporaryDirectory() as d:
                self.fetch(d, initial, day=2)
                before = {p.name: p.read_bytes() for p in Path(d).glob('*.json')}
                with self.assertRaisesRegex(fx.MarketDataError, 'incomplete ECB archive'):
                    self.fetch(d, initial + row + '2024-01-04,2,4\n2024-01-05,2,4\n')
                self.assertEqual({p.name: p.read_bytes() for p in Path(d).glob('*.json')}, before)
                self.assertEqual(fx.load(d).covered_through, dt.date(2024, 1, 2))

    def test_shared_lock_waits_and_reloads_predecessor(self):
        from concurrent.futures import ThreadPoolExecutor
        import threading
        entered, release, waiting = threading.Event(), threading.Event(), threading.Event()
        import fcntl
        flock = fcntl.flock
        def observe_wait(file, flags):
            try:
                return flock(file, flags)
            except BlockingIOError:
                waiting.set()
                raise
        initial = 'Date,USD,GBP\n2024-01-02,2,4\n'
        def first_get(url):
            entered.set()
            if not release.wait(timeout=5):
                raise AssertionError('first fetch was never released')
            return archive(initial), url
        with tempfile.TemporaryDirectory() as d, mock.patch.object(fcntl, 'flock', side_effect=observe_wait), ThreadPoolExecutor(2) as pool:
            first = pool.submit(steps.run_fx, dt.date(2024, 1, 2), d, get=first_get,
                                now=lambda: dt.datetime(2024, 1, 2, 23, tzinfo=dt.timezone.utc))
            try:
                self.assertTrue(entered.wait(timeout=5))
                second = pool.submit(self.fetch, d, initial + '2024-01-03,2,4\n', 3)
                self.assertTrue(waiting.wait(timeout=1), 'second fetch did not wait for the shared lock')
                self.assertFalse(second.done())
            finally:
                release.set()
            first_path = first.result(timeout=5)
            second_path = second.result(timeout=5)
            self.assertIsNone(record.read(first_path)['covered_before'])
            self.assertEqual(record.read(second_path)['covered_before'], '2024-01-02')
            self.assertEqual(fx.load(d).covered_through, dt.date(2024, 1, 3))
            self.assertEqual(len(list(Path(d).glob('*.json'))), 2)

    def test_shared_lock_requires_existing_data_directory(self):
        with tempfile.TemporaryDirectory() as d:
            missing = Path(d) / 'missing'
            with self.assertRaisesRegex(steps.MissingData, 'no data directory'):
                with steps._fx_lock(missing):
                    pass
            self.assertFalse(missing.exists())

    def test_shared_lock_wait_is_bounded(self):
        import fcntl
        with tempfile.TemporaryDirectory() as d:
            with open(Path(d) / '.fx.lock', 'a') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
                with mock.patch('time.monotonic', side_effect=[0, 301]):
                    with self.assertRaisesRegex(steps.MissingData, 'another job is recording ECB rates'):
                        self.fetch(d, 'Date,USD\n2024-01-02,2\n', day=2)
            self.fetch(d, 'Date,USD\n2024-01-02,2\n', day=2)

    def test_truncated_chunked_response_is_download_error(self):
        import http.client
        class Socket:
            def makefile(self, *args, **kwargs):
                return io.BytesIO(b'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nabc')
        response = http.client.HTTPResponse(Socket())
        response.begin()
        response.geturl = lambda: fx.URL
        with self.assertRaisesRegex(fx.MarketDataError, 'ECB request failed: IncompleteRead'):
            fx.http_get(fx.URL, opener=lambda *args, **kwargs: response)
