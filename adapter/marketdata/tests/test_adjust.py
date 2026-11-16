"""Dividend currency rules, synthetic prices and reference rates only."""
import datetime as dt
import unittest

from marketdata import adjust, fx

D = dt.date
DAYS = [D(2026, 3, 3), D(2026, 3, 4), D(2026, 3, 5)]
RATES = fx.parse('Date,USD,GBP\n2026-03-04,2,4\n2026-03-03,2,4\n')


class DividendCurrencyTest(unittest.TestCase):
    def forward(self, currency, cash=2, rates=RATES, day='2026-03-04'):
        return adjust.forward_factors('AAA', DAYS, dict.fromkeys(DAYS, 20), [],
                                      [dict(ticker='AAA', ex_dividend_date=day, cash_amount=cash, currency=currency)],
                                      DAYS[0], rates=rates)

    def backward(self, currency, cash=2, rates=RATES, day='2026-03-04'):
        return adjust.adjusted_closes([(d, [dict(T='AAA', c=20)]) for d in DAYS], 'AAA', [],
                                      [dict(ticker='AAA', ex_dividend_date=day, cash_amount=cash, currency=currency)],
                                      rates=rates)

    def test_eur_gbp_usd_and_holiday_in_both_directions(self):
        for currency, cash_usd in [('EUR', 4), ('GBP', 1), ('USD', 2)]:
            for day in ['2026-03-04', '2026-03-05']:
                with self.subTest(currency=currency, day=day):
                    f = self.forward(currency, day=day)
                    b = self.backward(currency, day=day)
                    self.assertAlmostEqual(f[D.fromisoformat(day)][1], 20 / (20 - cash_usd))
                    self.assertAlmostEqual(b[0][1], 20 - cash_usd)
        self.assertEqual(self.forward('USD', rates=None)[DAYS[1]][1], 20 / 18)

    def test_cash_guard_refuses_each_currency_with_ticker_and_date(self):
        for method in [self.forward, self.backward]:
            for currency in ['USD', 'EUR', 'GBP']:
                for cash in [0, -1, 80, float('nan'), float('inf'), True]:
                    with self.subTest(currency=currency, cash=cash), self.assertRaisesRegex(
                            adjust.CannotAdjust, 'AAA.*2026-03-04'):
                        method(currency, cash)

    def test_missing_rates_are_a_recording_failure(self):
        with self.assertRaisesRegex(fx.MissingData, 'ECB'):
            self.forward('EUR', rates=None)
