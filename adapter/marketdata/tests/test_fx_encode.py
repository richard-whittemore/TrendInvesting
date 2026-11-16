"""Currency adjustment through both encoders, using synthetic recordings."""
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from marketdata import encode, fx, record, steps
from marketdata.tests.test_encode import build, FIRST, LAST, IDENTITY, bar_of


def set_currency(directory, currency):
    path = Path(directory) / 'dividends_2026-11-01_2026-11-30.json'
    rec = record.read(path)
    path.unlink()
    rec['rows'][0]['currency'] = currency
    record.write(directory, rec)


def rates_record(directory, text='Date,USD,GBP\n2026-11-26,2,4\n'):
    return record.write(directory, dict(kind=fx.KIND, source=fx.SOURCE, session=str(LAST),
                                        fetched_at='2026-11-30T23:30:00Z', url=fx.URL, body=text))


def ranker(requests):
    return [dict(session=r['universe']['session'], decision_session=True, window=5,
                 members=[row['instrument_id'] for row in r['universe']['rows']]) for r in requests]


def stream(directory, compact, excluded=None, optional=()):
    return list(encode.iter_encode(directory, FIRST, LAST, IDENTITY, required=['SPY', 'IEF'],
                                   optional=optional, universe_members=not compact, excluded=excluded,
                                   ranker=ranker if compact else None))


class FXEncodingTest(unittest.TestCase):
    def test_converts_bars_and_compact_histories_from_recording(self):
        for compact in (False, True):
            with self.subTest(compact=compact), tempfile.TemporaryDirectory() as d:
                build(d)
                set_currency(d, 'GBP')
                rates_record(d)
                with mock.patch.object(fx, 'http_get', side_effect=AssertionError('live lookup')):
                    got = stream(d, compact)
                if compact:
                    history = next(e['payload'] for e in got if e['type'] == 'market.close.history'
                                   and e['payload']['instrument_id'] == 'CCC')
                    closes = {r['session']: r['close'] for r in history['closes']}
                    self.assertAlmostEqual(closes['2026-11-27'], 49 * 50 / 49.5)
                else:
                    self.assertAlmostEqual(bar_of(got, 'CCC', '2026-11-27')['adjusted']['close'], 49 * 50 / 49.5)

    def test_uncovered_and_stale_rates_skip_and_record_via_existing_exclusions(self):
        for compact in (False, True):
            for currency, text, reason in [('ZZZ', 'Date,USD,GBP\n2026-11-27,2,4\n', 'not covered'),
                                            ('GBP', 'Date,USD,GBP\n2026-11-27,N/A,N/A\n2026-11-21,2,4\n', '5 calendar days'),
                                            (None, 'Date,USD,GBP\n2026-11-27,2,4\n', 'currency')]:
                with self.subTest(compact=compact, currency=currency), tempfile.TemporaryDirectory() as d:
                    build(d)
                    set_currency(d, currency)
                    rates_record(d, text)
                    excluded = {}
                    got = stream(d, compact, excluded)
                    self.assertIn('CCC', excluded)
                    detail = excluded['CCC'][0].reason
                    for expected in ['CCC', '2026-11-27', str(currency), reason, 'skipped']:
                        self.assertIn(expected, detail)
                    if compact:
                        h = next(e['payload'] for e in got if e['type'] == 'market.close.history'
                                 and e['payload']['instrument_id'] == 'CCC')
                        self.assertNotIn('2026-11-27', [r['session'] for r in h['closes']])
                        self.assertEqual(h['closes'][-1]['close'], 49)
                    else:
                        self.assertFalse(any(e['type'] == 'market.bar.completed'
                                             and e['payload']['instrument_id'] == 'CCC'
                                             and e['event_time'].startswith('2026-11-27') for e in got))
                        self.assertEqual(bar_of(got, 'CCC', '2026-11-30')['adjusted']['close'], 49)

    def test_missing_or_malformed_ecb_refuses_both_encoders(self):
        for compact in (False, True):
            for malformed in (False, True):
                with self.subTest(compact=compact, malformed=malformed), tempfile.TemporaryDirectory() as d:
                    build(d)
                    set_currency(d, 'EUR')
                    if malformed:
                        rates_record(d, 'not a rates file')
                    with self.assertRaisesRegex(steps.MissingData, 'ECB'):
                        stream(d, compact)

    def test_boost_usd_is_byte_identical_and_never_reads_ecb(self):
        with tempfile.TemporaryDirectory() as d:
            build(d)
            path = Path(d) / 'dividends_2026-11-01_2026-11-30.json'
            rec = record.read(path)
            path.unlink()
            rec['rows'] = [dict(id='USD1', ticker='SPY', currency='USD', cash_amount=1,
                                ex_dividend_date='2026-11-27')]
            record.write(d, rec)
            args = dict(required=['SPY'], engine='boost')
            before = encode.encode(d, FIRST, LAST, IDENTITY, **args)
            rates_record(d, 'broken ECB input unrelated to Boost')
            with mock.patch.object(fx, 'load', side_effect=AssertionError('Boost reads ECB')):
                self.assertEqual(encode.encode(d, FIRST, LAST, IDENTITY, **args), before)
            self.assertEqual(bar_of(before, 'SPY', '2026-11-27')['adjusted']['close'], 600 * 600 / 599)

    def test_skipping_one_dividend_preserves_other_cash_on_the_same_date(self):
        for compact in (False, True):
            with self.subTest(compact=compact), tempfile.TemporaryDirectory() as d:
                build(d)
                set_currency(d, 'ZZZ')
                rates_record(d)
                path = Path(d) / 'dividends_2026-11-01_2026-11-30.json'
                rec = record.read(path)
                path.unlink()
                rec['rows'].append(dict(id='valid', ticker='CCC', ex_dividend_date='2026-11-27',
                                        cash_amount=1, currency='USD'))
                record.write(d, rec)
                excluded = {}
                got = stream(d, compact, excluded)
                if compact:
                    h = next(e['payload'] for e in got if e['type'] == 'market.close.history'
                             and e['payload']['instrument_id'] == 'CCC')
                    self.assertAlmostEqual(h['closes'][-1]['close'], 50)
                else:
                    self.assertAlmostEqual(bar_of(got, 'CCC', '2026-11-30')['adjusted']['close'], 50)
                self.assertIn('ZZZ', excluded['CCC'][0].reason)


class StableConversionTest(unittest.TestCase):
    def encoded(self, directory, compact):
        return ''.join(json.dumps(e, separators=(',', ':'), allow_nan=False) + '\n' for e in stream(directory, compact)).encode()

    def add_rates(self, directory, day, usd=2, gbp=4):
        return record.write(directory, dict(kind=fx.KIND, source=fx.SOURCE, session=day,
                            fetched_at=day + 'T23:30:00Z', url=fx.URL,
                            body='Date,USD,GBP\n{},{},{}\n'.format(day, usd, gbp)))

    def test_ex_date_fixing_arriving_after_publication_preserves_bytes(self):
        for compact in (False, True):
            with self.subTest(compact=compact), tempfile.TemporaryDirectory() as d:
                build(d)
                set_currency(d, 'GBP')
                self.add_rates(d, '2026-11-26')
                published = self.encoded(d, compact)
                self.add_rates(d, '2026-11-27', usd=8, gbp=4)
                self.assertEqual(self.encoded(d, compact), published)
                self.add_rates(d, '2026-12-01', usd=16, gbp=4)
                self.assertEqual(self.encoded(d, compact), published)

    def test_incomplete_prior_day_refuses_then_retries_stably(self):
        for compact in (False, True):
            for currency in ('GBP', 'ZZZ'):
                with self.subTest(compact=compact, currency=currency), tempfile.TemporaryDirectory() as d:
                    build(d)
                    set_currency(d, currency)
                    self.add_rates(d, '2026-11-25')
                    with self.assertRaisesRegex(steps.MissingData, 'coverage'):
                        self.encoded(d, compact)
                    self.add_rates(d, '2026-11-26', usd=4, gbp=4)
                    published = self.encoded(d, compact)
                    self.add_rates(d, '2026-11-27', usd=8, gbp=4)
                    self.assertEqual(self.encoded(d, compact), published)

    def test_malformed_currency_needs_no_fx_and_skips_member(self):
        for compact in (False, True):
            for currency in (None, '', 'gbp', [], {}, 1):
                with self.subTest(compact=compact, currency=currency), tempfile.TemporaryDirectory() as d:
                    build(d)
                    set_currency(d, currency)
                    excluded = {}
                    with mock.patch.object(fx, 'load', side_effect=AssertionError('malformed currency loads FX')):
                        got = stream(d, compact, excluded)
                    self.assertIn('currency', excluded['CCC'][0].reason)
                    self.assertTrue(got)

    def test_coverage_uses_union_even_when_newest_fixing_is_after_stream(self):
        for compact in (False, True):
            with self.subTest(compact=compact), tempfile.TemporaryDirectory() as d:
                build(d)
                set_currency(d, 'GBP')
                self.add_rates(d, '2026-11-25')
                self.add_rates(d, '2026-12-01')
                published = self.encoded(d, compact)
                self.add_rates(d, '2026-12-02', usd=8, gbp=4)
                self.assertEqual(self.encoded(d, compact), published)


class LazyRatesTest(unittest.TestCase):
    def test_member_first_observed_after_foreign_dividend_needs_no_fx(self):
        for compact in (False, True):
            with self.subTest(compact=compact), tempfile.TemporaryDirectory() as d:
                build(d)
                set_currency(d, 'GBP')
                for path in Path(d).glob('grouped_*.json'):
                    rec = record.read(path)
                    if rec['session'] <= '2026-11-27':
                        path.unlink()
                        rec['bars'] = [r for r in rec['bars'] if r['T'] != 'CCC']
                        record.write(d, rec)
                with mock.patch.object(fx, 'load', side_effect=AssertionError('unapplied dividend loads FX')):
                    got = stream(d, compact)
                self.assertTrue(got)


class FetchToIRAEncodingTest(unittest.TestCase):
    def build_april(self, directory):
        import datetime as dt
        from marketdata.tests.test_encode import row
        days = [dt.date(2026, 4, 1), dt.date(2026, 4, 2), dt.date(2026, 4, 6)]
        for day in days:
            record.write(directory, dict(kind='grouped', source='massive', session=str(day),
                         fetched_at=str(day) + 'T23:00:00Z', adjusted=False,
                         bars=[row('GPT', 20 if day < days[-1] else 19)]))
        record.write(directory, dict(kind='dividends', source='massive', session=str(days[-1]),
                     fetched_at='2026-04-06T23:00:00Z', **{'from': str(days[0])},
                     rows=[dict(id='synthetic', ticker='GPT', ex_dividend_date=str(days[-1]),
                                cash_amount=2, currency='GBP')]))
        record.write(directory, dict(kind='splits', source='massive', session=str(days[-1]),
                     fetched_at='2026-04-06T23:00:00Z', **{'from': str(days[0])}, rows=[]))
        return days

    def test_holiday_and_crash_fetches_reach_ira_encode(self):
        import datetime as dt
        from marketdata.tests.test_fx import FetchRecordingTest
        fetch = FetchRecordingTest().fetch
        text = 'Date,USD,GBP\n2026-04-02,2,4\n'
        for crash in (None, 'before', 'after'):
            with self.subTest(crash=crash), tempfile.TemporaryDirectory() as d:
                days = self.build_april(d)
                fetch(d, text, '2026-04-02T23:00:00')
                link = record.os.link
                def install(src, dst):
                    if crash == 'after':
                        link(src, dst)
                    raise OSError('synthetic crash')
                if crash:
                    with mock.patch.object(record.os, 'link', side_effect=install):
                        with self.assertRaisesRegex(OSError, 'synthetic crash'):
                            fetch(d, text + '2026-04-03,6,4\n2026-04-06,8,4\n', '2026-04-07T08:00:00')
                else:
                    fetch(d, text, '2026-04-07T08:00:00')
                def encoded():
                    return list(encode.iter_encode(d, days[0], days[-1], IDENTITY, required=['GPT']))
                if crash == 'before':
                    with self.assertRaisesRegex(steps.MissingData, 'coverage'):
                        encoded()
                else:
                    got = encoded()
                    self.assertEqual(fx.load(d).covered_through, dt.date(2026, 4, 6))
                    cash_usd = 3 if crash == 'after' else 1
                    self.assertAlmostEqual(bar_of(got, 'GPT', '2026-04-06')['adjusted']['close'],
                                           19 * 20 / (20 - cash_usd))
